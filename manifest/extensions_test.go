package manifest

import (
	"errors"
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
		"nodes": func(n *yaml.Node) error {
			checked = append(checked, "nodes")

			if n.Kind != yaml.SequenceNode {
				return errList
			}

			return nil
		},
		"no_nodes": nil,
		"logo": func(n *yaml.Node) error {
			checked = append(checked, "logo:"+n.Value)

			return nil
		},
	}

	m, err := ParseWith([]byte(extended), ext)
	if err != nil {
		t.Fatal(err)
	}

	nodes, logo := m.Extra["nodes"], m.Extra["logo"]
	if len(m.Extra) != 3 || nodes.Kind != yaml.SequenceNode || nodes.Content[0].Value != "small" || nodes.Line != 8 ||
		logo.Value != "logo.svg" || m.Logo.Light != "logo.svg" {
		t.Fatalf("extra %+v, logo %+v", m.Extra, m.Logo)
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

func TestAMovingKeyIsInExtraAsWell(t *testing.T) {
	m, err := Parse([]byte(strings.Replace(extended, "nodes:\n  - small\nno_nodes: true\n", "color: transparent\n", 1)))
	if err != nil {
		t.Fatal(err)
	}

	if !m.Color.Transparent || m.Extra["color"].Value != "transparent" || m.Extra["logo"].Value != "logo.svg" || len(m.Extra) != 2 {
		t.Fatalf("%+v %+v", m.Color, m.Extra)
	}

	// none at all leaves Extra nil
	m, err = Parse([]byte(strings.Replace(extended, "logo: logo.svg\nnodes:\n  - small\nno_nodes: true\n", "", 1)))
	if err != nil || m.Extra != nil {
		t.Fatalf("%+v %v", m.Extra, err)
	}
}
