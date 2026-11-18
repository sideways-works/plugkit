package tpl

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const commit = "3f2a9c0e41b7d5c1a0e9f2b6c4d8e7a1b3c5d7e9"

// lines builds a file with the given lines at their line numbers and a
// comment on every other line up to the last.
func lines(at map[int]string) string {
	last := 0
	for n := range at {
		last = max(last, n)
	}

	var b strings.Builder

	for n := 1; n <= last; n++ {
		if l, ok := at[n]; ok {
			b.WriteString(l + "\n")
		} else {
			fmt.Fprintf(&b, "# line %d\n", n)
		}
	}

	return b.String()
}

// TestErrorRows pins every row of the spec's error table, text exactly as
// written.
func TestErrorRows(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		in    Inputs
		files map[string]string
		want  string
	}{
		{
			name: "a missing key",
			src:  lines(map[int]string{14: "image: {{ .Values.imag }}"}),
			in:   Inputs{Values: map[string]any{"image": "golang"}},
			want: "ci.yaml:14: .Values.imag: no such key",
		},
		{
			name: "a variable not in env:",
			src:  lines(map[int]string{1: "env: [DEPLOY_REGION]", 9: "home: {{ .Env.HOME }}"}),
			in:   Inputs{Env: map[string]string{"HOME": "/root", "DEPLOY_REGION": "fsn1"}},
			want: "ci.yaml:9: .Env.HOME is not allowed: add HOME to env:",
		},
		{
			name: "a credential, even when listed",
			src:  lines(map[int]string{1: "env: [CI_TOKEN]", 9: "token: {{ .Env.CI_TOKEN }}"}),
			in:   Inputs{Env: map[string]string{"CI_TOKEN": "secret"}, Never: []string{"CI_TOKEN"}},
			want: "ci.yaml:9: .Env.CI_TOKEN is a credential and is never readable",
		},
		{
			name: "a remote include without a commit",
			src:  lines(map[int]string{3: `{{ include "https://…//go.yaml" . }}`}),
			want: `ci.yaml:3: include "https://…//go.yaml": a remote include needs @<commit>`,
		},
		{
			name: "a branch where a commit belongs",
			src:  lines(map[int]string{3: `{{ include "https://…//go.yaml@main" . }}`}),
			want: `ci.yaml:3: include "https://…//go.yaml@main": "main" is not a commit`,
		},
		{
			name: "a missing file of the repository",
			src:  lines(map[int]string{5: `{{ include "ci/x.yaml" . }}`}),
			want: `ci.yaml:5: include "ci/x.yaml": no such file at 3f2a9c0e41b7`,
		},
		{
			name: "files that include each other",
			src:  `{{ include "ci/a.yaml" . }}`,
			files: map[string]string{
				"ci/a.yaml": "a:\n{{ include \"ci/b.yaml\" . }}\n",
				"ci/b.yaml": "b:\n{{ include \"ci/a.yaml\" . }}\n",
			},
			want: `ci/a.yaml:2: include "ci/b.yaml": includes form a cycle: ci/a.yaml -> ci/b.yaml -> ci/a.yaml`,
		},
		{
			name: "includes deeper than four levels",
			src:  `{{ include "ci/a.yaml" . }}`,
			files: map[string]string{
				"ci/a.yaml": `{{ include "ci/b.yaml" . }}`,
				"ci/b.yaml": `{{ include "ci/c.yaml" . }}`,
				"ci/c.yaml": `{{ include "ci/d.yaml" . }}`,
				"ci/d.yaml": `{{ include "ci/e.yaml" . }}`,
				"ci/e.yaml": "e: 1\n",
			},
			want: "ci/d.yaml:1: includes nest deeper than four levels",
		},
		{
			name: "a required value no source gave",
			src:  lines(map[int]string{20: `image: {{ required "image is missing" (index .Values "image") }}`}),
			want: "ci.yaml:20: required: image is missing",
		},
		{
			name: "one template name defined in two places",
			src:  lines(map[int]string{1: `{{ include "ci/shared.yaml" . }}`, 12: `{{- define "lint" }}lint{{ end }}`}),
			files: map[string]string{
				"ci/shared.yaml": lines(map[int]string{4: `{{- define "lint" }}other{{ end }}`}),
			},
			want: `ci/shared.yaml:4: define "lint" is also defined at ci.yaml:12`,
		},
		{
			name: "an action inside the file's own inputs",
			src:  "values:\n  image: {{ .Env.IMAGE }}\npipelines: {}\n",
			want: "ci.yaml:2: values: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "an unknown function",
			src:  lines(map[int]string{7: "at: {{ now }}"}),
			want: `ci.yaml:7: function "now" not defined`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Expand(t.Context(), "ci.yaml", []byte(tt.src), tt.in, MapResolver{Commit: commit, Files: tt.files})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got error\n%v\nwant\n%s", err, tt.want)
			}

			if strings.Contains(err.Error(), "__") {
				t.Errorf("the error shows the loader's own functions: %v", err)
			}

			if e, ok := errors.AsType[*Error](err); !ok || e.Line == 0 {
				t.Errorf("%T is not an *Error with a line", err)
			}
		})
	}
}

func TestCommandLineErrorRows(t *testing.T) {
	_, err := ParseSet(Set, "a.b")
	if err == nil || err.Error() != "--set a.b: expected key=value" {
		t.Errorf("ParseSet: %v", err)
	}

	_, _, err = ReadValuesFile("ci/values/x.yaml", []byte("a: 1\nb: 2\nc: 3\nd: e: f\n"))
	if err == nil || err.Error() != "-f ci/values/x.yaml: line 4: mapping values are not allowed here" {
		t.Errorf("ReadValuesFile: %v", err)
	}

	var doc map[string]any

	err = Expanded("ci.yaml", yaml.Unmarshal([]byte(strings.Repeat("a: 1\n", 30)+"b: c: d\n"), &doc))
	if err == nil || err.Error() != "ci.yaml (expanded) line 31: mapping values are not allowed here" {
		t.Errorf("Expanded: %v", err)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name  string
		src   string
		in    Inputs
		files map[string]string
		want  string
	}{
		{
			name: "a missing key below another",
			src:  "a: {{ .Values.a.b.c }}\n",
			in:   Inputs{Values: map[string]any{"a": map[string]any{"x": 1}}},
			want: "ci.yaml:1: .Values.a.b: no such key",
		},
		{
			name: "a field of a string",
			src:  "a: {{ .Values.a.b }}\n",
			in:   Inputs{Values: map[string]any{"a": "s"}},
			want: "ci.yaml:1: .Values.a.b: .Values.a is a string, not a map",
		},
		{
			name: "a missing key through $ inside range",
			src:  "{{ range .Values.l }}\n{{ $.Values.nope }}{{ end }}\n",
			in:   Inputs{Values: map[string]any{"l": []any{1}}},
			want: "ci.yaml:2: $.Values.nope: no such key",
		},
		{
			name: "default doesn't save a missing key",
			src:  `{{ .Values.x | default "a" }}`,
			want: "ci.yaml:1: .Values.x: no such key",
		},
		{
			name: "a missing define",
			src:  "\n{{ if .Defines.STAGING }}s{{ end }}",
			want: "ci.yaml:2: .Defines.STAGING: no such key",
		},
		{
			name: "a field of a list element",
			src:  "{{ range .Values.l }}{{ .name }}{{ end }}",
			in:   Inputs{Values: map[string]any{"l": []any{map[string]any{"id": 1}}}},
			want: "ci.yaml:1: .name: no such key",
		},
		{
			name: "html isn't a function",
			src:  `{{ html "<" }}`,
			want: `ci.yaml:1: function "html" not defined`,
		},
		{
			name: "js isn't a function",
			src:  `{{ js "<" }}`,
			want: `ci.yaml:1: function "js" not defined`,
		},
		{
			name: "urlquery isn't a function",
			src:  `{{ urlquery "<" }}`,
			want: `ci.yaml:1: function "urlquery" not defined`,
		},
		{
			name: "the loader's own functions can't be called",
			src:  `{{ __field . "" "Env.HOME" }}`,
			want: `ci.yaml:1: function "__field" not defined`,
		},
		{
			name: "a syntax error",
			src:  "a: 1\n{{ if }}\n",
			want: "ci.yaml:2: missing value for if",
		},
		{
			name: "an unclosed action",
			src:  "a: 1\nb: {{ .Values.a\n",
			want: "ci.yaml:3: unclosed action started at ci.yaml:2",
		},
		{
			name: "a built-in failing",
			src:  "\n\n{{ eq .Values.a \"x\" }}",
			in:   Inputs{Values: map[string]any{"a": 1}},
			want: "ci.yaml:3: error calling eq: incompatible types for comparison: int and string",
		},
		{
			name: "include with one argument",
			src:  `{{ include "x" }}`,
			want: "ci.yaml:1: wrong number of args for include: want 2 got 1",
		},
		{
			name: "hasKey with one argument",
			src:  `{{ hasKey .Values }}`,
			want: "ci.yaml:1: wrong number of args for hasKey: want 2 got 1",
		},
		{
			name: "an error inside an included file names that file",
			src:  "a:\n{{ include \"ci/x.yaml\" . }}\n",
			files: map[string]string{
				"ci/x.yaml": "x: 1\ny: {{ .Values.y }}\n",
			},
			want: "ci/x.yaml:2: .Values.y: no such key",
		},
		{
			name: "an error inside an included define names the file of the define",
			src:  "{{ include \"ci/defs.yaml\" . }}\njob: {{ include \"job\" (dict) }}\n",
			files: map[string]string{
				"ci/defs.yaml": "\n\n{{ define \"job\" }}\nname: {{ .name }}\n{{ end }}",
			},
			want: "ci/defs.yaml:4: .name: no such key",
		},
		{
			name: "a define used through template names its file too",
			src:  "{{ include \"ci/defs.yaml\" . }}\njob: {{ template \"job\" (dict) }}\n",
			files: map[string]string{
				"ci/defs.yaml": "{{ define \"job\" }}{{ .name }}{{ end }}",
			},
			want: "ci/defs.yaml:1: .name: no such key",
		},
		{
			name: "a define twice in one file",
			src:  "{{ define \"a\" }}x{{ end }}\n\n{{ define \"a\" }}y{{ end }}\n",
			want: `ci.yaml:3: define "a" is also defined at ci.yaml:1`,
		},
		{
			name: "an empty define twice in one file",
			src:  "{{ define \"a\" }}{{ end }}\n{{ block \"a\" . }}y{{ end }}\n",
			want: `ci.yaml:2: define "a" is also defined at ci.yaml:1`,
		},
		{
			name: "two included files define one name",
			src:  "{{ include \"ci/a.yaml\" . }}\n{{ include \"ci/b.yaml\" . }}\n",
			files: map[string]string{
				"ci/a.yaml": `{{ define "job" }}a{{ end }}`,
				"ci/b.yaml": "\n{{ define \"job\" }}b{{ end }}",
			},
			want: `ci/b.yaml:2: define "job" is also defined at ci/a.yaml:1`,
		},
		{
			name: "a define including itself",
			src:  "{{ define \"a\" }}{{ include \"a\" . }}{{ end }}\n{{ include \"a\" . }}\n",
			want: `ci.yaml:1: include "a": includes form a cycle: a -> a`,
		},
		{
			name: "the file including itself",
			src:  "\n{{ include \"ci.yaml\" . }}\n",
			want: `ci.yaml:2: include "ci.yaml": includes form a cycle: ci.yaml -> ci.yaml`,
		},
		{
			name: "a path out of the repository",
			src:  `{{ include "../x.yaml" . }}`,
			want: `ci.yaml:1: include "../x.yaml": not a path in the repository`,
		},
		{
			name: "a path that isn't clean",
			src:  `{{ include "ci/./x.yaml" . }}`,
			want: `ci.yaml:1: include "ci/./x.yaml": not a path in the repository`,
		},
		{
			name: "an action inside an indented block",
			src:  "values:\n  a: 1\n  b: {{ .Env.B }}\n{{ .Values.a }}\n",
			want: "ci.yaml:3: values: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "a column-0 sequence under values: is in the block",
			src:  "values:\n- {{ .Values.a }}\n",
			want: "ci.yaml:2: values: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "a column-0 sequence under defines: is in the block",
			src:  "defines:\n- A\n-\n",
			want: "ci.yaml:1: defines: holds a mapping of names to values",
		},
		{
			name: "a column-0 sequence under env: is in the block",
			src:  "env:\n- A\n- {{ .Values.a }}\n",
			want: "ci.yaml:3: env: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "an action on the key line",
			src:  "x: 1\nenv: [{{ .Values.a }}]\n",
			want: "ci.yaml:2: env: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "a misspelled define",
			src:  "{{ define \"lint\" }}x{{ end }}\n{{ include \"lnit\" . }}",
			want: `ci.yaml:2: include "lnit": no define named "lnit" and no such file at 3f2a9c0e41b7`,
		},
		{
			name: "a missing file named like a path keeps the plain row",
			src:  `{{ include "lint.yaml" . }}`,
			want: `ci.yaml:1: include "lint.yaml": no such file at 3f2a9c0e41b7`,
		},
		{
			name: "an action in defines:",
			src:  "defines:\n  A: x\n  # {{ .Values.a }}\nb: 1\n",
			want: "ci.yaml:3: defines: holds a template action; values:, defines: and env: are plain YAML",
		},
		{
			name: "values: that isn't YAML",
			src:  "a: 1\nvalues:\n  a: 1\n  b: c: d\npipelines: {{ 1 }}\n",
			want: "ci.yaml:4: values: mapping values are not allowed here",
		},
		{
			name: "values: twice",
			src:  "values:\n  a: 1\nvalues:\n  b: 1\n",
			want: "ci.yaml:3: values: is also at line 1",
		},
		{
			name: "env: that isn't a list of names",
			src:  "env: {A: 1}\n",
			want: "ci.yaml:1: env: holds a list of variable names",
		},
		{
			name: "a define that isn't a scalar",
			src:  "defines:\n  A: [1]\n",
			want: "ci.yaml:1: defines: A is a string, number or boolean",
		},
		{
			name: "index past a missing key",
			src:  `{{ index .Values "nope" "x" }}`,
			want: "ci.yaml:1: error calling index: index of nil pointer",
		},
		{
			name: "index past a missing key, not rewritten",
			src:  `{{ index (.Values) "nope" "x" }}`,
			want: "ci.yaml:1: error calling index: index of nil pointer",
		},
		{
			name: "index out of range",
			src:  `{{ index .Values.l 5 }}`,
			in:   Inputs{Values: map[string]any{"l": []any{1}}},
			want: "ci.yaml:1: error calling index: index out of range: 5",
		},
		{
			name: "index out of range, not rewritten",
			src:  `{{ index (.Values.l) 5 }}`,
			in:   Inputs{Values: map[string]any{"l": []any{1}}},
			want: "ci.yaml:1: error calling index: index out of range: 5",
		},
		{
			name: "call finds nothing to call",
			src:  "\n{{ call .Values.n 1 }}",
			in:   Inputs{Values: map[string]any{"n": 3}},
			want: "ci.yaml:2: error calling call: non-function .Values.n of type int",
		},
		{
			name: "dict with an odd number of arguments",
			src:  `{{ dict "a" }}`,
			want: "ci.yaml:1: dict: odd number of arguments",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Expand(t.Context(), "ci.yaml", []byte(tt.src), tt.in, MapResolver{Commit: commit, Files: tt.files})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got error\n%v\nwant\n%s", err, tt.want)
			}

			if strings.Contains(err.Error(), "__") && !strings.Contains(tt.src, "__") {
				t.Errorf("the error shows the loader's own functions: %v", err)
			}
		})
	}
}
