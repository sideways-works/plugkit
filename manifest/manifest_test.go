package manifest

import (
	"errors"
	"strings"
	"testing"
)

const image = "golang:1.25.7-bookworm@sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1"

const good = `name: go-driver
kind: driver
program: go-driver
build:
  image: ` + image + `
  run: [go, build, -trimpath, -o, "{out}/go-driver", ./cmd/go-driver]
  env: {CGO_ENABLED: "0", GOOS: "{os}", GOARCH: "{arch}"}
platforms: [linux/amd64, linux/arm64, darwin/amd64, darwin/arm64]
files: [LICENSE, NOTICE]
`

func TestAManifestReadsWithItsProgramFilesAndImage(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}

	if m.Name != "go-driver" || m.Kind != "driver" || m.Program != "go-driver" || len(m.Platforms) != 4 || m.Build.Network {
		t.Fatalf("manifest: %+v", m)
	}

	if got := m.Binary("darwin/arm64"); got != "go-driver-darwin-arm64" {
		t.Fatalf("binary: %s", got)
	}

	if got := m.ImageDigest(); got != "sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1" {
		t.Fatalf("image digest: %s", got)
	}

	if got := Expand(m.Build.Env["GOARCH"], "/out", "linux/arm64"); got != "arm64" {
		t.Fatalf("expand: %s", got)
	}

	if !m.IsProgramFile("go-driver-linux-amd64") || !m.IsProgramFile("go-driver.mjs") || m.IsProgramFile("go-driver-windows-amd64") {
		t.Fatal("program files")
	}
}

func TestAnAssetsOnlyPluginIsBuiltForAnyPlatform(t *testing.T) {
	m, err := Parse([]byte(`name: board
kind: extension
build:
  image: busybox@sha256:` + strings.Repeat("a", 64) + `
  run: [sh, -c, "cp -r web {out}/dist"]
platforms: [any]
assets: [dist/]
`))
	if err != nil {
		t.Fatal(err)
	}

	if !m.AnyPlatform() || m.Program != "" || m.Assets[0] != "dist" || !m.Supports("darwin/arm64") || len(m.ProgramFiles()) != 0 {
		t.Fatalf("manifest: %+v", m)
	}
}

func TestManifestsAreRefused(t *testing.T) {
	cases := map[string]struct{ from, to, says string }{
		"an unknown key":           {"files:", "extra: 1\nfiles:", "field extra not found"},
		"an unknown build key":     {"  env:", "  shell: bash\n  env:", "field shell not found"},
		"an image by tag":          {image, "golang:1.25.7", "pinned by digest"},
		"a short digest":           {image, "golang@sha256:abc", "pinned by digest"},
		"an unknown placeholder":   {`"{out}/go-driver"`, `"{dir}/go-driver"`, "{dir} is no placeholder"},
		"no program":               {"program: go-driver\n", "", "program is missing"},
		"a program named .mjs":     {"program: go-driver", "program: go-driver.mjs", "without .mjs"},
		"no platforms":             {"platforms: [linux/amd64, linux/arm64, darwin/amd64, darwin/arm64]", "", "platforms is missing"},
		"a platform by dash":       {"linux/arm64,", "linux-arm64,", `"linux-arm64": os/arch`},
		"any beside others":        {"linux/arm64,", "any,", "any is the only platform"},
		"os and arch with any":     {"[linux/amd64, linux/arm64, darwin/amd64, darwin/arm64]", "[any]", "{os} has no value for platforms [any]"},
		"a file outside":           {"[LICENSE, NOTICE]", "[../LICENSE]", "without .."},
		"two files of one name":    {"[LICENSE, NOTICE]", "[LICENSE, docs/LICENSE]", "has the name LICENSE"},
		"a file named as manifest": {"[LICENSE, NOTICE]", "[docs/manifest.yaml]", "has the name manifest.yaml"},
		"an asset outside":         {"files:", "assets: [../web]\nfiles:", "without .. or ."},
		"an asset over a file":     {"files:", "assets: [LICENSE/x]\nfiles:", "has the name LICENSE"},
		"no command":               {`  run: [go, build, -trimpath, -o, "{out}/go-driver", ./cmd/go-driver]` + "\n", "", "build.run is missing"},
		"two documents":            {"files: [LICENSE, NOTICE]\n", "files: [LICENSE, NOTICE]\n---\nname: x\n", "one document only"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(good, c.from) {
				t.Fatalf("the fixture has no %q", c.from)
			}

			_, err := Parse([]byte(strings.Replace(good, c.from, c.to, 1)))
			if !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want an error saying %q", err, c.says)
			}
		})
	}

	if _, err := Parse(nil); !errors.Is(err, ErrManifest) {
		t.Fatalf("empty: %v", err)
	}
}
