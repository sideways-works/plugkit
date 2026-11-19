// Package repository reads a plugin repository's manifest, the list of the plugins it holds, and
// lists their versions from its tags with plain git: no forge's API, the caller's git credentials.
//
// The manifest is repository-manifest.yaml at the repository's root, or a root manifest.yaml with
// a plugins list. A root manifest.yaml without one is the manifest of the repository's one plugin,
// at the path ".".
package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"

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
	Manifest    string `yaml:"manifest"`
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind"`
	Description string `yaml:"description"`
	Logo        string `yaml:"logo"`
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
		case e.Logo != "" && !local(e.Logo):
			add("plugins[%d].logo %q: a file of the repository, without ..", i, e.Logo)
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

// Plugin is a plugin of a repository as a listing shows it, its versions newest first.
type Plugin struct {
	Path, Name, Kind, Description, Logo string
	Versions                            []Version
}

type Version struct {
	Version string
	Commit  string
}

type Options struct {
	// Prereleases lists versions with a pre-release part too.
	Prereleases bool
	// GitEnv is added to git's environment, for credentials the caller gives git.
	GitEnv []string
}

// List lists the plugins of the repository at url with their versions: the tags git ls-remote
// lists that match the manifest's tags, and the rest from one shallow fetch of its default branch.
func List(ctx context.Context, url string, opt Options) ([]Plugin, error) {
	g, err := newGit(ctx, opt.GitEnv)
	if err != nil {
		return nil, err
	}
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
		p, err := g.plugin(ctx, head, e)
		if err != nil {
			return nil, err
		}

		if seen[p.Name] {
			return nil, fmt.Errorf("%w: two plugins are named %s", ErrRepository, p.Name)
		}

		seen[p.Name] = true

		if p.Versions, err = versions(r.Tags, p, tags, opt.Prereleases); err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, nil
}

// At is the manifest of the plugin at path in the repository at url, read at the commit its tag
// for version names, and that commit.
func At(ctx context.Context, url, dir, version string) (manifest.Manifest, string, error) {
	return AtWith(ctx, url, dir, version, Options{Prereleases: true})
}

// AtWith is At with git's environment from opt.
func AtWith(ctx context.Context, url, dir, version string, opt Options) (manifest.Manifest, string, error) {
	g, err := newGit(ctx, opt.GitEnv)
	if err != nil {
		return manifest.Manifest{}, "", err
	}
	defer g.close()

	head, err := g.fetch(ctx, url, "HEAD")
	if err != nil {
		return manifest.Manifest{}, "", err
	}

	r, err := g.repository(ctx, head)
	if err != nil {
		return manifest.Manifest{}, "", err
	}

	i := slices.IndexFunc(r.Plugins, func(e Entry) bool { return e.Path() == path.Clean(dir) })
	if i < 0 {
		return manifest.Manifest{}, "", fmt.Errorf("%w: %s lists no plugin at %s", ErrRepository, url, dir)
	}

	p, err := g.plugin(ctx, head, r.Plugins[i])
	if err != nil {
		return manifest.Manifest{}, "", err
	}

	tag := strings.NewReplacer("{name}", p.Name, "{path}", p.Path, "{version}", strings.TrimPrefix(version, "v")).Replace(r.Tags)

	commit, err := g.fetch(ctx, url, "refs/tags/"+tag)
	if err != nil {
		return manifest.Manifest{}, "", fmt.Errorf("%w: %s has no tag %s for version %s of %s: %w", ErrRepository, url, tag, version, p.Name, err)
	}

	m, err := g.manifest(ctx, commit, r.Plugins[i].Manifest)
	if err != nil {
		return manifest.Manifest{}, "", err
	}

	return m, commit, nil
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

		all = append(all, found{Version{Version: m[1], Commit: commit}, v})
	}

	slices.SortFunc(all, func(a, b found) int { return b.v.Compare(a.v) })

	out := make([]Version, 0, len(all))
	for _, f := range all {
		out = append(out, f.Version)
	}

	return out, nil
}

// git runs git in a scratch bare repository of its own.
type git struct {
	dir string
	env []string
}

func newGit(ctx context.Context, env []string) (*git, error) {
	dir, err := os.MkdirTemp("", "plugkit-repository-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRepository, err)
	}

	g := &git{dir: dir, env: env}
	if _, err := g.run(ctx, "init", "-q", "--bare"); err != nil {
		g.close()

		return nil, err
	}

	return g, nil
}

func (g *git) close() { _ = os.RemoveAll(g.dir) } //nolint:errcheck // a scratch repository

func (g *git) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...) //nolint:gosec // git, reading the repository the caller names
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), g.env...)

	var errOut bytes.Buffer

	cmd.Stderr = &errOut

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: git %s: %w: %s", ErrRepository, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}

	return out, nil
}

// fetch fetches ref of url, one commit deep, and returns its commit.
func (g *git) fetch(ctx context.Context, url, ref string) (string, error) {
	if _, err := g.run(ctx, "fetch", "-q", "--depth", "1", "--no-tags", url, ref); err != nil {
		return "", err
	}

	out, err := g.run(ctx, "rev-parse", "FETCH_HEAD^{commit}")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(out)), nil
}

// tags are the repository's tags by name, each with its commit.
func (g *git) tags(ctx context.Context, url string) (map[string]string, error) {
	out, err := g.run(ctx, "ls-remote", "--tags", url)
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

	p := Plugin{Path: e.Path(), Name: e.Name, Kind: e.Kind, Description: e.Description, Logo: e.Logo}

	if p.Name == "" {
		p.Name = m.Name
	}

	if p.Kind == "" {
		p.Kind = m.Kind
	}

	if p.Description == "" {
		p.Description = m.Description
	}

	if p.Logo == "" && m.Logo != "" {
		p.Logo = path.Join(p.Path, m.Logo)
	}

	return p, nil
}
