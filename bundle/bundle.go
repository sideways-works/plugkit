// Package bundle packs a built plugin into a zip and unpacks one safely. A plugin's zip holds, at
// its top level and with no directory around them, manifest.yaml, the program's files
// (<program>-<os>-<arch> for each platform, or <program>.mjs), the manifest's files, and its asset
// directories. Nothing else is accepted, and only the program may be executable.
package bundle

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sideways-works/plugkit/manifest"
)

var ErrBundle = errors.New("plugin zip")

// Limits bound what an unpack writes, by the bytes it actually reads, so a zip whose headers lie
// can't fill the disk.
type Limits struct {
	Bytes   int64
	Entries int
}

var DefaultLimits = Limits{Bytes: 1 << 30, Entries: 20000}

// Role is what an entry of the zip is.
type Role int

const (
	Outside Role = iota
	Manifest
	Program
	File
	Asset
	AssetDir
	// Extra is the logo, or a meta plugin's schema file, under its own path.
	Extra
)

// RoleOf says what name (slash-separated, relative to the zip's top) is for the plugin m.
func RoleOf(m manifest.Manifest, name string) Role {
	switch {
	case name == manifest.FileName:
		return Manifest
	case m.IsProgramFile(name):
		return Program
	case slices.ContainsFunc(m.Files, func(f string) bool { return manifest.FileBase(f) == name }):
		return File
	}

	for _, a := range m.Assets {
		switch {
		case name == a:
			return AssetDir
		case strings.HasPrefix(name, a+"/"):
			return Asset
		case strings.HasPrefix(a, name+"/"):
			return AssetDir
		}
	}

	for _, e := range m.Extras() {
		switch {
		case name == e:
			return Extra
		case strings.HasPrefix(e, name+"/"):
			return AssetDir
		}
	}

	return Outside
}

// fixedTime is every entry's time, so the same build packs to the same bytes.
var fixedTime = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// Pack writes the plugin laid out in dir as a zip to w: its entries sorted, each with a fixed time
// and with the mode its role gives, so the same files always make the same zip.
func Pack(dir string, w io.Writer) error {
	m, _, err := readManifest(dir)
	if err != nil {
		return err
	}

	var names []string

	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err //nolint:wrapcheck // said below
		}

		rel = filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err //nolint:wrapcheck // said below
		}

		role := RoleOf(m, rel)

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symlink", ErrBundle, rel)
		case d.IsDir() && (role == AssetDir || role == Asset):
			return nil
		case d.IsDir() || !info.Mode().IsRegular() || role == Outside || role == AssetDir:
			return fmt.Errorf("%w: %s is no part of plugin %s: a zip holds %s, the program's files, the manifest's files and its assets", ErrBundle, rel, m.Name, manifest.FileName)
		case role != Program && info.Mode()&0o111 != 0:
			return fmt.Errorf("%w: %s is executable, and only the program may be", ErrBundle, rel)
		}

		names = append(names, rel)

		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: pack %s: %w", ErrBundle, dir, err)
	}

	if m.Program != "" && !slices.ContainsFunc(names, func(n string) bool { return RoleOf(m, n) == Program }) {
		return fmt.Errorf("%w: pack %s: no file of program %s (%s)", ErrBundle, dir, m.Program, strings.Join(m.ProgramFiles(), ", "))
	}

	if err := manifest.CheckSchemas(m, readIn(dir)); err != nil {
		return fmt.Errorf("%w: pack %s: %w", ErrBundle, dir, err)
	}

	slices.Sort(names)

	return write(dir, m, names, w)
}

func write(dir string, m manifest.Manifest, names []string, w io.Writer) error {
	zw := zip.NewWriter(w)

	for _, name := range names {
		mode := fs.FileMode(0o644)
		if RoleOf(m, name) == Program {
			mode = 0o755
		}

		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: fixedTime}
		h.SetMode(mode)

		fw, err := zw.CreateHeader(h)
		if err != nil {
			return fmt.Errorf("%w: pack: %w", ErrBundle, err)
		}

		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // a file of the plugin's layout
		if err != nil {
			return fmt.Errorf("%w: pack: %w", ErrBundle, err)
		}

		_, err = io.Copy(fw, f)
		_ = f.Close() //nolint:errcheck // read only

		if err != nil {
			return fmt.Errorf("%w: pack %s: %w", ErrBundle, name, err)
		}
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("%w: pack: %w", ErrBundle, err)
	}

	return nil
}

// readIn reads a file of dir by its slash-separated path, which the manifest's checks keep in dir.
func readIn(dir string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, filepath.FromSlash(name))) //nolint:gosec // a file of the plugin's layout
	}
}

func readManifest(dir string) (manifest.Manifest, []byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifest.FileName)) //nolint:gosec // the plugin's own manifest
	if err != nil {
		return manifest.Manifest{}, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	m, err := manifest.Parse(data)
	if err != nil {
		return manifest.Manifest{}, nil, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	return m, data, nil
}

// Unpack checks the zip at file entry by entry and writes it into dest, which it creates: every
// entry a part of the plugin its top-level manifest.yaml names, none a symlink, none outside dest,
// none executable but the program, and all within limits. On an error dest may hold part of it;
// the caller unpacks into a directory of its own and removes it.
func Unpack(file, dest string, limits Limits) (manifest.Manifest, error) {
	if limits.Bytes <= 0 {
		limits.Bytes = DefaultLimits.Bytes
	}

	zr, err := zip.OpenReader(file)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}
	defer zr.Close() //nolint:errcheck // read only

	if limits.Entries > 0 && len(zr.File) > limits.Entries {
		return manifest.Manifest{}, fmt.Errorf("%w: %d entries, more than %d", ErrBundle, len(zr.File), limits.Entries)
	}

	m, err := topManifest(zr.File, limits)
	if err != nil {
		return manifest.Manifest{}, err
	}

	if err := os.MkdirAll(dest, 0o750); err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	left := limits.Bytes
	seen := map[string]bool{}

	for _, f := range zr.File {
		role, err := checkEntry(m, f, seen)
		if err != nil || role == AssetDir {
			if err != nil {
				return manifest.Manifest{}, err
			}

			continue
		}

		n, err := extract(f, filepath.Join(dest, filepath.FromSlash(f.Name)), role == Program, left)
		if errors.Is(err, errTooBig) {
			return manifest.Manifest{}, fmt.Errorf("%w: %s: the zip unpacks to more than %d bytes", ErrBundle, f.Name, limits.Bytes)
		}

		if err != nil {
			return manifest.Manifest{}, err
		}

		left -= n
	}

	if m.Program != "" && !slices.ContainsFunc(m.ProgramFiles(), func(f string) bool { return seen[f] }) {
		return manifest.Manifest{}, fmt.Errorf("%w: no file of program %s (%s)", ErrBundle, m.Program, strings.Join(m.ProgramFiles(), ", "))
	}

	if err := manifest.CheckSchemas(m, readIn(dest)); err != nil {
		return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	return m, nil
}

func checkEntry(m manifest.Manifest, f *zip.File, seen map[string]bool) (Role, error) {
	name := strings.TrimSuffix(f.Name, "/")
	mode := f.Mode()
	role := RoleOf(m, name)

	switch {
	case !safeName(name):
		return 0, fmt.Errorf("%w: entry %q leaves the plugin's directory", ErrBundle, f.Name)
	case seen[name]:
		return 0, fmt.Errorf("%w: entry %q is there twice", ErrBundle, f.Name)
	case mode&fs.ModeSymlink != 0:
		return 0, fmt.Errorf("%w: entry %q is a symlink", ErrBundle, f.Name)
	case mode.IsDir() && (role == AssetDir || role == Asset):
		return AssetDir, nil
	case mode.IsDir() || !mode.IsRegular() || role == Outside || role == AssetDir:
		return 0, fmt.Errorf("%w: entry %q is no part of plugin %s: a zip holds %s, the program's files, the manifest's files and its assets", ErrBundle, f.Name, m.Name, manifest.FileName)
	case role != Program && mode&0o111 != 0:
		return 0, fmt.Errorf("%w: entry %q is executable, and only the program may be", ErrBundle, f.Name)
	}

	seen[name] = true

	return role, nil
}

func safeName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || strings.Contains(name, ":") {
		return false
	}

	for part := range strings.SplitSeq(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}

	return path.Clean(name) == name
}

// topManifest is the manifest at the zip's top level; one only deeper down gets its own error.
func topManifest(files []*zip.File, limits Limits) (manifest.Manifest, error) {
	var deeper string

	for _, f := range files {
		switch {
		case f.Name == manifest.FileName:
			data, err := readCapped(f, min(limits.Bytes, 1<<20))
			if err != nil {
				return manifest.Manifest{}, err
			}

			m, err := manifest.Parse(data)
			if err != nil {
				return manifest.Manifest{}, fmt.Errorf("%w: %w", ErrBundle, err)
			}

			return m, nil
		case path.Base(f.Name) == manifest.FileName && deeper == "":
			deeper = f.Name
		}
	}

	if deeper != "" {
		return manifest.Manifest{}, fmt.Errorf("%w: %s is not at the top level (it is %s): pack the plugin's files with no directory around them", ErrBundle, manifest.FileName, deeper)
	}

	return manifest.Manifest{}, fmt.Errorf("%w: no %s at the top level", ErrBundle, manifest.FileName)
}

var errTooBig = errors.New("over the size limit")

func readCapped(f *zip.File, limit int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrBundle, f.Name, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, limit+1))

	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %s: %w", ErrBundle, f.Name, err)
	case int64(len(data)) > limit:
		return nil, fmt.Errorf("%w: %s: %w of %d bytes", ErrBundle, f.Name, errTooBig, limit)
	}

	return data, nil
}

func extract(f *zip.File, dest string, program bool, left int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrBundle, f.Name, err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	mode := fs.FileMode(0o644)
	if program {
		mode = 0o755
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // a checked entry, in the caller's own directory
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrBundle, err)
	}

	n, err := io.Copy(out, io.LimitReader(rc, left+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}

	switch {
	case err != nil:
		return n, fmt.Errorf("%w: %s: %w", ErrBundle, f.Name, err)
	case n > left:
		return n, errTooBig
	}

	return n, nil
}
