// Package manifest reads manifest.yaml, the file in a plugin's directory that names the plugin and
// says how to build it: the image the build runs in, its command, the platforms it is built for,
// and the files and asset directories that go with the program.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

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
	Program   string   `yaml:"program"`
	Build     Build    `yaml:"build"`
	Platforms []string `yaml:"platforms"`
	// Files are copied from the plugin's directory to the top level, beside the program.
	Files []string `yaml:"files"`
	// Assets are directories the build writes under {out}, kept under the same relative paths.
	Assets []string `yaml:"assets"`
}

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
	knownHold = []string{"{out}", "{os}", "{arch}"}
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

	return bad
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
				add("build: %s is no placeholder: {out}, {os} and {arch} are", h)
			case anyPlatform && h != "{out}":
				add("build: %s has no value for platforms [any]", h)
			}
		}
	}
}

// AnyPlatform tells whether the plugin is built once for every platform.
func (m Manifest) AnyPlatform() bool { return slices.Equal(m.Platforms, []string{Any}) }

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
func Expand(s, out, osArch string) string {
	goos, arch, _ := strings.Cut(osArch, "/")

	return strings.NewReplacer("{out}", out, "{os}", goos, "{arch}", arch).Replace(s)
}

// Digest is the SHA-256 of a manifest's bytes in hex.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
