package bundle

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// manifestText has comments and odd spacing, so only a byte-for-byte copy keeps it.
const manifestText = `# the plugin
name:   hello
kind: driver
program: hello
build:
  image: golang@sha256:` + "564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1" + `
  run: [go, build, -o, "{out}/hello", ./cmd/hello]   # the command
platforms: [linux/amd64, darwin/arm64]
files: [LICENSE]
assets: [web/dist]
`

type entry struct {
	name string
	mode fs.FileMode
	body string
}

func layout(t *testing.T, entries ...entry) string {
	t.Helper()

	dir := t.TempDir()

	for _, e := range entries {
		p := filepath.Join(dir, filepath.FromSlash(e.name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(e.body), e.mode); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

var plugin = []entry{
	{"manifest.yaml", 0o644, manifestText},
	{"hello-linux-amd64", 0o755, "linux binary"},
	{"hello-darwin-arm64", 0o755, "mac binary"},
	{"LICENSE", 0o644, "license"},
	{"web/dist/app.js", 0o644, "customElements.define()"},
	{"web/dist/fonts/a.woff2", 0o644, "font"},
}

func packed(t *testing.T, dir string) []byte {
	t.Helper()

	var b bytes.Buffer
	if err := Pack(dir, &b); err != nil {
		t.Fatal(err)
	}

	return b.Bytes()
}

func TestAPackedPluginUnpacksAsItWasLaidOut(t *testing.T) {
	first, again := packed(t, layout(t, plugin...)), packed(t, layout(t, plugin...))
	if !bytes.Equal(first, again) {
		t.Fatal("the same files packed to different zips")
	}

	zr, err := zip.NewReader(bytes.NewReader(first), int64(len(first)))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}

	want := []string{"LICENSE", "hello-darwin-arm64", "hello-linux-amd64", "manifest.yaml", "web/dist/app.js", "web/dist/fonts/a.woff2"}
	if !slices.Equal(names, want) {
		t.Fatalf("entries %q, want %q", names, want)
	}

	dest := filepath.Join(t.TempDir(), "plugin")

	m, err := Unpack(writeZip(t, first), dest, Limits{})
	if err != nil {
		t.Fatal(err)
	}

	if m.Name != "hello" {
		t.Fatalf("manifest %+v", m)
	}

	for _, e := range plugin {
		info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(e.name)))
		if err != nil {
			t.Fatal(err)
		}

		if exec := info.Mode()&0o111 != 0; exec != (e.mode == 0o755) {
			t.Fatalf("%s: mode %v", e.name, info.Mode())
		}
	}

	if data, _ := os.ReadFile(filepath.Join(dest, "manifest.yaml")); string(data) != manifestText { //nolint:errcheck // compared
		t.Fatalf("the manifest changed:\n%s", data)
	}
}

func TestPackRefusesWhatIsNoPartOfThePlugin(t *testing.T) {
	cases := map[string]struct {
		extra entry
		says  string
	}{
		"an executable asset":       {entry{"web/dist/run.sh", 0o755, "x"}, "only the program may be"},
		"an executable file":        {entry{"LICENSE", 0o755, "x"}, "only the program may be"},
		"a stray file":              {entry{"notes.txt", 0o644, "x"}, "no part of plugin hello"},
		"another platform's binary": {entry{"hello-windows-amd64", 0o755, "x"}, "no part of plugin hello"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			entries := slices.DeleteFunc(slices.Clone(plugin), func(e entry) bool { return e.name == c.extra.name })

			var b bytes.Buffer

			err := Pack(layout(t, append(entries, c.extra)...), &b)
			if !errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}
		})
	}
}

type raw struct {
	name string
	mode fs.FileMode
	body string
}

func rawZip(t *testing.T, entries ...raw) string {
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

	return writeZip(t, b.Bytes())
}

func writeZip(t *testing.T, data []byte) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "plugin.zip")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestUnpackRefusesUnsafeZips(t *testing.T) {
	base := []raw{{"manifest.yaml", 0o644, manifestText}, {"hello-linux-amd64", 0o755, "bin"}}

	cases := map[string]struct {
		entries []raw
		limits  Limits
		says    string
	}{
		"a path up":               {append(base, raw{"../x", 0o644, "x"}), Limits{}, "leaves the plugin's directory"},
		"a path up in the assets": {append(base, raw{"web/dist/../../x", 0o644, "x"}), Limits{}, "leaves the plugin's directory"},
		"an absolute path":        {append(base, raw{"/etc/x", 0o644, "x"}), Limits{}, "leaves the plugin's directory"},
		"a symlink":               {append(base, raw{"web/dist/link", fs.ModeSymlink | 0o777, "/etc/passwd"}), Limits{}, "is a symlink"},
		"an extra executable":     {append(base, raw{"helper", 0o755, "x"}), Limits{}, "no part of plugin hello"},
		"an executable asset":     {append(base, raw{"web/dist/run", 0o755, "x"}), Limits{}, "only the program may be"},
		"an oversized entry":      {append(base, raw{"web/dist/big.js", 0o644, strings.Repeat("x", 4096)}), Limits{Bytes: 1024}, "more than 1024 bytes"},
		"too many entries":        {append(base, raw{"web/dist/a", 0o644, ""}, raw{"web/dist/b", 0o644, ""}), Limits{Entries: 3}, "more than 3"},
		"no manifest":             {base[1:], Limits{}, "no manifest.yaml at the top level"},
		"a wrapped manifest":      {[]raw{{"hello/manifest.yaml", 0o644, manifestText}, {"hello/hello-linux-amd64", 0o755, "bin"}}, Limits{}, "not at the top level (it is hello/manifest.yaml)"},
		"no program":              {[]raw{{"manifest.yaml", 0o644, manifestText}, {"LICENSE", 0o644, "license"}}, Limits{}, "no file of program hello"},
		"an entry twice":          {append(base, raw{"LICENSE", 0o644, "a"}, raw{"LICENSE", 0o644, "b"}), Limits{}, "there twice"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "plugin")

			_, err := Unpack(rawZip(t, c.entries...), dest, c.limits)
			if !errors.Is(err, ErrBundle) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}

			if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "x")); err == nil {
				t.Fatal("a file was written outside the plugin")
			}
		})
	}
}
