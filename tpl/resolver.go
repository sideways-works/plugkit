package tpl

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// NotFoundError is a file a resolver doesn't have at Commit.
type NotFoundError struct {
	Commit string
}

// Error names the commit by its first 12 digits.
func (e *NotFoundError) Error() string {
	if e.Commit == "" {
		return "no such file"
	}

	return "no such file at " + abbrev(e.Commit)
}

// Is makes a NotFoundError match fs.ErrNotExist.
func (e *NotFoundError) Is(target error) bool { return target == fs.ErrNotExist }

func abbrev(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}

	return commit
}

// MapResolver serves includes from maps, for tests and callers that hold
// the files already.
type MapResolver struct {
	// Commit is the repository commit Files are at, named in errors.
	Commit string
	// Files maps repository paths to contents.
	Files map[string]string
	// Remotes maps "<git URL>//<path>@<commit>" to contents.
	Remotes map[string]string
}

// Local returns Files[path].
func (m MapResolver) Local(_ context.Context, path string) ([]byte, error) {
	s, ok := m.Files[path]
	if !ok {
		return nil, &NotFoundError{Commit: m.Commit}
	}

	return []byte(s), nil
}

// Remote returns the entry of Remotes for the file.
func (m MapResolver) Remote(_ context.Context, gitURL, path, commit string) ([]byte, error) {
	s, ok := m.Remotes[remoteRef(gitURL, path, commit)]
	if !ok {
		return nil, &NotFoundError{Commit: commit}
	}

	return []byte(s), nil
}

// ErrNoRemote is returned by a resolver that can't read remote includes.
var ErrNoRemote = errors.New("remote includes are not available here")

// FSResolver reads repository paths from a file system, such as a checkout
// of Commit. It reads no remote includes.
type FSResolver struct {
	FS     fs.FS
	Commit string
}

// Local reads path from the file system.
func (f FSResolver) Local(_ context.Context, path string) ([]byte, error) {
	b, err := fs.ReadFile(f.FS, path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &NotFoundError{Commit: f.Commit}
	}

	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return b, nil
}

// Remote returns ErrNoRemote.
func (f FSResolver) Remote(context.Context, string, string, string) ([]byte, error) {
	return nil, ErrNoRemote
}
