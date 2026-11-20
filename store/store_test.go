package store

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sideways-works/plugkit/build"
	"github.com/sideways-works/plugkit/bundle"
	"github.com/sideways-works/plugkit/container"
	"github.com/sideways-works/plugkit/manifest"
)

const (
	goImage = "golang:1.25.7-bookworm@sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1"
	shImage = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
	host    = runtime.GOOS + "/" + runtime.GOARCH
)

func needTool(t *testing.T, image string) string {
	t.Helper()

	tool, err := container.Find("")
	if err != nil || !container.Reachable(context.Background(), tool) {
		t.Skipf("no container tool to build with (docker or podman): %v", err)
	}

	if !container.HasImage(context.Background(), tool, image) {
		t.Skipf("%s doesn't have %s; pull it to run this test", tool, image)
	}

	return tool
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

// goRepo is a repository holding a tiny Go plugin at plugins/hello, and its commit.
func goRepo(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()
	write(t, root, map[string]string{
		"go.mod":                          "module example.com/plugins\n\ngo 1.25\n",
		"plugins/hello/cmd/hello/main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n",
		"plugins/hello/LICENSE":           "license\n",
		"plugins/hello/manifest.yaml": `name: hello
kind: driver
for: test
program: hello
build:
  image: ` + goImage + `
  run: [go, build, -o, "{out}/hello", ./cmd/hello]
  env: {CGO_ENABLED: "0", GOOS: "{os}", GOARCH: "{arch}", GOTOOLCHAIN: local}
platforms: [` + host + `]
files: [LICENSE]
`,
	})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "plugin")

	return root, git(t, root, "rev-parse", "HEAD")
}

func TestACommitIsBuiltOnceAndThenComesFromTheCache(t *testing.T) {
	tool := needTool(t, goImage)
	repo, commit := goRepo(t)
	cache := t.TempDir()

	var builds atomic.Int32

	s := Store{Dir: cache, Tool: tool, OnBuild: func(string) { builds.Add(1) }}
	p := Pin{Repository: repo, Path: "plugins/hello", Commit: commit}

	r, err := s.Resolve(context.Background(), p, host)
	if err != nil {
		t.Fatal(err)
	}

	out, err := exec.CommandContext(context.Background(), r.Program).Output()
	if err != nil || string(out) != "hello\n" || r.Manifest.Kind != "driver" || filepath.Dir(r.Program) != r.Dir {
		t.Fatalf("resolved %+v, which says %q: %v", r, out, err)
	}

	if _, err := os.Stat(filepath.Join(r.Dir, "LICENSE")); err != nil {
		t.Fatal(err)
	}

	// a second resolve neither builds nor reaches the repository: a tool that isn't there and a
	// repository that is gone change nothing
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}

	again, err := Store{Dir: cache, Tool: "/nonexistent/docker", OnBuild: s.OnBuild}.Resolve(context.Background(), p, host)
	if err != nil || again.Program != r.Program {
		t.Fatalf("again: %+v %v", again, err)
	}

	if n := builds.Load(); n != 1 {
		t.Fatalf("%d builds, want 1", n)
	}

	if filepath.Dir(r.Dir) != cache || !sha256Hex.MatchString(filepath.Base(r.Dir)) {
		t.Fatalf("entry %s", r.Dir)
	}
}

func TestACommitWithoutAManifestSaysSo(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"README": "x"})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "x")

	_, err := Store{Dir: t.TempDir()}.Resolve(context.Background(), Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}, host)
	if !errors.Is(err, ErrNoManifest) {
		t.Fatal(err)
	}
}

const zipManifest = `# packed by hand
name: hello
kind:  driver
for: test
program: hello
build:
  image: busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
  run: [sh, -c, "true"]
platforms: [linux/amd64, linux/arm64, darwin/amd64, darwin/arm64]
files: [LICENSE]
`

type raw struct {
	name string
	mode fs.FileMode
	body string
}

func zipOf(t *testing.T, entries ...raw) []byte {
	t.Helper()

	var b bytes.Buffer

	zw := zip.NewWriter(&b)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)

		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

func goodZip(t *testing.T) []byte {
	t.Helper()

	entries := []raw{{"manifest.yaml", 0o644, zipManifest}, {"LICENSE", 0o644, "license"}}
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		entries = append(entries, raw{"hello-" + p, 0o755, "binary for " + p})
	}

	return zipOf(t, entries...)
}

func sum(data []byte) string {
	s := sha256.Sum256(data)

	return hex.EncodeToString(s[:])
}

// serve serves each zip by its path, and counts the downloads.
func serve(t *testing.T, zips map[string][]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := zips[r.URL.Path]
		if !ok {
			http.NotFound(w, r)

			return
		}

		hits.Add(1)

		_, _ = w.Write(data) //nolint:errcheck // a test server
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

func TestAZipIsDownloadedOnceCheckedAndUnpacked(t *testing.T) {
	data := goodZip(t)
	srv, hits := serve(t, map[string][]byte{"/hello.zip": data})
	cache := t.TempDir()
	s := Store{Dir: cache, Client: srv.Client()}
	p := Pin{Zip: &Zip{URL: srv.URL + "/hello.zip", SHA256: sum(data)}}

	for range 2 {
		r, err := s.Resolve(context.Background(), p, "darwin/arm64")
		if err != nil {
			t.Fatal(err)
		}

		if r.Dir != filepath.Join(cache, sum(data)) || r.Program != filepath.Join(r.Dir, "hello-darwin-arm64") || r.Manifest.Name != "hello" {
			t.Fatalf("resolved %+v", r)
		}

		if got, _ := os.ReadFile(filepath.Join(r.Dir, "manifest.yaml")); string(got) != zipManifest { //nolint:errcheck // compared
			t.Fatalf("manifest %q", got)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want 1", n)
	}

	if _, err := s.Resolve(context.Background(), p, "windows/amd64"); err == nil || !strings.Contains(err.Error(), "not built for windows/amd64") {
		t.Fatal(err)
	}
}

func TestBadZipsAreRefusedAndLeaveNothingInTheCache(t *testing.T) {
	good := goodZip(t)
	base := []raw{{"manifest.yaml", 0o644, zipManifest}, {"hello-" + strings.ReplaceAll(host, "/", "-"), 0o755, "bin"}}

	cases := map[string]struct {
		data   []byte
		sum    string
		limits bundle.Limits
		says   string
	}{
		"a wrong checksum":   {good, strings.Repeat("0", 64), bundle.Limits{}, "checksum mismatch"},
		"a path up":          {zipOf(t, append(base, raw{"../x", 0o644, "x"})...), "", bundle.Limits{}, "leaves the plugin's directory"},
		"a symlink":          {zipOf(t, append(base, raw{"LICENSE", fs.ModeSymlink | 0o777, "/etc/passwd"})...), "", bundle.Limits{}, "is a symlink"},
		"an extra program":   {zipOf(t, append(base, raw{"helper", 0o755, "x"})...), "", bundle.Limits{}, "no part of plugin hello"},
		"an oversized entry": {zipOf(t, append(base, raw{"LICENSE", 0o644, strings.Repeat("x", 4096)})...), "", bundle.Limits{Bytes: 1024}, "more than 1024 bytes"},
		"a wrapped manifest": {zipOf(t, raw{"hello/manifest.yaml", 0o644, zipManifest}, raw{"hello/hello-linux-amd64", 0o755, "x"}), "", bundle.Limits{}, "not at the top level"},
		"no manifest":        {zipOf(t, base[1:]...), "", bundle.Limits{}, "no manifest.yaml at the top level"},
		"not a zip":          {[]byte("<html>"), "", bundle.Limits{}, "not a valid zip"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, map[string][]byte{"/p.zip": c.data})
			cache := t.TempDir()

			want := c.sum
			if want == "" {
				want = sum(c.data)
			}

			_, err := Store{Dir: cache, Client: srv.Client(), Limits: c.limits}.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/p.zip", SHA256: want}}, host)
			if err == nil || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}

			if ents, _ := os.ReadDir(cache); len(ents) > 0 { //nolint:errcheck // an empty cache reads as empty
				t.Fatalf("the cache holds %v", ents)
			}

			if _, err := os.Stat(filepath.Join(filepath.Dir(cache), "x")); err == nil {
				t.Fatal("a file was written outside the cache")
			}
		})
	}
}

func TestOnlyHTTPSZipsArePinned(t *testing.T) {
	_, err := Store{Dir: t.TempDir()}.Resolve(context.Background(), Pin{Zip: &Zip{URL: "http://example.com/p.zip", SHA256: strings.Repeat("a", 64)}}, host)
	if !errors.Is(err, ErrPin) || !strings.Contains(err.Error(), "an https URL") {
		t.Fatal(err)
	}
}

func TestAnAssetsOnlyPluginGoesThroughBuildZipAndCache(t *testing.T) {
	tool := needTool(t, shImage)
	root := t.TempDir()
	m := `name: board
kind: extension
for: test
build:
  image: ` + shImage + `
  run: [sh, -c, "mkdir -p {out}/web/dist && cp -R src/. {out}/web/dist/"]
platforms: [any]
assets: [web/dist/]
`
	write(t, root, map[string]string{"manifest.yaml": m, "src/board.js": "customElements.define()", "src/fonts/a.woff2": "font", "src/i18n/de.json": "{}"})

	out := filepath.Join(t.TempDir(), "board")
	if _, err := build.Build(context.Background(), build.Options{Tool: tool, Root: root, Manifest: []byte(m), Out: out}); err != nil {
		t.Fatal(err)
	}

	var z bytes.Buffer
	if err := bundle.Pack(out, &z); err != nil {
		t.Fatal(err)
	}

	srv, hits := serve(t, map[string][]byte{"/board.zip": z.Bytes()})
	s := Store{Dir: t.TempDir(), Client: srv.Client()}

	for _, platform := range []string{"linux/amd64", "darwin/arm64"} {
		r, err := s.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/board.zip", SHA256: sum(z.Bytes())}}, platform)
		if err != nil {
			t.Fatal(err)
		}

		if r.Program != "" || r.Manifest.Kind != "extension" {
			t.Fatalf("resolved %+v", r)
		}

		var files []string

		_ = filepath.WalkDir(r.Dir, func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // compared below
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(r.Dir, p) //nolint:errcheck // under r.Dir
				files = append(files, filepath.ToSlash(rel))

				if info, _ := d.Info(); info.Mode()&0o111 != 0 { //nolint:errcheck // a file just walked
					t.Errorf("%s is executable", rel)
				}
			}

			return nil
		})

		want := []string{"manifest.yaml", "web/dist/board.js", "web/dist/fonts/a.woff2", "web/dist/i18n/de.json"}
		if !slices.Equal(files, want) {
			t.Fatalf("the plugin's directory holds %q, want %q", files, want)
		}
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("%d downloads, want 1", n)
	}
}

func TestTrimKeepsWhatWasUsedAndDropsTheRest(t *testing.T) {
	data := goodZip(t)
	srv, _ := serve(t, map[string][]byte{"/hello.zip": data})
	cache := t.TempDir()
	s := Store{Dir: cache, Client: srv.Client()}

	r, err := s.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/hello.zip", SHA256: sum(data)}}, host)
	if err != nil {
		t.Fatal(err)
	}

	write(t, cache, map[string]string{".sources/" + strings.Repeat("b", 64): strings.Repeat("c", 64), ".new-123/x": "half"})

	if removed, err := Trim(cache, time.Hour, time.Now()); err != nil || len(removed) != 1 {
		t.Fatalf("a fresh cache lost %v: %v", removed, err)
	}

	removed, err := Trim(cache, time.Hour, time.Now().Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(r.Dir); err == nil || len(removed) != 2 {
		t.Fatalf("an old cache kept its entry, removed %v", removed)
	}
}

// A host gives each plugin a directory of its name beside the store's entries: the store's own
// directories take no name a plugin can have, so plugins named git and sources keep theirs.
func TestPluginsNamedGitAndSourcesKeepTheirDirectoriesBesideTheStore(t *testing.T) {
	tool := needTool(t, shImage)

	root := t.TempDir()
	write(t, root, map[string]string{
		"hello.sh": "#!/bin/sh\necho hello\n",
		"manifest.yaml": `name: hello
kind: driver
for: test
program: hello
build:
  image: ` + shImage + `
  run: [sh, -c, "cp hello.sh {out}/hello"]
platforms: [` + host + `]
`,
	})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "plugin")

	data := goodZip(t)
	srv, _ := serve(t, map[string][]byte{"/hello.zip": data})

	cache := t.TempDir()
	write(t, cache, map[string]string{"git/scratch": "the git plugin's", "sources/scratch": "the sources plugin's"})

	s := Store{Dir: cache, Tool: tool, Client: srv.Client()}

	if _, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}, host); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/hello.zip", SHA256: sum(data)}}, host); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"git", "sources"} {
		ents, err := os.ReadDir(filepath.Join(cache, d))
		if err != nil || len(ents) != 1 || ents[0].Name() != "scratch" {
			t.Fatalf("%s/ holds %v: %v", d, ents, err)
		}
	}

	if _, err := Trim(cache, time.Hour, time.Now().Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	ents, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}

	var left []string
	for _, e := range ents {
		left = append(left, e.Name())
	}

	if !slices.Equal(left, []string{".sources", "git", "sources"}) {
		t.Fatalf("after the trim the cache holds %v", left)
	}

	for _, f := range []string{"git/scratch", "sources/scratch"} {
		if _, err := os.Stat(filepath.Join(cache, f)); err != nil {
			t.Fatal(err)
		}
	}
}

const metaManifest = `name: store
kind: meta
for: test
logo: logo.svg
schemas:
  put: {message: schemas/put.json, answer: schemas/put-answer.json}
ack_timeout: 30s
`

// A meta plugin resolves to its directory as a plugin built for any platform does, and needs no
// container: nothing of it is built.
func TestAMetaPluginResolvesToItsDirectory(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"meta/store/manifest.yaml":           metaManifest,
		"meta/store/logo.svg":                "<svg/>",
		"meta/store/schemas/put.json":        `{"type": "object"}`,
		"meta/store/schemas/put-answer.json": `{"type": "object"}`,
	})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "meta")

	s := Store{Dir: t.TempDir(), Tool: "/nonexistent/docker"}

	r, err := s.Resolve(context.Background(), Pin{Repository: root, Path: "meta/store", Commit: git(t, root, "rev-parse", "HEAD")}, "darwin/arm64")
	if err != nil {
		t.Fatal(err)
	}

	if r.Program != "" || r.Manifest.Kind != "meta" {
		t.Fatalf("resolved %+v", r)
	}

	for _, f := range []string{"manifest.yaml", "logo.svg", "schemas/put.json", "schemas/put-answer.json"} {
		if _, err := os.Stat(filepath.Join(r.Dir, f)); err != nil {
			t.Fatal(err)
		}
	}

	var z bytes.Buffer
	if err := bundle.Pack(r.Dir, &z); err != nil {
		t.Fatal(err)
	}

	srv, _ := serve(t, map[string][]byte{"/store.zip": z.Bytes(), "/bad.zip": zipOf(t,
		raw{"manifest.yaml", 0o644, metaManifest}, raw{"logo.svg", 0o644, "<svg/>"},
		raw{"schemas/put.json", 0o644, "type: object"}, raw{"schemas/put-answer.json", 0o644, "{}"})})

	s = Store{Dir: t.TempDir(), Client: srv.Client()}

	if r, err := s.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/store.zip", SHA256: sum(z.Bytes())}}, "linux/arm64"); err != nil || r.Program != "" {
		t.Fatalf("by zip: %+v %v", r, err)
	}

	bad := zipOf(t, raw{"manifest.yaml", 0o644, metaManifest}, raw{"logo.svg", 0o644, "<svg/>"},
		raw{"schemas/put.json", 0o644, "type: object"}, raw{"schemas/put-answer.json", 0o644, "{}"})
	if _, err := s.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/bad.zip", SHA256: sum(bad)}}, "linux/arm64"); err == nil || !strings.Contains(err.Error(), "schemas/put.json is no JSON") {
		t.Fatal(err)
	}
}

// A build's {commit} and {version} stamp the program, here through -ldflags -X as a Go release does,
// and a build of the commit with a version is kept apart from one without.
func TestABuildIsStampedWithItsCommitAndVersion(t *testing.T) {
	tool := needTool(t, goImage)

	root := t.TempDir()
	write(t, root, map[string]string{
		"go.mod":  "module example.com/hello\n\ngo 1.25\n",
		"main.go": "package main\n\nimport \"fmt\"\n\nvar version = \"dev\"\n\nfunc main() { fmt.Println(version) }\n",
		"manifest.yaml": `name: hello
kind: driver
for: test
program: hello
build:
  image: ` + goImage + `
  run: [go, build, "-ldflags=-X main.version={version}+{commit}", -o, "{out}/hello", .]
  env: {CGO_ENABLED: "0", GOOS: "{os}", GOARCH: "{arch}", GOTOOLCHAIN: local}
platforms: [` + host + `]
`,
	})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "plugin")

	commit := git(t, root, "rev-parse", "HEAD")

	var builds atomic.Int32

	s := Store{Dir: t.TempDir(), Tool: tool, OnBuild: func(string) { builds.Add(1) }}

	says := func(version string) string {
		t.Helper()

		r, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: commit, Version: version}, host)
		if err != nil {
			t.Fatal(err)
		}

		out, err := exec.CommandContext(context.Background(), r.Program).Output()
		if err != nil {
			t.Fatal(err)
		}

		return strings.TrimSpace(string(out))
	}

	if got := says(""); got != "+"+commit {
		t.Fatalf("without a version: %q", got)
	}

	for range 2 {
		if got := says("v1.2.3"); got != "1.2.3+"+commit {
			t.Fatalf("version 1.2.3: %q", got)
		}
	}

	if n := builds.Load(); n != 2 {
		t.Fatalf("%d builds, want one without a version and one with", n)
	}
}

func TestAnUntrustedStoreFetchesFromNoLocalPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"manifest.yaml": metaManifest})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "x")

	pin := Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}

	for _, repo := range []string{root, "file://" + root, "-uhack", "ext::sh -c x"} {
		pin.Repository = repo
		if _, err := (Store{Dir: t.TempDir(), Untrusted: true}).Resolve(context.Background(), pin, host); !errors.Is(err, ErrPin) || !strings.Contains(err.Error(), "not a remote git may fetch from") {
			t.Errorf("%s: %v", repo, err)
		}
	}
}

// A resolve writes nothing another resolve shares but the entry, by a rename the first one wins:
// many at once, in goroutines and in processes, all get the one entry and leave no scratch behind.
func TestManyResolvesAtOnceShareOneEntry(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{
		"manifest.yaml":           metaManifest,
		"logo.svg":                "<svg/>",
		"schemas/put.json":        "{}",
		"schemas/put-answer.json": "{}",
	})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "meta")

	cache := t.TempDir()
	commit := git(t, root, "rev-parse", "HEAD")

	const n = 6

	var wg sync.WaitGroup

	errs := make(chan error, 2*n)

	for range n {
		wg.Go(func() {
			_, err := Store{Dir: cache}.Resolve(context.Background(), Pin{Repository: root, Commit: commit}, host)
			errs <- err
		})

		wg.Go(func() {
			cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself

			cmd.Env = append(os.Environ(), envResolve+"="+cache+"\n"+root+"\n"+commit)

			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("a process: %w: %s", err, out)
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}

	ents, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}

	var entries, other []string

	for _, e := range ents {
		switch {
		case sha256Hex.MatchString(e.Name()):
			entries = append(entries, e.Name())
		case e.Name() != aliasesDir:
			other = append(other, e.Name())
		}
	}

	if len(entries) != 1 || len(other) > 0 {
		t.Fatalf("the cache holds the entries %v and %v", entries, other)
	}

	aliases, _ := os.ReadDir(filepath.Join(cache, aliasesDir)) //nolint:errcheck // compared below
	if len(aliases) != 1 {
		t.Fatalf("aliases %v", aliases)
	}
}

// envResolve makes the test binary a process that resolves one pin, for the test of many at once.
const envResolve = "PLUGKIT_TEST_RESOLVE"

func TestMain(m *testing.M) {
	if v := os.Getenv(envResolve); v != "" {
		f := strings.Split(v, "\n")

		if _, err := (Store{Dir: f[0]}).Resolve(context.Background(), Pin{Repository: f[1], Commit: f[2]}, host); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	os.Exit(m.Run())
}

// A server that serves no commit by its id gets the pin's ref fetched, one deep, and never every
// branch and tag.
func TestTheFallbackFetchesThePinsRefAlone(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"manifest.yaml": metaManifest, "logo.svg": "<svg/>", "schemas/put.json": "{}", "schemas/put-answer.json": "{}"})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "meta")

	older := git(t, root, "rev-parse", "HEAD")
	write(t, root, map[string]string{"README": "x"})
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "more")

	// protocol v0 serves only what it advertises, and the older commit is no ref's tip
	v0 := []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.version", "GIT_CONFIG_VALUE_0=0"}
	s := Store{Dir: t.TempDir(), GitEnv: v0}

	if _, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: older}, host); !errors.Is(err, ErrPin) {
		t.Fatalf("a commit no ref is at, without a ref: %v", err)
	}

	if _, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: older, Ref: "refs/heads/master"}, host); err == nil || !strings.Contains(err.Error(), "has no commit "+older+" at refs/heads/master") {
		t.Fatalf("a ref whose tip is another commit: %v", err)
	}

	git(t, root, "tag", "v1", older)

	if r, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: older, Ref: "refs/tags/v1"}, host); err != nil || r.Manifest.Kind != "meta" {
		t.Fatalf("by its tag: %+v %v", r, err)
	}
}

func TestAnUntrustedFetchIsCapped(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"manifest.yaml": metaManifest, "logo.svg": "<svg/>", "schemas/put.json": "{}", "schemas/put-answer.json": "{}", "big": random(t, 1<<20)})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "meta")

	s := Store{Dir: t.TempDir(), Untrusted: true, AllowLocal: true, MaxBytes: 64 << 10}

	_, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}, host)
	if !errors.Is(err, ErrPin) || !strings.Contains(err.Error(), "over the size limit") {
		t.Fatal(err)
	}

	s.MaxBytes = 0
	if _, err := s.Resolve(context.Background(), Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}, host); err != nil {
		t.Fatalf("within the default cap: %v", err)
	}
}

// random is n bytes that don't compress, as text.
func random(t *testing.T, n int) string {
	t.Helper()

	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return hex.EncodeToString(b)
}

// A host never builds, unpacks or runs a plugin made for another system.
func TestAPluginForAnotherSystemIsRefusedFirst(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"manifest.yaml": metaManifest, "logo.svg": "<svg/>", "schemas/put.json": "{}", "schemas/put-answer.json": "{}"})
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "meta")

	pin := Pin{Repository: root, Commit: git(t, root, "rev-parse", "HEAD")}
	cache := t.TempDir()

	var builds atomic.Int32

	s := Store{Dir: cache, For: "pipelinegen", OnBuild: func(string) { builds.Add(1) }}

	if _, err := s.Resolve(context.Background(), pin, host); !errors.Is(err, manifest.ErrFor) || !strings.Contains(err.Error(), "is for test, and this is pipelinegen") || builds.Load() != 0 {
		t.Fatalf("by commit: %v, %d builds", err, builds.Load())
	}

	// one cached for any system is refused on the next resolve as well
	if _, err := (Store{Dir: cache}).Resolve(context.Background(), pin, host); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Resolve(context.Background(), pin, host); !errors.Is(err, manifest.ErrFor) {
		t.Fatalf("from the cache: %v", err)
	}

	// a zip's manifest is read before anything of it is unpacked: this one's path up never is
	other := zipOf(t, raw{"manifest.yaml", 0o644, zipManifest}, raw{"../x", 0o644, "x"})
	srv, _ := serve(t, map[string][]byte{"/other.zip": other})
	zipCache := t.TempDir()

	_, err := Store{Dir: zipCache, For: "pipelinegen", Client: srv.Client()}.Resolve(context.Background(), Pin{Zip: &Zip{URL: srv.URL + "/other.zip", SHA256: sum(other)}}, host)
	if !errors.Is(err, manifest.ErrFor) || !strings.Contains(err.Error(), "plugin hello is for test, and this is pipelinegen") {
		t.Fatalf("by zip: %v", err)
	}

	if ents, _ := os.ReadDir(zipCache); len(ents) > 0 { //nolint:errcheck // an empty cache reads as empty
		t.Fatalf("the cache holds %v", ents)
	}
}
