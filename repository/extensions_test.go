package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/sideways-works/plugkit/manifest"
	"go.yaml.in/yaml/v3"
)

func TestAListingGivesTheProductsKeysOfManifestAndEntry(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{
		"repository-manifest.yaml": "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n" +
			"  - manifest: step/a/manifest.yaml\n    nodes: [big]\n    color: transparent\n",
		"step/a/manifest.yaml": pluginManifest("a", "step", "A") + "nodes: [small]\nno_nodes: false\n",
	})
	r.git("tag", "step/a/v1.0.0")

	url := r.bare()

	// a product's keys unknown to the reader are refused, the entry's with its line
	if _, err := List(context.Background(), url, Options{}); err == nil || !strings.Contains(err.Error(), "plugins[0]: line 5: field nodes not found") {
		t.Fatalf("without extensions: %v", err)
	}

	ext := manifest.Extensions{"nodes": nil, "no_nodes": nil}

	got, err := List(context.Background(), url, Options{Extensions: ext})
	if err != nil || len(got) != 1 || got[0].Err != nil {
		t.Fatalf("%+v %v", got, err)
	}

	p := got[0]

	nodes := p.Extra["nodes"]
	if len(nodes.Content) != 1 || nodes.Content[0].Value != "big" || p.Extra["no_nodes"].Value != "false" ||
		p.Extra["color"].Value != "transparent" || p.Extra["logo"].Value != "logo.svg" || !p.Color.Transparent {
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
