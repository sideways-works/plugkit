package repository

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// MaxReadme bounds a plugin's README in a release.
const MaxReadme = 1 << 20

// ErrReadme is a README a release couldn't give.
var ErrReadme = errors.New("readme")

// readme is README.md in dir at commit: none when there is no such file, an error when it is
// something other than a file or over MaxReadme bytes. Its size is read before its content.
func (g *git) readme(ctx context.Context, commit, dir string) ([]byte, error) {
	name := path.Join(dir, "README.md")

	out, err := g.run(ctx, "ls-tree", "-l", commit, "--", name)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrReadme, name, err)
	}

	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, nil
	}

	// <mode> SP <type> SP <object> SP <size> TAB <path>
	meta, _, _ := strings.Cut(line, "\t")

	f := strings.Fields(meta)
	if len(f) != 4 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
		return nil, fmt.Errorf("%w %s: not a file", ErrReadme, name)
	}

	if n, err := strconv.ParseInt(f[3], 10, 64); err != nil || n > MaxReadme {
		return nil, fmt.Errorf("%w %s: over %d bytes", ErrReadme, name, MaxReadme)
	}

	data, err := g.run(ctx, "cat-file", "blob", f[2])
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrReadme, name, err)
	}

	return data, nil
}
