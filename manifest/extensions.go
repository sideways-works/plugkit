package manifest

import (
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Extensions are the keys a product adds to a manifest beyond the ones plugkit reads, each with
// the check of its value, nil for none. A key that is neither plugkit's nor among them is refused
// with its line, so a typo fails where the manifest is read. The product reads the values from
// Extra and owns their meaning.
type Extensions map[string]func(*yaml.Node) error

// anyKey in Extensions accepts every key the others don't name.
const anyKey = "*"

// AnyExtension keeps every key plugkit doesn't read as it is, unchecked: for a host that lists
// plugins and leaves their meaning to the product that runs them.
var AnyExtension = Extensions{anyKey: nil} //nolint:gochecknoglobals // a value to pass, never changed

// Moving are the keys plugkit still reads into Manifest's fields and gives in Extra as well, while
// the products move to reading them from there; plugkit stops reading them in a later version.
var Moving = []string{"logo", "color", "spec", "schemas", "ack_timeout", "permissions"} //nolint:gochecknoglobals // a list to read

// check refuses each key of extra that e doesn't accept, and each value its check refuses, in the
// order of the keys; a moving key is plugkit's still, and checked by e only when e names it.
func (e Extensions) check(extra map[string]yaml.Node, moving []string, lines map[string]int, add func(string, ...any)) {
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		n := extra[k]

		f, ok := e[k]

		switch {
		case !ok && slices.Contains(moving, k):
			continue
		case !ok:
			if f, ok = e[anyKey]; !ok {
				add("line %d: field %s not found", lines[k], k)

				continue
			}
		}

		if f == nil {
			continue
		}

		if err := f(&n); err != nil {
			add("line %d: %s: %v", lines[k], k, err)
		}
	}
}

// CheckMapping is e's check of m, a mapping such as a repository manifest's entry: extra are the
// keys its reader doesn't read; each refusal goes to add, with its line.
func (e Extensions) CheckMapping(m *yaml.Node, extra map[string]yaml.Node, add func(string, ...any)) {
	lines := map[string]int{}

	if m != nil && m.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(m.Content); i += 2 {
			lines[m.Content[i].Value] = m.Content[i].Line
		}
	}

	e.check(extra, nil, lines, add)
}

// extraOf is extra, the top-level keys of data plugkit doesn't read, with the moving keys added,
// and the line of each top-level key.
func extraOf(data []byte, extra map[string]yaml.Node) (map[string]yaml.Node, map[string]int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err //nolint:wrapcheck // the caller wraps it
	}

	out, lines := map[string]yaml.Node{}, map[string]int{}
	maps.Copy(out, extra)

	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		m := doc.Content[0]
		for i := 0; i+1 < len(m.Content); i += 2 {
			k := m.Content[i].Value
			lines[k] = m.Content[i].Line

			if slices.Contains(Moving, k) {
				out[k] = *m.Content[i+1]
			}
		}
	}

	if len(out) == 0 {
		out = nil
	}

	return out, lines, nil
}
