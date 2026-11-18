package tpl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"
)

// Record is what an expansion read: its source, every file it included and
// every value, define and environment variable it read. Two expansions with
// equal records expand to the same text.
type Record struct {
	// Source is the expanded file.
	Source File
	// Includes are the included files, sorted by reference.
	Includes []File
	// Values, Defines and Env are the inputs read, sorted by key.
	Values  []Read
	Defines []Read
	Env     []Read
}

// File is a file by reference and content: a repository path or
// <git URL>//<path>@<commit>, and the SHA-256 of its bytes in hex.
type File struct {
	Ref    string
	SHA256 string
}

// Read is one input read. Key is the path below .Values, .Defines or .Env;
// an empty key is the whole input. Missing records a key looked up and not
// found, so adding it later changes the record.
type Read struct {
	Key     []string
	Value   any
	Missing bool
}

const envKey = "Env"

var sections = []string{"Values", "Defines", envKey}

func readKey(path []string) string { return strings.Join(path, "\x00") }

// note records that the expansion read path, a section name followed by keys.
// An empty path is the whole input.
func (x *expander) note(path []string, v any, missing bool) {
	if len(path) == 0 {
		for _, s := range sections {
			x.note([]string{s}, x.root[s], false)
		}

		return
	}

	x.reads[readKey(path)] = Read{Key: slices.Clone(path), Value: clone(v), Missing: missing}
}

func (x *expander) record() Record {
	r := Record{Source: x.source, Includes: slices.Clone(x.includes)}
	slices.SortFunc(r.Includes, func(a, b File) int { return strings.Compare(a.Ref, b.Ref) })

	bySection := map[string][]Read{}

	for _, rd := range x.reads {
		bySection[rd.Key[0]] = append(bySection[rd.Key[0]], Read{Key: rd.Key[1:], Value: rd.Value, Missing: rd.Missing})
	}

	r.Values = collapse(bySection["Values"])
	r.Defines = collapse(bySection["Defines"])
	r.Env = collapse(bySection[envKey])

	return r
}

// collapse sorts reads by key and drops those inside a key already read
// whole.
func collapse(reads []Read) []Read {
	slices.SortFunc(reads, func(a, b Read) int { return slices.Compare(a.Key, b.Key) })

	var (
		out   []Read
		cover []string
		have  bool
	)

	for _, r := range reads {
		if have && len(r.Key) >= len(cover) && slices.Equal(r.Key[:len(cover)], cover) {
			continue
		}

		out = append(out, r)

		if !r.Missing {
			cover, have = r.Key, true
		}
	}

	return out
}

// Digest is a stable digest of the record: a change of the source, of an
// include or of any input read changes it.
func (r Record) Digest() string {
	h := sha256.New()
	d := digester{h: h}

	d.str("plugkit/tpl record v1")
	d.file(r.Source)
	d.num(len(r.Includes))

	for _, f := range r.Includes {
		d.file(f)
	}

	for i, reads := range [][]Read{r.Values, r.Defines, r.Env} {
		d.str(sections[i])
		d.num(len(reads))

		for _, rd := range reads {
			d.num(len(rd.Key))

			for _, k := range rd.Key {
				d.str(k)
			}

			if rd.Missing {
				d.str("missing")

				continue
			}

			d.str("value")
			d.value(rd.Value)
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

type digester struct{ h hash.Hash }

func (d digester) raw(s string) { _, _ = d.h.Write([]byte(s)) }

func (d digester) str(s string) { d.raw(strconv.Itoa(len(s)) + ":" + s) }

func (d digester) num(n int) { d.raw("#" + strconv.Itoa(n) + ";") }

func (d digester) file(f File) {
	d.str(f.Ref)
	d.str(f.SHA256)
}

func (d digester) value(v any) {
	switch v := v.(type) {
	case nil:
		d.raw("n")
	case bool:
		d.raw("b" + strconv.FormatBool(v))
	case string:
		d.raw("s")
		d.str(v)
	case []any:
		d.raw("l")
		d.num(len(v))

		for _, e := range v {
			d.value(e)
		}
	case map[string]any:
		d.raw("m")
		d.num(len(v))

		for _, k := range sortedKeys(v) {
			d.str(k)
			d.value(v[k])
		}
	default:
		d.raw(fmt.Sprintf("%T", v))
		d.str(fmt.Sprint(v))
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
