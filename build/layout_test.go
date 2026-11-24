package build

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sideways-works/plugkit/bundle"
	"github.com/sideways-works/plugkit/manifest"
	"go.yaml.in/yaml/v3"
)

func pack(t *testing.T, dir string) []byte {
	t.Helper()

	var b bytes.Buffer
	if err := bundle.Pack(dir, &b); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

// A host that runs the build's commands itself and then lays the plugin out gets Build's zip.
func TestLayoutAfterTheCommandsGivesBuildsZip(t *testing.T) {
	tool := needTool(t, GoImage)

	other := "darwin/arm64"
	if hostOSArc == other {
		other = "linux/amd64"
	}

	root, m := goPlugin(t, hostOSArc+", "+other)
	ctx := context.Background()

	built := filepath.Join(t.TempDir(), "hello")
	if _, err := Build(ctx, Options{Tool: tool, Root: root, Dir: "plugins/hello", Manifest: m, Out: built}); err != nil {
		t.Fatal(err)
	}

	parsed, err := manifest.Parse(m)
	if err != nil {
		t.Fatal(err)
	}

	b := builder{o: Options{Root: root, Dir: "plugins/hello", Manifest: m}, m: parsed, tool: tool}
	scratch := map[string]string{}

	for _, p := range parsed.Platforms {
		scratch[p] = t.TempDir()
		if err := b.run(ctx, p, scratch[p]); err != nil {
			t.Fatal(err)
		}
	}

	laid := filepath.Join(t.TempDir(), "hello")
	if _, err := Layout(ctx, LayoutOptions{Root: root, Dir: "plugins/hello", Manifest: m, Out: laid, Scratch: scratch}); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(pack(t, built), pack(t, laid)) {
		t.Fatalf("Layout packs %q, Build %q", files(t, laid), files(t, built))
	}
}

const layoutManifest = `name: hello
kind: driver
program: hello
build:
  image: busybox@sha256:` + "a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec" + `
  run: [make, "{out}"]
platforms: [linux/amd64, darwin/arm64]
files: [LICENSE]
assets: [web]
`

// scratchFor is what a build of layoutManifest writes for each platform, its assets marked with the
// platform.
func scratchFor(t *testing.T, platforms ...string) map[string]string {
	t.Helper()

	out := map[string]string{}

	for _, p := range platforms {
		dir := t.TempDir()
		write(t, dir, map[string]string{"web/app.js": p})

		if err := os.WriteFile(filepath.Join(dir, "hello"), []byte("binary for "+p), 0o755); err != nil {
			t.Fatal(err)
		}

		out[p] = dir
	}

	return out
}

func TestLayoutNeedsNoContainer(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"LICENSE": "license\n", "manifest.yaml": layoutManifest})

	out := filepath.Join(t.TempDir(), "hello")
	if _, err := Layout(context.Background(), LayoutOptions{Root: root, Manifest: []byte(layoutManifest), Out: out, Scratch: scratchFor(t, "darwin/arm64", "linux/amd64")}); err != nil {
		t.Fatal(err)
	}

	want := []string{"LICENSE", "hello-darwin-arm64", "hello-linux-amd64", "manifest.yaml", "web/app.js"}
	if got := files(t, out); !slices.Equal(got, want) {
		t.Fatalf("layout %q, want %q", got, want)
	}

	// the assets are the manifest's first platform's, whatever order the host built in
	if data, _ := os.ReadFile(filepath.Join(out, "web", "app.js")); string(data) != "linux/amd64" { //nolint:errcheck // compared
		t.Fatalf("assets of %s", data)
	}

	info, err := os.Stat(filepath.Join(out, "hello-linux-amd64"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("program: %v %v", info, err)
	}
}

func TestLayoutRefusesScratchThatIsNoBuildOfThePlugin(t *testing.T) {
	root := t.TempDir()
	write(t, root, map[string]string{"LICENSE": "license\n"})

	for name, c := range map[string]struct {
		scratch map[string]string
		says    string
	}{
		"a platform the manifest lacks": {scratchFor(t, "windows/amd64"), "not built for windows/amd64"},
		"no scratch":                    {nil, "no build's {out} to lay out"},
		"no program in it":              {map[string]string{"linux/amd64": t.TempDir()}, "the build wrote no {out}/hello"},
	} {
		out := filepath.Join(t.TempDir(), "hello")

		_, err := Layout(context.Background(), LayoutOptions{Root: root, Manifest: []byte(layoutManifest), Out: out, Scratch: c.scratch})
		if !errors.Is(err, ErrBuild) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s: got %v, want %q", name, err, c.says)
		}
	}
}

func TestALayoutCopiesTheFilesAProductsKeyNames(t *testing.T) {
	text := layoutManifest + "icons: [icons/a.svg]\n"
	ext := manifest.Extensions{"icons": {Files: func(n *yaml.Node) []string { return []string{n.Content[0].Value} }}}

	root := t.TempDir()
	write(t, root, map[string]string{"LICENSE": "license\n", "manifest.yaml": text, "icons/a.svg": "<svg/>"})

	out := filepath.Join(t.TempDir(), "hello")
	if _, err := Layout(context.Background(), LayoutOptions{
		Root: root, Manifest: []byte(text), Out: out, Scratch: scratchFor(t, "darwin/arm64", "linux/amd64"), Extensions: ext,
	}); err != nil {
		t.Fatal(err)
	}

	if data, _ := os.ReadFile(filepath.Join(out, "icons", "a.svg")); string(data) != "<svg/>" { //nolint:errcheck // compared
		t.Fatalf("icons/a.svg: %q", data)
	}
}
