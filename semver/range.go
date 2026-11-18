package semver

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrRange = errors.New("range")

var errSyntax = errors.New("not supported")

// Range is a set of alternatives, each a list of comparators that must all hold.
type Range struct {
	text string
	alts [][]comparator
}

type op int

const (
	opEQ op = iota
	opLT
	opLE
	opGT
	opGE
	// opOut holds outside [v, hi): a != with a partial version.
	opOut
	// opNone holds for nothing: < or > of *.
	opNone
)

type comparator struct {
	op    op
	v, hi Version
	// names is set on a comparator written with a pre-release, which lets pre-releases of its
	// major.minor.patch match.
	names bool
}

func ParseRange(s string) (Range, error) {
	r := Range{text: s}

	for alt := range strings.SplitSeq(s, "||") {
		tokens, err := tokenize(alt)
		if err != nil {
			return Range{}, fmt.Errorf("%w %q: %w", ErrRange, s, err)
		}

		if len(tokens) == 0 {
			return Range{}, fmt.Errorf("%w %q: an empty alternative", ErrRange, s)
		}

		var cs []comparator

		for _, t := range tokens {
			c, err := parseComparator(t)
			if err != nil {
				return Range{}, fmt.Errorf("%w %q: %w", ErrRange, s, err)
			}

			cs = append(cs, c...)
		}

		r.alts = append(r.alts, cs)
	}

	return r, nil
}

var operators = []string{">=", "<=", "!=", ">", "<", "="}

// tokenize splits an alternative into comparators, an operator followed by space joined to the
// version after it.
func tokenize(alt string) ([]string, error) {
	fields := strings.Fields(alt)

	var out []string

	for i := 0; i < len(fields); i++ {
		f := fields[i]

		switch {
		case f == "-":
			return nil, fmt.Errorf("hyphen ranges are %w: write >=A <=B", errSyntax)
		case strings.HasPrefix(f, "^") || strings.HasPrefix(f, "~"):
			return nil, fmt.Errorf("%q: caret and tilde ranges are %w: write >= and < bounds", f, errSyntax)
		}

		for _, o := range operators {
			if f == o {
				if i+1 == len(fields) {
					return nil, fmt.Errorf("%w %q: no version after it", ErrVersion, f)
				}

				i++
				f += fields[i]

				break
			}
		}

		out = append(out, f)
	}

	return out, nil
}

// partial is a version that may leave out its minor or patch, or write them as x, X or *.
type partial struct {
	v     Version
	parts int
}

func parsePartial(s string) (partial, error) {
	if s == "*" || s == "x" || s == "X" {
		return partial{}, nil
	}

	parts, pre, build, err := split(s)
	if err != nil {
		return partial{}, err
	}

	if len(parts) > 3 {
		return partial{}, fmt.Errorf("%w %q: more than major.minor.patch", ErrVersion, s)
	}

	var p partial

	for i, part := range parts {
		if part == "*" || part == "x" || part == "X" {
			if slices.ContainsFunc(parts[i:], func(rest string) bool { return rest != "*" && rest != "x" && rest != "X" }) {
				return partial{}, fmt.Errorf("%w %q: a number after a wildcard", ErrVersion, s)
			}

			break
		}

		n, err := number(part)
		if err != nil {
			return partial{}, fmt.Errorf("%w %q: %w", ErrVersion, s, err)
		}

		*[]*uint64{&p.v.Major, &p.v.Minor, &p.v.Patch}[i] = n
		p.parts++
	}

	if (len(pre) > 0 || build != "") && p.parts < 3 {
		return partial{}, fmt.Errorf("%w %q: a pre-release or build needs major.minor.patch", ErrVersion, s)
	}

	p.v.Pre, p.v.Build = pre, build

	return p, nil
}

// next is the first version past every one the partial covers, below its pre-releases.
func (p partial) next() Version {
	switch p.parts {
	case 1:
		return Version{Major: p.v.Major + 1, Pre: []string{"0"}}
	case 2:
		return Version{Major: p.v.Major, Minor: p.v.Minor + 1, Pre: []string{"0"}}
	}

	return Version{}
}

func (p partial) low() Version { return Version{Major: p.v.Major, Minor: p.v.Minor, Patch: p.v.Patch} }

func parseComparator(t string) ([]comparator, error) {
	o := "="

	for _, cand := range operators {
		if rest, ok := strings.CutPrefix(t, cand); ok {
			o, t = cand, rest

			break
		}
	}

	p, err := parsePartial(t)

	switch {
	case err != nil:
		return nil, err
	case p.parts == 3:
		return exact(o, p.v), nil
	case p.parts == 0:
		return wildcard(o)
	}

	return bounds(o, p), nil
}

func exact(o string, v Version) []comparator {
	if o == "!=" {
		return []comparator{{op: opOut, v: v, hi: v}}
	}

	ops := map[string]op{"=": opEQ, "<": opLT, "<=": opLE, ">": opGT, ">=": opGE}

	return []comparator{{op: ops[o], v: v, names: v.Prerelease()}}
}

func wildcard(o string) ([]comparator, error) {
	switch o {
	case "=", ">=", "<=":
		return []comparator{{op: opGE}}, nil
	case "<", ">":
		return []comparator{{op: opNone}}, nil
	}

	return nil, fmt.Errorf("!=* is %w: it excludes every version", errSyntax)
}

// bounds are the comparators of a version that leaves out its minor or patch, as npm reads them.
func bounds(o string, p partial) []comparator {
	switch o {
	case "=":
		return []comparator{{op: opGE, v: p.low()}, {op: opLT, v: p.next()}}
	case ">=":
		return []comparator{{op: opGE, v: p.low()}}
	case "<=":
		return []comparator{{op: opLT, v: p.next()}}
	case ">":
		return []comparator{{op: opGE, v: withoutPre(p.next())}}
	case "<":
		return []comparator{{op: opLT, v: Version{Major: p.v.Major, Minor: p.v.Minor, Pre: []string{"0"}}}}
	}

	return []comparator{{op: opOut, v: p.low(), hi: p.next()}}
}

func withoutPre(v Version) Version {
	v.Pre = nil

	return v
}

func (c comparator) holds(v Version) bool {
	switch c.op {
	case opEQ:
		return v.Compare(c.v) == 0
	case opLT:
		return v.Compare(c.v) < 0
	case opLE:
		return v.Compare(c.v) <= 0
	case opGT:
		return v.Compare(c.v) > 0
	case opGE:
		return v.Compare(c.v) >= 0
	case opOut:
		if c.hi.Compare(c.v) == 0 {
			return v.Compare(c.v) != 0
		}

		return v.Compare(c.v) < 0 || v.Compare(c.hi) >= 0
	case opNone:
	}

	return false
}

// Contains says whether v satisfies the range.
func (r Range) Contains(v Version) bool {
	for _, alt := range r.alts {
		if holdsAll(alt, v) {
			return true
		}
	}

	return false
}

func holdsAll(alt []comparator, v Version) bool {
	named := !v.Prerelease()

	for _, c := range alt {
		if !c.holds(v) {
			return false
		}

		if c.names && c.v.sameRelease(v) {
			named = true
		}
	}

	return named
}

func (r Range) String() string { return r.text }
