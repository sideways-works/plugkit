package tpl

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const definesKey = "defines"

// SetKind is the command-line flag a value was given with.
type SetKind string

// The flags that set values.
const (
	// Set reads the value as YAML reads a scalar: a number, boolean or string.
	Set SetKind = "--set"
	// SetString always sets a string.
	SetString SetKind = "--set-string"
	// SetJSON sets any JSON value.
	SetJSON SetKind = "--set-json"
)

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseSet parses the argument of --set, --set-string or --set-json, such as
// "a.b=1", into a map holding the value at that key path, ready for Merge.
func ParseSet(kind SetKind, arg string) (map[string]any, error) {
	where := string(kind) + " " + arg
	key, raw, ok := strings.Cut(arg, "=")
	keys := strings.Split(key, ".")

	if !ok || slices.Contains(keys, "") {
		return nil, &Error{File: where, Msg: "expected key=value"}
	}

	var v any

	switch kind {
	case Set:
		v = scalar(raw)
	case SetString:
		v = raw
	case SetJSON:
		var err error

		v, err = parseJSON(raw)
		if err != nil {
			return nil, &Error{File: where, Msg: err.Error()}
		}
	default:
		return nil, &Error{File: where, Msg: "unknown flag"}
	}

	for _, k := range slices.Backward(keys) {
		v = map[string]any{k: v}
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, &Error{File: where, Msg: "expected key=value"}
	}

	return m, nil
}

// scalar reads s as YAML reads a plain scalar, keeping numbers and booleans;
// anything else stays the string s.
func scalar(s string) any {
	n := yaml.Node{Kind: yaml.ScalarNode, Value: s}

	var v any

	err := n.Decode(&v)
	if err != nil {
		return s
	}

	switch v.(type) {
	case int, int64, uint64, float64, bool:
		return normalizeScalar(v)
	default:
		return s
	}
}

func parseJSON(raw string) (any, error) {
	var probe any

	err := json.Unmarshal([]byte(raw), &probe)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	var n yaml.Node

	err = yaml.Unmarshal([]byte(raw), &n)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	v, err := fromNode(&n, 0)
	if err != nil {
		return nil, err
	}

	return v, nil
}

// ParseDefine parses the argument of -D: NAME sets the define to true,
// NAME=value to the string value.
func ParseDefine(arg string) (name string, value any, err error) {
	name, raw, hasValue := strings.Cut(arg, "=")
	if !identifier.MatchString(name) {
		return "", nil, &Error{File: "-D " + arg, Msg: "expected NAME or NAME=value"}
	}

	if !hasValue {
		return name, true, nil
	}

	return name, raw, nil
}

// ReadValuesFile reads a values file given with -f, YAML or JSON. Its
// defines: key holds defines, which are returned apart from the values.
func ReadValuesFile(name string, data []byte) (values, defines map[string]any, err error) {
	where := "-f " + name

	var doc yaml.Node

	err = yaml.Unmarshal(data, &doc)
	if err != nil {
		return nil, nil, &Error{File: where, Msg: yamlMessage(err.Error())}
	}

	v, err := fromNode(&doc, 0)
	if err != nil {
		return nil, nil, &Error{File: where, Msg: err.Error()}
	}

	if v == nil {
		return map[string]any{}, map[string]any{}, nil
	}

	values, ok := v.(map[string]any)
	if !ok {
		return nil, nil, &Error{File: where, Msg: fmt.Sprintf("line %d: a values file holds a mapping", topLine(&doc))}
	}

	defines, err = definesOf(values[definesKey], defineLine(&doc))
	if err != nil {
		return nil, nil, &Error{File: where, Msg: err.Error()}
	}

	delete(values, definesKey)

	return values, defines, nil
}

func topLine(doc *yaml.Node) int {
	if len(doc.Content) > 0 {
		return doc.Content[0].Line
	}

	return doc.Line
}

func defineLine(doc *yaml.Node) int {
	if len(doc.Content) == 0 {
		return 0
	}

	m := doc.Content[0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == definesKey {
			return m.Content[i].Line
		}
	}

	return 0
}

// definesOf checks a defines: mapping: names are identifiers and values are
// strings, numbers or booleans.
func definesOf(v any, line int) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}

	m, ok := v.(map[string]any)
	if !ok {
		return nil, lineError(line, "defines: holds a mapping of names to values")
	}

	for k, d := range m {
		if !identifier.MatchString(k) {
			return nil, lineError(line, fmt.Sprintf("defines: %q is not a name", k))
		}

		switch d.(type) {
		case string, bool, int, float64, uint64:
		default:
			return nil, lineError(line, fmt.Sprintf("defines: %s is a string, number or boolean", k))
		}
	}

	return m, nil
}

type nodeError struct {
	line int
	msg  string
}

func (e *nodeError) Error() string {
	if e.line == 0 {
		return e.msg
	}

	return fmt.Sprintf("line %d: %s", e.line, e.msg)
}

func lineError(line int, msg string) error { return &nodeError{line: line, msg: msg} }

// yamlMessage turns a yaml error into "line N: message".
func yamlMessage(s string) string {
	s = strings.TrimPrefix(s, "yaml: ")
	s = strings.TrimPrefix(s, "unmarshal errors:\n  ")

	return strings.Replace(s, "not allowed in this context", "not allowed here", 1)
}

// yamlLine splits "line N: message" into its parts.
func yamlLine(s string) (int, string) {
	rest, ok := strings.CutPrefix(s, "line ")
	if !ok {
		return 0, s
	}

	num, msg, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, s
	}

	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, s
	}

	return n, msg
}

const maxNodeDepth = 1000

// fromNode converts a YAML node into maps with string keys, lists and
// scalars, the only shapes templates see.
func fromNode(n *yaml.Node, depth int) (any, error) {
	if depth > maxNodeDepth {
		return nil, lineError(n.Line, "nested too deeply")
	}

	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil //nolint:nilnil // an empty document is null
		}

		return fromNode(n.Content[0], depth+1)
	case yaml.AliasNode:
		return fromNode(n.Alias, depth+1)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))

		for _, c := range n.Content {
			v, err := fromNode(c, depth+1)
			if err != nil {
				return nil, err
			}

			out = append(out, v)
		}

		return out, nil
	case yaml.MappingNode:
		return fromMapping(n, depth)
	case yaml.ScalarNode:
		return fromScalar(n)
	default:
		return nil, nil //nolint:nilnil // yaml.v3 has no other kinds
	}
}

func fromMapping(n *yaml.Node, depth int) (map[string]any, error) {
	out := map[string]any{}
	seen := map[string]int{}

	var merged []map[string]any

	for i := 0; i+1 < len(n.Content); i += 2 {
		k, val := n.Content[i], n.Content[i+1]

		v, err := fromNode(val, depth+1)
		if err != nil {
			return nil, err
		}

		if k.Tag == "!!merge" {
			merged = append(merged, mergeSources(v)...)

			continue
		}

		if k.Kind != yaml.ScalarNode {
			return nil, lineError(k.Line, "a mapping key is a string")
		}

		if prev, ok := seen[k.Value]; ok {
			return nil, lineError(k.Line, fmt.Sprintf("mapping key %q already defined at line %d", k.Value, prev))
		}

		seen[k.Value] = k.Line
		out[k.Value] = v
	}

	for _, m := range merged {
		for k, v := range m {
			if _, ok := out[k]; !ok {
				out[k] = v
			}
		}
	}

	return out, nil
}

func mergeSources(v any) []map[string]any {
	switch v := v.(type) {
	case map[string]any:
		return []map[string]any{v}
	case []any:
		var out []map[string]any

		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}

		return out
	default:
		return nil
	}
}

func fromScalar(n *yaml.Node) (any, error) {
	var v any

	err := n.Decode(&v)
	if err != nil {
		return nil, lineError(n.Line, yamlMessage(err.Error()))
	}

	switch v := v.(type) {
	case time.Time:
		return n.Value, nil
	case []byte:
		return string(v), nil
	default:
		return normalizeScalar(v), nil
	}
}

func normalizeScalar(v any) any {
	switch v := v.(type) {
	case int64:
		return int(v)
	case uint64:
		if v <= math.MaxInt64 {
			return int(v)
		}

		return v
	default:
		return v
	}
}

var errUnsupported = errors.New("unsupported value")

// normalizeMap converts a caller's map into the shapes templates see.
func normalizeMap(m map[string]any) (map[string]any, error) {
	if m == nil {
		return map[string]any{}, nil
	}

	v, err := normalize(reflect.ValueOf(m))
	if err != nil {
		return nil, err
	}

	out, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}

	return out, nil
}

func normalize(rv reflect.Value) (any, error) {
	if !rv.IsValid() {
		return nil, nil //nolint:nilnil // nil is the value
	}

	switch rv.Kind() {
	case reflect.Interface, reflect.Pointer:
		if rv.IsNil() {
			return nil, nil //nolint:nilnil // nil is the value
		}

		return normalize(rv.Elem())
	case reflect.Map:
		return normalizeMapValue(rv)
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())

		for i := range out {
			v, err := normalize(rv.Index(i))
			if err != nil {
				return nil, err
			}

			out[i] = v
		}

		return out, nil
	case reflect.String:
		return rv.String(), nil
	case reflect.Bool:
		return rv.Bool(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return normalizeScalar(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	default:
		return nil, fmt.Errorf("%w of type %s", errUnsupported, rv.Type())
	}
}

func normalizeMapValue(rv reflect.Value) (any, error) {
	if rv.Type().Key().Kind() != reflect.String {
		return nil, fmt.Errorf("%w: a map keyed by %s", errUnsupported, rv.Type().Key())
	}

	out := make(map[string]any, rv.Len())
	it := rv.MapRange()

	for it.Next() {
		v, err := normalize(it.Value())
		if err != nil {
			return nil, err
		}

		out[it.Key().String()] = v
	}

	return out, nil
}

func clone(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			out[k] = clone(e)
		}

		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = clone(e)
		}

		return out
	default:
		return v
	}
}

// Merge merges layers of values, a later layer winning. Maps merge key by
// key; anything else, a list or a scalar, is replaced whole. The result
// shares nothing with the layers.
func Merge(layers ...map[string]any) map[string]any {
	out := map[string]any{}

	for _, l := range layers {
		mergeInto(out, l)
	}

	return out
}

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		sm, isMap := v.(map[string]any)
		dm, wasMap := dst[k].(map[string]any)

		if isMap && wasMap {
			mergeInto(dm, sm)

			continue
		}

		dst[k] = clone(v)
	}
}

// ExpandedError is an error in the expanded text, which the user doesn't
// see: it names the line of the expanded file, as validate --expanded
// prints it.
type ExpandedError struct {
	Name string
	Line int
	Msg  string
}

// Error formats the error as NAME (expanded) line LINE: MSG.
func (e *ExpandedError) Error() string {
	return fmt.Sprintf("%s (expanded) line %d: %s", e.Name, e.Line, e.Msg)
}

// Expanded rewrites an error from reading the expanded text of name as
// YAML into an *ExpandedError. Other errors are returned as they are.
func Expanded(name string, err error) error {
	if err == nil {
		return nil
	}

	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		line, msg := yamlLine(te.Errors[0])

		return &ExpandedError{Name: name, Line: line, Msg: msg}
	}

	s := err.Error()
	if !strings.HasPrefix(s, "yaml: ") {
		return err
	}

	line, msg := yamlLine(yamlMessage(s))

	return &ExpandedError{Name: name, Line: line, Msg: msg}
}
