package tpl

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/fstest"
)

const remoteRepo = "https://github.com/sideways-works/ci-shared.git"

func TestIncludes(t *testing.T) {
	remote := map[string]string{
		remoteRepo + "//go.yaml@" + commit:    "go: {{ .Values.v }}\n{{ include \"lib/x.yaml\" . }}",
		remoteRepo + "//lib/x.yaml@" + commit: "x: remote",
	}

	tests := []struct {
		name  string
		src   string
		files map[string]string
		want  string
	}{
		{
			name:  "a file with the dot passed",
			src:   `{{ include "ci/common.yaml" . }}`,
			files: map[string]string{"ci/common.yaml": "v: {{ .Values.v }}"},
			want:  "v: 1",
		},
		{
			name:  "a file with other data",
			src:   `{{ include "ci/job.yaml" (dict "pkg" "./api") }}`,
			files: map[string]string{"ci/job.yaml": "pkg: {{ .pkg }}"},
			want:  "pkg: ./api",
		},
		{
			name:  "a define before a file of the same name",
			src:   `{{ define "common" }}define{{ end }}{{ include "common" . }}`,
			files: map[string]string{"common": "file"},
			want:  "define",
		},
		{
			name: "a fragments file, included once, then its defines by name",
			src:  "{{ include \"ci/defs.yaml\" . }}\n{{ include \"a\" . }} {{ template \"b\" 2 }}",
			files: map[string]string{
				"ci/defs.yaml": "{{ define \"a\" }}a{{ .Values.v }}{{ end }}\n{{- define \"b\" }}b{{ . }}{{ end }}",
			},
			want: "\na1 b2",
		},
		{
			name:  "a file included twice is the same place",
			src:   "{{ range list 1 2 }}{{ include \"ci/job.yaml\" . }}{{ end }}",
			files: map[string]string{"ci/job.yaml": "{{ define \"j\" }}j{{ end }}[{{ include \"j\" . }}{{ . }}]"},
			want:  "[j1][j2]",
		},
		{
			name: "an included file uses its own define",
			src:  `{{ include "ci/a.yaml" . }}`,
			files: map[string]string{
				"ci/a.yaml": "{{ define \"own\" }}own{{ end }}{{ include \"own\" . }}",
			},
			want: "own",
		},
		{
			name:  "nindent places an include",
			src:   "jobs:\n  test:\n    {{- include \"ci/job.yaml\" . | nindent 4 }}\n",
			files: map[string]string{"ci/job.yaml": "steps:\n  - run: [go, test]"},
			want:  "jobs:\n  test:\n    steps:\n      - run: [go, test]\n",
		},
		{
			name: "four levels deep",
			src:  `{{ include "ci/a.yaml" . }}`,
			files: map[string]string{
				"ci/a.yaml": `a{{ include "ci/b.yaml" . }}`,
				"ci/b.yaml": `b{{ include "ci/c.yaml" . }}`,
				"ci/c.yaml": `c{{ include "ci/d.yaml" . }}`,
				"ci/d.yaml": `d`,
			},
			want: "abcd",
		},
		{
			name: "defines don't count as levels",
			src:  `{{ define "x" }}{{ include "ci/a.yaml" . }}{{ end }}{{ include "x" . }}`,
			files: map[string]string{
				"ci/a.yaml": `a{{ include "ci/b.yaml" . }}`,
				"ci/b.yaml": `b{{ include "ci/c.yaml" . }}`,
				"ci/c.yaml": `c{{ include "ci/d.yaml" . }}`,
				"ci/d.yaml": `d`,
			},
			want: "abcd",
		},
		{
			name: "a path in a remote file is read from that repository",
			src:  `{{ include "https://github.com/sideways-works/ci-shared.git//go.yaml@` + commit + `" . }}`,
			want: "go: 1\nx: remote",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := MapResolver{Commit: commit, Files: tt.files, Remotes: remote}

			res, err := Expand(t.Context(), "ci.yaml", []byte(tt.src), Inputs{Values: map[string]any{"v": 1}}, r)
			if err != nil {
				t.Fatal(err)
			}

			if string(res.Text) != tt.want {
				t.Errorf("got %q, want %q", res.Text, tt.want)
			}
		})
	}
}

func TestIncludesAreRecorded(t *testing.T) {
	src := `{{ include "ci/a.yaml" . }}{{ include "https://github.com/sideways-works/ci-shared.git//go.yaml@` + commit + `" . }}{{ include "ci/a.yaml" . }}`
	r := MapResolver{
		Files:   map[string]string{"ci/a.yaml": "a"},
		Remotes: map[string]string{remoteRepo + "//go.yaml@" + commit: "go"},
	}

	res, err := Expand(t.Context(), "ci.yaml", []byte(src), Inputs{}, r)
	if err != nil {
		t.Fatal(err)
	}

	want := []File{
		{Ref: "ci/a.yaml", SHA256: sum([]byte("a"))},
		{Ref: remoteRepo + "//go.yaml@" + commit, SHA256: sum([]byte("go"))},
	}
	if !slices.Equal(res.Read.Includes, want) {
		t.Errorf("got %v, want %v", res.Read.Includes, want)
	}

	if res.Read.Source != (File{Ref: "ci.yaml", SHA256: sum([]byte(src))}) {
		t.Errorf("source %v", res.Read.Source)
	}
}

func TestRemoteIncludeNames(t *testing.T) {
	sha256Commit := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	tests := []struct {
		name string
		ref  string
		want string
	}{
		{"no commit", remoteRepo + "//go.yaml", `include "` + remoteRepo + `//go.yaml": a remote include needs @<commit>`},
		{"an empty commit", remoteRepo + "//go.yaml@", `include "` + remoteRepo + `//go.yaml@": a remote include needs @<commit>`},
		{"a short hash", remoteRepo + "//go.yaml@3f2a9c0", `include "` + remoteRepo + `//go.yaml@3f2a9c0": "3f2a9c0" is not a full commit hash`},
		{"an uppercase hash", remoteRepo + "//go.yaml@3F2A9C0E41B7D5C1A0E9F2B6C4D8E7A1B3C5D7E9", `include "` + remoteRepo + `//go.yaml@3F2A9C0E41B7D5C1A0E9F2B6C4D8E7A1B3C5D7E9": "3F2A9C0E41B7D5C1A0E9F2B6C4D8E7A1B3C5D7E9" is not a full commit hash`},
		{"a branch", remoteRepo + "//go.yaml@main", `include "` + remoteRepo + `//go.yaml@main": "main" is not a commit`},
		{"a tag", remoteRepo + "//go.yaml@v1.2.0", `include "` + remoteRepo + `//go.yaml@v1.2.0": "v1.2.0" is not a commit`},
		{"no path", remoteRepo + "//@" + commit, `include "` + remoteRepo + `//@` + commit + `": expected <git URL>//<path>@<commit>`},
		{"a path out of the repository", remoteRepo + "//../go.yaml@" + commit, `include "` + remoteRepo + `//../go.yaml@` + commit + `": expected <git URL>//<path>@<commit>`},
		{"a missing remote file", remoteRepo + "//nope.yaml@" + commit, `include "` + remoteRepo + `//nope.yaml@` + commit + `": no such file at 3f2a9c0e41b7`},
		{"an scp-style URL with a missing file", "git@github.com:sideways-works/ci-shared.git//go.yaml@" + commit, `include "git@github.com:sideways-works/ci-shared.git//go.yaml@` + commit + `": no such file at 3f2a9c0e41b7`},
		{"a SHA-256 commit with a missing file", remoteRepo + "//go.yaml@" + sha256Commit, `include "` + remoteRepo + `//go.yaml@` + sha256Commit + `": no such file at 0123456789ab`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Expand(t.Context(), "ci.yaml", []byte(`{{ include "`+tt.ref+`" . }}`), Inputs{}, MapResolver{})
			if err == nil || err.Error() != "ci.yaml:1: "+tt.want {
				t.Fatalf("got %v\nwant ci.yaml:1: %s", err, tt.want)
			}
		})
	}
}

func TestFSResolver(t *testing.T) {
	r := FSResolver{FS: fstest.MapFS{"ci/a.yaml": {Data: []byte("a: {{ .Values.v }}")}}, Commit: commit}

	res, err := Expand(t.Context(), "ci.yaml", []byte(`{{ include "ci/a.yaml" . }}`), Inputs{Values: map[string]any{"v": 1}}, r)
	if err != nil {
		t.Fatal(err)
	}

	if string(res.Text) != "a: 1" {
		t.Errorf("got %q", res.Text)
	}

	_, err = Expand(t.Context(), "ci.yaml", []byte(`{{ include "ci/b.yaml" . }}`), Inputs{}, r)
	if err == nil || err.Error() != `ci.yaml:1: include "ci/b.yaml": no such file at 3f2a9c0e41b7` {
		t.Errorf("missing file: %v", err)
	}

	_, err = r.Remote(t.Context(), remoteRepo, "go.yaml", commit)
	if !errors.Is(err, ErrNoRemote) {
		t.Errorf("remote: %v", err)
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Expand(ctx, "ci.yaml", []byte(`{{ include "ci/a.yaml" . }}`), Inputs{}, MapResolver{Files: map[string]string{"ci/a.yaml": "a"}})
	if err == nil || err.Error() != `ci.yaml:1: include "ci/a.yaml": context canceled` {
		t.Errorf("got %v", err)
	}
}
