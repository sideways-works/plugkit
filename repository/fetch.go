package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sideways-works/plugkit/manifest"
)

// MaxFetch bounds each file Options.Fetch names.
const MaxFetch = 1 << 20

// ErrFetch is a file Options.Fetch named that couldn't be given.
var ErrFetch = errors.New("fetch")

// Fetched is a file Options.Fetch named: its content and media type by its content, or Err.
type Fetched struct {
	Data []byte
	Type string
	Err  error
}

// fetched reads each name: a file of the repository at commit, by its path from the repository's
// root, or an https URL fetched for version as a logo's is, from opt.LogoCache when kept there.
func (g *git) fetched(ctx context.Context, commit, version string, names []string, opt Options) map[string]Fetched {
	if len(names) == 0 {
		return nil
	}

	out := map[string]Fetched{}

	for _, name := range names {
		if _, done := out[name]; done {
			continue
		}

		var (
			data []byte
			err  error
		)

		switch {
		case manifest.LogoURL(name):
			data, err = fetchURL(ctx, name, version, opt)
		case local(name):
			var found bool

			data, found, err = g.blob(ctx, commit, name, MaxFetch)
			if err == nil && !found {
				err = fmt.Errorf("%w at %s", errNoFile, commit)
			}
		default:
			err = errNoName
		}

		if err != nil {
			out[name] = Fetched{Err: fmt.Errorf("%w %s: %w", ErrFetch, name, err)}

			continue
		}

		out[name] = Fetched{Data: data, Type: mediaType(data)}
	}

	return out
}

// mediaType is data's media type by its content: an image a listing shows, or what net/http
// detects.
func mediaType(data []byte) string {
	if t := imageType(data); t != "" {
		return t
	}

	return http.DetectContentType(data)
}

// fetchURL is the file at the https URL u for version, kept in opt.LogoCache when it is set.
func fetchURL(ctx context.Context, u, version string, opt Options) ([]byte, error) {
	kept := ""

	if opt.LogoCache != "" {
		sum := sha256.Sum256([]byte("fetch\x00" + u + "\x00" + version))
		kept = filepath.Join(opt.LogoCache, hex.EncodeToString(sum[:]))

		if data, err := os.ReadFile(kept); err == nil && len(data) <= MaxFetch { //nolint:gosec // the cache's own file, by its hash
			return data, nil
		}
	}

	data, err := download(ctx, u, opt, MaxFetch)
	if err != nil {
		return nil, err
	}

	if kept != "" {
		// a file not kept is fetched again, and nothing else changes
		_ = keepFile(kept, data) //nolint:errcheck // as above
	}

	return data, nil
}

// blob is the file name at commit, within limit bytes: found false when there is none, an error
// when it is something other than a file or over limit. Its size is read before its content.
func (g *git) blob(ctx context.Context, commit, name string, limit int64) ([]byte, bool, error) {
	out, err := g.run(ctx, "ls-tree", "-l", commit, "--", name)
	if err != nil {
		return nil, false, err
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, false, nil
	}

	// <mode> SP <type> SP <object> SP <size> TAB <path>
	meta, _, _ := strings.Cut(line, "\t")

	f := strings.Fields(meta)
	if len(f) != 4 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
		return nil, false, errNotAFile
	}

	if n, err := strconv.ParseInt(f[3], 10, 64); err != nil || n > limit {
		return nil, false, fmt.Errorf("%w: over %d bytes", errTooBig, limit)
	}

	data, err := g.run(ctx, "cat-file", "blob", f[2])
	if err != nil {
		return nil, false, err
	}

	return data, true, nil
}

var (
	errNotAFile = errors.New("not a file")
	errNoFile   = errors.New("no such file")
	errNoName   = errors.New("neither a path of the repository, without .. or ., nor an https URL")
)
