// Package manifest reads manifest.yaml, the file in a plugin's directory that names the plugin and
// says how to build it: the image the build runs in, its command, the platforms it is built for,
// the files and asset directories that go with the program, and the paths of the plugin's
// directory it includes as they are.
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
	"net/url"
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
	// Node plugin. It may be left out only with platforms [any], for a plugin of assets or included
	// paths alone.
	Program string `yaml:"program"`
	// Runtime is what runs the program besides the machine itself: a string names the program
	// runtime alone, a mapping may add the image a host runs the program in and whether the host may
	// run it directly.
	Runtime   Runtime  `yaml:"runtime"`
	Build     Build    `yaml:"build"`
	Platforms []string `yaml:"platforms"`
	// Files are copied from the plugin's directory to the top level, beside the program.
	Files []string `yaml:"files"`
	// Assets are directories the build writes under {out}, kept under the same relative paths.
	Assets []string `yaml:"assets"`
	// Include are files and directories of the plugin's directory, copied as git tracks them under
	// the same relative paths, after the build if there is one. A plugin with no program may have
	// no build at all: then nothing runs.
	Include []string `yaml:"include"`
	// Description, Logo and Color are for listings.
	Description string `yaml:"description"`
	Logo        Logo   `yaml:"logo"`
	Color       Color  `yaml:"color"`
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
	// Spec is the manifest's spec, a mapping for the host to read and check, as JSON; plugkit
	// carries it and never looks into it. Empty when the manifest has none.
	Spec json.RawMessage `yaml:"-"`
	// Extra are the top-level keys plugkit doesn't read, which the Extensions given to ParseWith
	// accept, and the Moving keys, each as its YAML holds it, lines and all. Nil when there are none.
	Extra map[string]yaml.Node `yaml:"-"`
}

// document is a manifest as its YAML holds it: spec is read as a node, to convert to JSON, and the
// keys plugkit doesn't read are kept to check against the extensions.
type document struct {
	Manifest `yaml:",inline"`

	Spec  yaml.Node            `yaml:"spec"`
	Extra map[string]yaml.Node `yaml:",inline"`
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
	Image string            `yaml:"image"`
	Run   Commands          `yaml:"run"`
	Env   map[string]string `yaml:"env"`
	// Network gives the build a network; Copy runs it in a writable copy of the repository, for a
	// build that writes beside its sources (npm ci).
	Network bool `yaml:"network"`
	Copy    bool `yaml:"copy"`
}

// Commands are a build's commands, each an argv run with no shell, in order, the first that fails
// failing the build: one argv in the manifest, or a list of them.
type Commands [][]string

var errCommands = errors.New("build.run: an argv, such as [go, build, ./cmd/x], or a list of them")

func (c *Commands) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.SequenceNode {
		return errCommands
	}

	if len(n.Content) > 0 && n.Content[0].Kind == yaml.ScalarNode {
		var argv []string
		if err := n.Decode(&argv); err != nil {
			return fmt.Errorf("%w: %w", errCommands, err)
		}

		*c = Commands{argv}

		return nil
	}

	var all [][]string
	if err := n.Decode(&all); err != nil {
		return fmt.Errorf("%w: %w", errCommands, err)
	}

	*c = all

	return nil
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

// Parse reads a manifest and refuses unknown keys, an image without a digest, placeholders
// other than {out}, {os} and {arch}, and a spec that is no mapping JSON can say.
func Parse(data []byte) (Manifest, error) { return ParseWith(data, nil) }

// ParseWith is Parse with a product's extensions: the keys they accept are kept in Extra, checked
// by their checks, and every other key plugkit doesn't read is refused with its line.
func ParseWith(data []byte, ext Extensions) (Manifest, error) {
	var d document

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&d); err != nil {
		if errors.Is(err, io.EOF) {
			return Manifest{}, fmt.Errorf("%w: empty", ErrManifest)
		}

		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}

	var more any
	if err := dec.Decode(&more); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: one document only", ErrManifest)
	}

	m := d.Manifest

	spec, err := specOf(&d.Spec)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}

	m.Spec = spec

	extra, lines, err := extraOf(data, d.Extra)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifest, err)
	}

	m.Extra = extra

	var refused []string

	ext.check(m.Extra, Moving, lines, func(format string, args ...any) { refused = append(refused, fmt.Sprintf(format, args...)) })

	refused = append(refused, m.check()...)

	if problems := refused; len(problems) > 0 {
		return Manifest{}, fmt.Errorf("%w: %s", ErrManifest, strings.Join(problems, "; "))
	}

	for i, a := range m.Assets {
		m.Assets[i] = strings.TrimSuffix(a, "/")
	}

	for i, inc := range m.Include {
		m.Include[i] = strings.TrimSuffix(inc, "/")
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
	m.checkInclude(add)

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
		add("program is missing; only a plugin built for platforms [any] may have none, holding assets or included paths alone")
	case m.Program == "" && len(m.Assets) == 0 && len(m.Include) == 0:
		add("a plugin with no program delivers nothing: it needs assets its build writes, or include")
	case m.Program != "" && (!program.MatchString(m.Program) || strings.HasSuffix(m.Program, ".mjs")):
		add("program %q: a file name, without .mjs: the build writes <program> or <program>.mjs", m.Program)
	}

	// a plugin of included paths alone has no build; a program or assets need one
	if m.Program != "" || len(m.Assets) > 0 || len(m.Include) == 0 || m.Build.set() {
		m.Build.check(m.AnyPlatform(), add)
	}

	m.checkLayout(add)

	return bad
}

// Builds tells whether the plugin has a build to run; one of kind meta or of included paths alone
// has none, and needs no container.
func (m Manifest) Builds() bool { return m.Kind != KindMeta && m.Build.set() }

// set tells whether the manifest has a build key at all.
func (b Build) set() bool {
	return b.Image != "" || len(b.Run) > 0 || len(b.Env) > 0 || b.Network || b.Copy
}

// checkInclude checks the included paths: each relative to the plugin's directory, and none at
// or within another's, or at or within a path the zip holds otherwise.
func (m Manifest) checkInclude(add func(string, ...any)) {
	taken := map[string]string{FileName: "the manifest"}

	for _, f := range m.Files {
		taken[FileBase(f)] = "files: " + f
	}

	for _, a := range m.Assets {
		taken[strings.TrimSuffix(a, "/")] = "assets: " + a
	}

	for _, e := range m.Extras() {
		taken[e] = "the logo or a schema"
	}

	for _, p := range m.ProgramFiles() {
		taken[p] = "the program"
	}

	var seen []string

	for _, raw := range m.Include {
		inc := strings.TrimSuffix(raw, "/")
		if !localPath(inc) {
			add("include: %q: a file or directory of the plugin's directory, relative, without .. or .", raw)

			continue
		}

		if i := slices.IndexFunc(seen, func(s string) bool { return overlaps(s, inc) }); i >= 0 {
			add("include: %q and %q: the same path, or one within the other; list it once", seen[i], raw)
		}

		for _, at := range slices.Sorted(maps.Keys(taken)) {
			if overlaps(at, inc) {
				add("include: %q: %s is there too (%s); a path comes from one place", raw, at, taken[at])
			}
		}

		seen = append(seen, inc)
	}
}

// overlaps tells whether a and b are the same path or one lies within the other.
func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
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
	for _, l := range []struct{ key, logo string }{{"logo.light", m.Logo.Light}, {"logo.dark", m.Logo.Dark}} {
		switch {
		case l.logo == "" || localPath(l.logo):
		case strings.Contains(l.logo, "://"):
			if !LogoURL(l.logo) {
				add("%s %q: an https URL with a host and no user or password, at most %d bytes", l.key, l.logo, maxLogoURL)
			}
		default:
			add("%s %q: a file of the plugin's directory, without .., or an https URL", l.key, l.logo)
		}
	}

	for _, c := range []struct{ key, color string }{{"color.light", m.Color.Light}, {"color.dark", m.Color.Dark}} {
		if c.color != "" && !hexColor.MatchString(c.color) {
			add("%s %q: a color as #rrggbb, six hex digits", c.key, c.color)
		}
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

// Runtime is what runs the program besides the machine itself.
type Runtime struct {
	// Program is the program runtime with an optional semver range: node for a <program>.mjs,
	// "node >=22 <27". Empty means a native binary. The string form of runtime sets it alone.
	Program string `yaml:"program"`
	// Image is the image a host runs the program in, pinned by digest: name[:tag]@sha256:<64 hex>.
	Image string `yaml:"image"`
	// Native says the program needs nothing of its host: a host may run it directly, as a process,
	// when it trusts the plugin.
	Native bool `yaml:"native"`
}

var errRuntime = errors.New("runtime")

// UnmarshalYAML reads a string as the program runtime, and a mapping of program, image and native,
// refusing any other key.
func (r *Runtime) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&r.Program) //nolint:wrapcheck // the decoder's own error, with its line
	case yaml.MappingNode:
	default:
		return fmt.Errorf("%w: line %d: a string, such as \"node >=22\", or a mapping of program, image and native", errRuntime, n.Line)
	}

	var out Runtime

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]

		var err error

		switch k.Value {
		case "program":
			err = v.Decode(&out.Program)
		case "image":
			err = v.Decode(&out.Image)
		case "native":
			err = v.Decode(&out.Native)
		default:
			return fmt.Errorf("%w: line %d: field %s not found: it takes program, image and native", errRuntime, k.Line, k.Value)
		}

		if err != nil {
			return err //nolint:wrapcheck // the decoder's own error, with its line
		}
	}

	*r = out

	return nil
}

// IsZero tells whether the manifest names no runtime.
func (r Runtime) IsZero() bool { return r == Runtime{} }

// RuntimeOf is the program runtime's name and its version range, empty when the manifest names
// none.
func (m Manifest) RuntimeOf() (string, string) {
	n, r, _ := strings.Cut(strings.TrimSpace(m.Runtime.Program), " ")

	return n, strings.TrimSpace(r)
}

func (m Manifest) checkRuntime(add func(string, ...any)) {
	if m.Runtime.IsZero() {
		return
	}

	if m.Runtime.Program != "" {
		n, r := m.RuntimeOf()

		if !name.MatchString(n) {
			add("runtime %q: a name, such as node, and an optional semver range", m.Runtime.Program)
		} else if r != "" {
			if _, err := semver.ParseRange(r); err != nil {
				add("runtime %q: %v", m.Runtime.Program, err)
			}
		}
	}

	if img := m.Runtime.Image; img != "" && (!digest.MatchString(img) || strings.HasPrefix(img, "@")) {
		add("runtime.image %q: an image pinned by digest, name@sha256:<64 hex>", img)
	}

	if m.Program == "" {
		add("runtime: only a plugin with a program has a runtime")
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
	case m.Program != "" || b.Image != "" || len(b.Run) > 0 || len(b.Env) > 0 || b.Network || b.Copy:
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

// Logo is the image a listing shows of a plugin, for a light and for a dark color scheme: each a
// file of the plugin's directory or an https URL. The string form sets Light alone, and either one
// alone stands in for both.
type Logo struct {
	Light string `yaml:"light"`
	Dark  string `yaml:"dark"`
}

var errLogo = errors.New("logo")

// UnmarshalYAML reads a string, such as "logo.svg", as Light, and a mapping of light and dark.
func (l *Logo) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*l = Logo{}

		return n.Decode(&l.Light) //nolint:wrapcheck // the decoder's own error, with its line
	case yaml.MappingNode:
	default:
		return fmt.Errorf("%w: line %d: a string, such as \"logo.svg\", or a mapping of light and dark", errLogo, n.Line)
	}

	var out Logo

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]

		var err error

		switch k.Value {
		case "light":
			err = v.Decode(&out.Light)
		case "dark":
			err = v.Decode(&out.Dark)
		default:
			return fmt.Errorf("%w: line %d: field %s not found: it takes light and dark", errLogo, k.Line, k.Value)
		}

		if err != nil {
			return err //nolint:wrapcheck // the decoder's own error, with its line
		}
	}

	*l = out

	return nil
}

// Color is the background a listing gives a plugin's logo, for a light and for a dark color scheme,
// each #rrggbb; or Transparent, the string transparent, for none. Either color alone stands in for
// both. How it contrasts with what is drawn on it is the listing's to judge.
type Color struct {
	Light       string `yaml:"light"`
	Dark        string `yaml:"dark"`
	Transparent bool   `yaml:"-"`
}

// colorTransparent is the string form of a color that is none.
const colorTransparent = "transparent"

var (
	errColor = errors.New("color")
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// UnmarshalYAML reads the string transparent, and a mapping of light and dark.
func (c *Color) UnmarshalYAML(n *yaml.Node) error {
	switch {
	case n.Kind == yaml.ScalarNode && n.Value == colorTransparent:
		*c = Color{Transparent: true}

		return nil
	case n.Kind != yaml.MappingNode:
		return fmt.Errorf("%w: line %d: the string %s, or a mapping of light and dark, each #rrggbb", errColor, n.Line, colorTransparent)
	}

	var out Color

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]

		var err error

		switch k.Value {
		case "light":
			err = v.Decode(&out.Light)
		case "dark":
			err = v.Decode(&out.Dark)
		default:
			return fmt.Errorf("%w: line %d: field %s not found: it takes light and dark", errColor, k.Line, k.Value)
		}

		if err != nil {
			return err //nolint:wrapcheck // the decoder's own error, with its line
		}
	}

	*c = out

	return nil
}

// IsZero tells whether the manifest names no color.
func (c Color) IsZero() bool { return c == Color{} }

// ForLight is the color for a light color scheme: Light, or else Dark; none when transparent.
func (c Color) ForLight() string {
	if c.Light != "" {
		return c.Light
	}

	return c.Dark
}

// ForDark is the color for a dark color scheme: Dark, or else Light; none when transparent.
func (c Color) ForDark() string {
	if c.Dark != "" {
		return c.Dark
	}

	return c.Light
}

// IsZero tells whether there is no logo.
func (l Logo) IsZero() bool { return l == Logo{} }

// ForLight is the logo for a light color scheme: Light, or else Dark.
func (l Logo) ForLight() string {
	if l.Light != "" {
		return l.Light
	}

	return l.Dark
}

// ForDark is the logo for a dark color scheme: Dark, or else Light.
func (l Logo) ForDark() string {
	if l.Dark != "" {
		return l.Dark
	}

	return l.Light
}

// Files are the logos named, each once.
func (l Logo) Files() []string {
	var out []string

	for _, f := range []string{l.Light, l.Dark} {
		if f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}

	return out
}

// maxLogoURL bounds a logo's URL.
const maxLogoURL = 2048

// LogoURL tells whether logo is an https URL, which a listing fetches, rather than a file of the
// plugin's directory.
func LogoURL(logo string) bool {
	u, err := url.Parse(logo)

	return err == nil && len(logo) <= maxLogoURL && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Opaque == ""
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

	if len(b.Run) == 0 || slices.ContainsFunc(b.Run, func(argv []string) bool { return len(argv) == 0 }) {
		add("build.run is missing a command: an argv, as a list, or a list of them")
	}

	for k := range b.Env {
		if !envName.MatchString(k) {
			add("build.env: %q is no variable name", k)
		}
	}

	all := slices.Concat(b.Run...)
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
// the logo, unless it is a URL, and a meta plugin's schemas.
func (m Manifest) Extras() []string {
	var out []string

	for _, l := range m.Logo.Files() {
		if !LogoURL(l) {
			out = append(out, l)
		}
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
// through, without a leading v (see StampVersion when it came through none).
type Values struct {
	Out, Platform, Commit, Version string
}

func (v Values) Expand(s string) string {
	goos, arch, _ := strings.Cut(v.Platform, "/")

	return strings.NewReplacer("{out}", v.Out, "{os}", goos, "{arch}", arch, "{commit}", v.Commit, "{version}", StampVersion(v.Version, v.Commit)).Replace(s)
}

// StampVersion is the version a build is stamped with: version without a leading v, or with none
// 0.0.0-dev and the commit's first 12 characters as build metadata, a version Go and npm both take.
func StampVersion(version, commit string) string {
	if v := strings.TrimPrefix(version, "v"); v != "" {
		return v
	}

	if len(commit) >= 12 {
		return "0.0.0-dev+" + commit[:12]
	}

	return "0.0.0-dev"
}

// Digest is the SHA-256 of a manifest's bytes in hex.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
