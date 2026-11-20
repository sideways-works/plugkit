package build

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/sideways-works/plugkit/container"
)

// The images the builds run in; a test whose image the machine lacks is skipped rather than pull
// hundreds of megabytes.
const (
	GoImage   = "golang:1.25.7-bookworm@sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1"
	ShImage   = "busybox:1.37@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"
	hostOSArc = runtime.GOOS + "/" + runtime.GOARCH
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

// goManifest has comments and odd spacing, so only a byte-for-byte copy keeps it.
const goManifest = `# a tiny plugin
name:    hello
kind: driver
program: hello
build:
  image: ` + GoImage + `
  run: [go, build, -o, "{out}/hello", ./cmd/hello]   # stdlib only
  env: {CGO_ENABLED: "0", GOOS: "{os}", GOARCH: "{arch}", GOTOOLCHAIN: local}
platforms: [PLATFORMS]
files: [LICENSE]
`

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

// goPlugin is a repository with the plugin in plugins/hello and its module at the root.
func goPlugin(t *testing.T, platforms string) (string, []byte) {
	t.Helper()

	root := t.TempDir()
	m := strings.Replace(goManifest, "PLATFORMS", platforms, 1)
	write(t, root, map[string]string{
		"go.mod":                             "module example.com/plugins\n\ngo 1.25\n",
		"plugins/hello/cmd/hello/main.go":    "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello\") }\n",
		"plugins/hello/LICENSE":              "license\n",
		"plugins/hello/manifest.yaml":        m,
		"plugins/hello/notes/not-in-the-zip": "x",
	})

	return root, []byte(m)
}

func TestAGoPluginIsBuiltForEachPlatformInItsImage(t *testing.T) {
	tool := needTool(t, GoImage)

	other := "darwin/arm64"
	if hostOSArc == other {
		other = "linux/amd64"
	}

	root, m := goPlugin(t, "windows/amd64, "+other+", "+hostOSArc)
	out := filepath.Join(t.TempDir(), "hello")

	if _, err := Build(context.Background(), Options{Tool: tool, Root: root, Dir: "plugins/hello", Manifest: m, Platforms: []string{hostOSArc, other}, Out: out}); err != nil {
		t.Fatal(err)
	}

	ents, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}

	want := []string{"LICENSE", "hello-" + strings.ReplaceAll(other, "/", "-"), "hello-" + runtime.GOOS + "-" + runtime.GOARCH, "manifest.yaml"}
	slices.Sort(want)

	if !slices.Equal(names, want) {
		t.Fatalf("layout %q, want %q", names, want)
	}

	if data, _ := os.ReadFile(filepath.Join(out, "manifest.yaml")); string(data) != string(m) { //nolint:errcheck // compared
		t.Fatalf("the manifest changed:\n%s", data)
	}

	got, err := exec.CommandContext(context.Background(), filepath.Join(out, "hello-"+runtime.GOOS+"-"+runtime.GOARCH)).Output()
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("the built program says %q: %v", got, err)
	}
}

func TestABuildForAPlatformTheManifestLacksIsRefused(t *testing.T) {
	root, m := goPlugin(t, "linux/amd64")

	_, err := Build(context.Background(), Options{Root: root, Dir: "plugins/hello", Manifest: m, Platforms: []string{"windows/amd64"}, Out: filepath.Join(t.TempDir(), "x")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), "not built for windows/amd64") {
		t.Fatal(err)
	}
}

const assetsManifest = `name: board
kind: extension
build:
  image: ` + ShImage + `
  run: [sh, -c, "mkdir -p {out}/dist && cp -R web/. {out}/dist/ && ls /sys/class/net > {out}/dist/net && env | sort > {out}/dist/env && if touch probe 2>/dev/null; then echo writable; else echo read-only; fi > {out}/dist/repo"]
platforms: [any]
assets: [dist]
`

func TestAnAssetsOnlyPluginIsBuiltOnce(t *testing.T) {
	tool := needTool(t, ShImage)
	root := t.TempDir()
	write(t, root, map[string]string{"manifest.yaml": assetsManifest, "web/board.js": "customElements.define('x-board', B)", "web/i18n/de.json": "{}"})

	out := filepath.Join(t.TempDir(), "board")
	if _, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(assetsManifest), Out: out}); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"manifest.yaml", "dist/board.js", "dist/i18n/de.json"} {
		info, err := os.Stat(filepath.Join(out, f))
		if err != nil || info.Mode()&0o111 != 0 {
			t.Fatalf("%s: %v %v", f, info, err)
		}
	}

	read := func(f string) string {
		data, err := os.ReadFile(filepath.Join(out, "dist", f))
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	if net, repo := read("net"), read("repo"); net != "lo\n" || repo != "read-only\n" {
		t.Fatalf("the build saw networks %q and a %s repository", net, repo)
	}

	if env := read("env"); !strings.Contains(env, "GOFLAGS=-trimpath\n") || !strings.Contains(env, "HOME=/tmp\n") || strings.Contains(env, "GOCACHE=") {
		t.Fatalf("the build's environment:\n%s", env)
	}
}

func TestAStrayExecutableInTheAssetsIsRefused(t *testing.T) {
	tool := needTool(t, ShImage)
	root := t.TempDir()
	m := strings.Replace(assetsManifest, "ls /sys/class/net", "chmod +x {out}/dist/run.sh && ls /sys/class/net", 1)
	write(t, root, map[string]string{"manifest.yaml": m, "web/run.sh": "echo"})

	_, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(m), Out: filepath.Join(t.TempDir(), "board")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), "dist/run.sh is executable") {
		t.Fatal(err)
	}
}

func TestAFailingBuildSaysWhatItPrinted(t *testing.T) {
	tool := needTool(t, ShImage)
	root := t.TempDir()
	m := strings.Replace(assetsManifest, `"mkdir -p {out}/dist && cp -R web/. {out}/dist/ && ls /sys/class/net > {out}/dist/net && env | sort > {out}/dist/env && if touch probe 2>/dev/null; then echo writable; else echo read-only; fi > {out}/dist/repo"`, `"echo no network here >&2; exit 3"`, 1)
	write(t, root, map[string]string{"manifest.yaml": m})

	_, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(m), Out: filepath.Join(t.TempDir(), "board")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), "no network here") {
		t.Fatal(err)
	}
}

// Several commands run in order, no shell around them, each in a container of its own; with
// build.copy they write in a copy of the repository, never the repository itself.
func TestCommandsRunInOrderInACopyTheyWrite(t *testing.T) {
	tool := needTool(t, ShImage)
	root := t.TempDir()
	m := `name: board
kind: extension
build:
  image: ` + ShImage + `
  copy: true
  run:
    - [mkdir, -p, made]
    - [touch, made/in-the-copy]
    - [cp, -R, made, "{out}/dist"]
platforms: [any]
assets: [dist]
`
	write(t, root, map[string]string{"manifest.yaml": m, "web/board.js": "x"})
	checkout(t, root)

	out := filepath.Join(t.TempDir(), "board")
	if _, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(m), Out: out}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(out, "dist", "in-the-copy")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(root, "made")); err == nil {
		t.Fatal("the build wrote in the repository")
	}

	// the first command that fails ends the build
	failing := strings.Replace(m, "    - [mkdir, -p, made]\n", "    - [false]\n", 1)
	write(t, root, map[string]string{"manifest.yaml": failing})

	_, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(failing), Out: filepath.Join(t.TempDir(), "board")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), ": false: ") {
		t.Fatalf("a failing command: %v", err)
	}

	write(t, root, map[string]string{"manifest.yaml": m})
}

// build.copy copies the files git tracks and nothing else: an untracked or ignored file, Bazel's
// links and node_modules never reach the build, and a tracked symlink is refused.
func TestTheCopyHoldsTheTrackedFilesAlone(t *testing.T) {
	tool := needTool(t, ShImage)
	root := t.TempDir()
	m := `name: board
kind: extension
build:
  image: ` + ShImage + `
  copy: true
  run: [cp, -R, ., "{out}/dist"]
platforms: [any]
assets: [dist]
`
	write(t, root, map[string]string{"manifest.yaml": m, "web/board.js": "x"})
	checkout(t, root)

	// what a checkout gathers that git doesn't track
	write(t, root, map[string]string{"node_modules/left/index.js": "x", "notes.txt": "mine"})

	if err := os.Symlink("/etc", filepath.Join(root, "bazel-out")); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "board")
	if _, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(m), Out: out}); err != nil {
		t.Fatal(err)
	}

	var got []string

	_ = filepath.WalkDir(filepath.Join(out, "dist"), func(p string, d fs.DirEntry, err error) error { //nolint:errcheck // compared below
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(out, "dist"), p) //nolint:errcheck // under dist
			got = append(got, filepath.ToSlash(rel))
		}

		return nil
	})

	if !slices.Equal(got, []string{"manifest.yaml", "web/board.js"}) {
		t.Fatalf("the copy held %q", got)
	}

	// a tracked symlink is refused, by its path
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "web", "passwd")); err != nil {
		t.Fatal(err)
	}

	git(t, root, "add", "web/passwd")

	_, err := Build(context.Background(), Options{Tool: tool, Root: root, Manifest: []byte(m), Out: filepath.Join(t.TempDir(), "board")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), "web/passwd is a symlink") {
		t.Fatalf("a tracked symlink: %v", err)
	}

	// a directory git doesn't track at all is no checkout to copy
	plain := t.TempDir()
	write(t, plain, map[string]string{"manifest.yaml": m})

	_, err = Build(context.Background(), Options{Tool: tool, Root: plain, Manifest: []byte(m), Out: filepath.Join(t.TempDir(), "board")})
	if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), "no git checkout") {
		t.Fatalf("no checkout: %v", err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// checkout makes root a git checkout with its files committed.
func checkout(t *testing.T, root string) {
	t.Helper()

	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "plugin")
}
