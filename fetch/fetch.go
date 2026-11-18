// Package fetch downloads a pinned release's file for a platform into the cache and checks it
// against its pin: a file that doesn't match is never kept, so it never runs. Files are cached
// by their checksum.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/pin"
)

var ErrChecksum = errors.New("checksum mismatch")

// ErrPin is a pin no file can be fetched or checked by.
var ErrPin = errors.New("pin")

// ErrNotSupported is a release on a plain git server, which is built from its tag; fetch can't
// build one yet.
var ErrNotSupported = errors.New("not supported yet")

const (
	sumsFile = "SHA256SUMS"
	sumsDir  = "sums"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Got is a fetched file: its path, its checksum, and with a pin by sums the release's
// SHA256SUMS, which a plugins directory keeps beside the file as the bootstrap does.
type Got struct {
	Path   string
	SHA256 string
	Sums   []byte
}

// Fetcher downloads into Cache, under the directory Under names.
type Fetcher struct {
	Client *http.Client
	Cache  string
	Under  string
	// Forges names the kind of git server (pin.GitHub, pin.GitLab) of a host other than
	// github.com and gitlab.com.
	Forges map[string]string
}

// Fetch returns the pinned file for platform: the cache's copy when an earlier fetch put it
// there, otherwise one downloaded and checked against the pin.
func (f Fetcher) Fetch(ctx context.Context, p pin.Pin, platform string) (Got, error) {
	files, sums, err := f.wanted(ctx, p, platform)
	if err != nil {
		return Got{}, err
	}

	for _, w := range files {
		dest := filepath.Join(f.Cache, f.Under, w.sum, p.ProgramName())
		if fileSum(dest) == w.sum {
			used(dest)

			return Got{Path: dest, SHA256: w.sum, Sums: sums}, nil
		}
	}

	for i, w := range files {
		data, err := f.download(ctx, p, w.file)
		if errors.Is(err, errNotFound) && i < len(files)-1 {
			continue
		}

		if err != nil {
			return Got{}, err
		}

		if sum := hexSum(data); sum != w.sum {
			return Got{}, fmt.Errorf("%w: %s %s: %s is %s, pinned %s", ErrChecksum, p.Name, p.Version, w.file, sum, w.sum)
		}

		dest := filepath.Join(f.Cache, f.Under, w.sum, p.ProgramName())
		if err := keep(dest, data); err != nil {
			return Got{}, fmt.Errorf("fetch %s: %w", p.Name, err)
		}

		return Got{Path: dest, SHA256: w.sum, Sums: sums}, nil
	}

	return Got{}, fmt.Errorf("%w: %s %s has nothing for %s", ErrPin, p.Name, p.Version, platform)
}

type wantedFile struct{ file, sum string }

// wanted are the files that may hold the pin's program for platform, in the order to try them,
// each with the checksum it must have.
func (f Fetcher) wanted(ctx context.Context, p pin.Pin, platform string) ([]wantedFile, []byte, error) {
	if p.Sums != "" {
		sums, err := f.sums(ctx, p)
		if err != nil {
			return nil, nil, err
		}

		listed := pin.Listed(sums)

		for _, file := range []string{p.Binary(platform), p.ScriptFile()} {
			if sum, ok := listed[file]; ok && sha256Hex.MatchString(sum) {
				return []wantedFile{{file, sum}}, sums, nil
			}
		}

		return nil, nil, fmt.Errorf("%w: %s %s: its %s lists no %s or %s", ErrPin, p.Name, p.Version, sumsFile, p.Binary(platform), p.ScriptFile())
	}

	var out []wantedFile

	if sum, ok := p.SHA256.For(platform); ok && sha256Hex.MatchString(strings.ToLower(sum)) {
		out = append(out, wantedFile{p.Binary(platform), strings.ToLower(sum)})
	}

	if sha256Hex.MatchString(strings.ToLower(p.Script)) {
		out = append(out, wantedFile{p.ScriptFile(), strings.ToLower(p.Script)})
	}

	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%w: %s %s has no checksum for %s: a release pinned by one sha256 is checked by its linux-amd64 file's; "+
			"pin it by sums (the checksum of its %s) to fetch it for another platform", ErrPin, p.Name, p.Version, platform, sumsFile)
	}

	return out, nil, nil
}

// sums is the release's SHA256SUMS once its checksum is the pin's: the cache's copy, kept by
// that checksum, or a download.
func (f Fetcher) sums(ctx context.Context, p pin.Pin) ([]byte, error) {
	want := strings.ToLower(p.Sums)
	if !sha256Hex.MatchString(want) {
		return nil, fmt.Errorf("%w: %s %s: sums %q is no sha256", ErrPin, p.Name, p.Version, p.Sums)
	}

	kept := filepath.Join(f.Cache, f.Under, sumsDir, want)
	if data, err := os.ReadFile(kept); err == nil && hexSum(data) == want { //nolint:gosec // the cache's own file, checked by its checksum
		used(kept)

		return data, nil
	}

	data, err := f.download(ctx, p, sumsFile)
	if err != nil {
		return nil, err
	}

	if sum := hexSum(data); sum != want {
		return nil, fmt.Errorf("%w: %s %s: %s is %s, pinned %s", ErrChecksum, p.Name, p.Version, sumsFile, sum, want)
	}

	if err := keep(kept, data); err != nil {
		return nil, fmt.Errorf("fetch %s: %w", p.Name, err)
	}

	return data, nil
}

var errNotFound = errors.New("not found")

func (f Fetcher) download(ctx context.Context, p pin.Pin, file string) ([]byte, error) {
	u, err := f.fileURL(p, file)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", p.Name, err)
	}

	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", p.Name, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: fetch %s: %s: %w (%s)", ErrPin, p.Name, u, errNotFound, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: fetch %s: %s: %s", ErrPin, p.Name, u, resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %s: %w", p.Name, u, err)
	}

	if len(data) > maxFile {
		return nil, fmt.Errorf("%w: fetch %s: %s is over %d bytes", ErrPin, p.Name, u, maxFile)
	}

	return data, nil
}

// fileURL is where a release's file is: the pin's URL pattern when it has one, else the release
// asset of its forge.
func (f Fetcher) fileURL(p pin.Pin, file string) (string, error) {
	if p.URL != "" {
		return fmt.Sprintf(p.URL, p.Repository, p.Tag(), file), nil
	}

	repo, err := pin.ParseRepository(p.Repository, f.Forges)
	if err != nil {
		return "", fmt.Errorf("%w: fetch %s: %w", ErrPin, p.Name, err)
	}

	switch repo.Forge {
	case pin.GitHub:
		return repo.Origin + "/" + repo.Project + "/releases/download/" + p.Tag() + "/" + file, nil
	case pin.GitLab:
		return repo.Origin + "/" + repo.Project + "/-/releases/" + url.PathEscape(p.Tag()) + "/downloads/" + file, nil
	}

	return "", fmt.Errorf("%w: fetch %s: %s is a plain git server, whose releases are built from their tag: %w", ErrPin, p.Name, p.Repository, ErrNotSupported)
}

// maxFile bounds what a download may read, so a wrong URL can't fill memory or the cache.
const maxFile = 200 << 20

// keep writes data to dest by a rename, so a reader never sees half a file.
func keep(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "download-*")
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone once renamed

	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Chmod(tmp.Name(), 0o750) //nolint:gosec // an executable
	}

	if err == nil {
		err = os.Rename(tmp.Name(), dest)
	}

	return err //nolint:wrapcheck // the caller says whose
}

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

// FileSum is a file's SHA-256 in hex, or empty when it can't be read.
func FileSum(p string) string { return fileSum(p) }

func fileSum(p string) string {
	sum, err := pin.FileSHA256(p)
	if err != nil {
		return ""
	}

	return sum
}

// Accepts are the checksums a file for platform may have to match the pin.
func (f Fetcher) Accepts(ctx context.Context, p pin.Pin, platform string) ([]string, error) {
	files, _, err := f.wanted(ctx, p, platform)

	out := make([]string, 0, len(files))
	for _, w := range files {
		out = append(out, w.sum)
	}

	return out, err
}

// used marks a kept file as used now, so Trim keeps it.
func used(p string) {
	now := time.Now()
	_ = os.Chtimes(p, now, now) //nolint:errcheck // at worst it is fetched again
}

// Trim removes what dir (Cache joined with Under) holds that no fetch used for longer than keep:
// each release file's directory and each SHA256SUMS kept.
func Trim(dir string, keep time.Duration, now time.Time) ([]string, error) {
	var removed []string

	old := func(p string) bool {
		info, err := os.Stat(p)

		return err == nil && now.Sub(info.ModTime()) > keep
	}

	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("trim %s: %w", dir, err)
	}

	for _, e := range ents {
		p := filepath.Join(dir, e.Name())

		switch {
		case e.Name() == sumsDir:
			lists, err := os.ReadDir(p)
			if err != nil {
				return removed, fmt.Errorf("trim %s: %w", dir, err)
			}

			for _, l := range lists {
				if lp := filepath.Join(p, l.Name()); old(lp) {
					if err := os.Remove(lp); err != nil {
						return removed, fmt.Errorf("trim %s: %w", dir, err)
					}

					removed = append(removed, lp)
				}
			}
		case e.IsDir() && sha256Hex.MatchString(e.Name()) && allOld(p, old):
			if err := os.RemoveAll(p); err != nil {
				return removed, fmt.Errorf("trim %s: %w", dir, err)
			}

			removed = append(removed, p)
		}
	}

	return removed, nil
}

// allOld says whether every file in the release file's directory is old: one used lately keeps it.
func allOld(dir string, old func(string) bool) bool {
	files, err := os.ReadDir(dir)
	if err != nil {
		return false
	}

	for _, f := range files {
		if !old(filepath.Join(dir, f.Name())) {
			return false
		}
	}

	return true
}
