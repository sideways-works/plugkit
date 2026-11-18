package kit

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/sideways-works/plugkit/plugin"
)

const canary = "kit-canary-31d7"

// TestMain lets the test binary act as the plugin under test when asked.
func TestMain(m *testing.M) {
	if os.Getenv("KIT_TEST_PLUGIN") == "" {
		os.Exit(m.Run())
	}

	os.Exit(plugin.Serve(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, map[string]plugin.Handler{
		"describe": func([]byte) (any, error) { return map[string]string{"version": "v0.4.0"}, nil },
		"cant":     func([]byte) (any, error) { return nil, plugin.ErrCantTell },
		"leak":     func([]byte) (any, error) { return os.Getenv("KIT_TEST_TOKEN"), nil },
	}))
}

func harness(t *testing.T) *Harness {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	p := plugin.Plugin{Name: "fake", Path: self, Env: append(plugin.Env(nil, nil), "KIT_TEST_PLUGIN=1", "KIT_TEST_TOKEN="+os.Getenv("KIT_TEST_TOKEN"))}

	return &Harness{Plugin: p, Work: t.TempDir(), Canary: canary}
}

func results(h *Harness) map[string]string {
	out := map[string]string{}
	for _, r := range h.Results() {
		out[r.Check] = r.Problem
	}

	return out
}

func TestRefusals(t *testing.T) {
	h := harness(t)
	h.Refuses(t.Context(), func() any { return plugin.Request{Work: h.WorkDir()} }, "no-such-call", "cant", "describe")

	got := results(h)
	if got["refuses no-such-call"] != "" || got["refuses cant"] != "said it can't tell, which is an answer" || got["refuses describe"] != "answered it" {
		t.Fatalf("%q", got)
	}
}

func TestTheDescribedVersionIsTheReleases(t *testing.T) {
	h := harness(t)

	var d map[string]string
	if err := h.Call(t.Context(), "describe", plugin.Request{}, &d); err != nil {
		t.Fatal(err)
	}

	for release, want := range map[string]string{
		"v0.4.0": "", "0.4.0": "", "v0.4.1": "describe says v0.4.0, the release is v0.4.1", "main": `the release's version: version "main": needs major.minor.patch`,
	} {
		h := &Harness{}
		h.CheckVersion(d["version"], release)

		if got := h.Results()[0].Problem; got != want {
			t.Errorf("%s: %q", release, got)
		}
	}
}

func TestTheCanaryNeverComesBack(t *testing.T) {
	h := harness(t)
	if err := h.Call(t.Context(), "describe", plugin.Request{}, nil); err != nil {
		t.Fatal(err)
	}

	h.CheckCanary()

	restore := SetEnv("KIT_TEST_TOKEN", canary)
	leaky := harness(t)

	restore()

	var answer string
	if err := leaky.Call(t.Context(), "leak", plugin.Request{}, &answer); err != nil || answer != canary {
		t.Fatalf("%q %v", answer, err)
	}

	leaky.CheckCanary()

	if p := results(h)["no answer or message carries the token"]; p != "" {
		t.Errorf("a clean plugin: %q", p)
	}

	if p := results(leaky)["no answer or message carries the token"]; p == "" {
		t.Error("a plugin that leaks the token passed")
	}

	if _, ok := os.LookupEnv("KIT_TEST_TOKEN"); ok {
		t.Error("SetEnv left the variable behind")
	}
}

type failing struct{}

var errWrite = errors.New("closed")

func (failing) Write([]byte) (int, error) { return 0, errWrite }

func TestPrint(t *testing.T) {
	rs := []Result{{Check: "a"}, {Check: "b", Problem: "bad"}, {Check: "c", Problem: "why", Skipped: true}, {Check: "d", Problem: "fyi", Note: true}}

	var out bytes.Buffer

	failed, err := Print(&out, rs)
	if err != nil || failed != 1 || out.String() != "ok    a\nFAIL  b: bad\nskip  c: why\nnote  d: fyi\n" {
		t.Fatalf("%d %v %q", failed, err, out.String())
	}

	if _, err := Print(failing{}, rs); !errors.Is(err, errWrite) {
		t.Fatalf("%v", err)
	}
}

func TestEachCallHasAWorkDirectoryOfItsOwn(t *testing.T) {
	h := harness(t)

	a, b := h.WorkDir(), h.WorkDir()
	if a == b {
		t.Fatal(a)
	}

	for _, d := range []string{a, b} {
		if info, err := os.Stat(d); err != nil || !info.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
}
