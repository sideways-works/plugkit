package tpl

import (
	"strconv"
	"strings"
	"text/template/parse"
)

// The rewriter routes every read of the template's input through the
// expander's own functions, which record what was read, apply the env:
// rules and word the errors. Only the parse tree changes, so every position
// still points into the file the user wrote.
//
//	.Values.a.b       -> __field . "" "Values.a.b"
//	$.Env.X           -> __field $ "$" "Env.X"
//	.                 -> __dot .
//	index .Values "k" -> __index . "" "Values" "k"
//	hasKey .Values k  -> __hasKey . "" "Values" k
//	include N D       -> __include "file" "line" N D
//
// A whole input passed as include's or template's data isn't a read: the
// included template records what it reads itself.
const (
	fnInclude = "include"
	fnHasKey  = "hasKey"
)

type rewriter struct {
	file string
	text string
	err  *Error
}

func (w *rewriter) tree(t *parse.Tree) *Error {
	w.list(t.Root)

	return w.err
}

func (w *rewriter) list(l *parse.ListNode) {
	if l == nil {
		return
	}

	for _, n := range l.Nodes {
		w.node(n)
	}
}

func (w *rewriter) node(n parse.Node) {
	switch n := n.(type) {
	case *parse.ActionNode:
		w.pipe(n.Pipe, false)
	case *parse.IfNode:
		w.branch(&n.BranchNode)
	case *parse.RangeNode:
		w.branch(&n.BranchNode)
	case *parse.WithNode:
		w.branch(&n.BranchNode)
	case *parse.TemplateNode:
		w.pipe(n.Pipe, true)
	case *parse.ListNode:
		w.list(n)
	}
}

func (w *rewriter) branch(b *parse.BranchNode) {
	w.pipe(b.Pipe, false)
	w.list(b.List)
	w.list(b.ElseList)
}

// pipe rewrites a pipeline. When data is set the pipeline is the data of a
// template action, and a whole input passed on is left alone.
func (w *rewriter) pipe(p *parse.PipeNode, data bool) {
	if p == nil {
		return
	}

	if data && len(p.Cmds) == 1 && len(p.Cmds[0].Args) == 1 && isWhole(p.Cmds[0].Args[0]) {
		return
	}

	for i, c := range p.Cmds {
		w.command(c, i > 0)
	}
}

// isWhole reports whether n is the whole data, . or $.
func isWhole(n parse.Node) bool {
	switch n := n.(type) {
	case *parse.DotNode:
		return true
	case *parse.VariableNode:
		return len(n.Ident) == 1 && n.Ident[0] == "$"
	default:
		return false
	}
}

// command rewrites one command; piped is set when it receives the previous
// command's value as its last argument.
func (w *rewriter) command(c *parse.CommandNode, piped bool) {
	switch first := c.Args[0].(type) {
	case *parse.IdentifierNode:
		switch first.Ident {
		case fnInclude:
			w.include(c, first, piped)
		case "index", fnHasKey:
			w.lookup(c, first, piped)
		case "call":
			// call never finds a function, and its error shows its first
			// argument as written.
			if len(c.Args) > 2 {
				w.args(c.Args[2:])
			}
		default:
			w.args(c.Args[1:])
		}
	case *parse.PipeNode:
		w.pipe(first, false)
		w.args(c.Args[1:])
	default:
		if len(c.Args) == 1 && !piped {
			if call := w.access(first); call != nil {
				c.Args = call
			}
		}
	}
}

func (w *rewriter) args(args []parse.Node) {
	for i, a := range args {
		args[i] = w.arg(a)
	}
}

// arg returns the node that reads a in argument position.
func (w *rewriter) arg(a parse.Node) parse.Node {
	switch a := a.(type) {
	case *parse.PipeNode:
		w.pipe(a, false)

		return a
	case *parse.IdentifierNode:
		if a.Ident == fnInclude {
			w.fail(a.Pos, "wrong number of args for include: want 2 got 0")
		}

		return a
	default:
		call := w.access(a)
		if call == nil {
			return a
		}

		return &parse.PipeNode{
			NodeType: parse.NodePipe,
			Pos:      a.Position(),
			Cmds:     []*parse.CommandNode{{NodeType: parse.NodeCommand, Pos: a.Position(), Args: call}},
		}
	}
}

// access returns the call that reads the field, variable, dot or chain n, or
// nil when n reads no input.
func (w *rewriter) access(n parse.Node) []parse.Node {
	recv, base, path, ok := w.receiver(n)
	if !ok {
		return nil
	}

	if path == "" {
		if !isWhole(recv) {
			return nil
		}

		return []parse.Node{ident("__dot", n.Position()), recv}
	}

	return []parse.Node{ident("__field", n.Position()), recv, str(base, n.Position()), str(path, n.Position())}
}

// receiver splits a field, variable, dot or chain into the value it starts
// from, how that value is written in messages, and the dotted path below it.
func (w *rewriter) receiver(n parse.Node) (recv parse.Node, base, path string, ok bool) {
	switch n := n.(type) {
	case *parse.DotNode:
		return n, "", "", true
	case *parse.FieldNode:
		return &parse.DotNode{NodeType: parse.NodeDot, Pos: n.Pos}, "", strings.Join(n.Ident, "."), true
	case *parse.VariableNode:
		v := &parse.VariableNode{NodeType: parse.NodeVariable, Pos: n.Pos, Ident: n.Ident[:1]}

		return v, n.Ident[0], strings.Join(n.Ident[1:], "."), true
	case *parse.ChainNode:
		inner := n.Node.String()
		if _, isPipe := n.Node.(*parse.PipeNode); isPipe {
			inner = "(" + inner + ")"
		}

		return w.arg(n.Node), inner, strings.Join(n.Field, "."), true
	default:
		return nil, "", "", false
	}
}

func (w *rewriter) include(c *parse.CommandNode, id *parse.IdentifierNode, piped bool) {
	got := len(c.Args) - 1
	if piped {
		got++
	}

	if got != 2 {
		w.fail(id.Pos, "wrong number of args for include: want 2 got "+strconv.Itoa(got))

		return
	}

	args := c.Args[1:]
	for i, a := range args {
		if i == 1 && isWhole(a) {
			continue
		}

		args[i] = w.arg(a)
	}

	line := strconv.Itoa(w.line(id.Pos))
	c.Args = append([]parse.Node{ident("__include", id.Pos), str(w.file, id.Pos), str(line, id.Pos)}, args...)
}

// lookup rewrites index and hasKey on an input, so that only the key looked
// up is recorded, not the whole map.
func (w *rewriter) lookup(c *parse.CommandNode, id *parse.IdentifierNode, piped bool) {
	got := len(c.Args) - 1
	if piped {
		got++
	}

	if id.Ident == fnHasKey && got != 2 {
		w.fail(id.Pos, "wrong number of args for hasKey: want 2 got "+strconv.Itoa(got))

		return
	}

	if len(c.Args) < 2 {
		w.args(c.Args[1:])

		return
	}

	recv, base, path, ok := w.receiver(c.Args[1])
	if !ok {
		w.args(c.Args[1:])

		return
	}

	rest := c.Args[2:]
	w.args(rest)

	call := make([]parse.Node, 0, 4+len(rest))
	call = append(call, ident("__"+id.Ident, id.Pos), recv, str(base, id.Pos), str(path, id.Pos))
	call = append(call, rest...)
	c.Args = call
}

func (w *rewriter) line(pos parse.Pos) int {
	return 1 + strings.Count(w.text[:pos], "\n")
}

func (w *rewriter) fail(pos parse.Pos, msg string) {
	if w.err == nil {
		w.err = errorAt(w.file, w.line(pos), "%s", msg)
	}
}

func ident(name string, pos parse.Pos) *parse.IdentifierNode {
	id := parse.NewIdentifier(name)
	id.Pos = pos

	return id
}

func str(s string, pos parse.Pos) *parse.StringNode {
	return &parse.StringNode{NodeType: parse.NodeString, Pos: pos, Quoted: strconv.Quote(s), Text: s}
}
