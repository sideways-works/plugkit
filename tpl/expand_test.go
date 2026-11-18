package tpl

import (
	"reflect"
	"testing"
)

func expand(t *testing.T, src string, in Inputs, files map[string]string) Result {
	t.Helper()

	res, err := Expand(t.Context(), "ci.yaml", []byte(src), in, MapResolver{Commit: commit, Files: files})
	if err != nil {
		t.Fatal(err)
	}

	return res
}

func TestFunctions(t *testing.T) {
	values := Inputs{Values: map[string]any{
		"m":    map[string]any{"b": []any{1, 2}, "a": "x"},
		"list": []any{"a", 1, true},
		"path": "ci/values/staging.yaml",
		"n":    3,
	}}

	tests := []struct {
		name string
		src  string
		want string
	}{
		{"indent", `{{ indent 2 "a\nb" }}`, "  a\n  b"},
		{"indent zero", `{{ indent 0 "a" }}`, "a"},
		{"nindent", `x:{{ nindent 2 "a\nb" }}`, "x:\n  a\n  b"},
		{"toYaml map", `{{ toYaml .Values.m }}`, "a: x\nb:\n  - 1\n  - 2"},
		{"toYaml scalar", `{{ toYaml "a: b" }}`, "'a: b'"},
		{"toYaml list", `{{ toYaml .Values.list }}`, "- a\n- 1\n- true"},
		{"toJson", `{{ toJson (dict "b" (list 1 true) "a" "<x>") }}`, `{"a":"<x>","b":[1,true]}`},
		{"toJson string", `{{ toJson "a" }}`, `"a"`},
		{"quote string", `{{ quote "a \"b\"\n" }}`, `"a \"b\"\n"`},
		{"quote number", `{{ quote .Values.n }}`, `"3"`},
		{"quote bool", `{{ quote true }}`, `"true"`},
		{"default empty", `{{ default "a" "" }}`, "a"},
		{"default missing", `{{ default 2 (index .Values "replicas") }}`, "2"},
		{"default set", `{{ default 2 .Values.n }}`, "3"},
		{"default false", `{{ default true false }}`, "true"},
		{"default piped", `{{ "" | default "a" }}`, "a"},
		{"required", `{{ required "n is missing" .Values.n }}`, "3"},
		{"dict", `{{ $d := dict "a" 1 "b" "x" }}{{ $d.a }}{{ $d.b }}`, "1x"},
		{"dict empty", `{{ len (dict) }}`, "0"},
		{"list", `{{ len (list 1 2 3) }}{{ index (list "a" "b") 1 }}`, "3b"},
		{"list empty", `{{ len (list) }}`, "0"},
		{"hasKey", `{{ hasKey (dict "a" 1) "a" }} {{ hasKey (dict "a" 1) "b" }}`, "true false"},
		{"hasKey input", `{{ hasKey .Values "n" }} {{ hasKey .Values.m "c" }}`, "true false"},
		{"join", `{{ join "," .Values.list }}`, "a,1,true"},
		{"join piped", `{{ list "a" "b" | join "-" }}`, "a-b"},
		{"split", `{{ index (split "," "a,b,c") 1 }} {{ len (split "," "a,b,c") }}`, "b 3"},
		{"split and join", `{{ split "/" .Values.path | join "." }}`, "ci.values.staging.yaml"},
		{"upper", `{{ upper "ab" }}`, "AB"},
		{"lower", `{{ lower "AB" }}`, "ab"},
		{"trim", `{{ trim "  a b \n" }}`, "a b"},
		{"replace", `{{ replace "-" "_" "a-b-c" }}`, "a_b_c"},
		{"base", `{{ base .Values.path }}`, "staging.yaml"},
		{"dir", `{{ dir .Values.path }}`, "ci/values"},
		{"base of dir", `{{ base (dir "./api/...") }}`, "api"},
		{"include define", `{{ define "x" }}<{{ .a }}>{{ end }}{{ include "x" (dict "a" 1) }}`, "<1>"},
		{"include piped", `{{ define "x" }}a{{ end }}{{ include "x" . | upper }}`, "A"},
		{"include data piped", `{{ define "x" }}{{ .a }}{{ end }}{{ dict "a" 2 | include "x" }}`, "2"},
		{"and or not", `{{ and true 1 }} {{ or 0 "b" }} {{ not false }}`, "1 b true"},
		{"len", `{{ len .Values.list }} {{ len "abc" }}`, "3 3"},
		{"index", `{{ index .Values.m "a" }} {{ index .Values.m.b 1 }} {{ index .Values "m" "a" }}`, "x 2 x"},
		{"index of a string", `{{ index "abc" 1 }} {{ index .Values.path 0 }}`, "98 99"},
		{"index the same either way", `{{ index .Values.m.b 0 }} {{ index (.Values.m.b) 0 }}`, "1 1"},
		{"slice", `{{ slice "abcd" 1 3 }} {{ len (slice .Values.list 1) }}`, "bc 2"},
		{"print", `{{ print "a" 1 }} {{ printf "%s-%d" "a" 1 }} {{ println "a" }}`, "a1 a-1 a\n"},
		{"comparisons", `{{ eq 1 1 }},{{ ne 1 2 }},{{ lt 1 2 }},{{ le 2 2 }},{{ gt 2 1 }},{{ ge 1 2 }}`, "true,true,true,true,true,false"},
		{"eq with a value", `{{ eq .Values.n 3 }}`, "true"},
		{"template", `{{ define "x" }}[{{ . }}]{{ end }}{{ template "x" .Values.n }}`, "[3]"},
		{"block", `{{ block "x" .Values.n }}<{{ . }}>{{ end }}`, "<3>"},
		{"with else", `{{ with index .Values "nope" }}y{{ else }}n{{ end }}`, "n"},
		{"range else", `{{ range (list) }}y{{ else }}none{{ end }}`, "none"},
		{"range map sorted", `{{ range $k, $v := .Values.m }}{{ $k }} {{ end }}`, "a b "},
		{"chain", `{{ (index .Values "m").a }}`, "x"},
		{"variables", `{{ $m := .Values.m }}{{ $m.a }}`, "x"},
		{"comment", `a{{/* {{ define "x" }} */}}b`, "ab"},
		{"trim markers", "a  {{- 1 -}}  b", "a1b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := expand(t, tt.src, values, nil)
			if string(res.Text) != tt.want {
				t.Errorf("got %q, want %q", res.Text, tt.want)
			}
		})
	}
}

func TestOptionalValues(t *testing.T) {
	src := "replicas: {{ default 2 (index .Values \"replicas\") }}\n" +
		"{{- if hasKey .Values \"region\" }}\n" +
		"region: {{ .Values.region }}\n" +
		"{{- end }}\n"

	tests := []struct {
		values map[string]any
		want   string
	}{
		{nil, "replicas: 2\n"},
		{map[string]any{"replicas": 3, "region": "fsn1"}, "replicas: 3\nregion: fsn1\n"},
	}

	for _, tt := range tests {
		res := expand(t, src, Inputs{Values: tt.values}, nil)
		if string(res.Text) != tt.want {
			t.Errorf("with %v: got %q, want %q", tt.values, res.Text, tt.want)
		}
	}
}

func TestRangeReachesTheInputThroughDollar(t *testing.T) {
	src := "{{ range .Values.packages }}{{ . }}@{{ $.Values.image }}:{{ $.Defines.ENV }} {{ end }}"
	in := Inputs{
		Values:  map[string]any{"packages": []any{"./api/...", "./cli/..."}, "image": "golang"},
		Defines: map[string]any{"ENV": "staging"},
	}

	res := expand(t, src, in, nil)
	if want := "./api/...@golang:staging ./cli/...@golang:staging "; string(res.Text) != want {
		t.Errorf("got %q, want %q", res.Text, want)
	}
}

func TestPrecedence(t *testing.T) {
	src := "values:\n" +
		"  a: file\n  b: file\n  c: file\n  e: file\n" +
		"  m: {x: file, y: file, z: {deep: file}}\n" +
		"  l: [1, 2]\n" +
		"defines:\n  A: file\n  B: file\n  C: file\n" +
		"out: {{ .Values.a }} {{ .Values.b }} {{ .Values.c }} {{ .Values.e }} {{ .Values.m.x }} {{ .Values.m.y }} {{ .Values.m.z.deep }} {{ .Values.m.w }} {{ .Values.l }} {{ .Defines.A }} {{ .Defines.B }} {{ .Defines.C }}\n"

	v1, d1, err := ReadValuesFile("one.yaml", []byte("b: f1\nc: f1\ne: f1\nm: {y: f1, w: f1}\nl: [3]\ndefines: {B: f1, C: f1}\n"))
	if err != nil {
		t.Fatal(err)
	}

	v2, d2, err := ReadValuesFile("two.json", []byte(`{"e": "f2", "m": {"z": {"deep": "f2"}}, "defines": {"C": "f2"}}`))
	if err != nil {
		t.Fatal(err)
	}

	set1, err := ParseSet(Set, "c=cli1")
	if err != nil {
		t.Fatal(err)
	}

	set2, err := ParseSet(SetString, "c=cli")
	if err != nil {
		t.Fatal(err)
	}

	name, value, err := ParseDefine("C=cli")
	if err != nil {
		t.Fatal(err)
	}

	in := Inputs{
		Values:  Merge(v1, v2, set1, set2),
		Defines: Merge(d1, d2, map[string]any{name: value}),
	}

	res := expand(t, src, in, nil)
	want := src[:len(src)-len("out: {{ .Values.a }} {{ .Values.b }} {{ .Values.c }} {{ .Values.e }} {{ .Values.m.x }} {{ .Values.m.y }} {{ .Values.m.z.deep }} {{ .Values.m.w }} {{ .Values.l }} {{ .Defines.A }} {{ .Defines.B }} {{ .Defines.C }}\n")] +
		"out: file f1 cli f2 file f1 f2 f1 [3] file f1 cli\n"

	if string(res.Text) != want {
		t.Errorf("got\n%s\nwant\n%s", res.Text, want)
	}
}

func TestMerge(t *testing.T) {
	low := map[string]any{"m": map[string]any{"a": 1, "b": 1}, "l": []any{1, 2}, "s": "low", "keep": true}
	high := map[string]any{"m": map[string]any{"b": 2, "c": 2}, "l": []any{3}, "s": map[string]any{"now": "a map"}}

	got := Merge(low, high)

	want := map[string]any{
		"m":    map[string]any{"a": 1, "b": 2, "c": 2},
		"l":    []any{3},
		"s":    map[string]any{"now": "a map"},
		"keep": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if m, ok := got["m"].(map[string]any); ok {
		m["a"] = 9
	}

	if !reflect.DeepEqual(low["m"], map[string]any{"a": 1, "b": 1}) {
		t.Error("Merge shares maps with its layers")
	}

	if len(Merge()) != 0 || len(Merge(nil, nil)) != 0 {
		t.Error("Merge of nothing isn't empty")
	}
}

func TestActionFreeFilesExpandToThemselves(t *testing.T) {
	for _, src := range []string{
		"",
		"version: 1\n",
		"version: 1",
		"version: 1\r\npipelines: {}\r\n",
		"values:\n  a: 1\ndefines:\n  A: b\nenv: [X]\npipelines:\n  main: {}\n",
		"a: '{ {not an action} }'\nb: }}\nc: \"{\"\n",
		"  # indented comment\n\n\n\tx: ü\n",
	} {
		res := expand(t, src, Inputs{Values: map[string]any{"a": 2}}, nil)
		if string(res.Text) != src {
			t.Errorf("%q expanded to %q", src, res.Text)
		}
	}
}

func TestOwnInputBlocks(t *testing.T) {
	tests := []struct {
		name string
		src  string
		in   Inputs
		want string
	}{
		{
			name: "a flow list and a block list",
			src:  "env:\n- A\n- B\nvalues: {x: 1}\n---\n{{ .Env.A }}{{ .Env.B }}{{ .Values.x }}",
			in:   Inputs{Env: map[string]string{"A": "a", "B": "b"}},
			want: "env:\n- A\n- B\nvalues: {x: 1}\n---\nab1",
		},
		{
			name: "a column-0 sequence under env:",
			src:  "env:\n- DEPLOY_REGION\n# a comment\n- RELEASE_CHANNEL\nout: {{ .Env.DEPLOY_REGION }}-{{ .Env.RELEASE_CHANNEL }}\n",
			in:   Inputs{Env: map[string]string{"DEPLOY_REGION": "fsn1", "RELEASE_CHANNEL": "beta"}},
			want: "env:\n- DEPLOY_REGION\n# a comment\n- RELEASE_CHANNEL\nout: fsn1-beta\n",
		},
		{
			name: "a multi-line flow list",
			src:  "env: [A,\nB\n]\nvalues: {a: 1, s: 'x]',\nb: [2,\n3]}\nout: {{ .Env.A }}{{ .Env.B }}{{ .Values.a }}{{ index .Values.b 1 }}\n",
			in:   Inputs{Env: map[string]string{"A": "a", "B": "b"}},
			want: "env: [A,\nB\n]\nvalues: {a: 1, s: 'x]',\nb: [2,\n3]}\nout: ab13\n",
		},
		{
			name: "a column-0 action right after a column-0 sequence",
			src:  "env:\n- A\n{{- range list 1 }}\na: {{ $.Env.A }}\n{{- end }}\n",
			in:   Inputs{Env: map[string]string{"A": "a"}},
			want: "env:\n- A\na: a\n",
		},
		{
			name: "an action at column 0 right after values:",
			src:  "values:\n  l: [a, b]\n{{- range .Values.l }}\n{{ . }}: 1\n{{- end }}\n",
			want: "values:\n  l: [a, b]\na: 1\nb: 1\n",
		},
		{
			name: "an action at column 0 right after defines:",
			src:  "defines:\n  X: x\n{{ .Defines.X }}: 1\n",
			want: "defines:\n  X: x\nx: 1\n",
		},
		{
			name: "an action at column 0 right after env:",
			src:  "env: [A]\n{{- range list 1 }}\na: {{ $.Env.A }}\n{{- end }}\n",
			in:   Inputs{Env: map[string]string{"A": "a"}},
			want: "env: [A]\na: a\n",
		},
		{
			name: "a comment at column 0 inside a block",
			src:  "values:\n  a: 1\n# the next one\n  b: 2\nout: {{ .Values.a }}{{ .Values.b }}\n",
			want: "values:\n  a: 1\n# the next one\n  b: 2\nout: 12\n",
		},
		{
			name: "a flow list over several lines",
			src:  "env: [\n  A\n]\na: {{ .Env.A }}",
			in:   Inputs{Env: map[string]string{"A": "a"}},
			want: "env: [\n  A\n]\na: a",
		},
		{
			name: "comments and blank lines stay in the block",
			src:  "values:\n  # the image\n\n  image: golang\n# done\nimage: {{ .Values.image }}\n",
			want: "values:\n  # the image\n\n  image: golang\n# done\nimage: golang\n",
		},
		{
			name: "a key that only starts like one",
			src:  "values_extra: 1\nvalues:\n  a: 1\nenvironment: {{ .Values.a }}\n",
			want: "values_extra: 1\nvalues:\n  a: 1\nenvironment: 1\n",
		},
		{
			name: "empty blocks",
			src:  "values:\ndefines:\nenv:\nx: {{ len .Values }}{{ len .Defines }}{{ len .Env }}\n",
			want: "values:\ndefines:\nenv:\nx: 000\n",
		},
		{
			name: "file values are read as YAML types",
			src:  "values:\n  n: 3\n  b: true\n  s: '3'\nx: {{ eq .Values.n 3 }},{{ eq .Values.b true }},{{ eq .Values.s \"3\" }}\n",
			want: "values:\n  n: 3\n  b: true\n  s: '3'\nx: true,true,true\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := expand(t, tt.src, tt.in, nil)
			if string(res.Text) != tt.want {
				t.Errorf("got %q, want %q", res.Text, tt.want)
			}
		})
	}
}
