package manifest

import (
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Extensions are the keys a product adds to a manifest beyond the ones plugkit reads. A key that
// is neither plugkit's nor among them is refused with its line, so a typo fails where the manifest
// is read. The product reads the values from Extra and owns their meaning.
type Extensions map[string]Extension

// Extension is one key of a product: Check checks its value, and Files names the files of the
// plugin's directory the value names, which go into the zip under their own paths beside the
// manifest, as ExtraFiles. plugkit checks each such path: relative to the plugin's directory,
// without .. or ., and clear of the program, the files, the assets and include. Either may be nil.
type Extension struct {
	Check func(*yaml.Node) error
	Files func(*yaml.Node) []string
}

// anyKey in Extensions accepts every key the others don't name.
const anyKey = "*"

// AnyExtension keeps every key plugkit doesn't read as it is, unchecked: for a host that lists
// plugins and leaves their meaning to the product that runs them. It names no files, so it is for
// listings, not for packing, unpacking or building a plugin whose keys name files.
var AnyExtension = Extensions{anyKey: {}} //nolint:gochecknoglobals // a value to pass, never changed

// check refuses each key of extra that e doesn't accept, each value its check refuses and each
// file it names that is no path of the plugin's directory, in the order of the keys, and returns
// the files.
func (e Extensions) check(extra map[string]yaml.Node, lines map[string]int, add func(string, ...any)) []string {
	var files []string

	for _, k := range slices.Sorted(maps.Keys(extra)) {
		n := extra[k]

		f, ok := e[k]

		if !ok {
			if f, ok = e[anyKey]; !ok {
				add("line %d: field %s not found", lines[k], k)

				continue
			}
		}

		if f.Check != nil {
			if err := f.Check(&n); err != nil {
				add("line %d: %s: %v", lines[k], k, err)

				continue
			}
		}

		if f.Files == nil {
			continue
		}

		for _, name := range f.Files(&n) {
			if !localPath(name) {
				add("line %d: %s: file %q: a file of the plugin's directory, relative, without .. or .", lines[k], k, name)

				continue
			}

			files = append(files, name)
		}
	}

	slices.Sort(files)

	return slices.Compact(files)
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

	e.check(extra, lines, add)
}

// extraOf is extra, the top-level keys of data plugkit doesn't read, and the line of each
// top-level key.
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
			lines[m.Content[i].Value] = m.Content[i].Line
		}
	}

	if len(out) == 0 {
		out = nil
	}

	return out, lines, nil
}
