package manifest

import (
	"errors"
	"slices"
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

const meta = `name: store
kind: meta
description: The store a step writes its results to
logo: logo.svg
implements: ["store >=1.0 <2"]
schemas:
  put: {message: schemas/put.json, answer: schemas/put-answer.json}
ack_timeout: 30s
`

func TestAMetaPluginIsSchemasAlone(t *testing.T) {
	m, err := Parse([]byte(meta))
	if err != nil {
		t.Fatal(err)
	}

	if !m.AnyPlatform() || !m.Supports("darwin/arm64") || len(m.ProgramFiles()) != 0 || m.Schemas["put"].Answer != "schemas/put-answer.json" {
		t.Fatalf("meta: %+v", m)
	}

	if got := m.Extras(); !slices.Equal(got, []string{"logo.svg", "schemas/put-answer.json", "schemas/put.json"}) {
		t.Fatalf("extras %v", got)
	}

	files := map[string]string{"schemas/put.json": `{"type": "object"}`, "schemas/put-answer.json": "{}"}
	read := func(f string) ([]byte, error) { return []byte(files[f]), nil }

	if err := CheckSchemas(m, read); err != nil {
		t.Fatal(err)
	}

	files["schemas/put.json"] = "type: object"

	if err := CheckSchemas(m, read); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), "schemas/put.json is no JSON") {
		t.Fatal(err)
	}
}

func TestListingFieldsAndMetaKeysAreRefused(t *testing.T) {
	cases := map[string]struct{ text, says string }{
		"a meta plugin's build":      {meta + "build: {image: " + image + ", run: [true]}\n", "has no build and no program"},
		"a meta plugin's program":    {meta + "program: store\n", "has no build and no program"},
		"a meta plugin's platform":   {meta + "platforms: [linux/amd64]\n", "platforms [any], or none"},
		"a schema up":                {strings.Replace(meta, "schemas/put.json", "../put.json", 1), "schemas.put"},
		"a bad ack timeout":          {strings.Replace(meta, "30s", "soon", 1), "ack_timeout"},
		"a zero ack timeout":         {strings.Replace(meta, "30s", "0s", 1), "ack_timeout"},
		"a bad range":                {strings.Replace(meta, ">=1.0 <2", "^1.0", 1), "<name> <semver range>"},
		"a requirement without one":  {meta + "requires: [store]\n", "<name> <semver range>"},
		"a logo up":                  {strings.Replace(meta, "logo.svg", "../logo.svg", 1), "logo"},
		"schemas on a driver":        {good + "schemas:\n  put: {message: a.json, answer: b.json}\n", "belong to a plugin of kind meta"},
		"an ack timeout on a driver": {good + "ack_timeout: 1s\n", "belong to a plugin of kind meta"},
		"an unknown schema key":      {strings.Replace(meta, "answer: schemas/put-answer.json", "answer: schemas/put-answer.json, error: e.json", 1), "field error not found"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.text))
			if !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}
		})
	}

	withListing := good + "description: Builds Go\nlogo: docs/logo.svg\nrequires: [\"store >=1.2 <2 || >=3\"]\n"
	if m, err := Parse([]byte(withListing)); err != nil || m.Description != "Builds Go" || len(m.Extras()) != 1 {
		t.Fatalf("a driver with a listing: %+v %v", m, err)
	}
}

func TestCommitAndVersionAreFilledIn(t *testing.T) {
	m, err := Parse([]byte(`name: board
kind: extension
build:
  image: ` + image + `
  run: [sh, -c, "echo {version} {commit} > {out}/dist/v"]
platforms: [any]
assets: [dist]
`))
	if err != nil {
		t.Fatal(err)
	}

	got := Values{Out: "/out", Commit: "abc", Version: "1.2.3"}.Expand(m.Build.Run[0][2])
	if got != "echo 1.2.3 abc > /out/dist/v" {
		t.Fatalf("expanded %q", got)
	}

	if _, err := Parse([]byte(strings.Replace(good, "{out}/go-driver", "{out}/{tag}", 1))); err == nil || !strings.Contains(err.Error(), "{tag} is no placeholder") {
		t.Fatal(err)
	}
}

const permissions = `permissions:
  - {name: runs:read, description: See the runs and their logs, default: viewer}
  - {name: runs:re-run, description: Run a failed batch again, default: operator}
`

func TestPermissionsAreReadOnEveryKind(t *testing.T) {
	for _, base := range []string{good, meta} {
		m, err := Parse([]byte(base + permissions))
		if err != nil {
			t.Fatal(err)
		}

		if len(m.Permissions) != 2 || m.Permissions[1] != (Permission{Name: "runs:re-run", Description: "Run a failed batch again", Default: "operator"}) {
			t.Fatalf("permissions %+v", m.Permissions)
		}
	}

	for _, role := range Roles {
		if _, err := Parse([]byte(good + strings.Replace(permissions, "default: viewer", "default: "+role, 1))); err != nil {
			t.Errorf("%s: %v", role, err)
		}
	}
}

func TestPermissionsAreRefused(t *testing.T) {
	cases := map[string]struct{ from, to, says string }{
		"a name without an area": {"name: runs:read", "name: read", `"read": area:action`},
		"a name in capitals":     {"name: runs:read", "name: Runs:read", `"Runs:read": area:action`},
		"a name with a digit":    {"name: runs:read", "name: runs2:read", `"runs2:read": area:action`},
		"a name with two colons": {"name: runs:read", "name: runs:read:all", `area:action`},
		"a name twice":           {"name: runs:re-run", "name: runs:read", "runs:read is there twice"},
		"no description":         {"description: See the runs and their logs, ", "", "description is missing"},
		"an empty description":   {"See the runs and their logs", `" "`, "description is missing"},
		"no default":             {", default: viewer", "", `default "" is none of`},
		"an unknown role":        {"default: viewer", "default: owner", `default "owner" is none of display, viewer, operator, maintainer, admin`},
		"an unknown key":         {"default: viewer}", "default: viewer, scope: all}", "field scope not found"},
		"permissions not a list": {permissions, "permissions: {runs:read: viewer}\n", "cannot unmarshal"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(permissions, c.from) {
				t.Fatalf("the fixture has no %q", c.from)
			}

			_, err := Parse([]byte(good + strings.Replace(permissions, c.from, c.to, 1)))
			if !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}
		})
	}
}

func TestARuntimeNamesWhatRunsTheProgram(t *testing.T) {
	for text, want := range map[string][2]string{
		"runtime: node\n":              {"node", ""},
		"runtime: \"node >=22 <27\"\n": {"node", ">=22 <27"},
		"":                             {"", ""},
	} {
		m, err := Parse([]byte(good + text))
		if err != nil {
			t.Fatal(err)
		}

		if n, r := m.RuntimeOf(); n != want[0] || r != want[1] {
			t.Errorf("%q: %q %q", text, n, r)
		}
	}

	for text, says := range map[string]string{
		good + "runtime: Node\n":         `runtime "Node": a name`,
		good + "runtime: \"node ^22\"\n": `runtime "node ^22"`,
		meta + "runtime: node\n":         "only a plugin with a program has a runtime",
	} {
		if _, err := Parse([]byte(text)); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: got %v, want %q", text, err, says)
		}
	}
}

func TestABuildRunsOneArgvOrSeveral(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil || len(m.Build.Run) != 1 || m.Build.Run[0][0] != "go" {
		t.Fatalf("one argv: %+v %v", m.Build.Run, err)
	}

	several := strings.Replace(good, `  run: [go, build, -trimpath, -o, "{out}/go-driver", ./cmd/go-driver]`,
		"  copy: true\n  run:\n    - [npm, ci]\n    - [npm, pkg, set, \"version={version}\"]\n    - [cp, dist/x.mjs, \"{out}/\"]", 1)

	m, err = Parse([]byte(several))
	if err != nil || len(m.Build.Run) != 3 || !m.Build.Copy || m.Build.Run[1][3] != "version={version}" {
		t.Fatalf("several: %+v %v", m.Build, err)
	}

	for bad, says := range map[string]string{
		strings.Replace(several, "    - [npm, ci]\n", "    - []\n", 1):     "build.run is missing a command",
		strings.Replace(several, "    - [npm, ci]\n", "    - npm ci\n", 1): "build.run: an argv",
		meta + "build: {copy: true}\n":                                     "has no build and no program",
	} {
		if _, err := Parse([]byte(bad)); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), says) {
			t.Errorf("got %v, want %q", err, says)
		}
	}
}

func TestAnUntaggedBuildIsStampedAsADevVersion(t *testing.T) {
	for _, c := range []struct{ version, commit, want string }{
		{"1.2.3", "0123456789abcdef0123456789abcdef01234567", "1.2.3"},
		{"v1.2.3", "", "1.2.3"},
		{"", "0123456789abcdef0123456789abcdef01234567", "0.0.0-dev+0123456789ab"},
		{"", "", "0.0.0-dev"},
	} {
		if got := StampVersion(c.version, c.commit); got != c.want {
			t.Errorf("%q %q: %q, want %q", c.version, c.commit, got, c.want)
		}
	}

	if got := (Values{Commit: "0123456789abcdef0123456789abcdef01234567"}).Expand("v{version}"); got != "v0.0.0-dev+0123456789ab" {
		t.Fatalf("expanded %q", got)
	}
}

const included = `name: cache
kind: service
platforms: [any]
include: [service.yaml, conf/]
`

// A plugin of included paths alone has no build: nothing runs. Beside a build, a plugin of any
// kind may include paths too, and so may a meta plugin.
func TestIncludedPathsNeedNoBuild(t *testing.T) {
	m, err := Parse([]byte(included))
	if err != nil {
		t.Fatal(err)
	}

	if m.Builds() || m.Program != "" || !slices.Equal(m.Include, []string{"service.yaml", "conf"}) || !m.Supports("linux/arm64") {
		t.Fatalf("include alone: %+v", m)
	}

	m, err = Parse([]byte(good + "include: [conf, docs/README.md]\n"))
	if err != nil || !m.Builds() || len(m.Include) != 2 {
		t.Fatalf("include beside a build: %+v %v", m, err)
	}

	m, err = Parse([]byte(meta + "include: [docs]\n"))
	if err != nil || m.Builds() || len(m.Include) != 1 {
		t.Fatalf("include in a meta plugin: %+v %v", m, err)
	}

	if m, err := Parse([]byte(meta)); err != nil || m.Builds() {
		t.Fatalf("a meta plugin: %+v %v", m, err)
	}
}

func TestIncludedPathsAreRefused(t *testing.T) {
	cases := map[string]struct{ text, says string }{
		"an absolute path":          {included + "include: [/etc/passwd]\n", ""},
		"a path up":                 {strings.Replace(included, "conf/", "../conf", 1), `include: "../conf": a file or directory of the plugin's directory`},
		"a path up within":          {strings.Replace(included, "conf/", "conf/../../x", 1), `include: "conf/../../x"`},
		"a root path":               {strings.Replace(included, "conf/", "/etc", 1), `include: "/etc"`},
		"the directory itself":      {strings.Replace(included, "conf/", ".", 1), `include: "."`},
		"a dot within":              {strings.Replace(included, "conf/", "conf/./a", 1), `include: "conf/./a"`},
		"the manifest":              {strings.Replace(included, "conf/", "manifest.yaml", 1), `include: "manifest.yaml": a file or directory`},
		"a path twice":              {strings.Replace(included, "conf/", "service.yaml", 1), `"service.yaml" and "service.yaml": the same path`},
		"a path and its parent":     {strings.Replace(included, "conf/", "conf, conf/a.yaml", 1), `"conf" and "conf/a.yaml": the same path, or one within the other`},
		"a path and its child":      {strings.Replace(included, "conf/", "conf/a.yaml, conf", 1), `"conf/a.yaml" and "conf": the same path, or one within the other`},
		"a listed file":             {included + "files: [docs/service.yaml]\n", `include: "service.yaml": service.yaml is there too (files: docs/service.yaml)`},
		"a listed file within":      {included + "files: [conf]\n", `include: "conf/": conf is there too (files: conf)`},
		"the logo":                  {included + "logo: conf/logo.svg\n", `conf/logo.svg is there too (the logo or a schema)`},
		"a schema":                  {meta + "include: [schemas]\n", `schemas/put.json is there too (the logo or a schema)`},
		"an asset directory":        {good + "assets: [web]\ninclude: [web/extra]\n", `include: "web/extra": web is there too (assets: web)`},
		"a program file":            {good + "include: [go-driver-linux-amd64]\n", "go-driver-linux-amd64 is there too (the program)"},
		"a program with no build":   {included + "program: cache\n", "build.image"},
		"assets with no build":      {included + "assets: [dist]\n", "build.image"},
		"half a build":              {included + "build: {network: true}\n", "build.image"},
		"a platform with no build":  {strings.Replace(included, "[any]", "[linux/amd64]", 1), "only a plugin built for platforms [any] may have none"},
		"nothing to deliver":        {"name: cache\nkind: service\nplatforms: [any]\n", "delivers nothing"},
		"nothing but files":         {"name: cache\nkind: service\nplatforms: [any]\nfiles: [LICENSE]\n", "delivers nothing"},
		"an include that is a list": {included + "include: [[a]]\n", ""},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			text := c.text
			if strings.Count(text, "include:") > 1 {
				// a second include key replaces the fixture's
				text = strings.Replace(text, "include: [service.yaml, conf/]\n", "", 1)
			}

			_, err := Parse([]byte(text))
			if !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want an error saying %q", err, c.says)
			}
		})
	}
}

// A spec is carried as JSON, its keys sorted, and nothing in it is checked.
func TestASpecIsCarriedAsJSON(t *testing.T) {
	m, err := Parse([]byte(included + `spec:
  port: 8080
  ratio: 0.5
  name: cache
  on: true
  none: null
  since: 2026-01-02
  octal: 0o17
  tags: [a, "1"]
  nested: {deep: [{x: 1}]}
  unknown-to-plugkit: {anything: goes}
`))
	if err != nil {
		t.Fatal(err)
	}

	want := `{"name":"cache","nested":{"deep":[{"x":1}]},"none":null,"octal":15,"on":true,"port":8080,"ratio":0.5,"since":"2026-01-02","tags":["a","1"],"unknown-to-plugkit":{"anything":"goes"}}`
	if string(m.Spec) != want {
		t.Fatalf("spec %s\nwant %s", m.Spec, want)
	}

	if m, err := Parse([]byte(included)); err != nil || m.Spec != nil {
		t.Fatalf("no spec: %s %v", m.Spec, err)
	}

	if m, err := Parse([]byte(included + "spec: {}\n")); err != nil || string(m.Spec) != "{}" {
		t.Fatalf("an empty spec: %s %v", m.Spec, err)
	}
}

func TestSpecsAreRefused(t *testing.T) {
	cases := map[string]struct{ spec, says string }{
		"a list":             {"spec: [a, b]\n", "spec (line 5): a mapping"},
		"a string":           {"spec: cache\n", "a mapping"},
		"left empty":         {"spec:\n", "a mapping"},
		"a number key":       {"spec: {1: a}\n", `key "1" is no string`},
		"a bool key":         {"spec: {true: a}\n", `key "true" is no string`},
		"a list key":         {"spec: {[a]: b}\n", "is no string"},
		"a merge key":        {"spec:\n  base: &b {x: 1}\n  more: {<<: *b}\n", "anchor or alias"},
		"an alias":           {"spec:\n  a: &x 1\n  b: *x\n", "anchor or alias"},
		"a merge alone":      {"spec:\n  <<: {x: 1}\n", `key "<<" is no string`},
		"a key twice":        {"spec:\n  a: 1\n  a: 2\n", `key "a" is there twice`},
		"infinity":           {"spec: {x: .inf}\n", ".inf is no finite number"},
		"not a number":       {"spec: {x: .nan}\n", ".nan is no finite number"},
		"a tag of its own":   {"spec: {x: !secret v}\n", "tag !secret"},
		"binary":             {"spec: {x: !!binary aGk=}\n", "tag !!binary"},
		"an integer too big": {"spec: {x: !!int 99999999999999999999}\n", "no 64-bit integer"},
		"too big":            {"spec: {x: \"" + strings.Repeat("a", MaxSpec) + "\"}\n", "more than 65536"},
		"nested too deep":    {"spec: {x: " + strings.Repeat("[", maxSpecDepth+1) + strings.Repeat("]", maxSpecDepth+1) + "}\n", "nested more than"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(included + c.spec))
			if !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want an error saying %q", err, c.says)
			}
		})
	}
}

// A logo is a file of the plugin's directory or an https URL, which is no file the plugin holds.
func TestALogoMayBeAnHTTPSURL(t *testing.T) {
	m, err := Parse([]byte(included + "logo: https://example.com/logos/cache.svg?v=2\n"))
	if err != nil || len(m.Extras()) != 0 || !LogoURL(m.Logo.Light) {
		t.Fatalf("%+v %v", m, err)
	}

	for logo, says := range map[string]string{
		"http://example.com/cache.svg":                     "an https URL with a host",
		"https:///cache.svg":                               "an https URL with a host",
		"https://me@example.com/cache.svg":                 "no user or password",
		"ftp://example.com/cache.svg":                      "an https URL",
		"https://example.com/" + strings.Repeat("a", 2100): "at most 2048 bytes",
		"https:cache.svg":                                  "or an https URL",
	} {
		if _, err := Parse([]byte(included + "logo: \"" + logo + "\"\n")); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), says) {
			t.Errorf("logo %.40q: got %v, want %q", logo, err, says)
		}
	}
}

func TestRuntimeTakesAStringOrAMapping(t *testing.T) {
	alpine := "docker.io/library/alpine@sha256:" + strings.Repeat("b", 64)

	for text, want := range map[string]Runtime{
		"runtime: \"node >=22\"\n":                                         {Program: "node >=22"},
		"runtime: {program: \"node >=22\"}\n":                              {Program: "node >=22"},
		"runtime: {image: " + alpine + "}\n":                               {Image: alpine},
		"runtime:\n  native: true\n  image: " + alpine + "\n":              {Image: alpine, Native: true},
		"runtime: {program: node, image: " + alpine + ", native: false}\n": {Program: "node", Image: alpine},
	} {
		m, err := Parse([]byte(good + text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}

		if m.Runtime != want {
			t.Errorf("%q: %+v, want %+v", text, m.Runtime, want)
		}
	}

	if m, err := Parse([]byte(good + "runtime: \"node >=22\"\n")); err != nil {
		t.Fatal(err)
	} else if n, r := m.RuntimeOf(); n != "node" || r != ">=22" {
		t.Errorf("RuntimeOf %q %q", n, r)
	}

	for text, says := range map[string]string{
		good + "runtime: {image: alpine:3}\n":             `runtime.image "alpine:3": an image pinned by digest`,
		meta + "runtime: {native: true}\n":                "only a plugin with a program has a runtime",
		meta + "runtime: {image: " + alpine + "}\n":       "only a plugin with a program has a runtime",
		good + "runtime: {other: 1}\n":                    "runtime: line 10: field other not found",
		good + "runtime: [node]\n":                        "runtime: line 10: a string",
		good + "runtime: {native: maybe}\n":               "cannot unmarshal",
		good + "runtime: {program: Node, native: true}\n": `runtime "Node": a name`,
	} {
		if _, err := Parse([]byte(text)); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: got %v, want %q", text, err, says)
		}
	}
}

func TestALogoTakesAStringOrAMapping(t *testing.T) {
	for text, want := range map[string]Logo{
		"logo: logo.svg\n": {Light: "logo.svg"},
		"logo: {light: logo.svg, dark: logo-dark.svg}\n":           {Light: "logo.svg", Dark: "logo-dark.svg"},
		"logo:\n  light: logo.svg\n":                               {Light: "logo.svg"},
		"logo: {dark: logo-dark.svg}\n":                            {Dark: "logo-dark.svg"},
		"logo: {light: logo.svg, dark: \"https://x.dev/d.png\"}\n": {Light: "logo.svg", Dark: "https://x.dev/d.png"},
	} {
		m, err := Parse([]byte(good + text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}

		if m.Logo != want {
			t.Errorf("%q: %+v, want %+v", text, m.Logo, want)
		}
	}

	// either alone stands in for both
	if l := (Logo{Dark: "d.svg"}); l.ForLight() != "d.svg" || l.ForDark() != "d.svg" {
		t.Errorf("dark alone: %q %q", l.ForLight(), l.ForDark())
	}

	if l := (Logo{Light: "l.svg"}); l.ForLight() != "l.svg" || l.ForDark() != "l.svg" {
		t.Errorf("light alone: %q %q", l.ForLight(), l.ForDark())
	}

	// both files go along beside the manifest, a URL doesn't
	m, err := Parse([]byte(good + "logo: {light: logo.svg, dark: logo-dark.svg}\n"))
	if err != nil || !slices.Equal(m.Extras(), []string{"logo-dark.svg", "logo.svg"}) {
		t.Fatalf("extras %q, %v", m.Extras(), err)
	}

	for text, says := range map[string]string{
		good + "logo: {light: logo.svg, other: x.svg}\n": "logo: line 10: field other not found: it takes light and dark",
		good + "logo: [logo.svg]\n":                      "logo: line 10: a string",
		good + "logo: {dark: ../logo.svg}\n":             `logo.dark "../logo.svg": a file of the plugin's directory`,
		good + "logo: {light: \"http://x.dev/l.png\"}\n": `logo.light "http://x.dev/l.png": an https URL`,
	} {
		if _, err := Parse([]byte(text)); !errors.Is(err, ErrManifest) || !strings.Contains(err.Error(), says) {
			t.Errorf("%q: got %v, want %q", text, err, says)
		}
	}
}
