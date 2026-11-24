package repository

import (
	"context"
	"errors"
	"fmt"
	"path"
)

// MaxReadme bounds a plugin's README in a release.
const MaxReadme = 1 << 20

// ErrReadme is a README a release couldn't give.
var ErrReadme = errors.New("readme")

// readme is README.md in dir at commit: none when there is no such file, an error when it is
// something other than a file or over MaxReadme bytes. Its size is read before its content.
func (g *git) readme(ctx context.Context, commit, dir string) ([]byte, error) {
	name := path.Join(dir, "README.md")

	data, _, err := g.blob(ctx, commit, name, MaxReadme)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", ErrReadme, name, err)
	}

	return data, nil
}
