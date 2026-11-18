// Package tpl expands the Go templates in CI configuration files before
// they are read as YAML, so a template means the same in every tool that
// reads one.
package tpl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"text/template"
)

// Inputs are what a template reads besides its own values:, defines: and
// env: blocks.
type Inputs struct {
	// Values are merged from -f files and --set*, and apply over the file's
	// values:.
	Values map[string]any
	// Defines come from values files and -D, and apply over the file's
	// defines:.
	Defines map[string]any
	// Env is the environment the caller allows reading from, name to value.
	// A template reads only the names its env: block lists.
	Env map[string]string
	// Never names variables that are never readable, even when listed.
	Never []string
}

// Resolver reads the files that include names.
type Resolver interface {
	// Local reads a path of the repository at the file's commit.
	Local(ctx context.Context, path string) ([]byte, error)
	// Remote reads path of gitURL at commit, a full commit hash.
	Remote(ctx context.Context, gitURL, path, commit string) ([]byte, error)
}

// Result is an expanded file and everything its expansion read.
type Result struct {
	// Text is the expanded file.
	Text []byte
	// Read records the source, every include and every input read.
	Read Record
}

// Error is an expansion error at a line of a file the user wrote. File may
// also name a command-line argument, such as "--set a.b".
type Error struct {
	File string
	Line int
	Msg  string
}

// Error formats the error as FILE:LINE: MSG.
func (e *Error) Error() string {
	switch {
	case e.File == "":
		return e.Msg
	case e.Line == 0:
		return e.File + ": " + e.Msg
	default:
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	}
}

func errorAt(file string, line int, format string, args ...any) *Error {
	return &Error{File: file, Line: line, Msg: fmt.Sprintf(format, args...)}
}

// Expand expands src, the file name, with the inputs in, reading includes
// through r. A file without "{{" expands to itself.
func Expand(ctx context.Context, name string, src []byte, in Inputs, r Resolver) (Result, error) {
	own, err := readOwnInputs(name, src)
	if err != nil {
		return Result{}, err
	}

	x, err := newExpander(ctx, name, own, in, r)
	if err != nil {
		return Result{}, err
	}

	x.source = File{Ref: name, SHA256: sum(src)}

	if !bytes.Contains(src, []byte("{{")) {
		return Result{Text: bytes.Clone(src), Read: x.record()}, nil
	}

	main, err := x.parse(name, string(src), origin{})
	if err != nil {
		return Result{}, err
	}

	var out bytes.Buffer

	err = main.Execute(&out, x.root)
	if err != nil {
		return Result{}, x.convert(err)
	}

	return Result{Text: out.Bytes(), Read: x.record()}, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)

	return hex.EncodeToString(h[:])
}

// origin is where a file was read: the repository, or a remote repository at
// a commit. A path a file includes is read from the same origin.
type origin struct {
	url    string
	commit string
}

type site struct {
	file string
	line int
}

// frame is an include being expanded: a file, or a define included by name.
type frame struct {
	key    string
	isFile bool
	call   site
	arg    string
}

type loaded struct {
	body   *template.Template
	origin origin
}

type expander struct {
	ctx      context.Context //nolint:containedctx // one expansion, one context: include reads through it
	resolver Resolver
	set      *template.Template
	root     map[string]any
	rootPtr  uintptr
	listed   map[string]bool
	never    map[string]bool
	files    map[string]*loaded
	defines  map[string]site
	refs     []string
	stack    []frame
	reads    map[string]Read
	source   File
	includes []File
}

func newExpander(ctx context.Context, name string, own ownInputs, in Inputs, r Resolver) (*expander, error) {
	values, err := normalizeMap(in.Values)
	if err != nil {
		return nil, &Error{File: name, Msg: "values: " + err.Error()}
	}

	defines, err := normalizeMap(in.Defines)
	if err == nil {
		_, err = definesOf(defines, 0)
	}

	if err != nil {
		return nil, &Error{File: name, Msg: "defines: " + strings.TrimPrefix(err.Error(), "defines: ")}
	}

	x := &expander{
		ctx:      ctx,
		resolver: r,
		listed:   map[string]bool{},
		never:    map[string]bool{},
		files:    map[string]*loaded{},
		defines:  map[string]site{},
		reads:    map[string]Read{},
		stack:    []frame{{key: name, isFile: true}},
	}

	for _, n := range in.Never {
		x.never[n] = true
	}

	env := map[string]any{}

	for _, n := range own.env {
		x.listed[n] = true

		if v, ok := in.Env[n]; ok && !x.never[n] {
			env[n] = v
		}
	}

	x.root = map[string]any{
		"Values":  Merge(own.values, values),
		"Defines": Merge(own.defines, defines),
		envKey:    env,
	}
	x.rootPtr = uintptr(reflect.ValueOf(x.root).UnsafePointer())
	x.set = template.New("").Funcs(x.funcs())

	return x, nil
}

func (x *expander) isRoot(v any) bool {
	m, ok := v.(map[string]any)

	return ok && m != nil && uintptr(reflect.ValueOf(m).UnsafePointer()) == x.rootPtr
}
