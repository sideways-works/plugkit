package manifest

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const extended = `name: lint
kind: step
program: lint
platforms: [linux/amd64]
build: {image: "golang@sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1", run: [[go, build, -o, "{out}/lint", .]]}
logo: logo.svg
nodes:
  - small
no_nodes: true
`

var errList = errors.New("a list")

func TestAnExtensionKeyIsTheProductsAndATypoIsRefused(t *testing.T) {
	// without extensions, a key plugkit doesn't read is refused with its line
	if _, err := Parse([]byte(extended)); err == nil || !strings.Contains(err.Error(), "line 7: field nodes not found") ||
		!strings.Contains(err.Error(), "line 9: field no_nodes not found") {
		t.Fatalf("parsed: %v", err)
	}

	var checked []string

	ext := Extensions{
		"nodes": {Check: func(n *yaml.Node) error {
			checked = append(checked, "nodes")

			if n.Kind != yaml.SequenceNode {
				return errList
			}

			return nil
		}},
		"no_nodes": {},
		"logo": {Check: func(n *yaml.Node) error {
			checked = append(checked, "logo:"+n.Value)

			return nil
		}},
	}

	m, err := ParseWith([]byte(extended), ext)
	if err != nil {
		t.Fatal(err)
	}

	nodes, logo := m.Extra["nodes"], m.Extra["logo"]
	if len(m.Extra) != 3 || nodes.Kind != yaml.SequenceNode || nodes.Content[0].Value != "small" || nodes.Line != 8 ||
		logo.Value != "logo.svg" {
		t.Fatalf("extra %+v", m.Extra)
	}

	if strings.Join(checked, " ") != "logo:logo.svg nodes" {
		t.Fatalf("checked %q", checked)
	}

	// a check that refuses names the key and its line; a key the product doesn't name is a typo
	_, err = ParseWith([]byte(strings.Replace(extended, "nodes:\n  - small\n", "nodes: small\nnode: [small]\n", 1)), ext)
	if err == nil || !strings.Contains(err.Error(), "line 7: nodes: a list") || !strings.Contains(err.Error(), "line 8: field node not found") {
		t.Fatalf("typo: %v", err)
	}

	// a listing keeps every key as it is
	m, err = ParseWith([]byte(extended+"anything: {at: all}\n"), AnyExtension)
	if err != nil || len(m.Extra) != 4 || m.Extra["anything"].Kind != yaml.MappingNode {
		t.Fatalf("any: %+v %v", m.Extra, err)
	}
}

func TestAManifestOfPlugkitsKeysAloneHasNoExtra(t *testing.T) {
	m, err := Parse([]byte(strings.Replace(extended, "logo: logo.svg\nnodes:\n  - small\nno_nodes: true\n", "", 1)))
	if err != nil || m.Extra != nil {
		t.Fatalf("%+v %v", m.Extra, err)
	}

	if _, err := Parse([]byte(strings.Replace(extended, "nodes:\n  - small\nno_nodes: true\n", "", 1))); err == nil ||
		!strings.Contains(err.Error(), "line 6: field logo not found") {
		t.Fatalf("a logo without its product: %v", err)
	}
}

// files names a scalar's value, or each of a list's.
func files(n *yaml.Node) []string {
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}

	var out []string
	for _, c := range n.Content {
		out = append(out, c.Value)
	}

	return out
}

func TestTheFilesAKeyNamesAreTheZipsAndPlugkitChecksThem(t *testing.T) {
	ext := Extensions{"icons": {Files: files}}

	head, _, _ := strings.Cut(extended, "logo:")

	m, err := ParseWith([]byte(head+"icons: [icons/a.svg, b.png]\n"), ext)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(m.ExtraFiles, []string{"b.png", "icons/a.svg"}) || !slices.Equal(m.Extras(), []string{"b.png", "icons/a.svg"}) {
		t.Fatalf("extra files %q, extras %q", m.ExtraFiles, m.Extras())
	}

	for name, c := range map[string]struct{ yaml, want string }{
		"outside":     {head + "icons: [../x.svg]\n", `line 6: icons: file "../x.svg": a file of the plugin's directory`},
		"a URL":       {head + "icons: [\"https://example.com/x.svg\"]\n", `icons: file "https://example.com/x.svg"`},
		"the program": {head + "icons: [lint.mjs]\n", `"lint.mjs", a file a key names: lint.mjs is there too (the program)`},
		"an asset":    {head + "assets: [web]\nicons: [web/x.svg]\n", `"web/x.svg", a file a key names: web is there too (assets: web)`},
		"included":    {head + "include: [icons]\nicons: [icons/a.svg]\n", `include: "icons": icons/a.svg is there too (a file a key names)`},
	} {
		if _, err := ParseWith([]byte(c.yaml), ext); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
