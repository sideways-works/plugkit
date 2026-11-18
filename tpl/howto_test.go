package tpl

import (
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"
)

const lintRemote = "https://github.com/sideways-works/ci-shared.git//lint.yaml@3f2a9c0e41b7d5c1a0e9f2b6c4d8e7a1b3c5d7e9"

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}

	return b
}

// TestHowTo expands the examples of the spec's how-to section.
func TestHowTo(t *testing.T) {
	resolver := MapResolver{Remotes: map[string]string{lintRemote: string(readTestdata(t, "lint.yaml"))}}

	tests := []struct {
		name string
		file string
		in   Inputs
		want string
	}{
		{
			name: "a job per package",
			file: "matrix.yaml",
			want: "matrix.out.yaml",
		},
		{
			name: "staging is the default",
			file: "deploy.yaml",
			in:   Inputs{Env: map[string]string{"DEPLOY_REGION": "fsn1"}},
			want: "deploy.staging.out.yaml",
		},
		{
			name: "-D ENVIRONMENT=production adds the announce job",
			file: "deploy.yaml",
			in: Inputs{
				Env:     map[string]string{"DEPLOY_REGION": "fsn1"},
				Defines: map[string]any{"ENVIRONMENT": "production"},
			},
			want: "deploy.production.out.yaml",
		},
		{
			name: "a job shared by a remote include",
			file: "shared.yaml",
			want: "shared.out.yaml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Expand(t.Context(), "ci.yaml", readTestdata(t, tt.file), tt.in, resolver)
			if err != nil {
				t.Fatal(err)
			}

			want := string(readTestdata(t, tt.want))
			if string(res.Text) != want {
				t.Errorf("expanded to\n%s\nwant\n%s", res.Text, want)
			}

			var doc map[string]any

			err = yaml.Unmarshal(res.Text, &doc)
			if err != nil {
				t.Errorf("the expansion isn't YAML: %v", err)
			}
		})
	}
}

func TestMatrixJobs(t *testing.T) {
	res, err := Expand(t.Context(), "ci.yaml", readTestdata(t, "matrix.yaml"), Inputs{}, MapResolver{})
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Pipelines map[string]struct {
			Jobs map[string]any `yaml:"jobs"`
		} `yaml:"pipelines"`
	}

	err = yaml.Unmarshal(res.Text, &doc)
	if err != nil {
		t.Fatal(err)
	}

	for _, job := range []string{"test-api", "test-worker", "test-cli"} {
		if _, ok := doc.Pipelines["main"].Jobs[job]; !ok {
			t.Errorf("no job %s in %v", job, doc.Pipelines["main"].Jobs)
		}
	}
}
