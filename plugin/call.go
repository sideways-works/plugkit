package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/redact"
)

// Plugin is one executable the host calls, a process per call.
type Plugin struct {
	Name string
	Path string
	Dir  string
	// Env is the plugin's whole environment; nil gives it Env(nil, nil), never the host's own.
	Env     []string
	Timeout time.Duration
	// Log takes, a line at a time under the plugin's name, what it wrote on stderr during a call
	// that answered; a failed call carries it in its error.
	Log io.Writer
}

var baseVars = []string{"PATH", "HOME", "USER", "LANG", "LANGUAGE", "TZ", "TMPDIR"}

// Env is a plugin's environment: the host's basics and locale, the variables keep names, and
// set's in name order; no token, proxy or setting of the host's reaches it otherwise.
func Env(keep []string, set map[string]string) []string {
	var env []string

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")

		if slices.Contains(baseVars, name) || slices.Contains(keep, name) || strings.HasPrefix(name, "LC_") {
			env = append(env, kv)
		}
	}

	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		env = append(env, name+"="+set[name])
	}

	return env
}

func (p Plugin) env() []string {
	if p.Env == nil {
		return Env(nil, nil)
	}

	return p.Env
}

// Call sends req as JSON on stdin and decodes stdout into answer; exit code ExitCantTell is
// ErrCantTell.
func (p Plugin) Call(ctx context.Context, call string, req, answer any) error {
	if p.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	in, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrFailed, p.Name, call, err)
	}

	cmd := exec.CommandContext(ctx, p.Path, call) //nolint:gosec // a plugin the host pins by checksum
	cmd.Stdin = bytes.NewReader(in)
	cmd.Dir = p.Dir
	cmd.Env = p.env()

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == ExitCantTell {
			// why it can't tell is part of the answer (a missing toolchain, no evidence)
			return &cantTellError{plugin: p.Name, call: call, message: redact.Redact(tail(stderr.String(), 5))}
		}

		return fmt.Errorf("%w: %s %s: %w: %s", ErrFailed, p.Name, call, err, redact.Redact(tail(stderr.String(), 15)))
	}

	logLines(p.Log, p.Name+" "+call, stderr.String())

	if answer == nil {
		return nil
	}

	if err := json.Unmarshal(stdout.Bytes(), answer); err != nil {
		return fmt.Errorf("%w: %s %s: unreadable answer: %w", ErrFailed, p.Name, call, err)
	}

	return nil
}

type cantTellError struct {
	plugin, call, message string
}

func (e *cantTellError) Error() string {
	return fmt.Sprintf("%s %s: %v: %s", e.plugin, e.call, ErrCantTell, e.message)
}

func (e *cantTellError) Unwrap() error { return ErrCantTell }

// CantTellMessage is what a plugin that couldn't tell said about why, on one line, redacted.
func CantTellMessage(err error) string {
	ce, ok := errors.AsType[*cantTellError](err)
	if !ok {
		return ""
	}

	return strings.Join(strings.Fields(strings.ReplaceAll(ce.message, "\n", "; ")), " ")
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}

// logLines writes text to log a line at a time under prefix, secrets redacted.
func logLines(log io.Writer, prefix, text string) {
	if log == nil || strings.TrimSpace(text) == "" {
		return
	}

	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if _, err := fmt.Fprintf(log, "%s: %s\n", prefix, redact.Redact(line)); err != nil {
			return
		}
	}
}
