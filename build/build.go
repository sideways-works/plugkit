// Package build builds a plugin from its manifest in a container, the way its zip and the cache
// hold it: manifest.yaml byte for byte, the program for each platform, the manifest's files and its
// assets, all at the top level of one directory.
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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

	// a meta plugin is its manifest and files alone: nothing runs
	if m.Kind != manifest.KindMeta {
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

	for i, p := range platforms {
		if m.Kind == manifest.KindMeta {
			break
		}

		script, err := b.platform(ctx, p, i == 0)
		if err != nil {
			return m, err
		}

		// a Node plugin's script is the same on every platform: one build makes it
		if script {
			break
		}
	}

	if err := b.files(); err != nil {
		return m, err
	}

	if err := manifest.CheckSchemas(m, func(f string) ([]byte, error) {
		return os.ReadFile(filepath.Join(o.Out, filepath.FromSlash(f)))
	}); err != nil {
		return m, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	if err := os.WriteFile(filepath.Join(o.Out, manifest.FileName), o.Manifest, 0o644); err != nil { //nolint:gosec // a plugin's files are readable
		return m, fmt.Errorf("%w: %w", ErrBuild, err)
	}

	return m, nil
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
}

// platform builds for one platform, keeping the assets of the first build; it says whether the
// build wrote a Node script.
func (b builder) platform(ctx context.Context, osArch string, first bool) (bool, error) {
	scratch, err := os.MkdirTemp(filepath.Dir(b.o.Out), ".build-")
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrBuild, err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // a scratch directory

	if err := b.run(ctx, osArch, scratch); err != nil {
		return false, err
	}

	script, err := b.program(osArch, scratch)
	if err != nil {
		return false, err
	}

	if first {
		for _, a := range b.m.Assets {
			if err := copyAssets(filepath.Join(scratch, filepath.FromSlash(a)), filepath.Join(b.o.Out, filepath.FromSlash(a)), a); err != nil {
				return false, err
			}
		}
	}

	return script, nil
}

func (b builder) run(ctx context.Context, osArch, scratch string) error {
	expand := func(s string) string {
		if osArch == manifest.Any {
			return manifest.Expand(s, outAt, "")
		}

		return manifest.Expand(s, outAt, osArch)
	}

	env := map[string]string{"HOME": "/tmp", "GOFLAGS": "-trimpath"}

	for k, v := range b.m.Build.Env {
		if k == "GOFLAGS" {
			v = "-trimpath " + v
		}

		env[k] = expand(v)
	}

	cmd := make([]string, 0, len(b.m.Build.Run))
	for _, a := range b.m.Build.Run {
		cmd = append(cmd, expand(a))
	}

	network := "none"
	if b.m.Build.Network {
		network = "bridge"
	}

	user, userns, _ := container.Owner(ctx, b.tool, true)

	r := container.Run{
		Tool: b.tool, Image: b.m.Build.Image, Name: container.Name("plugin-build-" + b.m.Name),
		Dir:    path.Join(srcAt, b.o.Dir),
		Mounts: []container.Mount{{Host: b.o.Root, At: srcAt, ReadOnly: true}, {Host: scratch, At: outAt}},
		Env:    env, Network: network, User: user, UserNS: userns, Cmd: cmd,
	}

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

		return fmt.Errorf("%w: plugin %s for %s: %s: %w\n%s", ErrBuild, b.m.Name, osArch, strings.Join(b.m.Build.Run, " "), err, out)
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
