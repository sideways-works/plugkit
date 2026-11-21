// Package repository reads a plugin repository's manifest, the list of the plugins it holds, and
// lists their versions from its tags with plain git: no forge's API, the caller's git credentials.
//
// The manifest is repository-manifest.yaml at the repository's root, or a root manifest.yaml with
// a plugins list. A root manifest.yaml without one is the manifest of the repository's one plugin,
// at the path ".".
package repository

import (
	"bytes"
	"cmp"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/gitsafe"
	"github.com/sideways-works/plugkit/manifest"
	"github.com/sideways-works/plugkit/semver"
	"go.yaml.in/yaml/v3"
)

// FileName is the repository manifest at the repository's root.
const FileName = "repository-manifest.yaml"

// Format is the one version of the repository manifest this package reads.
const Format = 1

var ErrRepository = errors.New("repository manifest")

type Repository struct {
	Format int `yaml:"repository"`
	// Tags names a version's tag, with {name}, {path} and {version}.
	Tags    string  `yaml:"tags"`
	Plugins []Entry `yaml:"plugins"`
}

// Entry is one plugin: its manifest, whose directory is its path, and what a listing shows of it;
// empty fields take the plugin manifest's.
type Entry struct {
	Manifest    string        `yaml:"manifest"`
	Name        string        `yaml:"name"`
	Kind        string        `yaml:"kind"`
	Description string        `yaml:"description"`
	Logo        manifest.Logo `yaml:"logo"`
}

// Path is the plugin's directory, "." for the repository's root.
func (e Entry) Path() string { return path.Dir(e.Manifest) }

const (
	// The tags' defaults: a repository's one plugin at its root, and a repository of several.
	singleTags = "v{version}"
	manyTags   = "{name}-v{version}"
)

var (
	relPath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)
	holder  = regexp.MustCompile(`\{[^{}]*\}`)
)

// Parse reads a repository manifest, refusing unknown keys, another format, a path that leaves the
// repository, a plugin listed twice and a placeholder other than {name}, {path} and {version}.
func Parse(data []byte) (Repository, error) {
	var r Repository

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&r); err != nil {
		if errors.Is(err, io.EOF) {
			return Repository{}, fmt.Errorf("%w: empty", ErrRepository)
		}

		return Repository{}, fmt.Errorf("%w: %w", ErrRepository, err)
	}

	if r.Tags == "" {
		r.Tags = manyTags
		if len(r.Plugins) == 1 && r.Plugins[0].Path() == "." {
			r.Tags = singleTags
		}
	}

	if problems := r.check(); len(problems) > 0 {
		return Repository{}, fmt.Errorf("%w: %s", ErrRepository, strings.Join(problems, "; "))
	}

	return r, nil
}

func (r Repository) check() []string {
	var bad []string

	add := func(format string, args ...any) { bad = append(bad, fmt.Sprintf(format, args...)) }

	if r.Format != Format {
		add("repository: %d is no format this reads; it reads %d", r.Format, Format)
	}

	if strings.Count(r.Tags, "{version}") != 1 {
		add("tags %q: needs {version} once", r.Tags)
	}

	for _, h := range holder.FindAllString(r.Tags, -1) {
		if h != "{name}" && h != "{path}" && h != "{version}" {
			add("tags %q: %s is no placeholder: {name}, {path} and {version} are", r.Tags, h)
		}
	}

	if len(r.Plugins) == 0 {
		add("plugins lists none")
	}

	paths, names := map[string]bool{}, map[string]bool{}

	for i, e := range r.Plugins {
		switch {
		case !local(e.Manifest) || path.Base(e.Manifest) != manifest.FileName:
			add("plugins[%d].manifest %q: a %s in the repository, without ..", i, e.Manifest, manifest.FileName)
		case paths[e.Path()]:
			add("plugins[%d]: %s is listed twice", i, e.Path())
		case e.Name != "" && names[e.Name]:
			add("plugins[%d]: the name %s is listed twice", i, e.Name)
		case e.Logo.Light != "" && !local(e.Logo.Light):
			add("plugins[%d].logo.light %q: a file of the repository, without ..", i, e.Logo.Light)
		case e.Logo.Dark != "" && !local(e.Logo.Dark):
			add("plugins[%d].logo.dark %q: a file of the repository, without ..", i, e.Logo.Dark)
		}

		paths[e.Path()] = true
		if e.Name != "" {
			names[e.Name] = true
		}
	}

	return bad
}

func local(p string) bool {
	parts := strings.Split(p, "/")

	return relPath.MatchString(p) && !slices.Contains(parts, "..") && !slices.Contains(parts, ".")
}

// Plugin is a plugin of a repository as a listing shows it, its versions newest first. Err says
// why its manifest couldn't be read, and the listing holds the other plugins all the same.
type Plugin struct {
	Path, Name, Kind, Description string
	// Logo is the logo for a light color scheme, a file of the repository or an https URL. LogoData
	// is its content and LogoType its media type: an SVG, PNG, JPEG or WebP image of at most MaxLogo
	// bytes, by its content. LogoErr says why there is none; it never fails the plugin. An SVG is
	// the plugin author's, and a host sanitizes it before rendering it.
	Logo     string
	LogoData []byte
	LogoType string
	LogoErr  error
	// DarkLogo and the fields after it are the same for a dark color scheme. A plugin with one
	// logo has it in both.
	DarkLogo     string
	DarkLogoData []byte
	DarkLogoType string
	DarkLogoErr  error
	// Spec is the manifest's spec as JSON, for the host to read; empty when it has none.
	Spec     json.RawMessage
	Versions []Version
	Err      error
}

// Version is a version of a plugin, its tag and the commit the tag names.
type Version struct {
	Version, Tag, Commit string
}

// beforeRecheck runs between At's fetch of a tag and its second look at it; tests move the tag there.
var beforeRecheck = func() {}

// MaxLogo bounds a logo's bytes in a listing.
const MaxLogo = 256 << 10

var (
	ErrLogo  = errors.New("logo")
	ErrMoved = errors.New("the tag moved")
)

type Options struct {
	// Prereleases lists versions with a pre-release part too.
	Prereleases bool
	// GitEnv is added to git's environment, for credentials the caller gives git. Untrusted leaves
	// it out.
	GitEnv []string
	// Untrusted is for a URL from someone the host doesn't trust, a server's user say: only an https
	// or ssh remote, nothing of the host's git config, credential helpers or ssh keys, Credential as
	// the only credential, and the fetches bounded by Timeout and MaxBytes.
	Untrusted  bool
	Credential gitsafe.Credential
	// KnownHosts are the known_hosts lines ssh checks an untrusted ssh remote's host key against;
	// without them every ssh remote fails, as no host key comes from the account's own.
	KnownHosts string
	// Timeout bounds List or At, MaxBytes what its fetches write; with Untrusted, 0 is 2 minutes and
	// 256 MiB.
	Timeout  time.Duration
	MaxBytes int64
	// Commit is the commit At must find the version's tag naming, when the caller knows it.
	Commit string
	// AllowLocal lets an untrusted URL be file:// or a local path, and a logo's URL reach a
	// loopback address: for tests alone, as a local path reads the host's own files and loopback
	// reaches its own services.
	AllowLocal bool
	// LogoCache is a directory that keeps the logos fetched from a URL, by the URL and the
	// plugin's version, so a listing doesn't fetch them again; without one, each listing does.
	LogoCache string

	// env is added last, for tests that must give git what Untrusted takes away.
	env []string
	// roots are the certificates a logo's server is checked against, for tests; nil is the system's.
	roots *x509.CertPool
}

const (
	defaultTimeout  = 2 * time.Minute
	defaultMaxBytes = 256 << 20
)

// open checks url as opt says and starts git's scratch repository, with ctx bounded as opt says.
func open(ctx context.Context, url string, opt Options) (*git, context.Context, context.CancelFunc, error) {
	cancel := context.CancelFunc(func() {})

	if opt.Untrusted {
		if err := gitsafe.Check(url, opt.AllowLocal); err != nil {
			return nil, nil, nil, fmt.Errorf("%w: %w", ErrRepository, err)
		}

		timeout := cmp.Or(opt.Timeout, defaultTimeout)
		ctx, cancel = context.WithTimeout(ctx, timeout)
		opt.MaxBytes = cmp.Or(opt.MaxBytes, defaultMaxBytes)
	}

	g, err := newGit(ctx, url, opt)
	if err != nil {
		cancel()

		return nil, nil, nil, err
	}

	return g, ctx, cancel, nil
}

// List lists the plugins of the repository at url with their versions: the tags git ls-remote
// lists that match the manifest's tags, and the rest from one shallow fetch of its default branch.
func List(ctx context.Context, url string, opt Options) ([]Plugin, error) {
	g, ctx, cancel, err := open(ctx, url, opt)
	if err != nil {
		return nil, err
	}
	defer cancel()
	defer g.close()

	head, err := g.fetch(ctx, url, "HEAD")
	if err != nil {
		return nil, err
	}

	r, err := g.repository(ctx, head)
	if err != nil {
		return nil, err
	}

	tags, err := g.tags(ctx, url)
	if err != nil {
		return nil, err
	}

	out := make([]Plugin, 0, len(r.Plugins))
	seen := map[string]bool{}

	for _, e := range r.Plugins {
		p := g.listed(ctx, head, e)
		if p.Err == nil && seen[p.Name] {
			p.Err = fmt.Errorf("%w: another plugin is named %s", ErrRepository, p.Name)
		}

		seen[p.Name] = p.Name != ""

		// without its name, a plugin whose manifest didn't read has no tags to tell
		if p.Name != "" || !strings.Contains(r.Tags, "{name}") {
			if p.Versions, err = versions(r.Tags, p, tags, opt.Prereleases); err != nil {
				return nil, err
			}
		}

		if p.Err == nil {
			// a logo's URL is fetched once for each version: the newest, or with none the commit
			version := head
			if len(p.Versions) > 0 {
				version = p.Versions[0].Version
			}

			p.LogoData, p.LogoType, p.LogoErr, p.DarkLogoData, p.DarkLogoType, p.DarkLogoErr = g.logos(ctx, head, p.Logo, p.DarkLogo, version, opt)
		}

		out = append(out, p)
	}

	return out, nil
}

// listed is e as a listing shows it, with Err set when its manifest at commit doesn't read.
func (g *git) listed(ctx context.Context, commit string, e Entry) Plugin {
	p, err := g.plugin(ctx, commit, e)
	if err != nil {
		return Plugin{Path: e.Path(), Name: e.Name, Kind: e.Kind, Description: e.Description, Logo: e.Logo.ForLight(), DarkLogo: e.Logo.ForDark(), Err: err}
	}

	return p
}

// logos are the images at light and at dark, as logo reads each; the same name is read once.
func (g *git) logos(ctx context.Context, commit, light, dark, version string, opt Options) ([]byte, string, error, []byte, string, error) { //nolint:revive // light's three and dark's three
	var (
		lData, dData []byte
		lType, dType string
		lErr, dErr   error
	)

	if light != "" {
		lData, lType, lErr = g.logo(ctx, commit, light, version, opt)
	}

	switch {
	case dark == light:
		dData, dType, dErr = lData, lType, lErr
	case dark != "":
		dData, dType, dErr = g.logo(ctx, commit, dark, version, opt)
	}

	return lData, lType, lErr, dData, dType, dErr
}

// logo is the image at name, a file of the repository at commit or an https URL fetched for
// version, and its media type: SVG, PNG, JPEG or WebP by its content, within MaxLogo bytes.
func (g *git) logo(ctx context.Context, commit, name, version string, opt Options) ([]byte, string, error) {
	if manifest.LogoURL(name) {
		return fetchLogo(ctx, name, version, opt)
	}

	size, err := g.run(ctx, "cat-file", "-s", commit+":"+name)
	if err != nil {
		return nil, "", fmt.Errorf("%w %s: %w", ErrLogo, name, err)
	}

	if n, err := strconv.ParseInt(strings.TrimSpace(string(size)), 10, 64); err != nil || n > MaxLogo {
		return nil, "", fmt.Errorf("%w %s: over %d bytes", ErrLogo, name, MaxLogo)
	}

	data, err := g.run(ctx, "cat-file", "blob", commit+":"+name)
	if err != nil {
		return nil, "", fmt.Errorf("%w %s: %w", ErrLogo, name, err)
	}

	typ := imageType(data)
	if typ == "" {
		return nil, "", fmt.Errorf("%w %s: neither SVG, PNG, JPEG nor WebP", ErrLogo, name)
	}

	return data, typ, nil
}

// imageType is data's media type when it is an image a listing shows, by its content.
func imageType(data []byte) string {
	switch t := http.DetectContentType(data); t {
	case "image/png", "image/jpeg", "image/webp":
		return t
	case "text/xml; charset=utf-8", "text/plain; charset=utf-8":
		head := bytes.ToLower(data[:min(len(data), 1024)])
		if bytes.Contains(head, []byte("<svg")) {
			return "image/svg+xml"
		}
	}

	return ""
}

// At is the manifest of the plugin at path in the repository at url, read at the commit its tag
// for version names, and that commit. It refuses a tag that names another commit once read than it
// did when fetched, and one that names another commit than opt.Commit when that is set.
func At(ctx context.Context, url, dir, version string, opt Options) (manifest.Manifest, string, error) {
	r, err := at(ctx, url, dir, version, opt, false)

	return r.Manifest, r.Commit, err
}

// Release is a version of a plugin: its manifest and commit as At reads them, and its logos as a
// listing gives them, LogoErr and DarkLogoErr saying why there is none.
type Release struct {
	Manifest     manifest.Manifest
	Commit       string
	Logo         string
	LogoData     []byte
	LogoType     string
	LogoErr      error
	DarkLogo     string
	DarkLogoData []byte
	DarkLogoType string
	DarkLogoErr  error
}

// AtRelease is At with the version's logo: a file of the repository at the version's commit, or
// an https URL fetched as List fetches it.
func AtRelease(ctx context.Context, url, dir, version string, opt Options) (Release, error) {
	return at(ctx, url, dir, version, opt, true)
}

func at(ctx context.Context, url, dir, version string, opt Options, withLogo bool) (Release, error) {
	g, ctx, cancel, err := open(ctx, url, opt)
	if err != nil {
		return Release{}, err
	}
	defer cancel()
	defer g.close()

	head, err := g.fetch(ctx, url, "HEAD")
	if err != nil {
		return Release{}, err
	}

	r, err := g.repository(ctx, head)
	if err != nil {
		return Release{}, err
	}

	i := slices.IndexFunc(r.Plugins, func(e Entry) bool { return e.Path() == path.Clean(dir) })
	if i < 0 {
		return Release{}, fmt.Errorf("%w: %s lists no plugin at %s", ErrRepository, url, dir)
	}

	p, err := g.plugin(ctx, head, r.Plugins[i])
	if err != nil {
		return Release{}, err
	}

	tag := strings.NewReplacer("{name}", p.Name, "{path}", p.Path, "{version}", strings.TrimPrefix(version, "v")).Replace(r.Tags)

	commit, err := g.fetch(ctx, url, "refs/tags/"+tag)
	if err != nil {
		return Release{}, fmt.Errorf("%w: %s has no tag %s for version %s of %s: %w", ErrRepository, url, tag, version, p.Name, err)
	}

	m, err := g.manifest(ctx, commit, r.Plugins[i].Manifest)
	if err != nil {
		return Release{}, err
	}

	beforeRecheck()

	// the tag is read again: one moved while it was fetched names another commit than the one read
	now, err := g.tags(ctx, url, "refs/tags/"+tag, "refs/tags/"+tag+"^{}")

	switch {
	case err != nil:
		return Release{}, err
	case now[tag] != commit:
		return Release{}, fmt.Errorf("%w: %s's tag %s named %s when fetched and names %q now", ErrMoved, url, tag, commit, now[tag])
	case opt.Commit != "" && opt.Commit != commit:
		return Release{}, fmt.Errorf("%w: %s's tag %s names %s, not %s", ErrMoved, url, tag, commit, opt.Commit)
	}

	logo := logoOf(r.Plugins[i], m)

	rel := Release{Manifest: m, Commit: commit, Logo: logo.ForLight(), DarkLogo: logo.ForDark()}
	if withLogo {
		rel.LogoData, rel.LogoType, rel.LogoErr, rel.DarkLogoData, rel.DarkLogoType, rel.DarkLogoErr = g.logos(ctx, commit, rel.Logo, rel.DarkLogo,
			strings.TrimPrefix(version, "v"), opt)
	}

	return rel, nil
}

// logoOf is the logo a listing shows of e: the repository manifest's, files named from the
// repository's root, or else the plugin manifest's, files of its directory or URLs.
func logoOf(e Entry, m manifest.Manifest) manifest.Logo {
	if !e.Logo.IsZero() {
		return e.Logo
	}

	inRepo := func(l string) string {
		if l == "" || manifest.LogoURL(l) {
			return l
		}

		return path.Join(e.Path(), l)
	}

	return manifest.Logo{Light: inRepo(m.Logo.Light), Dark: inRepo(m.Logo.Dark)}
}

// versions are the tags of p that pattern names, newest first.
func versions(pattern string, p Plugin, tags map[string]string, pre bool) ([]Version, error) {
	quoted := regexp.QuoteMeta(pattern)
	quoted = strings.NewReplacer(
		regexp.QuoteMeta("{name}"), regexp.QuoteMeta(p.Name),
		regexp.QuoteMeta("{path}"), regexp.QuoteMeta(p.Path),
		regexp.QuoteMeta("{version}"), `([0-9][0-9A-Za-z.+-]*)`,
	).Replace(quoted)

	re, err := regexp.Compile("^" + quoted + "$")
	if err != nil {
		return nil, fmt.Errorf("%w: tags %q: %w", ErrRepository, pattern, err)
	}

	type found struct {
		Version

		v semver.Version
	}

	var all []found

	for tag, commit := range tags {
		m := re.FindStringSubmatch(tag)
		if m == nil {
			continue
		}

		v, err := semver.Parse(m[1])
		if err != nil || v.Prerelease() && !pre {
			continue
		}

		all = append(all, found{Version{Version: m[1], Tag: tag, Commit: commit}, v})
	}

	slices.SortFunc(all, func(a, b found) int { return b.v.Compare(a.v) })

	out := make([]Version, 0, len(all))
	for _, f := range all {
		out = append(out, f.Version)
	}

	return out, nil
}

// git runs git in a scratch bare repository of its own, repo under dir, beside an empty home.
type git struct {
	dir, repo string
	env       []string
	maxBytes  int64
}

func newGit(ctx context.Context, url string, opt Options) (*git, error) {
	dir, err := os.MkdirTemp("", "plugkit-repository-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRepository, err)
	}

	g := &git{dir: dir, repo: filepath.Join(dir, "repo"), maxBytes: opt.MaxBytes}

	if opt.Untrusted {
		home := filepath.Join(dir, "home")
		if err := os.Mkdir(home, 0o700); err != nil {
			g.close()

			return nil, fmt.Errorf("%w: %w", ErrRepository, err)
		}

		if err := gitsafe.Prepare(home, opt.KnownHosts); err != nil {
			g.close()

			return nil, fmt.Errorf("%w: %w", ErrRepository, err)
		}

		g.env = gitsafe.Env(os.Environ(), home, url, opt.Credential)
		if opt.AllowLocal {
			g.env = gitsafe.AllowLocal(g.env)
		}
	} else {
		g.env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), opt.GitEnv...)
	}

	g.env = append(g.env, opt.env...)

	if _, err := g.run(ctx, "init", "-q", "--bare", g.repo); err != nil {
		g.close()

		return nil, err
	}

	return g, nil
}

func (g *git) close() { _ = os.RemoveAll(g.dir) } //nolint:errcheck // a scratch repository

func (g *git) run(ctx context.Context, args ...string) ([]byte, error) {
	if args[0] != "init" {
		args = append([]string{"-C", g.repo}, args...)
	}

	out, err := gitsafe.Run(ctx, func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // git, reading the repository the caller names
		cmd.Env = g.env

		return cmd
	}, g.dir, g.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: git %s: %w", ErrRepository, strings.Join(args, " "), err)
	}

	return out, nil
}

// fetch fetches ref of url, one commit deep, and returns its commit.
func (g *git) fetch(ctx context.Context, url, ref string) (string, error) {
	if _, err := g.run(ctx, "fetch", "-q", "--depth", "1", "--no-tags", "--", url, ref); err != nil {
		return "", err
	}

	out, err := g.run(ctx, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

// tags are the repository's tags by name, each with its commit.
func (g *git) tags(ctx context.Context, url string, patterns ...string) (map[string]string, error) {
	out, err := g.run(ctx, append([]string{"ls-remote", "--tags", "--", url}, patterns...)...)
	if err != nil {
		return nil, err
	}

	tags := map[string]string{}

	for line := range strings.Lines(string(out)) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}

		name, ok := strings.CutPrefix(ref, "refs/tags/")
		if !ok {
			continue
		}

		// an annotated tag's line ending ^{} names its commit, the plain line the tag object
		if base, peeled := strings.CutSuffix(name, "^{}"); peeled {
			tags[base] = sha
		} else if _, seen := tags[name]; !seen {
			tags[name] = sha
		}
	}

	return tags, nil
}

func (g *git) file(ctx context.Context, commit, name string) ([]byte, bool, error) {
	if _, err := g.run(ctx, "cat-file", "-e", commit+":"+name); err != nil {
		return nil, false, nil //nolint:nilerr // the file isn't there
	}

	data, err := g.run(ctx, "show", commit+":"+name)

	return data, err == nil, err
}

// repository is the repository manifest at commit, or the one plugin a root manifest.yaml without a
// plugins list makes.
func (g *git) repository(ctx context.Context, commit string) (Repository, error) {
	if data, ok, err := g.file(ctx, commit, FileName); err != nil || ok {
		if err != nil {
			return Repository{}, err
		}

		return Parse(data)
	}

	data, ok, err := g.file(ctx, commit, manifest.FileName)

	switch {
	case err != nil:
		return Repository{}, err
	case !ok:
		return Repository{}, fmt.Errorf("%w: neither %s nor %s at the repository's root", ErrRepository, FileName, manifest.FileName)
	}

	var keys map[string]any
	if err := yaml.Unmarshal(data, &keys); err != nil {
		return Repository{}, fmt.Errorf("%w: %s: %w", ErrRepository, manifest.FileName, err)
	}

	if _, many := keys["plugins"]; many {
		return Parse(data)
	}

	return Repository{Format: Format, Tags: singleTags, Plugins: []Entry{{Manifest: manifest.FileName}}}, nil
}

func (g *git) manifest(ctx context.Context, commit, name string) (manifest.Manifest, error) {
	data, ok, err := g.file(ctx, commit, name)

	switch {
	case err != nil:
		return manifest.Manifest{}, err
	case !ok:
		return manifest.Manifest{}, fmt.Errorf("%w: no %s at %s", ErrRepository, name, commit)
	}

	m, err := manifest.Parse(data)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %s: %w", ErrRepository, name, err)
	}

	return m, nil
}

// plugin is e as a listing shows it, its empty fields taken from its manifest at commit.
func (g *git) plugin(ctx context.Context, commit string, e Entry) (Plugin, error) {
	m, err := g.manifest(ctx, commit, e.Manifest)
	if err != nil {
		return Plugin{}, err
	}

	p := Plugin{Path: e.Path(), Name: e.Name, Kind: e.Kind, Description: e.Description, Spec: m.Spec}

	if p.Name == "" {
		p.Name = m.Name
	}

	if p.Kind == "" {
		p.Kind = m.Kind
	}

	if p.Description == "" {
		p.Description = m.Description
	}

	logo := logoOf(e, m)
	p.Logo, p.DarkLogo = logo.ForLight(), logo.ForDark()

	return p, nil
}
