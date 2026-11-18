package tpl

import (
	"fmt"
	"maps"
	"reflect"
	"testing"
)

func TestEnv(t *testing.T) {
	in := Inputs{
		Env:   map[string]string{"A": "a", "B": "b", "TOKEN": "secret", "HOME": "/root"},
		Never: []string{"TOKEN", "UNLISTED_SECRET"},
	}
	head := "env: [A, B, TOKEN, UNSET]\nout: "

	tests := []struct {
		name    string
		src     string
		want    string
		wantErr string
	}{
		{name: "a listed variable", src: "{{ .Env.A }}{{ $.Env.B }}", want: "ab"},
		{name: "range sees only readable variables", src: "{{ range $k, $v := .Env }}{{ $k }}={{ $v }} {{ end }}", want: "A=a B=b "},
		{name: "toYaml of the environment", src: "{{ toYaml .Env }}", want: "A: a\nB: b"},
		{name: "index of a listed variable", src: `{{ index .Env "A" }}`, want: "a"},
		{name: "index of a listed, unset variable", src: `{{ index .Env "UNSET" | default "none" }}`, want: "none"},
		{name: "hasKey of a listed variable", src: `{{ hasKey .Env "A" }} {{ hasKey .Env "UNSET" }}`, want: "true false"},
		{name: "a listed, unset variable", src: "{{ .Env.UNSET }}", wantErr: "ci.yaml:2: .Env.UNSET: no such key"},
		{name: "an unlisted variable", src: "{{ .Env.HOME }}", wantErr: "ci.yaml:2: .Env.HOME is not allowed: add HOME to env:"},
		{name: "an unlisted variable through $", src: "{{ $.Env.HOME }}", wantErr: "ci.yaml:2: $.Env.HOME is not allowed: add HOME to env:"},
		{name: "an unlisted variable through index", src: `{{ index .Env "HOME" }}`, wantErr: "ci.yaml:2: .Env.HOME is not allowed: add HOME to env:"},
		{name: "an unlisted variable through hasKey", src: `{{ hasKey .Env "HOME" }}`, wantErr: "ci.yaml:2: .Env.HOME is not allowed: add HOME to env:"},
		{name: "an unlisted variable through with", src: "{{ with .Env }}{{ .HOME }}{{ end }}", wantErr: "ci.yaml:2: .HOME: no such key"},
		{name: "a listed credential", src: "{{ .Env.TOKEN }}", wantErr: "ci.yaml:2: .Env.TOKEN is a credential and is never readable"},
		{name: "a listed credential through index", src: `{{ index .Env "TOKEN" }}`, wantErr: "ci.yaml:2: .Env.TOKEN is a credential and is never readable"},
		{name: "an unlisted credential", src: "{{ .Env.UNLISTED_SECRET }}", wantErr: "ci.yaml:2: .Env.UNLISTED_SECRET is a credential and is never readable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Expand(t.Context(), "ci.yaml", []byte(head+tt.src), in, MapResolver{})
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("got error %v, want %s", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got := string(res.Text[len(head):]); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnvIsClosedWithoutAList(t *testing.T) {
	_, err := Expand(t.Context(), "ci.yaml", []byte("{{ .Env.HOME }}"), Inputs{Env: map[string]string{"HOME": "/root"}}, MapResolver{})
	if err == nil || err.Error() != "ci.yaml:1: .Env.HOME is not allowed: add HOME to env:" {
		t.Errorf("got %v", err)
	}
}

func TestRecord(t *testing.T) {
	src := "env: [A, B]\n" +
		"out: {{ .Values.a.b }} {{ index .Values \"missing\" }} {{ hasKey .Values \"region\" }} {{ .Values.list }} {{ index .Values.list 0 }}\n" +
		"{{ .Defines.X }} {{ .Env.A }} {{ .Values.a.b }} {{ index .Values \"m\" \"k\" }}"
	in := Inputs{
		Values: map[string]any{
			"a":      map[string]any{"b": 1, "c": 2},
			"list":   []any{1, 2},
			"m":      map[string]any{"k": "v", "other": 1},
			"unread": "x",
		},
		Defines: map[string]any{"X": true, "Y": "unread"},
		Env:     map[string]string{"A": "a", "B": "unread"},
	}

	res := expand(t, src, in, nil)

	want := Record{
		Source: File{Ref: "ci.yaml", SHA256: sum([]byte(src))},
		Values: []Read{
			{Key: []string{"a", "b"}, Value: 1},
			{Key: []string{"list"}, Value: []any{1, 2}},
			{Key: []string{"m", "k"}, Value: "v"},
			{Key: []string{"missing"}, Missing: true},
			{Key: []string{"region"}, Missing: true},
		},
		Defines: []Read{{Key: []string{"X"}, Value: true}},
		Env:     []Read{{Key: []string{"A"}, Value: "a"}},
	}

	if !reflect.DeepEqual(res.Read, want) {
		t.Errorf("got\n%#v\nwant\n%#v", res.Read, want)
	}
}

func TestRecordOfWholeInputs(t *testing.T) {
	in := Inputs{Values: map[string]any{"a": map[string]any{"b": 1}}, Defines: map[string]any{"X": "x"}}

	tests := []struct {
		name    string
		src     string
		values  []Read
		defines []Read
	}{
		{
			name:   "a map read whole covers its keys",
			src:    "{{ .Values.a.b }}{{ toJson .Values.a }}",
			values: []Read{{Key: []string{"a"}, Value: map[string]any{"b": 1}}},
		},
		{
			name:   "with reads its value whole",
			src:    "{{ with .Values }}{{ .a.b }}{{ end }}",
			values: []Read{{Key: []string{}, Value: map[string]any{"a": map[string]any{"b": 1}}}},
		},
		{
			name:    "the dot passed to a function reads everything",
			src:     "{{ len . }}",
			values:  []Read{{Key: []string{}, Value: map[string]any{"a": map[string]any{"b": 1}}}},
			defines: []Read{{Key: []string{}, Value: map[string]any{"X": "x"}}},
		},
		{
			name:   "the dot passed to include reads only what the include reads",
			src:    `{{ define "x" }}{{ .Values.a.b }}{{ end }}{{ include "x" . }}{{ template "x" $ }}`,
			values: []Read{{Key: []string{"a", "b"}, Value: 1}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := expand(t, tt.src, in, nil)
			if !reflect.DeepEqual(res.Read.Values, tt.values) || !reflect.DeepEqual(res.Read.Defines, tt.defines) {
				t.Errorf("got values %#v defines %#v", res.Read.Values, res.Read.Defines)
			}
		})
	}
}

func TestDigest(t *testing.T) {
	src := "env: [A, B]\nout: {{ .Values.a }} {{ .Defines.X }} {{ .Env.A }} {{ hasKey .Values \"opt\" }} {{ include \"ci/x.yaml\" . }}"
	base := Inputs{
		Values:  map[string]any{"a": 1, "unread": 1},
		Defines: map[string]any{"X": "x", "Y": "y"},
		Env:     map[string]string{"A": "a", "B": "b", "C": "c"},
	}
	files := map[string]string{"ci/x.yaml": "x"}

	run := func(t *testing.T, src string, in Inputs, files map[string]string) (string, string) {
		t.Helper()

		res := expand(t, src, in, files)

		return string(res.Text), res.Read.Digest()
	}

	text, digest := run(t, src, base, files)

	t.Run("same inputs, same text and digest", func(t *testing.T) {
		for range 5 {
			again, d := run(t, src, Inputs{
				Values:  map[string]any{"unread": 1, "a": 1},
				Defines: map[string]any{"Y": "y", "X": "x"},
				Env:     map[string]string{"C": "c", "B": "b", "A": "a"},
			}, map[string]string{"ci/x.yaml": "x"})
			if again != text || d != digest {
				t.Fatalf("expanded to %q %s, before %q %s", again, d, text, digest)
			}
		}
	})

	changed := []struct {
		name  string
		src   string
		in    func(in *Inputs)
		files map[string]string
		same  bool
	}{
		{name: "a value read", in: func(in *Inputs) { in.Values["a"] = 2 }},
		{name: "a value read, same text", in: func(in *Inputs) { in.Values["a"] = "1" }},
		{name: "a define read", in: func(in *Inputs) { in.Defines["X"] = "z" }},
		{name: "a variable read", in: func(in *Inputs) { in.Env["A"] = "z" }},
		{name: "a key looked up and missing, added", in: func(in *Inputs) { in.Values["opt"] = false }},
		{name: "an include", files: map[string]string{"ci/x.yaml": "y"}},
		{name: "the source", src: src + "\n"},
		{name: "a value not read", in: func(in *Inputs) { in.Values["unread"] = 2 }, same: true},
		{name: "a define not read", in: func(in *Inputs) { in.Defines["Y"] = "z" }, same: true},
		{name: "a listed variable not read", in: func(in *Inputs) { in.Env["B"] = "z" }, same: true},
		{name: "an unlisted variable", in: func(in *Inputs) { in.Env["C"] = "z" }, same: true},
	}

	for _, c := range changed {
		t.Run(c.name, func(t *testing.T) {
			in := Inputs{Values: Merge(base.Values), Defines: Merge(base.Defines), Env: maps.Clone(base.Env)}

			if c.in != nil {
				c.in(&in)
			}

			s := src
			if c.src != "" {
				s = c.src
			}

			f := files
			if c.files != nil {
				f = c.files
			}

			_, d := run(t, s, in, f)
			if (d == digest) != c.same {
				t.Errorf("digest %s, before %s; want same=%v", d, digest, c.same)
			}
		})
	}
}

func TestDigestOfActionFreeFiles(t *testing.T) {
	a := expand(t, "version: 1\n", Inputs{Values: map[string]any{"a": 1}}, nil)
	b := expand(t, "version: 1\n", Inputs{Values: map[string]any{"a": 2}}, nil)
	c := expand(t, "version: 2\n", Inputs{}, nil)

	if a.Read.Digest() != b.Read.Digest() {
		t.Error("an unread value changed the digest of an action-free file")
	}

	if a.Read.Digest() == c.Read.Digest() {
		t.Error("a change of the source kept the digest")
	}
}

func TestDigestTellsValueTypesApart(t *testing.T) {
	digests := map[string]string{}

	for _, v := range []any{1, "1", 1.5, true, "true", nil, []any{1}, map[string]any{"1": 1}, []any{}, map[string]any{}} {
		d := Record{Values: []Read{{Key: []string{"a"}, Value: v}}}.Digest()
		if prev, ok := digests[d]; ok {
			t.Errorf("%#v has the digest of %s", v, prev)
		}

		digests[d] = fmt.Sprintf("%#v", v)
	}
}
