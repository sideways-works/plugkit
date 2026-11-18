package tpl

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestParseSet(t *testing.T) {
	tests := []struct {
		kind    SetKind
		arg     string
		want    map[string]any
		wantErr string
	}{
		{Set, "a=1", map[string]any{"a": 1}, ""},
		{Set, "a.b=1", map[string]any{"a": map[string]any{"b": 1}}, ""},
		{Set, "a=1.5", map[string]any{"a": 1.5}, ""},
		{Set, "a=true", map[string]any{"a": true}, ""},
		{Set, "a=yes", map[string]any{"a": "yes"}, ""},
		{Set, "a=0x1F", map[string]any{"a": 31}, ""},
		{Set, "a=fsn1", map[string]any{"a": "fsn1"}, ""},
		{Set, "a=", map[string]any{"a": ""}, ""},
		{Set, "a=null", map[string]any{"a": "null"}, ""},
		{Set, "a=[1,2]", map[string]any{"a": "[1,2]"}, ""},
		{Set, "a=x=y,z", map[string]any{"a": "x=y,z"}, ""},
		{Set, "a=2001-12-14", map[string]any{"a": "2001-12-14"}, ""},
		{SetString, "a.b=1", map[string]any{"a": map[string]any{"b": "1"}}, ""},
		{SetString, "a=true", map[string]any{"a": "true"}, ""},
		{SetJSON, `a={"b":[1,2.5,"c"]}`, map[string]any{"a": map[string]any{"b": []any{1, 2.5, "c"}}}, ""},
		{SetJSON, "a=null", map[string]any{"a": nil}, ""},
		{Set, "a.b", nil, "--set a.b: expected key=value"},
		{Set, "=1", nil, "--set =1: expected key=value"},
		{Set, "a..b=1", nil, "--set a..b=1: expected key=value"},
		{SetString, "a", nil, "--set-string a: expected key=value"},
		{SetJSON, "a={", nil, "--set-json a={: invalid JSON: unexpected end of JSON input"},
		{SetJSON, "a=b", nil, "--set-json a=b: invalid JSON: invalid character 'b' looking for beginning of value"},
	}

	for _, tt := range tests {
		got, err := ParseSet(tt.kind, tt.arg)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("%s %s: got error %v, want %s", tt.kind, tt.arg, err, tt.wantErr)
			}

			continue
		}

		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s %s: got %#v, %v; want %#v", tt.kind, tt.arg, got, err, tt.want)
		}
	}
}

func TestParseDefine(t *testing.T) {
	tests := []struct {
		arg     string
		name    string
		value   any
		wantErr string
	}{
		{"STAGING", "STAGING", true, ""},
		{"REGION=fsn1", "REGION", "fsn1", ""},
		{"N=3", "N", "3", ""},
		{"EMPTY=", "EMPTY", "", ""},
		{"_x1=a=b", "_x1", "a=b", ""},
		{"", "", nil, "-D : expected NAME or NAME=value"},
		{"1X", "", nil, "-D 1X: expected NAME or NAME=value"},
		{"A-B=1", "", nil, "-D A-B=1: expected NAME or NAME=value"},
	}

	for _, tt := range tests {
		name, value, err := ParseDefine(tt.arg)
		if tt.wantErr != "" {
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("-D %s: got error %v, want %s", tt.arg, err, tt.wantErr)
			}

			continue
		}

		if err != nil || name != tt.name || value != tt.value {
			t.Errorf("-D %s: got %q %#v %v", tt.arg, name, value, err)
		}
	}
}

func TestReadValuesFile(t *testing.T) {
	tests := []struct {
		name        string
		file        string
		data        string
		values      map[string]any
		defines     map[string]any
		wantErr     string
		wantErrLine bool
	}{
		{
			name:    "YAML with defines",
			file:    "ci/values/production.yaml",
			data:    "replicas: 3\ndefines:\n  ENVIRONMENT: production\n  DEBUG: false\n",
			values:  map[string]any{"replicas": 3},
			defines: map[string]any{"ENVIRONMENT": "production", "DEBUG": false},
		},
		{
			name:    "JSON with tabs",
			file:    "x.json",
			data:    "{\n\t\"a\": {\"b\": [1, 2.5, null]},\n\t\"defines\": {\"X\": \"y\"}\n}\n",
			values:  map[string]any{"a": map[string]any{"b": []any{1, 2.5, nil}}},
			defines: map[string]any{"X": "y"},
		},
		{
			name:    "an empty file",
			file:    "empty.yaml",
			data:    "# nothing yet\n",
			values:  map[string]any{},
			defines: map[string]any{},
		},
		{
			name:    "anchors, merge keys and number keys",
			file:    "x.yaml",
			data:    "base: &b {a: 1, b: 1}\nderived:\n  <<: *b\n  b: 2\n1: one\nt: 2001-12-14\n",
			values:  map[string]any{"base": map[string]any{"a": 1, "b": 1}, "derived": map[string]any{"a": 1, "b": 2}, "1": "one", "t": "2001-12-14"},
			defines: map[string]any{},
		},
		{
			name:    "a list at the top",
			file:    "x.yaml",
			data:    "\n- a\n",
			wantErr: "-f x.yaml: line 2: a values file holds a mapping",
		},
		{
			name:    "a key twice",
			file:    "x.yaml",
			data:    "a: 1\nb:\n  c: 1\n  c: 2\n",
			wantErr: `-f x.yaml: line 4: mapping key "c" already defined at line 3`,
		},
		{
			name:    "defines that aren't a mapping",
			file:    "x.yaml",
			data:    "a: 1\ndefines: [A]\n",
			wantErr: "-f x.yaml: line 2: defines: holds a mapping of names to values",
		},
		{
			name:    "a define that isn't a name",
			file:    "x.yaml",
			data:    "defines:\n  my-flag: true\n",
			wantErr: `-f x.yaml: line 1: defines: "my-flag" is not a name`,
		},
		{
			name:    "invalid JSON",
			file:    "x.json",
			data:    "{\n  \"a\": 1,\n  \"b\": [1, 2\n}\n",
			wantErr: "-f x.json: line 2: did not find expected ',' or ']'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, defines, err := ReadValuesFile(tt.file, []byte(tt.data))
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("got error %v, want %s", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(values, tt.values) || !reflect.DeepEqual(defines, tt.defines) {
				t.Errorf("got %#v %#v", values, defines)
			}
		})
	}
}

func TestInputsAreNormalized(t *testing.T) {
	in := Inputs{Values: map[string]any{
		"strings": []string{"a", "b"},
		"ints":    map[string]int32{"n": 3},
		"big":     uint64(math.MaxUint64),
		"float":   float32(1.5),
	}}

	res := expand(t, `{{ join "," .Values.strings }} {{ eq .Values.ints.n 3 }} {{ .Values.big }} {{ .Values.float }}`, in, nil)
	if want := "a,b true 18446744073709551615 1.5"; string(res.Text) != want {
		t.Errorf("got %q, want %q", res.Text, want)
	}

	_, err := Expand(t.Context(), "ci.yaml", []byte("x"), Inputs{Values: map[string]any{"f": func() {}}}, MapResolver{})
	if err == nil || err.Error() != "ci.yaml: values: unsupported value of type func()" {
		t.Errorf("got %v", err)
	}
}

func TestDefinesFromTheCallerAreScalars(t *testing.T) {
	_, err := Expand(t.Context(), "ci.yaml", []byte("x"), Inputs{Defines: map[string]any{"A": []any{1}}}, MapResolver{})
	if err == nil || err.Error() != "ci.yaml: defines: A is a string, number or boolean" {
		t.Errorf("got %v", err)
	}
}

func TestExpanded(t *testing.T) {
	var doc struct {
		A int `yaml:"a"`
	}

	err := Expanded("ci.yaml", yaml.Unmarshal([]byte("\n\na: x\n"), &doc))
	if err == nil || err.Error() != "ci.yaml (expanded) line 3: cannot unmarshal !!str `x` into int" {
		t.Errorf("type error: %v", err)
	}

	other := errors.New("other") //nolint:err113 // a stand-in for any error that isn't YAML's
	if !errors.Is(Expanded("ci.yaml", other), other) || Expanded("ci.yaml", nil) != nil {
		t.Error("Expanded changed an error that isn't YAML's")
	}
}
