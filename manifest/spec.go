package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"go.yaml.in/yaml/v3"
)

// MaxSpec bounds a spec's bytes as JSON.
const MaxSpec = 64 << 10

// maxSpecDepth bounds how deeply a spec's mappings and lists nest.
const maxSpecDepth = 100

var errSpec = errors.New("spec")

// specOf converts the manifest's spec, a mapping the host reads and plugkit never looks into, to
// JSON: what YAML holds that JSON can't say, an anchor, an alias or a tag of its own, a key that is
// no string, a key twice, a number that is no finite one, is refused, and so is a spec over MaxSpec.
func specOf(n *yaml.Node) (json.RawMessage, error) {
	if n.Kind == 0 {
		return nil, nil
	}

	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w (line %d): a mapping, such as spec: {port: 8080}", errSpec, n.Line)
	}

	v, err := specValue(n, 0)
	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSpec, err)
	}

	if len(data) > MaxSpec {
		return nil, fmt.Errorf("%w: %d bytes as JSON, more than %d", errSpec, len(data), MaxSpec)
	}

	return data, nil
}

func specValue(n *yaml.Node, depth int) (any, error) {
	switch {
	case depth > maxSpecDepth:
		return nil, fmt.Errorf("%w (line %d): nested more than %d deep", errSpec, n.Line, maxSpecDepth)
	case n.Kind == yaml.AliasNode || n.Anchor != "":
		return nil, fmt.Errorf("%w (line %d): an anchor or alias, which JSON has no form of; write the value out", errSpec, n.Line)
	}

	switch n.Kind {
	case yaml.MappingNode:
		return specMapping(n, depth)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))

		for _, c := range n.Content {
			v, err := specValue(c, depth+1)
			if err != nil {
				return nil, err
			}

			out = append(out, v)
		}

		return out, nil
	case yaml.ScalarNode:
		return specScalar(n)
	default:
		return nil, fmt.Errorf("%w (line %d): no mapping, list or scalar", errSpec, n.Line)
	}
}

func specMapping(n *yaml.Node, depth int) (map[string]any, error) {
	out := make(map[string]any, len(n.Content)/2)

	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]

		switch {
		case k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str" || k.Anchor != "":
			return nil, fmt.Errorf("%w (line %d): key %q is no string; JSON keys are strings, so quote it", errSpec, k.Line, k.Value)
		case hasKey(out, k.Value):
			return nil, fmt.Errorf("%w (line %d): key %q is there twice", errSpec, k.Line, k.Value)
		}

		v, err := specValue(n.Content[i+1], depth+1)
		if err != nil {
			return nil, err
		}

		out[k.Value] = v
	}

	return out, nil
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]

	return ok
}

func specScalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!str", "!!timestamp":
		return n.Value, nil
	case "!!null":
		return nil, nil //nolint:nilnil // JSON's null
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("%w (line %d): %w", errSpec, n.Line, err)
		}

		return b, nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err != nil {
			return nil, fmt.Errorf("%w (line %d): %s is no 64-bit integer", errSpec, n.Line, n.Value)
		}

		return i, nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, fmt.Errorf("%w (line %d): %w", errSpec, n.Line, err)
		}

		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("%w (line %d): %s is no finite number, which JSON has no form of", errSpec, n.Line, n.Value)
		}

		return f, nil
	}

	return nil, fmt.Errorf("%w (line %d): tag %s, which JSON has no form of", errSpec, n.Line, n.Tag)
}
