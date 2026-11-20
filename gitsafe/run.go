package gitsafe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

var ErrTooBig = errors.New("over the size limit")

// Run runs the command newCmd makes with a context of Run's own, which it cancels once dir holds
// more than maxBytes (no limit when maxBytes is 0 or less), and returns the command's output. An
// error says what the command printed on stderr.
func Run(ctx context.Context, newCmd func(context.Context) *exec.Cmd, dir string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := newCmd(ctx)

	var out, errOut bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &errOut

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(cmd.Path), err)
	}

	over := watch(ctx, cancel, dir, maxBytes)

	err := cmd.Wait()

	switch {
	case over():
		return nil, fmt.Errorf("%w of %d bytes", ErrTooBig, maxBytes)
	case ctx.Err() != nil && err != nil:
		return nil, fmt.Errorf("%w: %w", err, context.Cause(ctx))
	case err != nil:
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}

	return out.Bytes(), nil
}

// watch cancels once dir holds more than maxBytes and says whether it did; with no limit it
// watches nothing.
func watch(ctx context.Context, cancel context.CancelFunc, dir string, maxBytes int64) func() bool {
	if maxBytes <= 0 {
		return func() bool { return false }
	}

	var over atomic.Bool

	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if sizeOf(dir) > maxBytes {
					over.Store(true)
					cancel()

					return
				}
			}
		}
	}()

	return func() bool {
		// a last look, so a command that ended between two ticks is measured too
		if sizeOf(dir) > maxBytes {
			over.Store(true)
		}

		return over.Load()
	}
}

func sizeOf(dir string) int64 {
	var n int64

	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck // a file gone mid-walk counts as nothing
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}

		return nil
	})

	return n
}
