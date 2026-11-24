package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAReleaseReadsThePluginsReadmeAtItsVersion(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{
		"repository-manifest.yaml": "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n  - {manifest: step/lint/manifest.yaml}\n",
		"step/lint/manifest.yaml":  pluginManifest("lint", "step", "Lints"),
		"step/lint/README.md":      "# lint\n\nthe first\n",
		"README.md":                "# the repository's\n",
	})
	r.git("tag", "step/lint/v1.0.0")
	r.commit(map[string]string{"step/lint/README.md": "# lint\n\nthe second\n"})
	r.git("tag", "step/lint/v1.1.0")

	url := r.bare()

	for version, want := range map[string]string{"1.0.0": "the first", "1.1.0": "the second"} {
		rel, err := AtRelease(context.Background(), url, "step/lint", version, Options{})
		if err != nil || rel.ReadmeErr != nil || string(rel.Readme) != "# lint\n\n"+want+"\n" {
			t.Fatalf("%s: %q %v %v", version, rel.Readme, rel.ReadmeErr, err)
		}
	}

	// At reads no README
	if _, _, err := At(context.Background(), url, "step/lint", "1.0.0", Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestAReleaseWithoutAReadmeHasNone(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{"manifest.yaml": pluginManifest("lint", "step", "Lints"), "README": "not markdown"})
	r.git("tag", "v1.0.0")

	rel, err := AtRelease(context.Background(), r.bare(), ".", "1.0.0", Options{})
	if err != nil || rel.ReadmeErr != nil || rel.Readme != nil {
		t.Fatalf("%q %v %v", rel.Readme, rel.ReadmeErr, err)
	}
}

func TestAReadmeThatIsNoFileOrTooLargeIsRefused(t *testing.T) {
	for name, set := range map[string]func(r *repo){
		"too large":   func(r *repo) { r.commit(map[string]string{"README.md": strings.Repeat("x", MaxReadme+1)}) },
		"a directory": func(r *repo) { r.commit(map[string]string{"README.md/inside": "x"}) },
		"a link": func(r *repo) {
			if err := os.Symlink("/etc/passwd", filepath.Join(r.dir, "README.md")); err != nil {
				t.Fatal(err)
			}

			r.commit(nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRepo(t)
			r.commit(map[string]string{"manifest.yaml": pluginManifest("lint", "step", "Lints")})
			set(r)
			r.git("tag", "v1.0.0")

			rel, err := AtRelease(context.Background(), r.bare(), ".", "1.0.0", Options{})
			if err != nil || !errors.Is(rel.ReadmeErr, ErrReadme) || rel.Readme != nil {
				t.Fatalf("%q %v %v", rel.Readme, rel.ReadmeErr, err)
			}
		})
	}
}
