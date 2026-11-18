package fetch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sideways-works/plugkit/pin"
)

type releases struct {
	*httptest.Server

	files map[string][]byte
	hits  atomic.Int32
}

func newReleases(t *testing.T, files map[string][]byte) *releases {
	t.Helper()

	r := &releases{files: files}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.hits.Add(1)

		data, ok := r.files[req.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(data) //nolint:errcheck // a test server
	}))
	t.Cleanup(r.Close)

	return r
}

const (
	release     = "/team/shell-driver/releases/download/v0.2.0/"
	platform    = "linux-arm64"
	linuxAMD64  = "linux-amd64"
	darwinARM64 = "darwin-arm64"
)

func (r *releases) pin() pin.Pin {
	return pin.Pin{Name: "shell", Repository: "team/shell-driver", Version: "v0.2.0", URL: r.URL + "/%s/releases/download/%s/%s"}
}

func (r *releases) fetcher(t *testing.T) Fetcher {
	t.Helper()

	return Fetcher{Client: r.Client(), Cache: t.TempDir(), Under: "releases"}
}

func TestAFileThatMatchesItsPinIsKeptAndFetchedOnce(t *testing.T) {
	bin := []byte("the shell driver for linux-arm64")
	r := newReleases(t, map[string][]byte{release + "shell-driver-" + platform: bin})
	f := r.fetcher(t)

	p := r.pin()
	p.SHA256 = pin.Checksums{platform: hexSum(bin)}

	got, err := f.Fetch(t.Context(), p, platform)
	if err != nil {
		t.Fatal(err)
	}

	if data, err := os.ReadFile(got.Path); err != nil || string(data) != string(bin) || got.SHA256 != hexSum(bin) {
		t.Fatalf("%+v: %q %v", got, data, err)
	}

	if again, err := f.Fetch(t.Context(), p, platform); err != nil || again.Path != got.Path || r.hits.Load() != 1 {
		t.Fatalf("again: %+v %v, %d downloads", again, err, r.hits.Load())
	}
}

func TestAFileThatDoesntMatchItsPinIsRefusedAndNotKept(t *testing.T) {
	r := newReleases(t, map[string][]byte{release + "shell-driver-" + platform: []byte("something else")})
	f := r.fetcher(t)

	p := r.pin()
	p.SHA256 = pin.Checksums{platform: hexSum([]byte("the pinned one"))}

	if _, err := f.Fetch(t.Context(), p, platform); !errors.Is(err, ErrChecksum) {
		t.Fatalf("err = %v", err)
	}

	if left, _ := filepath.Glob(filepath.Join(f.Cache, "releases", "*", "*")); len(left) != 0 { //nolint:errcheck // a valid pattern
		t.Fatalf("kept %v", left)
	}
}

// A pin by sums checks the release's SHA256SUMS by its checksum and the file by its line there,
// and keeps the list, so a second fetch downloads nothing.
func TestAPinBySumsChecksTheListAndThenTheFile(t *testing.T) {
	bin := []byte("pinned")
	sums := []byte(hexSum(bin) + "  shell-driver-" + platform + "\n" + hexSum([]byte("x")) + "  shell-driver-plan9-mips\n")
	r := newReleases(t, map[string][]byte{release + "shell-driver-" + platform: bin, release + "SHA256SUMS": sums})
	f := r.fetcher(t)

	p := r.pin()
	p.Sums = hexSum(sums)

	got, err := f.Fetch(t.Context(), p, platform)
	if err != nil {
		t.Fatal(err)
	}

	if got.SHA256 != hexSum(bin) || string(got.Sums) != string(sums) {
		t.Fatalf("%+v", got)
	}

	if _, err := f.Fetch(t.Context(), p, platform); err != nil || r.hits.Load() != 2 {
		t.Fatalf("again: %v, %d downloads", err, r.hits.Load())
	}

	p.Sums = hexSum([]byte("another list"))
	if _, err := r.fetcher(t).Fetch(t.Context(), p, platform); !errors.Is(err, ErrChecksum) {
		t.Fatalf("another list: %v", err)
	}

	p.Sums = hexSum(sums)
	if _, err := r.fetcher(t).Fetch(t.Context(), p, "darwin-arm64"); !errors.Is(err, ErrPin) {
		t.Fatalf("an unlisted platform: %v", err)
	}
}

// A Node plugin is released as one .mjs file: a pin by one sha256 is that file's checksum, on
// any platform.
func TestANodePluginIsItsScript(t *testing.T) {
	script := []byte("#!/usr/bin/env node\n")
	r := newReleases(t, map[string][]byte{release + "shell-driver.mjs": script})

	p := r.pin()
	p.SHA256 = pin.Checksums{linuxAMD64: hexSum(script)}
	p.Script = hexSum(script)

	for _, platform := range []string{linuxAMD64, darwinARM64} {
		if got, err := r.fetcher(t).Fetch(t.Context(), p, platform); err != nil || got.SHA256 != hexSum(script) {
			t.Fatalf("%s: %+v %v", platform, got, err)
		}
	}
}

// A pin by one sha256 names the linux-amd64 file: on another platform nothing can be checked by
// it, so nothing is downloaded.
func TestASHA256PinChecksNoOtherPlatform(t *testing.T) {
	r := newReleases(t, nil)

	p := r.pin()
	p.SHA256 = pin.Checksums{linuxAMD64: hexSum([]byte("x"))}

	if _, err := r.fetcher(t).Fetch(t.Context(), p, darwinARM64); !errors.Is(err, ErrPin) || r.hits.Load() != 0 {
		t.Fatalf("%v, %d downloads", err, r.hits.Load())
	}
}

// What no fetch used for longer than the time kept goes; a fetch from the cache counts as a use.
func TestTrimRemovesWhatNoFetchUsedLately(t *testing.T) {
	bin := []byte("pinned")
	sums := []byte(hexSum(bin) + "  shell-driver-" + platform + "\n")
	r := newReleases(t, map[string][]byte{release + "shell-driver-" + platform: bin, release + "SHA256SUMS": sums})
	f := r.fetcher(t)

	p := r.pin()
	p.Sums = hexSum(sums)

	got, err := f.Fetch(t.Context(), p, platform)
	if err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(f.Cache, "releases", hexSum([]byte("an old build")), "shell-driver")
	if err := os.MkdirAll(filepath.Dir(stale), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(stale, []byte("an old build"), 0o600); err != nil {
		t.Fatal(err)
	}

	month := 30 * 24 * time.Hour
	long := time.Now().Add(-2 * month)

	for _, p := range []string{stale, got.Path, filepath.Join(f.Cache, "releases", "sums", p.Sums)} {
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
	}

	// the pinned build is fetched again from the cache: it and its list are used now
	if _, err := f.Fetch(t.Context(), p, platform); err != nil || r.hits.Load() != 2 {
		t.Fatalf("again: %v, %d downloads", err, r.hits.Load())
	}

	removed, err := Trim(filepath.Join(f.Cache, "releases"), month, time.Now())
	if err != nil || len(removed) != 1 || removed[0] != filepath.Dir(stale) {
		t.Fatalf("removed %q, %v", removed, err)
	}

	if _, err := os.Stat(got.Path); err != nil {
		t.Fatalf("the build used lately went: %v", err)
	}

	removed, err = Trim(filepath.Join(f.Cache, "releases"), month, time.Now().Add(3*month))
	if err != nil || len(removed) != 2 {
		t.Fatalf("months later: %q, %v", removed, err)
	}
}

func TestTrimOfNothingIsNothing(t *testing.T) {
	if removed, err := Trim(filepath.Join(t.TempDir(), "releases"), time.Hour, time.Now()); err != nil || removed != nil {
		t.Fatalf("%q %v", removed, err)
	}
}

// A release's files are its forge's release assets for the tag: a monorepo plugin's tag is its
// directory and version.
func TestForgesServeTheTagsAssets(t *testing.T) {
	bin := []byte("lint tool")
	r := newReleases(t, map[string][]byte{
		"/team/plugins/releases/download/tools/lint/v0.1.0/lint-tools-" + platform:             bin,
		"/group/sub/plugins/-/releases/tools%2Flint%2Fv0.1.0/downloads/lint-tools-" + platform: bin,
	})

	for forge, project := range map[string]string{pin.GitHub: "team/plugins", pin.GitLab: "group/sub/plugins"} {
		f := Fetcher{Client: r.Client(), Cache: t.TempDir(), Under: "releases", Forges: map[string]string{"127.0.0.1": forge}}
		p := pin.Pin{Name: "lint", Repository: r.URL + "/" + project + ".git", Path: "tools/lint", Version: "v0.1.0", SHA256: pin.Checksums{platform: hexSum(bin)}}

		got, err := f.Fetch(t.Context(), p, platform)
		if err != nil || got.SHA256 != hexSum(bin) || filepath.Base(got.Path) != "lint-tools" {
			t.Fatalf("%s: %+v %v", forge, got, err)
		}
	}
}

func TestGitHubsURLIsTheOneOwnerNameAlwaysHad(t *testing.T) {
	for _, repo := range []string{"team/shell-driver", "https://github.com/team/shell-driver.git", "git@github.com:team/shell-driver.git"} {
		u, err := Fetcher{}.fileURL(pin.Pin{Repository: repo, Version: "v0.2.0"}, "shell-driver-linux-amd64")
		if err != nil || u != "https://github.com/team/shell-driver/releases/download/v0.2.0/shell-driver-linux-amd64" {
			t.Errorf("%s: %s %v", repo, u, err)
		}
	}
}

func TestAPlainGitServerIsNotSupportedYet(t *testing.T) {
	p := pin.Pin{Name: "x", Repository: "https://code.example.org/x.git", Version: "v1.0.0", SHA256: pin.Checksums{platform: hexSum([]byte("x"))}}

	if _, err := (Fetcher{Cache: t.TempDir()}).Fetch(t.Context(), p, platform); !errors.Is(err, ErrNotSupported) || !errors.Is(err, ErrPin) {
		t.Fatalf("%v", err)
	}
}
