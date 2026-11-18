package tpl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"reflect"
	"strconv"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"
)

// funcError is an error of a template function; the expander adds the file
// and line of the call.
type funcError struct{ msg string }

func (e *funcError) Error() string { return e.msg }

func fail(format string, args ...any) error {
	return &funcError{msg: fmt.Sprintf(format, args...)}
}

// functions are the functions the spec adds, but include, which needs the
// expander.
var functions = template.FuncMap{
	"indent":   indent,
	"nindent":  nindent,
	"toYaml":   toYAML,
	"toJson":   toJSON,
	"quote":    quote,
	"default":  orDefault,
	"required": required,
	"dict":     dict,
	"list":     list,
	fnHasKey:   hasKey,
	"join":     join,
	"split":    split,
	"upper":    strings.ToUpper,
	"lower":    strings.ToLower,
	"trim":     strings.TrimSpace,
	"replace":  replace,
	"base":     path.Base,
	"dir":      path.Dir,
}

// parseNames are the functions a template may call: Go's built-ins except
// html, js and urlquery, and the spec's. The internal ones the rewriter
// inserts are missing, so a template can't call them.
var parseNames = func() map[string]any {
	names := map[string]any{fnInclude: true}

	for _, n := range []string{"and", "call", "index", "slice", "len", "not", "or", "print", "printf", "println", "eq", "ge", "gt", "le", "lt", "ne"} {
		names[n] = true
	}

	for n := range functions {
		names[n] = true
	}

	return names
}()

func (x *expander) funcs() template.FuncMap {
	m := template.FuncMap{
		"__field":   x.field,
		"__index":   x.index,
		"__hasKey":  x.hasKey,
		"__dot":     x.dot,
		"__include": x.include,
	}
	maps.Copy(m, functions)

	return m
}

func indent(n int, s string) (string, error) {
	if n < 0 {
		return "", fail("indent: %d is negative", n)
	}

	pad := strings.Repeat(" ", n)

	return pad + strings.ReplaceAll(s, "\n", "\n"+pad), nil
}

func nindent(n int, s string) (string, error) {
	out, err := indent(n, s)

	return "\n" + out, err
}

func toYAML(v any) (string, error) {
	var b bytes.Buffer

	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)

	err := enc.Encode(v)
	if err == nil {
		err = enc.Close()
	}

	if err != nil {
		return "", fail("toYaml: %v", err)
	}

	return strings.TrimSuffix(b.String(), "\n"), nil
}

func toJSON(v any) (string, error) {
	var b bytes.Buffer

	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)

	err := enc.Encode(v)
	if err != nil {
		return "", fail("toJson: %v", err)
	}

	return strings.TrimSuffix(b.String(), "\n"), nil
}

func quote(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return `""`, nil
	case string:
		return strconv.Quote(v), nil
	case bool, int, int64, uint64, float64:
		return strconv.Quote(fmt.Sprint(v)), nil
	default:
		return "", fail("quote: %T is not a string, number or boolean", v)
	}
}

// empty is text/template's notion of empty: false, 0, nil, and a string,
// list or map of length zero.
func empty(v any) bool {
	truth, ok := template.IsTrue(v)

	return !ok || !truth
}

func orDefault(fallback, v any) any {
	if empty(v) {
		return fallback
	}

	return v
}

func required(msg string, v any) (any, error) {
	if empty(v) {
		return nil, fail("required: %s", msg)
	}

	return v, nil
}

func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fail("dict: odd number of arguments")
	}

	m := make(map[string]any, len(kv)/2)

	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fail("dict: key %v is not a string", kv[i])
		}

		m[k] = kv[i+1]
	}

	return m, nil
}

func list(v ...any) []any {
	return append([]any{}, v...)
}

func hasKey(m any, key string) (bool, error) {
	switch m := m.(type) {
	case nil:
		return false, nil
	case map[string]any:
		_, ok := m[key]

		return ok, nil
	default:
		return false, fail("hasKey: %T is not a map", m)
	}
}

func join(sep string, l any) (string, error) {
	rv := reflect.ValueOf(l)
	if l == nil {
		return "", nil
	}

	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return "", fail("join: %T is not a list", l)
	}

	parts := make([]string, rv.Len())

	for i := range parts {
		switch e := rv.Index(i).Interface().(type) {
		case nil:
		case string:
			parts[i] = e
		case bool, int, int64, uint64, float64:
			parts[i] = fmt.Sprint(e)
		default:
			return "", fail("join: element %d is a %T, not a string", i, e)
		}
	}

	return strings.Join(parts, sep), nil
}

func split(sep, s string) []any {
	parts := strings.Split(s, sep)
	out := make([]any, len(parts))

	for i, p := range parts {
		out[i] = p
	}

	return out
}

func replace(old, repl, s string) string {
	return strings.ReplaceAll(s, old, repl)
}

func display(base string, keys []string) string {
	return base + "." + strings.Join(keys, ".")
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}

	return strings.Split(p, ".")
}

// walk resolves keys below recv. Below the template's input, .Env.NAME is
// checked against the env: list and the never-readable names first.
func (x *expander) walk(recv any, base string, keys []string) (any, bool, error) {
	root := x.isRoot(recv)
	cur := recv

	for i, k := range keys {
		if root && i == 1 && keys[0] == envKey {
			err := x.envReadable(display(base, keys[:2]), k)
			if err != nil {
				return nil, root, err
			}
		}

		m, ok := cur.(map[string]any)
		if !ok && cur != nil {
			return nil, root, fail("%s: %s is a %s, not a map", display(base, keys[:i+1]), display(base, keys[:i]), kind(cur))
		}

		v, ok := m[k]
		if !ok {
			return nil, root, fail("%s: no such key", display(base, keys[:i+1]))
		}

		cur = v
	}

	return cur, root, nil
}

func kind(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "string"
	case bool:
		return "boolean"
	default:
		return "number"
	}
}

func (x *expander) envReadable(shown, name string) error {
	if x.never[name] {
		return fail("%s is a credential and is never readable", shown)
	}

	if !x.listed[name] {
		return fail("%s is not allowed: add %s to env:", shown, name)
	}

	return nil
}

func (x *expander) field(recv any, base, p string) (any, error) {
	keys := splitPath(p)

	v, root, err := x.walk(recv, base, keys)
	if err != nil {
		return nil, err
	}

	if root {
		x.note(keys, v, false)
	}

	return v, nil
}

func (x *expander) dot(recv any) any {
	if x.isRoot(recv) {
		x.note(nil, nil, false)
	}

	return recv
}

// index is text/template's index on an input: a missing map key gives nil.
// Below the template's input it records the value it ends at, or the key it
// didn't find.
func (x *expander) index(recv any, base, p string, args ...any) (any, error) {
	keys := splitPath(p)

	v, track, err := x.walk(recv, base, keys)
	if err != nil {
		return nil, err
	}

	for _, a := range args {
		if track && len(keys) == 1 && keys[0] == envKey {
			name := fmt.Sprint(a)

			err := x.envReadable(display(base, []string{envKey, name}), name)
			if err != nil {
				return nil, err
			}
		}

		if track {
			keys, track = x.noteStep(keys, v, a)
		}

		v, err = indexStep(v, a)
		if err != nil {
			return nil, err
		}
	}

	if track {
		x.note(keys, v, false)
	}

	return v, nil
}

// noteStep follows one more key of an input. A list read by position is
// recorded whole, and so is a key not found; either ends the tracking.
func (x *expander) noteStep(keys []string, v, a any) ([]string, bool) {
	m, isMap := v.(map[string]any)
	k, isKey := a.(string)

	if !isMap || !isKey {
		x.note(keys, v, false)

		return keys, false
	}

	keys = append(keys, k)

	if _, ok := m[k]; !ok {
		x.note(keys, nil, true)

		return keys, false
	}

	return keys, true
}

// indexStep is one step of text/template's index, with its error texts.
func indexStep(v, a any) (any, error) {
	switch c := v.(type) {
	case map[string]any:
		k, ok := a.(string)
		if !ok {
			return nil, fail("error calling index: value has type %T; should be string", a)
		}

		return c[k], nil
	case []any:
		i, err := position(a, len(c))
		if err != nil {
			return nil, err
		}

		return c[i], nil
	case string:
		i, err := position(a, len(c))
		if err != nil {
			return nil, err
		}

		return c[i], nil
	case nil:
		return nil, fail("error calling index: index of nil pointer")
	default:
		return nil, fail("error calling index: can't index item of type %T", v)
	}
}

func position(a any, n int) (int, error) {
	i, ok := toInt(a)
	if !ok {
		return 0, fail("error calling index: cannot index slice/array with type %T", a)
	}

	if i < 0 || i >= n {
		return 0, fail("error calling index: index out of range: %d", i)
	}

	return i, nil
}

func toInt(a any) (int, bool) {
	rv := reflect.ValueOf(a)

	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(rv.Uint()), true //nolint:gosec // an index too large for int is out of range either way
	default:
		return 0, false
	}
}

func (x *expander) hasKey(recv any, base, p string, key string) (bool, error) {
	keys := splitPath(p)

	v, root, err := x.walk(recv, base, keys)
	if err != nil {
		return false, err
	}

	if root && len(keys) == 1 && keys[0] == envKey {
		err := x.envReadable(display(base, []string{envKey, key}), key)
		if err != nil {
			return false, err
		}
	}

	ok, err := hasKey(v, key)
	if err != nil {
		return false, err
	}

	if root {
		var found any
		if m, isMap := v.(map[string]any); isMap {
			found = m[key]
		}

		x.note(append(keys, key), found, !ok)
	}

	return ok, nil
}
