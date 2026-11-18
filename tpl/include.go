package tpl

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
)

// maxDepth is how deep includes of files may nest.
const maxDepth = 4

// bodyName is the name a file's top level is registered under, apart from
// the names of defines.
func bodyName(ref string) string { return "\x00" + ref }

// parse parses a file, rewrites its reads, and registers its defines,
// refusing a name already defined elsewhere.
func (x *expander) parse(ref, text string, from origin) (*template.Template, error) {
	x.refs = append(x.refs, ref)
	slices.SortFunc(x.refs, func(a, b string) int { return len(b) - len(a) })

	sites := scanDefines(text)
	trees, perr := parse.Parse(ref, text, "", "", parseNames)

	err := x.checkDefines(ref, sites)
	if err != nil {
		return nil, err
	}

	if perr != nil {
		return nil, x.convert(perr)
	}

	body := trees[ref]
	delete(trees, ref)

	w := &rewriter{file: ref, text: text}
	for _, t := range append([]*parse.Tree{body}, mapValues(trees)...) {
		if e := w.tree(t); e != nil {
			return nil, e
		}
	}

	bt, err := x.add(bodyName(ref), body)
	if err != nil {
		return nil, err
	}

	lines := map[string]int{}
	for _, s := range sites {
		lines[s.name] = s.line
	}

	for _, t := range mapValues(trees) {
		if _, err := x.add(t.Name, t); err != nil {
			return nil, err
		}

		x.defines[t.Name] = site{file: ref, line: lines[t.Name]}
	}

	x.files[ref] = &loaded{body: bt, origin: from}

	return bt, nil
}

func mapValues(m map[string]*parse.Tree) []*parse.Tree {
	out := make([]*parse.Tree, 0, len(m))
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		out = append(out, m[k])
	}

	return out
}

func (x *expander) add(name string, t *parse.Tree) (*template.Template, error) {
	nt, err := x.set.AddParseTree(name, t)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}

	nt.Option("missingkey=error")

	return nt, nil
}

// checkDefines refuses a name defined twice, in this file or before.
func (x *expander) checkDefines(ref string, sites []defineSite) error {
	here := map[string]int{}

	for _, s := range sites {
		if s.name == ref {
			return errorAt(ref, s.line, "define %q has the name of the file", s.name)
		}

		if line, ok := here[s.name]; ok {
			return errorAt(ref, s.line, "define %q is also defined at %s:%d", s.name, ref, line)
		}

		if prev, ok := x.defines[s.name]; ok {
			return errorAt(ref, s.line, "define %q is also defined at %s:%d", s.name, prev.file, prev.line)
		}

		here[s.name] = s.line
	}

	return nil
}

type defineSite struct {
	name string
	line int
}

// scanDefines finds the define and block actions of a template and the
// lines they start on. It reads actions as Go's lexer does: strings and
// comments may hold "}}".
func scanDefines(text string) []defineSite {
	var out []defineSite

	line := 1

	for i := 0; ; {
		j := strings.Index(text[i:], "{{")
		if j < 0 {
			return out
		}

		line += strings.Count(text[i:i+j], "\n")
		start := i + j + 2
		end := actionEnd(text, start)

		if name, ok := defineName(text[start:end]); ok {
			out = append(out, defineSite{name: name, line: line})
		}

		line += strings.Count(text[i+j:end], "\n")
		i = end
	}
}

func actionEnd(text string, i int) int {
	rest := strings.TrimLeft(strings.TrimPrefix(text[i:], "-"), " \t\r\n")
	if strings.HasPrefix(rest, "/*") {
		at := strings.Index(rest, "*/")
		if at < 0 {
			return len(text)
		}

		after := len(text) - len(rest) + at + 2
		if e := strings.Index(text[after:], "}}"); e >= 0 {
			return after + e + 2
		}

		return len(text)
	}

	for k := i; k < len(text); k++ {
		switch text[k] {
		case '"', '\'', '`':
			k = quoteEnd(text, k)
		case '}':
			if strings.HasPrefix(text[k:], "}}") {
				return k + 2
			}
		}
	}

	return len(text)
}

func quoteEnd(text string, k int) int {
	q := text[k]

	for k++; k < len(text); k++ {
		switch {
		case text[k] == '\\' && q != '`':
			k++
		case text[k] == q:
			return k
		}
	}

	return k
}

var defineAction = regexp.MustCompile(`^-?\s*(define|block)\s`)

func defineName(action string) (string, bool) {
	loc := defineAction.FindStringIndex(action)
	if loc == nil {
		return "", false
	}

	rest := strings.TrimLeft(action[loc[1]:], " \t\r\n")

	q, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return "", false
	}

	name, err := strconv.Unquote(q)

	return name, err == nil
}

// include expands a define, a file of the repository or a remote file with
// data as its dot. file and line are where the call is.
func (x *expander) include(file, line, name string, data any) (string, error) {
	n, err := strconv.Atoi(line)
	if err != nil {
		return "", fail("include: line %q", line)
	}

	call := site{file: file, line: n}

	err = x.ctx.Err()
	if err != nil {
		return "", errorAt(file, n, "include %q: %v", name, err)
	}

	if _, ok := x.defines[name]; ok {
		return x.run(frame{key: name, call: call, arg: name}, x.set.Lookup(name), data)
	}

	ref, from, err := x.resolve(call, name)
	if err != nil {
		return "", err
	}

	f := frame{key: ref, isFile: true, call: call, arg: name}

	err = x.enter(f)
	if err != nil {
		return "", err
	}

	body, err := x.load(call, name, ref, from)
	if err != nil {
		return "", err
	}

	return x.run(f, body, data)
}

// enter refuses an include that closes a cycle or nests too deep.
func (x *expander) enter(f frame) error {
	for i, s := range x.stack {
		if s.key != f.key || s.isFile != f.isFile {
			continue
		}

		chain := make([]string, 0, len(x.stack)-i+1)
		for _, t := range x.stack[i:] {
			chain = append(chain, t.key)
		}

		chain = append(chain, f.key)

		first := f
		if i+1 < len(x.stack) {
			first = x.stack[i+1]
		}

		return errorAt(first.call.file, first.call.line, "include %q: includes form a cycle: %s", first.arg, strings.Join(chain, " -> "))
	}

	if !f.isFile {
		return nil
	}

	depth := 0

	for _, s := range x.stack[1:] {
		if s.isFile {
			depth++
		}
	}

	if depth >= maxDepth {
		return errorAt(f.call.file, f.call.line, "includes nest deeper than four levels")
	}

	return nil
}

func (x *expander) run(f frame, t *template.Template, data any) (string, error) {
	if !f.isFile {
		err := x.enter(f)
		if err != nil {
			return "", err
		}
	}

	x.stack = append(x.stack, f)
	defer func() { x.stack = x.stack[:len(x.stack)-1] }()

	var out bytes.Buffer

	err := t.Execute(&out, data)
	if err != nil {
		return "", x.convert(err)
	}

	return out.String(), nil
}

var fullHash = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

var hexOnly = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// resolve turns the name an include gives into the reference of the file it
// reads, and where that file is read from.
func (x *expander) resolve(call site, name string) (string, origin, error) {
	bad := func(format string, args ...any) error {
		return errorAt(call.file, call.line, "include %q: "+format, append([]any{name}, args...)...)
	}

	if !strings.Contains(name, "//") {
		if !validPath(name) {
			return "", origin{}, bad("not a path in the repository")
		}

		from := x.files[call.file].origin
		if from.url == "" {
			return name, from, nil
		}

		return remoteRef(from.url, name, from.commit), from, nil
	}

	start := 0
	if i := strings.Index(name, "://"); i >= 0 {
		start = i + 3
	}

	sep := strings.Index(name[start:], "//")
	if sep < 0 {
		return "", origin{}, bad("not a path in the repository")
	}

	url, rest := name[:start+sep], name[start+sep+2:]

	at := strings.LastIndex(rest, "@")
	if at < 0 || at == len(rest)-1 {
		return "", origin{}, bad("a remote include needs @<commit>")
	}

	p, commit := rest[:at], rest[at+1:]

	switch {
	case fullHash.MatchString(commit):
	case hexOnly.MatchString(commit):
		return "", origin{}, bad("%q is not a full commit hash", commit)
	default:
		return "", origin{}, bad("%q is not a commit", commit)
	}

	if url == "" || !validPath(p) {
		return "", origin{}, bad("expected <git URL>//<path>@<commit>")
	}

	return remoteRef(url, p, commit), origin{url: url, commit: commit}, nil
}

func remoteRef(url, p, commit string) string { return url + "//" + p + "@" + commit }

func validPath(p string) bool {
	return p != "" && fs.ValidPath(p) && p != "." && path.Clean(p) == p
}

// load reads, parses and registers a file the first time it is included.
func (x *expander) load(call site, name, ref string, from origin) (*template.Template, error) {
	if f, ok := x.files[ref]; ok {
		return f.body, nil
	}

	var (
		data []byte
		err  error
	)

	p := ref
	if from.url != "" {
		p = strings.TrimSuffix(strings.TrimPrefix(ref, from.url+"//"), "@"+from.commit)
		data, err = x.resolver.Remote(x.ctx, from.url, p, from.commit)
	} else {
		data, err = x.resolver.Local(x.ctx, p)
	}

	if err != nil {
		msg := readError(err, from.commit)
		if errors.Is(err, fs.ErrNotExist) && !strings.ContainsAny(name, "/.") {
			msg = fmt.Sprintf("no define named %q and %s", name, msg)
		}

		return nil, errorAt(call.file, call.line, "include %q: %s", name, msg)
	}

	x.includes = append(x.includes, File{Ref: ref, SHA256: sum(data)})

	return x.parse(ref, string(data), from)
}

func readError(err error, commit string) string {
	if nf, ok := errors.AsType[*NotFoundError](err); ok {
		if nf.Commit == "" {
			nf = &NotFoundError{Commit: commit}
		}

		return nf.Error()
	}

	if errors.Is(err, fs.ErrNotExist) {
		return (&NotFoundError{Commit: commit}).Error()
	}

	return err.Error()
}

// convert rewrites an error of text/template into an *Error at the line of
// the file the user wrote.
func (x *expander) convert(err error) error {
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}

	msg := err.Error()

	file, line, rest, ok := x.locate(msg)
	if !ok {
		return &Error{Msg: strings.TrimPrefix(msg, "template: ")}
	}

	if fe, ok := errors.AsType[*funcError](err); ok {
		rest = fe.msg
	} else {
		rest = stripExecuting(rest)
	}

	return &Error{File: file, Line: line, Msg: rest}
}

// locate splits "template: FILE:LINE[:COL]: rest", trying the names of the
// files read, since a remote reference holds colons itself.
func (x *expander) locate(msg string) (string, int, string, bool) {
	s, ok := strings.CutPrefix(msg, "template: ")
	if !ok {
		return "", 0, "", false
	}

	for _, ref := range x.refs {
		after, ok := strings.CutPrefix(s, ref+":")
		if !ok {
			continue
		}

		num, rest, ok := strings.Cut(after, ":")
		if !ok {
			continue
		}

		line, err := strconv.Atoi(num)
		if err != nil {
			continue
		}

		if col, more, ok := strings.Cut(rest, ":"); ok {
			if _, err := strconv.Atoi(col); err == nil {
				rest = more
			}
		}

		return ref, line, strings.TrimPrefix(rest, " "), true
	}

	return "", 0, "", false
}

// stripExecuting drops `executing "name" at <context>: ` from an exec error.
func stripExecuting(s string) string {
	after, ok := strings.CutPrefix(s, "executing ")
	if !ok {
		return s
	}

	q, err := strconv.QuotedPrefix(after)
	if err != nil {
		return s
	}

	after, ok = strings.CutPrefix(after[len(q):], " at <")
	if !ok {
		return s
	}

	_, msg, ok := strings.Cut(after, ">: ")
	if !ok {
		return s
	}

	return msg
}
