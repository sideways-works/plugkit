// Package store resolves a plugin pinned by commit or by zip to a directory of the cache holding
// it as its zip does: manifest.yaml, the program and the assets. A commit is built from its
// manifest once per platform, a zip downloaded and checked once; the same key never builds or
// downloads twice, and every entry appears whole, by a rename. Besides the entries, named by their
// 64-hex keys, the store keeps only names that start with a dot, so a host may give each plugin a
// directory of its name beside them as long as no plugin's name starts with one.
package store

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/build"
	"github.com/sideways-works/plugkit/bundle"
	"github.com/sideways-works/plugkit/fetch"
	"github.com/sideways-works/plugkit/gitsafe"
	"github.com/sideways-works/plugkit/manifest"
	"github.com/sideways-works/plugkit/pin"
)

var (
	ErrPin      = errors.New("plugin pin")
	ErrChecksum = fetch.ErrChecksum
	// ErrNoManifest is a commit whose plugin directory holds no manifest.yaml.
	ErrNoManifest = errors.New("no " + manifest.FileName)

	errTooBig = errors.New("over the size limit")
)

// Pin is a plugin pinned by a commit of its repository, or by a zip and its checksum.
type Pin struct {
	// Repository is a URL git fetches from; Path the plugin's directory in it.
	Repository, Path, Commit string
	// Version is the version of the tag the commit was found through (repository.At), without a
	// leading v: a build's {version}. Empty when the pin names the commit alone.
	Version string
	Zip     *Zip
}

type Zip struct {
	URL, SHA256 string
}

// Resolved is a plugin in the cache: Dir holds manifest.yaml, the program and the assets; Program
// is the program's file for the platform asked, empty for a plugin of assets alone.
type Resolved struct {
	Program  string
	Dir      string
	Manifest manifest.Manifest
}

type Store struct {
	Dir    string
	Client *http.Client
	// Tool is docker or podman, found on PATH when empty.
	Tool string
	Log  io.Writer
	// GitEnv is added to git's environment, for credentials the host gives git.
	GitEnv []string
	Limits bundle.Limits
	// OnBuild, when set, is called with the key of each build before it starts.
	OnBuild func(key string)
	// Untrusted is for a pin from someone the host doesn't trust: as repository.Options.Untrusted,
	// only an https or ssh repository, nothing of the host's git config, credential helpers or ssh
	// keys, and Credential as the only credential; GitEnv is left out.
	Untrusted  bool
	Credential gitsafe.Credential
	// home is git's HOME with Untrusted: an empty directory of the store's own.
	home string
}

var (
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitID  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// The store's own directories: the bare repositories commits are fetched into, and the files that
// name a commit's build by its source, so a resolve finds it without fetching.
const (
	mirrorsDir = ".git-clones"
	aliasesDir = ".sources"
)

// maxZip bounds a download, so a wrong URL can't fill the disk.
const maxZip = 1 << 30

// Resolve returns the plugin p pins, for platform os/arch.
func (s Store) Resolve(ctx context.Context, p Pin, osArch string) (Resolved, error) {
	switch {
	case p.Zip != nil && p.Commit != "":
		return Resolved{}, fmt.Errorf("%w: a commit or a zip, not both", ErrPin)
	case p.Zip != nil:
		return s.zip(ctx, *p.Zip, osArch)
	case p.Commit != "":
		return s.commit(ctx, p, osArch)
	}

	return Resolved{}, fmt.Errorf("%w: neither a commit nor a zip", ErrPin)
}

func (s Store) zip(ctx context.Context, z Zip, osArch string) (Resolved, error) {
	sum := strings.ToLower(z.SHA256)

	u, err := url.Parse(z.URL)

	switch {
	case !sha256Hex.MatchString(sum):
		return Resolved{}, fmt.Errorf("%w: zip sha256 %q: 64 hex digits", ErrPin, z.SHA256)
	case err != nil || u.Scheme != "https" || u.Host == "":
		return Resolved{}, fmt.Errorf("%w: zip url %q: an https URL", ErrPin, z.URL)
	}

	entry := filepath.Join(s.Dir, sum)
	if r, err := s.found(entry, osArch); err == nil || !errors.Is(err, os.ErrNotExist) {
		return r, err
	}

	if err := os.MkdirAll(s.Dir, 0o750); err != nil {
		return Resolved{}, fmt.Errorf("%w: %w", ErrPin, err)
	}

	file, err := s.download(ctx, z.URL)
	if err != nil {
		return Resolved{}, err
	}
	defer os.Remove(file) //nolint:errcheck // a download, unpacked or refused

	// the checksum is checked before the zip is opened at all
	if got, err := pin.FileSHA256(file); err != nil || got != sum {
		return Resolved{}, fmt.Errorf("%w: %s is %s, pinned %s", ErrChecksum, z.URL, got, sum)
	}

	if err := s.keep(entry, func(tmp string) error {
		_, err := bundle.Unpack(file, tmp, s.Limits)

		return err
	}); err != nil {
		return Resolved{}, err
	}

	return s.found(entry, osArch)
}

func (s Store) download(ctx context.Context, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrPin, err)
	}

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: fetch %s: %w", ErrPin, u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: fetch %s: %s", ErrPin, u, resp.Status)
	}

	f, err := os.CreateTemp(s.Dir, ".zip-")
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrPin, err)
	}

	n, err := io.Copy(f, io.LimitReader(resp.Body, maxZip+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err == nil && n > maxZip {
		err = fmt.Errorf("%w of %d bytes", errTooBig, maxZip)
	}

	if err != nil {
		_ = os.Remove(f.Name()) //nolint:errcheck // the error says it

		return "", fmt.Errorf("%w: fetch %s: %w", ErrPin, u, err)
	}

	return f.Name(), nil
}

func (s Store) commit(ctx context.Context, p Pin, osArch string) (Resolved, error) {
	if !commitID.MatchString(p.Commit) {
		return Resolved{}, fmt.Errorf("%w: commit %q: 40 hex digits", ErrPin, p.Commit)
	}

	alias := filepath.Join(s.Dir, aliasesDir, hexSum("source\x00"+p.Repository+"\x00"+p.Commit+"\x00"+p.Path+"\x00"+osArch+versioned(p.Version)))
	if key, err := os.ReadFile(alias); err == nil && sha256Hex.Match(key) { //nolint:gosec // the cache's own file
		if r, err := s.found(filepath.Join(s.Dir, string(key)), osArch); err == nil {
			return r, nil
		}
	}

	if s.Untrusted {
		if err := gitsafe.CheckURL(p.Repository); err != nil {
			return Resolved{}, fmt.Errorf("%w: %w", ErrPin, err)
		}

		home, err := os.MkdirTemp(s.Dir, ".home-")
		if err != nil {
			return Resolved{}, fmt.Errorf("%w: %w", ErrPin, err)
		}
		defer os.RemoveAll(home) //nolint:errcheck // an empty scratch home

		s.home = home
	}

	mirror, err := s.mirror(ctx, p.Repository, p.Commit)
	if err != nil {
		return Resolved{}, err
	}

	file := path.Join(p.Path, manifest.FileName)

	data, err := s.git(ctx, mirror, "show", p.Commit+":"+file)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s at %s: %w", ErrNoManifest, p.Repository, p.Commit, err)
	}

	m, err := manifest.Parse(data)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s at %s: %s: %w", ErrPin, p.Repository, p.Commit, file, err)
	}

	platform := osArch
	if m.AnyPlatform() {
		platform = manifest.Any
	} else if !m.Supports(osArch) {
		return Resolved{}, fmt.Errorf("%w: plugin %s at %s is not built for %s; its manifest's platforms are %s", ErrPin, m.Name, p.Commit, osArch, strings.Join(m.Platforms, ", "))
	}

	key := SourceKey(p.Repository, p.Commit, p.Path, manifest.Digest(data), platform, m.ImageDigest())
	if p.Version != "" {
		// a build stamped with a version is another build of the same commit
		key = hexSum(key + versioned(p.Version))
	}

	entry := filepath.Join(s.Dir, key)

	if _, err := s.found(entry, osArch); errors.Is(err, os.ErrNotExist) {
		if err := s.build(ctx, mirror, p, data, platform, key); err != nil {
			return Resolved{}, err
		}
	}

	if err := writeFile(alias, []byte(key)); err != nil {
		return Resolved{}, fmt.Errorf("%w: %w", ErrPin, err)
	}

	return s.found(entry, osArch)
}

// SourceKey is a build's key in the cache: the source, the manifest by its digest, the platform and
// the build image by its digest.
func SourceKey(repository, commit, dir, manifestDigest, platform, imageDigest string) string {
	return hexSum(strings.Join([]string{"build", repository, commit, dir, manifestDigest, platform, imageDigest}, "\n"))
}

func (s Store) build(ctx context.Context, mirror string, p Pin, data []byte, platform, key string) error {
	if s.OnBuild != nil {
		s.OnBuild(key)
	}

	src, err := os.MkdirTemp(s.Dir, ".src-")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPin, err)
	}
	defer os.RemoveAll(src) //nolint:errcheck // a scratch checkout

	if err := s.checkout(ctx, mirror, p.Commit, src); err != nil {
		return err
	}

	return s.keep(filepath.Join(s.Dir, key), func(tmp string) error {
		// Build makes its output directory itself
		if err := os.Remove(tmp); err != nil {
			return fmt.Errorf("%w: %w", ErrPin, err)
		}

		var platforms []string
		if platform != manifest.Any {
			platforms = []string{platform}
		}

		_, err := build.Build(ctx, build.Options{
			Tool: s.Tool, Root: src, Dir: p.Path, Manifest: data, Platforms: platforms, Out: tmp, Log: s.Log,
			Commit: p.Commit, Version: strings.TrimPrefix(p.Version, "v"),
		})

		return err
	})
}

// keep fills a new directory with fill and renames it to entry; an entry another process put
// there first is kept.
func (s Store) keep(entry string, fill func(tmp string) error) error {
	tmp, err := os.MkdirTemp(s.Dir, ".new-")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPin, err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // gone once renamed

	if err := fill(tmp); err != nil {
		return err
	}

	if err := os.Chmod(tmp, 0o750); err != nil { //nolint:gosec // a directory others read plugins from
		return fmt.Errorf("%w: %w", ErrPin, err)
	}

	if err := os.Rename(tmp, entry); err != nil {
		if _, serr := os.Stat(filepath.Join(entry, manifest.FileName)); serr == nil {
			return nil
		}

		return fmt.Errorf("%w: %w", ErrPin, err)
	}

	return nil
}

// found is the entry's plugin for platform, marked as used; os.ErrNotExist when there is none.
func (s Store) found(entry, osArch string) (Resolved, error) {
	file := filepath.Join(entry, manifest.FileName)

	data, err := os.ReadFile(file) //nolint:gosec // the cache's own entry
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %w", ErrPin, os.ErrNotExist)
	}

	m, err := manifest.Parse(data)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s: %w", ErrPin, entry, err)
	}

	now := time.Now()
	_ = os.Chtimes(file, now, now) //nolint:errcheck // at worst it is fetched again

	r := Resolved{Dir: entry, Manifest: m}
	if m.Program == "" {
		return r, nil
	}

	if !m.Supports(osArch) {
		return Resolved{}, fmt.Errorf("%w: plugin %s is not built for %s; its manifest's platforms are %s", ErrPin, m.Name, osArch, strings.Join(m.Platforms, ", "))
	}

	for _, name := range []string{m.Binary(osArch), m.Script()} {
		if info, err := os.Stat(filepath.Join(entry, name)); err == nil && info.Mode().IsRegular() { //nolint:gosec // a name the manifest's checks allow
			r.Program = filepath.Join(entry, name)

			return r, nil
		}
	}

	return Resolved{}, fmt.Errorf("%w: plugin %s holds neither %s nor %s", ErrPin, m.Name, m.Binary(osArch), m.Script())
}

// mirror is the cache's bare repository of repository holding commit, fetched only when it doesn't.
func (s Store) mirror(ctx context.Context, repository, commit string) (string, error) {
	dir := filepath.Join(s.Dir, mirrorsDir, hexSum(repository)[:24])

	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", fmt.Errorf("%w: %w", ErrPin, err)
		}

		if _, err := s.git(ctx, "", "init", "-q", "--bare", dir); err != nil {
			return "", err
		}
	}

	now := time.Now()
	_ = os.Chtimes(dir, now, now) //nolint:errcheck // at worst it is fetched again

	if _, err := s.git(ctx, dir, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return dir, nil
	}

	_, err := s.git(ctx, dir, "fetch", "-q", "--depth", "1", "--", repository, commit)
	if err != nil {
		// a server that serves no commit by its id still serves its branches and tags
		_, err = s.git(ctx, dir, "fetch", "-q", "--", repository, "+refs/heads/*:refs/remotes/src/*", "+refs/tags/*:refs/tags/*")
	}

	if err != nil {
		return "", err
	}

	if _, err := s.git(ctx, dir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("%w: %s has no commit %s", ErrPin, repository, commit)
	}

	return dir, nil
}

// checkout writes the commit's tree into dir, with no .git: the build sees the files alone.
func (s Store) checkout(ctx context.Context, mirror, commit, dir string) error {
	cmd := s.command(ctx, mirror, "archive", "--format=tar", commit)

	var errOut bytes.Buffer

	cmd.Stderr = &errOut

	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%w: checkout: %w", ErrPin, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: checkout: %w", ErrPin, err)
	}

	err = untarAll(tar.NewReader(out), dir)
	if err != nil {
		_, _ = io.Copy(io.Discard, out) //nolint:errcheck // only so git can end
	}

	if werr := cmd.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("%w: git archive %s: %w: %s", ErrPin, commit, werr, strings.TrimSpace(errOut.String()))
	}

	return err
}

func untarAll(tr *tar.Reader, dir string) error {
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("%w: checkout: %w", ErrPin, err)
		}

		if err := untar(tr, h, dir); err != nil {
			return fmt.Errorf("%w: checkout %s: %w", ErrPin, h.Name, err)
		}
	}
}

func untar(tr *tar.Reader, h *tar.Header, dir string) error {
	name := filepath.FromSlash(strings.TrimSuffix(h.Name, "/"))
	if name == "" || !filepath.IsLocal(name) {
		return nil
	}

	dest := filepath.Join(dir, name)

	switch h.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(dest, 0o750) //nolint:wrapcheck // the caller says whose
	case tar.TypeSymlink:
		// the build sees the tree in its container, where a link resolves inside the container
		return os.Symlink(h.Linkname, dest) //nolint:wrapcheck // the caller says whose
	case tar.TypeReg:
		mode := os.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}

		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // a file of the pinned commit
		if err != nil {
			return err //nolint:wrapcheck // the caller says whose
		}

		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}

		return err
	}

	return nil
}

func (s Store) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // git, fetching the pinned repository

	if s.Untrusted {
		cmd.Env = gitsafe.Env(os.Environ(), s.home, s.Credential)
	} else {
		cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), s.GitEnv...)
	}

	return cmd
}

func (s Store) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := s.command(ctx, dir, args...)

	var errOut bytes.Buffer

	cmd.Stderr = &errOut

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: git %s: %w: %s", ErrPin, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}

	return out, nil
}

func writeFile(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), ".alias-")
	if err != nil {
		return err //nolint:wrapcheck // the caller says whose
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone once renamed

	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}

	return err //nolint:wrapcheck // the caller says whose
}

func exists(p string) bool {
	_, err := os.Stat(p) //nolint:gosec // an entry of the cache by its key

	return err == nil
}

// versioned is what a version adds to a build's keys; nothing without one, so an unversioned build
// keeps the key it always had.
func versioned(version string) string {
	if version == "" {
		return ""
	}

	return "\x00version\x00" + strings.TrimPrefix(version, "v")
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

// Trim removes what dir holds that no resolve used for longer than keep: the entries, as
// fetch.Trim removes a release's files beside them, then the names of builds whose entry is gone,
// the repositories no build fetched from, and what an interrupted resolve left. It reads dir as
// holding the store's and fetch's files alone, so a host trims its plugins' directories otherwise.
func Trim(dir string, keep time.Duration, now time.Time) ([]string, error) {
	removed, err := fetch.Trim(dir, keep, now)
	if err != nil {
		return removed, err
	}

	old := func(p string) bool {
		info, err := os.Lstat(p)

		return err == nil && now.Sub(info.ModTime()) > keep
	}

	drop := func(p string) error {
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("trim %s: %w", dir, err)
		}

		removed = append(removed, p)

		return nil
	}

	aliases, _ := os.ReadDir(filepath.Join(dir, aliasesDir)) //nolint:errcheck // none yet
	for _, a := range aliases {
		p := filepath.Join(dir, aliasesDir, a.Name())

		key, err := os.ReadFile(p) //nolint:gosec // the cache's own file
		if err != nil || !sha256Hex.Match(key) || !exists(filepath.Join(dir, string(key))) {
			if err := drop(p); err != nil {
				return removed, err
			}
		}
	}

	for _, sub := range []string{mirrorsDir, "."} {
		ents, _ := os.ReadDir(filepath.Join(dir, sub)) //nolint:errcheck // none yet
		for _, e := range ents {
			p := filepath.Join(dir, sub, e.Name())
			own := sub == mirrorsDir || strings.HasPrefix(e.Name(), ".") && e.Name() != mirrorsDir && e.Name() != aliasesDir

			if own && old(p) {
				if err := drop(p); err != nil {
					return removed, err
				}
			}
		}
	}

	return removed, nil
}
