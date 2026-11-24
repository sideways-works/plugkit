package repository

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/sideways-works/plugkit/manifest"
	"go.yaml.in/yaml/v3"
)

func TestAListingGivesTheProductsKeysOfManifestAndEntry(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{
		"repository-manifest.yaml": "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n" +
			"  - manifest: step/a/manifest.yaml\n    nodes: [big]\n",
		"step/a/manifest.yaml": pluginManifest("a", "step", "A") + "nodes: [small]\nno_nodes: false\n",
	})
	r.git("tag", "step/a/v1.0.0")

	url := r.bare()

	// a product's keys unknown to the reader are refused, the entry's with its line
	if _, err := List(context.Background(), url, Options{}); err == nil || !strings.Contains(err.Error(), "plugins[0]: line 5: field nodes not found") {
		t.Fatalf("without extensions: %v", err)
	}

	ext := manifest.Extensions{"nodes": {}, "no_nodes": {}}

	got, err := List(context.Background(), url, Options{Extensions: ext})
	if err != nil || len(got) != 1 || got[0].Err != nil {
		t.Fatalf("%+v %v", got, err)
	}

	p := got[0]

	nodes := p.Extra["nodes"]
	if len(nodes.Content) != 1 || nodes.Content[0].Value != "big" || p.Extra["no_nodes"].Value != "false" || len(p.Extra) != 2 ||
		p.ManifestExtra["nodes"].Content[0].Value != "small" {
		t.Fatalf("extra %+v", p.Extra)
	}

	rel, err := AtRelease(context.Background(), url, "step/a", "1.0.0", Options{Extensions: ext})
	if err != nil || rel.Extra["nodes"].Content[0].Value != "big" || rel.Manifest.Extra["nodes"].Content[0].Value != "small" {
		t.Fatalf("release %+v %v", rel.Extra, err)
	}

	// a listing for another product keeps every key
	got, err = List(context.Background(), url, Options{Extensions: manifest.AnyExtension})
	if err != nil || len(got) != 1 || got[0].Extra["nodes"].Kind != yaml.SequenceNode {
		t.Fatalf("any: %+v %v", got, err)
	}
}

func TestAnEntryKeyThatIsAManifestsMovingKeyIsNoEntryKey(t *testing.T) {
	data := "repository: 1\nplugins:\n  - {manifest: step/a/manifest.yaml, spec: {}}\n"
	if _, err := Parse([]byte(data)); err == nil || !strings.Contains(err.Error(), "field spec not found") {
		t.Fatalf("parsed: %v", err)
	}
}

func TestAListingFetchesTheFilesTheProductNames(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{
		"repository-manifest.yaml": "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n" +
			"  - {manifest: step/a/manifest.yaml, banner: art/banner.txt}\n",
		"step/a/manifest.yaml": pluginManifest("a", "step", "A") + "icon: icon.svg\n",
		"step/a/icon.svg":      `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"art/banner.txt":       "the banner\n",
	})
	r.git("tag", "step/a/v1.0.0")

	url := r.bare()

	opt := Options{
		Extensions: manifest.Extensions{"icon": {}, "banner": {}},
		Fetch: func(p Plugin) []string {
			// a manifest's path is from the plugin's directory, an entry's from the repository's root
			icon := p.ManifestExtra["icon"]
			banner := p.Extra["banner"]

			return []string{path.Join(p.Path, icon.Value), banner.Value, "step/a/none.svg", "../outside"}
		},
	}

	check := func(what string, got map[string]Fetched) {
		t.Helper()

		if f := got["step/a/icon.svg"]; f.Err != nil || f.Type != "image/svg+xml" {
			t.Errorf("%s: icon %+v", what, f)
		}

		if f := got["art/banner.txt"]; f.Err != nil || string(f.Data) != "the banner\n" || !strings.HasPrefix(f.Type, "text/plain") {
			t.Errorf("%s: banner %+v", what, f)
		}

		if f := got["step/a/none.svg"]; !errors.Is(f.Err, ErrFetch) || f.Data != nil {
			t.Errorf("%s: none %+v", what, f)
		}

		if f := got["../outside"]; !errors.Is(f.Err, ErrFetch) {
			t.Errorf("%s: outside %+v", what, f)
		}
	}

	got, err := List(context.Background(), url, opt)
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}

	check("listed", got[0].Fetched)

	rel, err := AtRelease(context.Background(), url, "step/a", "1.0.0", opt)
	if err != nil {
		t.Fatal(err)
	}

	check("released", rel.Fetched)
}
