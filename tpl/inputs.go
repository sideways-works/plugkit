package tpl

import (
	"strings"

	"go.yaml.in/yaml/v3"
)

// ownInputs are the file's own values:, defines: and env: blocks.
type ownInputs struct {
	values  map[string]any
	defines map[string]any
	env     []string
}

// block is one of the file's own input blocks: its key line and the lines
// after it that YAML reads as part of its value.
type block struct {
	key  string
	line int
	text string
}

// readOwnInputs reads the file's values:, defines: and env: as text, since
// the file isn't YAML before it is expanded.
func readOwnInputs(name string, src []byte) (ownInputs, error) {
	blocks, err := findBlocks(name, string(src))
	if err != nil {
		return ownInputs{}, err
	}

	var own ownInputs

	for _, b := range blocks {
		v, err := b.parse(name)
		if err != nil {
			return ownInputs{}, err
		}

		switch b.key {
		case "values":
			own.values, err = valuesOf(v, b, name)
		case definesKey:
			own.defines, err = definesOf(v, 0)
			if err != nil {
				err = errorAt(name, b.line, "%s", err.Error())
			}
		default:
			own.env, err = envOf(v, b, name)
		}

		if err != nil {
			return ownInputs{}, err
		}
	}

	return own, nil
}

func valuesOf(v any, b block, name string) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, errorAt(name, b.line, "values: holds a mapping")
	}

	return m, nil
}

func envOf(v any, b block, name string) ([]string, error) {
	if v == nil {
		return nil, nil
	}

	list, ok := v.([]any)
	if !ok {
		return nil, errorAt(name, b.line, "env: holds a list of variable names")
	}

	out := make([]string, 0, len(list))

	for _, e := range list {
		s, ok := e.(string)
		if !ok || !identifier.MatchString(s) {
			return nil, errorAt(name, b.line, "env: %v is not a variable name", e)
		}

		out = append(out, s)
	}

	return out, nil
}

var ownKeys = []string{"values", definesKey, "env"}

func findBlocks(name, src string) ([]block, error) {
	lines := strings.SplitAfter(src, "\n")

	var (
		out   []block
		open  = -1
		depth int
	)

	for i, l := range lines {
		if open >= 0 {
			if depth > 0 || continuesBlock(l) {
				depth = flowDepth(l, depth)

				continue
			}

			out[open].text = strings.Join(lines[out[open].line-1:i], "")
			open = -1
		}

		key, ok := ownKey(l)
		if !ok {
			continue
		}

		for _, b := range out {
			if b.key == key {
				return nil, errorAt(name, i+1, "%s: is also at line %d", key, b.line)
			}
		}

		out = append(out, block{key: key, line: i + 1})
		open = len(out) - 1
		depth = flowDepth(l[len(key)+1:], 0)
	}

	if open >= 0 {
		out[open].text = strings.Join(lines[out[open].line-1:], "")
	}

	for _, b := range out {
		if at := strings.Index(b.text, "{{"); at >= 0 {
			line := b.line + strings.Count(b.text[:at], "\n")

			return nil, errorAt(name, line, "%s: holds a template action; values:, defines: and env: are plain YAML", b.key)
		}
	}

	return out, nil
}

// ownKey reports whether the line starts one of the file's own blocks.
func ownKey(l string) (string, bool) {
	for _, k := range ownKeys {
		rest, ok := strings.CutPrefix(l, k+":")
		if ok && (rest == "" || strings.ContainsAny(rest[:1], " \t\r\n")) {
			return k, true
		}
	}

	return "", false
}

// continuesBlock reports whether a line outside any flow collection belongs
// to the block above it, as YAML reads it: an indented, blank or comment
// line, or a sequence entry at column 0. Any other line at column 0 ends
// the block, be it a key, an action or a document marker.
func continuesBlock(l string) bool {
	if l == "" || strings.ContainsAny(l[:1], " \t\r\n#") {
		return true
	}

	return l[0] == '-' && (len(l) == 1 || strings.ContainsAny(l[1:2], " \t\r\n"))
}

// flowDepth returns how many flow collections, [ or {, are open after the
// line s, given depth open before it. Brackets in quoted scalars and
// comments don't count.
func flowDepth(s string, depth int) int {
	var quote byte

	prev := byte(' ')

	for i := 0; i < len(s); i++ {
		c := s[i]

		switch {
		case quote == '\'' && c == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
			} else {
				quote = 0
			}
		case quote == '"' && c == '\\':
			i++
		case quote == '"' && c == '"':
			quote = 0
		case quote != 0:
		case c == '#' && strings.IndexByte(" \t", prev) >= 0:
			return depth
		case (c == '\'' || c == '"') && strings.IndexByte(" \t[{,:", prev) >= 0:
			quote = c
		case c == '[' || c == '{':
			depth++
		case (c == ']' || c == '}') && depth > 0:
			depth--
		}

		prev = c
	}

	return depth
}

// parse reads the block as plain YAML and returns the value of its key.
func (b block) parse(name string) (any, error) {
	var doc yaml.Node

	err := yaml.Unmarshal([]byte(b.text), &doc)
	if err != nil {
		line, msg := yamlLine(yamlMessage(err.Error()))

		return nil, errorAt(name, b.line+max(line, 1)-1, "%s: %s", b.key, msg)
	}

	v, err := fromNode(&doc, 0)
	if err != nil {
		line, msg := yamlLine(err.Error())

		return nil, errorAt(name, b.line+max(line, 1)-1, "%s: %s", b.key, msg)
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, errorAt(name, b.line, "%s: is a key of the file's top-level mapping", b.key)
	}

	return m[b.key], nil
}
