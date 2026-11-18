// Package kit is the generic part of a plugin conformance kit: it calls a plugin binary against
// a kind's fixtures in work directories of their own, collects each check's result, and holds
// the checks every kind shares (unknown calls refused, the version described, no token leaked).
package kit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sideways-works/plugkit/plugin"
	"github.com/sideways-works/plugkit/semver"
)

type Result struct {
	Check   string
	Problem string
	Skipped bool
	Note    bool
}

// Caller calls the plugin under test once.
type Caller interface {
	Call(ctx context.Context, call string, req, answer any) error
}

// Harness runs one plugin's checks. Canary is a value put where a credential would be: a plugin
// never gets a credential, so no answer or error it gives may carry it.
type Harness struct {
	Plugin Caller
	Work   string
	Canary string

	results []Result
	calls   int
	seen    strings.Builder
}

// Call calls the plugin, keeping what it answered and any error for CheckCanary.
func (h *Harness) Call(ctx context.Context, call string, req, answer any) error {
	err := h.Plugin.Call(ctx, call, req, answer)
	h.Saw(err, answer)

	return err
}

// Saw keeps, for CheckCanary, what a call made some other way answered.
func (h *Harness) Saw(err error, answer any) {
	if err != nil {
		h.seen.WriteString(err.Error())
	}

	h.seen.WriteString(Canonical(answer))
}

// Check adds a result: passed when problem is empty, else failed with problem formatted by args.
func (h *Harness) Check(name, problem string, args ...any) {
	if problem != "" && len(args) > 0 {
		problem = fmt.Sprintf(problem, args...)
	}

	h.results = append(h.results, Result{Check: name, Problem: problem})
}

func (h *Harness) Skip(name, why string) {
	h.results = append(h.results, Result{Check: name, Problem: why, Skipped: true})
}

func (h *Harness) Add(r Result) { h.results = append(h.results, r) }

func (h *Harness) Results() []Result { return h.results }

// WorkDir is a new directory under Work for one call.
func (h *Harness) WorkDir() string {
	h.calls++
	dir := filepath.Join(h.Work, fmt.Sprintf("call-%d", h.calls))
	_ = os.MkdirAll(dir, 0o750) //nolint:errcheck // a failure shows as the call's own

	return dir
}

// Refuses checks that each call fails, and isn't "can't tell", which is an answer.
func (h *Harness) Refuses(ctx context.Context, request func() any, calls ...string) {
	for _, call := range calls {
		err := h.Call(ctx, call, request(), nil)

		switch {
		case err == nil:
			h.Check("refuses "+call, "answered it")
		case errors.Is(err, plugin.ErrCantTell):
			h.Check("refuses "+call, "said it can't tell, which is an answer")
		default:
			h.Check("refuses "+call, "")
		}
	}
}

// CheckVersion fails a plugin whose describe names another version than the release it was
// built from; a leading v doesn't count.
func (h *Harness) CheckVersion(described, release string) {
	const name = "describe names the release's version"

	d, errD := semver.Parse(described)
	r, errR := semver.Parse(release)

	switch {
	case errR != nil:
		h.Check(name, "the release's version: %v", errR)
	case errD != nil:
		h.Check(name, "describe's version: %v", errD)
	case d.Compare(r) != 0 || d.Build != r.Build:
		h.Check(name, "describe says %s, the release is %s", described, release)
	default:
		h.Check(name, "")
	}
}

// CheckCanary fails a plugin when anything it answered or said carries the canary.
func (h *Harness) CheckCanary() {
	problem := ""
	if h.Canary != "" && strings.Contains(h.seen.String(), h.Canary) {
		problem = "the token's value came back"
	}

	h.Check("no answer or message carries the token", problem)
}

// SetEnv sets a variable for the checks and returns what restores the caller's.
func SetEnv(name, value string) func() {
	old, had := os.LookupEnv(name)
	_ = os.Setenv(name, value) //nolint:errcheck // a failure leaves the check weaker, not wrong

	return func() {
		if had {
			_ = os.Setenv(name, old) //nolint:errcheck // restoring the caller's environment
		} else {
			_ = os.Unsetenv(name) //nolint:errcheck // restoring the caller's environment
		}
	}
}

// Canonical is v as JSON, or the error encoding it.
func Canonical(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}

	return string(data)
}

// Print writes one line per result and returns how many failed.
func Print(w io.Writer, results []Result) (int, error) {
	failed := 0

	for _, r := range results {
		var err error

		switch {
		case r.Skipped:
			_, err = fmt.Fprintf(w, "skip  %s: %s\n", r.Check, r.Problem)
		case r.Note:
			_, err = fmt.Fprintf(w, "note  %s: %s\n", r.Check, r.Problem)
		case r.Problem == "":
			_, err = fmt.Fprintf(w, "ok    %s\n", r.Check)
		default:
			failed++
			_, err = fmt.Fprintf(w, "FAIL  %s: %s\n", r.Check, r.Problem)
		}

		if err != nil {
			return failed, fmt.Errorf("write output: %w", err)
		}
	}

	return failed, nil
}
