package pin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTagAndProgram(t *testing.T) {
	for _, tc := range []struct {
		p            Pin
		tag, program string
	}{
		{Pin{Repository: "acme/thing", Version: "v1.2.3"}, "v1.2.3", "thing"},
		{Pin{Repository: "https://gitlab.com/acme/thing.git", Version: "v1.2.3"}, "v1.2.3", "thing"},
		{Pin{Repository: "acme/plugins", Path: "tools/lint", Version: "v0.1.0"}, "tools/lint/v0.1.0", "lint-tools"},
		{Pin{Repository: "acme/plugins", Path: "/solo/", Version: "v0.1.0"}, "solo/v0.1.0", "solo"},
		{Pin{Repository: "acme/pipelinegen-sbom-go", Program: "go-sbom", Version: "v2.0.0"}, "v2.0.0", "go-sbom"},
	} {
		if tc.p.Tag() != tc.tag || tc.p.ProgramName() != tc.program {
			t.Errorf("%+v: tag %s, program %s", tc.p, tc.p.Tag(), tc.p.ProgramName())
		}
	}

	p := Pin{Repository: "acme/thing"}
	if p.Binary("linux-amd64") != "thing-linux-amd64" || p.ScriptFile() != "thing.mjs" {
		t.Error(p.Binary("linux-amd64"), p.ScriptFile())
	}
}

func TestParseRepository(t *testing.T) {
	forges := map[string]string{"git.example.com": GitLab}

	for _, tc := range []struct {
		in   string
		want Repo
	}{
		{"acme/thing", Repo{GitHub, "https://github.com", "acme/thing"}},
		{"https://github.com/acme/thing.git", Repo{GitHub, "https://github.com", "acme/thing"}},
		{"git@github.com:acme/thing.git", Repo{GitHub, "https://github.com", "acme/thing"}},
		{"ssh://git@GitHub.com/acme/thing", Repo{GitHub, "https://github.com", "acme/thing"}},
		{"https://gitlab.com/group/sub/thing", Repo{GitLab, "https://gitlab.com", "group/sub/thing"}},
		{"git@git.example.com:group/thing.git", Repo{GitLab, "https://git.example.com", "group/thing"}},
		{"https://code.example.org/r/thing.git", Repo{Git, "https://code.example.org", "r/thing"}},
		{"http://Git.Example.com:8080/group/thing", Repo{GitLab, "http://git.example.com:8080", "group/thing"}},
	} {
		got, err := ParseRepository(tc.in, forges)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v", tc.in, got, err)
		}
	}

	for _, bad := range []string{"thing", "a/b/c", "file:///srv/thing.git", "https://github.com/", "ftp://example.com/a/b", "https://github.com/a/../b"} {
		if r, err := ParseRepository(bad, nil); !errors.Is(err, ErrRepository) {
			t.Errorf("%s: %+v, %v", bad, r, err)
		}
	}
}

func TestChecksumsAndSums(t *testing.T) {
	c := Checksums{"linux-amd64": "a", AnyPlatform: "b"}
	if s, _ := c.For("linux-amd64"); s != "a" {
		t.Error(s)
	}

	if s, _ := c.For("darwin-arm64"); s != "b" {
		t.Error(s)
	}

	if _, ok := (Checksums{"linux-amd64": "a"}).For("darwin-arm64"); ok {
		t.Error("another platform's checksum")
	}

	l := Listed([]byte("ABC  x-linux-amd64\r\ndef *x.mjs\n\nbroken\n"))
	if l["x-linux-amd64"] != "abc" || l["x.mjs"] != "def" || len(l) != 2 {
		t.Errorf("listed %v", l)
	}

	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}

	if sum, err := FileSHA256(f); err != nil || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Errorf("%s, %v", sum, err)
	}
}
