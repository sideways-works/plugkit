// Package build builds a plugin from its manifest in a container, the way its zip and the cache
// hold it: manifest.yaml byte for byte, the program for each platform, the manifest's files, its
// assets and its included paths, all in one directory. A plugin with no build, of kind meta or of
// included paths alone, is laid out with no container.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sideways-works/plugkit/container"
	"github.com/sideways-works/plugkit/manifest"
)

var ErrBuild = errors.New("plugin build")

var (
	errNotPlain   = errors.New("is no plain file")
	errExecutable = errors.New("is executable, and only the program may be")
)

// Where the build sees the repository and its output.
const (
	srcAt = "/src"
	outAt = "/out"
)

type Options struct {
	// Tool is docker or podman, found on PATH when empty.
	Tool string
	// Root is the repository, mounted read-only; Dir is the plugin's directory in it, slash-separated,
	// empty for the root, and the build's working directory.
	Root, Dir string
	// Manifest is manifest.yaml as the plugin's directory holds it, written to Out unchanged.
	Manifest []byte
	// Platforms are os/arch pairs to build for, all of the manifest's when empty.
	Platforms []string
	// Out is the directory the plugin is laid out in; it must not exist yet.
	Out string
	Log io.Writer
	// Commit and Version are what {commit} and {version} stand for: the full commit the build is of,
	// and the version of the tag it came through, empty when it came through none.
	Commit, Version string
	// Tree says Root holds a commit's tree and nothing else, as a store's export of a pinned commit
	// does; otherwise Root is a git checkout, and build.copy and include copy the files its index
	// tracks.
	Tree bool
}

// Build runs the manifest's command once per platform and lays the plugin out in o.Out.
func Build(ctx context.Context, o Options) (manifest.Manifest, error) {
	m, err := manifest.Parse(o.Manifest)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	platforms, err := choose(m, o.Platforms)
	if err != nil {
		return m, err
	}

	if o.Log == nil {
		o.Log = io.Discard
	}

	b := builder{o: o, m: m}

	// a plugin with no build, of kind meta or of included paths alone, runs nothing
	if m.Builds() {
		if b.tool, err = container.Find(o.Tool); err != nil {
			return m, fmt.Errorf("%w: %w", ErrBuild, err)
		}

		if _, err := container.Ensure(ctx, b.tool, m.Build.Image, o.Log); err != nil {
			return m, fmt.Errorf("%w: %w", ErrBuild, err)
		}
	}

	if err := os.Mkdir(o.Out, 0o750); err != nil {
		return m, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	if m.Build.Copy && m.Builds() {
		dir, err := os.MkdirTemp(filepath.Dir(o.Out), ".src-")
		if err != nil {
			return m, fmt.Errorf("%w: %w", ErrBuild, err)
		}
		defer os.RemoveAll(dir) //nolint:errcheck // the build's own copy

		if err := copySources(ctx, o, dir); err != nil {
			return m, err
		}

		b.src = dir
	}

	scratch := map[string]string{}

	defer func() {
		for _, dir := range scratch {
			_ = os.RemoveAll(dir) //nolint:errcheck // a scratch directory
		}
	}()

	built, err := b.runAll(ctx, platforms, scratch)
	if err != nil {
		return m, err
	}

	return m, b.layout(ctx, built, scratch)
}

// runAll runs the build's commands for each platform in order, into a scratch directory of its own
// it adds to scratch, and says which platforms it built.
func (b builder) runAll(ctx context.Context, platforms []string, scratch map[string]string) ([]string, error) {
	var built []string

	for _, p := range platforms {
		if !b.m.Builds() {
			break
		}

		dir, err := os.MkdirTemp(filepath.Dir(b.o.Out), ".build-")
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBuild, err)
		}

		scratch[p] = dir
		built = append(built, p)

		if err := b.run(ctx, p, dir); err != nil {
			return nil, err
		}

		// a Node plugin's script is the same on every platform: one build makes it
		if b.m.Program != "" && regular(filepath.Join(dir, b.m.Script())) {
			break
		}
	}

	return built, nil
}

// LayoutOptions are as Options for the plugin's layout alone: Scratch has the directory each
// platform's build wrote {out} into, by os/arch, or any for a plugin of platforms [any].
type LayoutOptions struct {
	Root, Dir string
	Manifest  []byte
	Tree      bool
	Out       string
	Scratch   map[string]string
}

// Layout lays the plugin out in o.Out, which must not exist yet, exactly as Build does once its
// commands ran: for a host that runs the build's commands itself, each platform's into a scratch
// directory of its own. It moves each program out of its scratch directory and copies the assets
// of the first of the manifest's platforms, so the layout is Build's when the commands wrote what
// Build's would. A plugin that builds nothing takes no scratch directory.
func Layout(ctx context.Context, o LayoutOptions) (manifest.Manifest, error) {
	m, err := manifest.Parse(o.Manifest)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	var order []string

	switch {
	case !m.Builds() && len(o.Scratch) > 0:
		return m, fmt.Errorf("%w: plugin %s has no build to lay out", ErrBuild, m.Name)
	case m.Builds() && len(o.Scratch) == 0:
		return m, fmt.Errorf("%w: plugin %s: no build's {out} to lay out", ErrBuild, m.Name)
	case m.Builds():
		want := slices.Sorted(maps.Keys(o.Scratch))

		if order, err = choose(m, want); err != nil {
			return m, err
		}

		if !m.AnyPlatform() {
			order = slices.DeleteFunc(slices.Clone(m.Platforms), func(p string) bool { return o.Scratch[p] == "" })
		}
	}

	if err := os.Mkdir(o.Out, 0o750); err != nil {
		return m, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	b := builder{o: Options{Root: o.Root, Dir: o.Dir, Manifest: o.Manifest, Tree: o.Tree, Out: o.Out}, m: m}

	return m, b.layout(ctx, order, o.Scratch)
}

// layout moves each platform's program from its scratch directory, in order, keeping the first's
// assets and stopping at a Node script, then copies the included paths, files and extras and writes
// the manifest.
func (b builder) layout(ctx context.Context, platforms []string, scratch map[string]string) error {
	for i, p := range platforms {
		script, err := b.program(p, scratch[p])
		if err != nil {
			return err
		}

		if i == 0 {
			for _, a := range b.m.Assets {
				if err := copyAssets(filepath.Join(scratch[p], filepath.FromSlash(a)), filepath.Join(b.o.Out, filepath.FromSlash(a)), a); err != nil {
					return err
				}
			}
		}

		if script {
			break
		}
	}

	if err := b.copies(ctx); err != nil {
		return err
	}

	if err := manifest.CheckSchemas(b.m, func(f string) ([]byte, error) {
		return os.ReadFile(filepath.Join(b.o.Out, filepath.FromSlash(f)))
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrBuild, err)
	}

	if err := os.WriteFile(filepath.Join(b.o.Out, manifest.FileName), b.o.Manifest, 0o644); err != nil { //nolint:gosec // a plugin's files are readable
		return fmt.Errorf("%w: %w", ErrBuild, err)
	}

	return nil
}

func choose(m manifest.Manifest, want []string) ([]string, error) {
	if m.AnyPlatform() {
		if len(want) > 0 && !slices.Equal(want, []string{manifest.Any}) {
			return nil, fmt.Errorf("%w: plugin %s is built once for platforms [any]", ErrBuild, m.Name)
		}

		return []string{manifest.Any}, nil
	}

	if len(want) == 0 {
		return m.Platforms, nil
	}

	for _, p := range want {
		if !slices.Contains(m.Platforms, p) {
			return nil, fmt.Errorf("%w: plugin %s is not built for %s; its manifest's platforms are %s", ErrBuild, m.Name, p, strings.Join(m.Platforms, ", "))
		}
	}

	return want, nil
}

type builder struct {
	o    Options
	m    manifest.Manifest
	tool string
	// src is the writable copy of the repository the build runs in with build.copy, else empty.
	src string
}

func (b builder) run(ctx context.Context, osArch, scratch string) error {
	values := manifest.Values{Out: outAt, Platform: osArch, Commit: b.o.Commit, Version: b.o.Version}
	if osArch == manifest.Any {
		values.Platform = ""
	}

	env := map[string]string{"HOME": "/tmp", "GOFLAGS": "-trimpath"}

	for k, v := range b.m.Build.Env {
		if k == "GOFLAGS" {
			v = "-trimpath " + v
		}

		env[k] = values.Expand(v)
	}

	network := "none"
	if b.m.Build.Network {
		network = "bridge"
	}

	user, userns, _ := container.Owner(ctx, b.tool, true)

	// the copy is the build's own to write in; the repository itself is never written
	src := container.Mount{Host: b.o.Root, At: srcAt, ReadOnly: true}
	if b.src != "" {
		src = container.Mount{Host: b.src, At: srcAt}
	}

	// each command runs in a container of its own, no shell around it, and the first that fails
	// ends the build: what one leaves for the next is in the repository's copy and in {out}
	for _, argv := range b.m.Build.Run {
		cmd := make([]string, 0, len(argv))
		for _, a := range argv {
			cmd = append(cmd, values.Expand(a))
		}

		r := container.Run{
			Tool: b.tool, Image: b.m.Build.Image, Name: container.Name("plugin-build-" + b.m.Name),
			Dir:    path.Join(srcAt, b.o.Dir),
			Mounts: []container.Mount{src, {Host: scratch, At: outAt}},
			Env:    env, Network: network, User: user, UserNS: userns, Cmd: cmd,
		}

		if err := b.runOne(ctx, r, osArch); err != nil {
			return err
		}
	}

	return nil
}

func (b builder) runOne(ctx context.Context, r container.Run, osArch string) error {
	c, err := r.Command(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBuild, err)
	}

	var tail bytes.Buffer

	c.Stdout = io.MultiWriter(b.o.Log, &tail)
	c.Stderr = c.Stdout

	if err := c.Run(); err != nil {
		out := tail.String()
		if len(out) > 4000 {
			out = "..." + out[len(out)-4000:]
		}

		return fmt.Errorf("%w: plugin %s for %s: %s: %w\n%s", ErrBuild, b.m.Name, osArch, strings.Join(r.Cmd, " "), err, out)
	}

	return nil
}

// program moves the program the build wrote in scratch to the layout, and says whether it is a
// Node script.
func (b builder) program(osArch, scratch string) (bool, error) {
	if b.m.Program == "" {
		return false, nil
	}

	bin, script := filepath.Join(scratch, b.m.Program), filepath.Join(scratch, b.m.Script())
	hasBin, hasScript := regular(bin), regular(script)

	switch {
	case hasBin && hasScript:
		return false, fmt.Errorf("%w: plugin %s: the build wrote both {out}/%s and {out}/%s; a plugin is one of them", ErrBuild, b.m.Name, b.m.Program, b.m.Script())
	case hasScript:
		return true, moveTo(script, filepath.Join(b.o.Out, b.m.Script()), 0o755)
	case hasBin && osArch == manifest.Any:
		return false, fmt.Errorf("%w: plugin %s: a build for platforms [any] writes {out}/%s, a script that runs everywhere", ErrBuild, b.m.Name, b.m.Script())
	case hasBin:
		return false, moveTo(bin, filepath.Join(b.o.Out, b.m.Binary(osArch)), 0o755)
	}

	return false, fmt.Errorf("%w: plugin %s for %s: the build wrote no {out}/%s", ErrBuild, b.m.Name, osArch, b.m.Program)
}

func regular(p string) bool {
	info, err := os.Lstat(p)

	return err == nil && info.Mode().IsRegular()
}

func moveTo(from, to string, mode fs.FileMode) error {
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("%w: %w", ErrBuild, err)
	}

	if err := os.Chmod(to, mode); err != nil {
		return fmt.Errorf("%w: %w", ErrBuild, err)
	}

	return nil
}

// copyAssets copies the asset directory the build wrote, refusing a symlink or an executable file:
// only the program runs.
func copyAssets(from, to, name string) error {
	info, err := os.Lstat(from)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%w: the build wrote no asset directory {out}/%s", ErrBuild, name)
	}

	err = filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err //nolint:wrapcheck // said below
		}

		at := path.Join(name, filepath.ToSlash(rel))

		info, err := d.Info()
		if err != nil {
			return err //nolint:wrapcheck // said below
		}

		switch {
		case d.IsDir():
			return os.MkdirAll(filepath.Join(to, rel), 0o750)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s %w", at, errNotPlain)
		case info.Mode()&0o111 != 0:
			return fmt.Errorf("%s %w", at, errExecutable)
		}

		return copyFile(p, filepath.Join(to, rel))
	})
	if err != nil {
		return fmt.Errorf("%w: assets: %w", ErrBuild, err)
	}

	return nil
}

// copies lays out what the plugin's directory gives the layout: the included paths, after the build
// and refusing a path it wrote, then the manifest's files and extras.
func (b builder) copies(ctx context.Context) error {
	if err := b.include(ctx); err != nil {
		return err
	}

	return b.files()
}

// files copies the manifest's files from the plugin's directory; a symlink is refused, since it
// could name a file of the machine outside the repository.
func (b builder) files() error {
	type copied struct{ from, to string }

	var all []copied

	for _, f := range b.m.Files {
		all = append(all, copied{f, manifest.FileBase(f)})
	}

	for _, e := range b.m.Extras() {
		// an extra the build wrote into an asset directory is there already
		if _, err := os.Lstat(filepath.Join(b.o.Out, filepath.FromSlash(e))); err == nil {
			continue
		}

		all = append(all, copied{e, e})
	}

	for _, c := range all {
		f := c.from
		src := filepath.Join(b.o.Root, filepath.FromSlash(b.o.Dir), filepath.FromSlash(f))

		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("%w: files: %w", ErrBuild, err)
		}

		if !info.Mode().IsRegular() || !inside(b.o.Root, src) {
			return fmt.Errorf("%w: files: %s is no plain file of the repository", ErrBuild, f)
		}

		to := filepath.Join(b.o.Out, filepath.FromSlash(c.to))
		if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
			return fmt.Errorf("%w: files: %w", ErrBuild, err)
		}

		if err := copyFile(src, to); err != nil {
			return fmt.Errorf("%w: files: %w", ErrBuild, err)
		}
	}

	return nil
}

// inside tells whether p, its symlinks resolved, lies in root.
func inside(root, p string) bool {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}

	q, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(r, q)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func copyFile(from, to string) error {
	in, err := os.Open(from) //nolint:gosec // a file of the plugin's directory or of its build
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}
	defer in.Close() //nolint:errcheck // read only

	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644) //nolint:gosec // a plugin's files are readable
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}

	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}

	return err
}

var (
	errSymlink    = errors.New("is a symlink, which the copy a build writes in doesn't take")
	errNoCheckout = errors.New("build.copy and include copy the files git tracks, and this is no git checkout")
)

// include copies the manifest's included paths from the plugin's directory, as git tracks them:
// each file under its path relative to the plugin's directory. A symlink, anywhere on the path, or
// an executable file is refused, and so is a path the build wrote or one with no tracked file.
func (b builder) include(ctx context.Context) error {
	if len(b.m.Include) == 0 {
		return nil
	}

	files, err := tracked(ctx, b.o)
	if err != nil {
		return err
	}

	dir := path.Clean(b.o.Dir)
	if b.o.Dir == "" {
		dir = "."
	}

	for _, inc := range b.m.Include {
		at := path.Join(dir, inc)
		n := 0

		for _, f := range files {
			if f != at && !strings.HasPrefix(f, at+"/") {
				continue
			}

			rel := inc + strings.TrimPrefix(f, at)

			copied, err := b.includeFile(f, rel)
			if err != nil {
				return fmt.Errorf("%w: include: %w", ErrBuild, err)
			}

			if copied {
				n++
			}
		}

		if n == 0 {
			return fmt.Errorf("%w: include: %s: no file git tracks in the plugin's directory", ErrBuild, inc)
		}
	}

	return nil
}

var (
	errTwice   = errors.New("is there already: the build wrote it, and an included path doesn't replace it")
	errLink    = errors.New("is a symlink")
	errOutside = errors.New("is no plain file of the repository, or a symlink lies on its path")
)

// includeFile copies f, a file of the repository, to rel in the layout; it says false for a
// tracked file the checkout no longer holds.
func (b builder) includeFile(f, rel string) (bool, error) {
	src := filepath.Join(b.o.Root, filepath.FromSlash(f))

	info, err := os.Lstat(src)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		// tracked, and deleted in the checkout: the checkout as it is
		return false, nil
	case err != nil:
		return false, err //nolint:wrapcheck // the caller says whose
	case info.Mode()&fs.ModeSymlink != 0:
		return false, fmt.Errorf("%s %w", rel, errLink)
	case !info.Mode().IsRegular() || !inside(b.o.Root, src) || resolved(src) != filepath.Join(resolved(b.o.Root), filepath.FromSlash(f)):
		// a directory on the way that is a link would lead out of the repository
		return false, fmt.Errorf("%s %w", rel, errOutside)
	case info.Mode()&0o111 != 0:
		return false, fmt.Errorf("%s %w", rel, errExecutable)
	}

	to := filepath.Join(b.o.Out, filepath.FromSlash(rel))
	if _, err := os.Lstat(to); err == nil {
		return false, fmt.Errorf("%s %w", rel, errTwice)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return false, err //nolint:wrapcheck // the caller says whose
	}

	return true, copyFile(src, to)
}

// copySources copies what a build with build.copy writes in: a commit's whole tree, or of a
// checkout the files its index tracks, never an untracked or ignored one (Bazel's links,
// node_modules, earlier outputs), so the build sees the commit and nothing of the machine.
func copySources(ctx context.Context, o Options, to string) error {
	files, err := tracked(ctx, o)
	if err != nil {
		return err
	}

	for _, f := range files {
		from := filepath.Join(o.Root, filepath.FromSlash(f))

		info, err := os.Lstat(from)

		switch {
		case errors.Is(err, fs.ErrNotExist):
			// tracked, and deleted in the checkout: the build sees the checkout as it is
			continue
		case err != nil:
			return fmt.Errorf("%w: copy of the repository: %w", ErrBuild, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: copy of the repository: %s %w", ErrBuild, f, errSymlink)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%w: copy of the repository: %s %w", ErrBuild, f, errNotPlain)
		}

		dest := filepath.Join(to, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("%w: copy of the repository: %w", ErrBuild, err)
		}

		if err := copyMode(from, dest, info.Mode().Perm()); err != nil {
			return fmt.Errorf("%w: copy of the repository: %w", ErrBuild, err)
		}
	}

	return nil
}

// tracked are the files to copy, slash-separated from Root: every file of a commit's tree, or the
// files a checkout's index tracks.
func tracked(ctx context.Context, o Options) ([]string, error) {
	if o.Tree {
		var files []string

		err := filepath.WalkDir(o.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			rel, err := filepath.Rel(o.Root, p)
			files = append(files, filepath.ToSlash(rel))

			return err //nolint:wrapcheck // said below
		})
		if err != nil {
			return nil, fmt.Errorf("%w: copy of the repository: %w", ErrBuild, err)
		}

		return files, nil
	}

	top, err := exec.CommandContext(ctx, "git", "-C", o.Root, "rev-parse", "--show-toplevel").Output() //nolint:gosec // git, asked about the repository the caller names
	if err != nil || filepath.Clean(strings.TrimSpace(string(top))) != resolved(o.Root) {
		return nil, fmt.Errorf("%w: %s: %w", ErrBuild, o.Root, errNoCheckout)
	}

	out, err := exec.CommandContext(ctx, "git", "-C", o.Root, "ls-files", "-z", "--cached").Output() //nolint:gosec // as above
	if err != nil {
		return nil, fmt.Errorf("%w: %s: git ls-files: %w", ErrBuild, o.Root, err)
	}

	return strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 }), nil
}

func resolved(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}

	return filepath.Clean(p)
}

func copyMode(from, to string, mode fs.FileMode) error {
	in, err := os.Open(from) //nolint:gosec // a file of the repository being copied
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}
	defer in.Close() //nolint:errcheck // read only

	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o600) //nolint:gosec // the copy's own file
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}

	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}

	return err
}
