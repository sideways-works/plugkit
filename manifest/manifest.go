// Package manifest reads manifest.yaml, the file in a plugin's directory that names the plugin and
// says how to build it: the image the build runs in, its command, the platforms it is built for,
// and the files and asset directories that go with the program.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/semver"
	"go.yaml.in/yaml/v3"
)

// FileName is the manifest's name in a plugin's directory and at the top of its zip.
const FileName = "manifest.yaml"

// Any is the one platform of a build whose output runs everywhere: a Node script, or assets alone.
const Any = "any"

var ErrManifest = errors.New("manifest")

type Manifest struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
	// Program is the executable the build writes into {out}: <program>, or <program>.mjs for a
	// Node plugin. It may be left out only with platforms [any], for a plugin of assets alone.
	Program string `yaml:"program"`
	// Runtime is what runs the program besides the machine itself, with an optional semver range:
	// node for a <program>.mjs, "node >=22 <27". Empty means a native binary.
	Runtime   string   `yaml:"runtime"`
	Build     Build    `yaml:"build"`
	Platforms []string `yaml:"platforms"`
	// Files are copied from the plugin's directory to the top level, beside the program.
	Files []string `yaml:"files"`
	// Assets are directories the build writes under {out}, kept under the same relative paths.
	Assets []string `yaml:"assets"`
	// Description and Logo (a file of the plugin's directory) are for listings.
	Description string `yaml:"description"`
	Logo        string `yaml:"logo"`
	// Implements and Requires are "<name> <semver range>": the interfaces the plugin offers and those
	// it needs another plugin to offer.
	Implements []string `yaml:"implements"`
	Requires   []string `yaml:"requires"`
	// Schemas, of a meta plugin alone, are its messages by name, each a JSON Schema file of the
	// plugin's directory for the message and one for its answer.
	Schemas map[string]Schema `yaml:"schemas"`
	// AckTimeout, of a meta plugin alone, is a Go duration: how long a receiver has to ack or nack
	// each message.
	AckTimeout string `yaml:"ack_timeout"`
	// Permissions are what the plugin lets people do, each granted by default from a role up.
	Permissions []Permission `yaml:"permissions"`
}

// Permission is one thing a plugin lets people do: its name (area:action), what it allows, and the
// least role that has it by default.
type Permission struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Default     string `yaml:"default"`
}

// Roles are the roles a permission's default may name, least first.
var Roles = []string{"display", "viewer", "operator", "maintainer", "admin"}

// Schema is a meta plugin's message: the JSON Schema files of the message and of its answer.
type Schema struct {
	Message string `yaml:"message"`
	Answer  string `yaml:"answer"`
}

// KindMeta is a plugin of schemas alone: nothing is built and nothing runs.
const KindMeta = "meta"

type Build struct {
	// Image is pinned by digest: name[:tag]@sha256:<64 hex>.
	Image   string            `yaml:"image"`
	Run     []string          `yaml:"run"`
	Env     map[string]string `yaml:"env"`
	Network bool              `yaml:"network"`
}

var (
	name      = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	program   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	platform  = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+$`)
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	digest    = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	holder    = regexp.MustCompile(`\{[^{}]*\}`)
	relPath   = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	reserved  = []string{FileName}
	knownHold = []string{"{out}", "{os}", "{arch}", "{commit}", "{version}"}
)

// Parse reads a manifest and refuses unknown keys, an image without a digest, and placeholders
// other than {out}, {os} and {arch}.
func Parse(data []byte) (Manifest, error) {
	var m Manifest

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return Manifest{}, fmt.Errorf("%w: empty", ErrManifest)
		}

		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}

	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: one document only", ErrManifest)
	}

	if problems := m.check(); len(problems) > 0 {
		return Manifest{}, fmt.Errorf("%w: %s", ErrManifest, strings.Join(problems, "; "))
	}

	for i, a := range m.Assets {
		m.Assets[i] = strings.TrimSuffix(a, "/")
	}

	return m, nil
}

func (m Manifest) check() []string {
	var bad []string

	add := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }

	if !name.MatchString(m.Name) {
		add("name %q: lower case letters, digits and dashes", m.Name)
	}

	if !name.MatchString(m.Kind) {
		add("kind %q: lower case letters, digits and dashes", m.Kind)
	}

	m.checkListing(add)
	m.checkPermissions(add)
	m.checkRuntime(add)

	if m.Kind == KindMeta {
		m.checkMeta(add)

		return bad
	}

	if len(m.Schemas) > 0 || m.AckTimeout != "" {
		add("schemas and ack_timeout belong to a plugin of kind %s", KindMeta)
	}

	m.checkPlatforms(add)

	switch {
	case m.Program == "" && !m.AnyPlatform():
		add("program is missing; only a plugin built for platforms [any] may have none, holding assets alone")
	case m.Program == "" && len(m.Assets) == 0:
		add("a plugin with no program needs assets")
	case m.Program != "" && (!program.MatchString(m.Program) || strings.HasSuffix(m.Program, ".mjs")):
		add("program %q: a file name, without .mjs: the build writes <program> or <program>.mjs", m.Program)
	}

	m.Build.check(m.AnyPlatform(), add)
	m.checkLayout(add)

	return bad
}

// checkLayout checks the files and assets the zip holds beside the program.
func (m Manifest) checkLayout(add func(string, ...any)) {
	seen := map[string]bool{}

	for _, f := range m.Files {
		base := path.Base(f)

		switch {
		case !relPath.MatchString(f) || slices.Contains(strings.Split(f, "/"), ".."):
			add("files: %q: a path relative to the plugin's directory, without ..", f)
		case seen[base] || slices.Contains(reserved, base) || m.isProgramFile(base):
			add("files: %q: another file of the zip's top level has the name %s", f, base)
		}

		seen[base] = true
	}

	for _, a := range m.Assets {
		dir := strings.TrimSuffix(a, "/")
		top, _, _ := strings.Cut(dir, "/")

		switch {
		case !relPath.MatchString(dir) || slices.Contains(strings.Split(dir, "/"), "..") || slices.Contains(strings.Split(dir, "/"), "."):
			add("assets: %q: a directory relative to the plugin's directory, without .. or .", a)
		case seen[top] || slices.Contains(reserved, top) || m.isProgramFile(top):
			add("assets: %q: another entry of the zip's top level has the name %s", a, top)
		}
	}
}

func (m Manifest) checkListing(add func(string, ...any)) {
	if m.Logo != "" && !localPath(m.Logo) {
		add("logo %q: a file of the plugin's directory, without ..", m.Logo)
	}

	for key, list := range map[string][]string{"implements": m.Implements, "requires": m.Requires} {
		for _, e := range list {
			n, r, ok := strings.Cut(strings.TrimSpace(e), " ")
			if _, err := semver.ParseRange(strings.TrimSpace(r)); !ok || !name.MatchString(n) || err != nil {
				add("%s: %q: <name> <semver range>, such as \"store >=1.2 <2\"", key, e)
			}
		}
	}
}

// RuntimeOf is the runtime's name and its version range, empty when the manifest names none.
func (m Manifest) RuntimeOf() (string, string) {
	n, r, _ := strings.Cut(strings.TrimSpace(m.Runtime), " ")

	return n, strings.TrimSpace(r)
}

func (m Manifest) checkRuntime(add func(string, ...any)) {
	if m.Runtime == "" {
		return
	}

	n, r := m.RuntimeOf()

	if !name.MatchString(n) {
		add("runtime %q: a name, such as node, and an optional semver range", m.Runtime)
	} else if r != "" {
		if _, err := semver.ParseRange(r); err != nil {
			add("runtime %q: %v", m.Runtime, err)
		}
	}

	if m.Program == "" {
		add("runtime %q: only a plugin with a program has a runtime", m.Runtime)
	}
}

var permissionName = regexp.MustCompile(`^[a-z-]+:[a-z-]+$`)

func (m Manifest) checkPermissions(add func(string, ...any)) {
	seen := map[string]bool{}

	for i, p := range m.Permissions {
		switch {
		case !permissionName.MatchString(p.Name):
			add("permissions[%d].name %q: area:action, lower-case letters and dashes", i, p.Name)
		case seen[p.Name]:
			add("permissions[%d]: %s is there twice", i, p.Name)
		}

		seen[p.Name] = true

		if strings.TrimSpace(p.Description) == "" {
			add("permissions[%d] (%s): description is missing", i, p.Name)
		}

		if !slices.Contains(Roles, p.Default) {
			add("permissions[%d] (%s): default %q is none of %s", i, p.Name, p.Default, strings.Join(Roles, ", "))
		}
	}
}

func (m Manifest) checkMeta(add func(string, ...any)) {
	b := m.Build

	switch {
	case m.Program != "" || b.Image != "" || len(b.Run) > 0 || len(b.Env) > 0 || b.Network:
		add("a plugin of kind %s has no build and no program", KindMeta)
	case len(m.Platforms) > 0 && !slices.Equal(m.Platforms, []string{Any}):
		add("a plugin of kind %s is the same on every platform: platforms [any], or none", KindMeta)
	case len(m.Assets) > 0:
		add("a plugin of kind %s has no assets: nothing builds them", KindMeta)
	}

	for _, n := range slices.Sorted(maps.Keys(m.Schemas)) {
		sc := m.Schemas[n]
		if !name.MatchString(n) || !localPath(sc.Message) || !localPath(sc.Answer) {
			add("schemas.%s: a message's name, and its message and answer, each a file of the plugin's directory", n)
		}
	}

	if m.AckTimeout != "" {
		if d, err := time.ParseDuration(m.AckTimeout); err != nil || d <= 0 {
			add("ack_timeout %q: a Go duration above zero, such as 30s", m.AckTimeout)
		}
	}
}

func localPath(p string) bool {
	return relPath.MatchString(p) && !slices.Contains(strings.Split(p, "/"), "..") && !slices.Contains(strings.Split(p, "/"), ".") && p != FileName
}

func (m Manifest) checkPlatforms(add func(string, ...any)) {
	switch {
	case len(m.Platforms) == 0:
		add("platforms is missing: os/arch pairs such as linux/amd64, or [any]")
	case slices.Contains(m.Platforms, Any) && len(m.Platforms) > 1:
		add("platforms: any is the only platform of a build that runs everywhere")
	}

	for i, p := range m.Platforms {
		if p != Any && !platform.MatchString(p) {
			add("platforms: %q: os/arch, such as linux/amd64", p)
		}

		if slices.Contains(m.Platforms[:i], p) {
			add("platforms: %q twice", p)
		}
	}
}

func (b Build) check(anyPlatform bool, add func(string, ...any)) {
	if !digest.MatchString(b.Image) || strings.HasPrefix(b.Image, "@") {
		add("build.image %q: an image pinned by digest, name@sha256:<64 hex>", b.Image)
	}

	if len(b.Run) == 0 {
		add("build.run is missing: the command, as a list")
	}

	for k := range b.Env {
		if !envName.MatchString(k) {
			add("build.env: %q is no variable name", k)
		}
	}

	all := slices.Clone(b.Run)
	for _, v := range b.Env {
		all = append(all, v)
	}

	for _, s := range all {
		for _, h := range holder.FindAllString(s, -1) {
			switch {
			case !slices.Contains(knownHold, h):
				add("build: %s is no placeholder: {out}, {os}, {arch}, {commit} and {version} are", h)
			case anyPlatform && (h == "{os}" || h == "{arch}"):
				add("build: %s has no value for platforms [any]", h)
			}
		}
	}
}

// AnyPlatform tells whether the plugin is built once for every platform, as a meta plugin is.
func (m Manifest) AnyPlatform() bool {
	return m.Kind == KindMeta || slices.Equal(m.Platforms, []string{Any})
}

// Extras are the files of the plugin's directory kept under their own paths beside the manifest:
// the logo and a meta plugin's schemas.
func (m Manifest) Extras() []string {
	var out []string

	if m.Logo != "" {
		out = append(out, m.Logo)
	}

	for _, name := range slices.Sorted(maps.Keys(m.Schemas)) {
		out = append(out, m.Schemas[name].Message, m.Schemas[name].Answer)
	}

	slices.Sort(out)

	return slices.Compact(out)
}

// CheckSchemas reads each of a meta plugin's schema files with read and says which isn't JSON.
func CheckSchemas(m Manifest, read func(string) ([]byte, error)) error {
	for _, name := range slices.Sorted(maps.Keys(m.Schemas)) {
		for _, f := range []string{m.Schemas[name].Message, m.Schemas[name].Answer} {
			data, err := read(f)
			if err != nil {
				return fmt.Errorf("%w: schemas.%s: %w", ErrManifest, name, err)
			}

			if !json.Valid(data) {
				return fmt.Errorf("%w: schemas.%s: %s is no JSON", ErrManifest, name, f)
			}
		}
	}

	return nil
}

// Supports tells whether the plugin is built for platform, given as os/arch.
func (m Manifest) Supports(osArch string) bool {
	return m.AnyPlatform() || slices.Contains(m.Platforms, osArch)
}

// Binary is the name of the program's file for platform (os/arch) in the zip and the cache.
func (m Manifest) Binary(osArch string) string {
	return m.Program + "-" + strings.ReplaceAll(osArch, "/", "-")
}

// Script is the name of a Node plugin's one file, the same on every platform.
func (m Manifest) Script() string { return m.Program + ".mjs" }

// ProgramFiles are the names the program may have at the zip's top level.
func (m Manifest) ProgramFiles() []string {
	if m.Program == "" {
		return nil
	}

	out := []string{m.Script()}

	for _, p := range m.Platforms {
		if p != Any {
			out = append(out, m.Binary(p))
		}
	}

	return out
}

func (m Manifest) isProgramFile(name string) bool { return slices.Contains(m.ProgramFiles(), name) }

// IsProgramFile tells whether name is one of the program's files at the zip's top level.
func (m Manifest) IsProgramFile(name string) bool { return m.isProgramFile(name) }

// FileBase is where a listed file lies at the zip's top level.
func FileBase(file string) string { return path.Base(file) }

// ImageDigest is the build image's sha256:<hex>.
func (m Manifest) ImageDigest() string {
	_, d, _ := strings.Cut(m.Build.Image, "@")

	return d
}

// Expand fills the placeholders of s; with platforms [any], os and arch are empty and unused.
func Expand(s, out, osArch string) string { return Values{Out: out, Platform: osArch}.Expand(s) }

// Values are what a build's placeholders stand for: {out}; {os} and {arch} of Platform (os/arch);
// {commit}, the full commit being built; and {version}, the version of the tag the build came
// through, without a leading v, empty when it came through none.
type Values struct {
	Out, Platform, Commit, Version string
}

func (v Values) Expand(s string) string {
	goos, arch, _ := strings.Cut(v.Platform, "/")

	return strings.NewReplacer("{out}", v.Out, "{os}", goos, "{arch}", arch, "{commit}", v.Commit, "{version}", v.Version).Replace(s)
}

// Digest is the SHA-256 of a manifest's bytes in hex.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
