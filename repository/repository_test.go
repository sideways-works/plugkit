package repository

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/cgi" //nolint:gosec // git's own http-backend, serving the test's repository
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sideways-works/plugkit/gitsafe"
)

const image = "golang@sha256:564e366a28ad1d70f460a2b97d1d299a562f08707eb0ecb24b659e5bd6c108e1"

func pluginManifest(name, kind, description string) string {
	return "name: " + name + "\nkind: " + kind + "\nprogram: " + name + "\ndescription: " + description + "\nlogo: logo.svg\n" +
		"build: {image: \"" + image + "\", run: [go, build, -o, \"{out}/" + name + "\", .]}\nplatforms: [linux/amd64]\n"
}

type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()

	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "-q", "-b", "main")

	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()

	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", r.dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

// commit writes files and commits them, returning the commit.
func (r *repo) commit(files map[string]string) string {
	r.t.Helper()

	for name, body := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			r.t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}

	r.git("add", "-A")
	r.git("commit", "-q", "-m", "change")

	return r.git("rev-parse", "HEAD")
}

// bare is a bare clone of the repository with its tags, as a server holds it.
func (r *repo) bare() string {
	r.t.Helper()

	dir := filepath.Join(r.t.TempDir(), "plugins.git")
	r.git("clone", "-q", "--bare", r.dir, dir)

	return dir
}

func monorepo(t *testing.T) (string, map[string]string) {
	t.Helper()

	r := newRepo(t)
	commits := map[string]string{}

	commits["0.1.0"] = r.commit(map[string]string{
		"repository-manifest.yaml": "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n" +
			"  - {manifest: step/go-test/manifest.yaml}\n  - {manifest: sbom/x/manifest.yaml, name: x-sbom, description: Lists x}\n",
		"step/go-test/manifest.yaml": pluginManifest("go-test", "step", "Runs go test, first"),
		"sbom/x/manifest.yaml":       pluginManifest("x", "sbom", "the manifest's"),
	})
	r.git("tag", "step/go-test/v0.1.0")
	r.git("tag", "sbom/x/v1.0.0")

	commits["0.2.0"] = r.commit(map[string]string{"step/go-test/manifest.yaml": pluginManifest("go-test", "step", "Runs go test")})
	r.git("tag", "-a", "-m", "annotated", "step/go-test/v0.2.0")

	commits["0.3.0-rc.1"] = r.commit(map[string]string{"step/go-test/README": "next"})
	r.git("tag", "step/go-test/v0.3.0-rc.1")
	r.git("tag", "step/go-test/not-a-version")
	r.git("tag", "elsewhere/v9.9.9")

	return r.bare(), commits
}

func TestAMonorepoListsEachPluginWithItsTags(t *testing.T) {
	url, commits := monorepo(t)

	got, err := List(context.Background(), url, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}

	goTest, x := got[0], got[1]

	switch {
	case goTest.Path != "step/go-test" || goTest.Name != "go-test" || goTest.Kind != "step" || goTest.Description != "Runs go test" || goTest.Logo != "step/go-test/logo.svg":
		t.Fatalf("go-test: %+v", goTest)
	case x.Name != "x-sbom" || x.Description != "Lists x" || x.Kind != "sbom":
		t.Fatalf("x: %+v", x)
	}

	want := []Version{{"0.2.0", commits["0.2.0"]}, {"0.1.0", commits["0.1.0"]}}
	if !slices.Equal(goTest.Versions, want) {
		t.Fatalf("go-test's versions %+v, want %+v", goTest.Versions, want)
	}

	if !slices.Equal(x.Versions, []Version{{"1.0.0", commits["0.1.0"]}}) {
		t.Fatalf("x's versions %+v", x.Versions)
	}

	pre, err := List(context.Background(), url, Options{Prereleases: true})
	if err != nil || len(pre[0].Versions) != 3 || pre[0].Versions[0] != (Version{"0.3.0-rc.1", commits["0.3.0-rc.1"]}) {
		t.Fatalf("with prereleases: %+v %v", pre, err)
	}
}

func TestAtReadsThePluginsManifestAtItsTag(t *testing.T) {
	url, commits := monorepo(t)

	m, commit, err := At(context.Background(), url, "step/go-test", "0.1.0")
	if err != nil || commit != commits["0.1.0"] || m.Description != "Runs go test, first" {
		t.Fatalf("0.1.0: %+v %s %v", m, commit, err)
	}

	m, commit, err = At(context.Background(), url, "step/go-test", "v0.2.0")
	if err != nil || commit != commits["0.2.0"] || m.Description != "Runs go test" {
		t.Fatalf("0.2.0: %+v %s %v", m, commit, err)
	}

	if _, _, err := At(context.Background(), url, "step/go-test", "0.9.0"); !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "no tag step/go-test/v0.9.0") {
		t.Fatal(err)
	}

	if _, _, err := At(context.Background(), url, "step/nothing", "0.1.0"); !errors.Is(err, ErrRepository) {
		t.Fatal(err)
	}
}

func TestARootManifestWithPluginsIsTheRepositorysManifest(t *testing.T) {
	r := newRepo(t)
	commit := r.commit(map[string]string{
		"manifest.yaml":   "repository: 1\nplugins:\n  - {manifest: a/manifest.yaml}\n  - {manifest: b/manifest.yaml}\n",
		"a/manifest.yaml": pluginManifest("a", "driver", "A"),
		"b/manifest.yaml": pluginManifest("b", "driver", "B"),
	})
	r.git("tag", "a-v1.2.3")
	r.git("tag", "b-v0.0.1")

	got, err := List(context.Background(), r.bare(), Options{})
	if err != nil || len(got) != 2 || !slices.Equal(got[0].Versions, []Version{{"1.2.3", commit}}) || !slices.Equal(got[1].Versions, []Version{{"0.0.1", commit}}) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestARepositoryOfOnePluginIsItsRootManifest(t *testing.T) {
	r := newRepo(t)
	first := r.commit(map[string]string{"manifest.yaml": pluginManifest("lint", "driver", "Lints")})
	r.git("tag", "v1.0.0")
	r.commit(map[string]string{"README": "x"})
	r.git("tag", "v1.1.0")

	url := r.bare()

	got, err := List(context.Background(), url, Options{})
	if err != nil || len(got) != 1 || got[0].Path != "." || got[0].Name != "lint" || len(got[0].Versions) != 2 || got[0].Versions[1].Commit != first {
		t.Fatalf("%+v %v", got, err)
	}

	if m, commit, err := At(context.Background(), url, ".", "1.0.0"); err != nil || commit != first || m.Name != "lint" {
		t.Fatalf("at 1.0.0: %v", err)
	}
}

func TestARepositoryWithoutAManifestSaysSo(t *testing.T) {
	r := newRepo(t)
	r.commit(map[string]string{"README": "x"})

	if _, err := List(context.Background(), r.bare(), Options{}); !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "neither repository-manifest.yaml nor manifest.yaml") {
		t.Fatal(err)
	}
}

func TestRepositoryManifestsAreRefused(t *testing.T) {
	const good = "repository: 1\ntags: \"{path}/v{version}\"\nplugins:\n  - {manifest: a/manifest.yaml, name: a}\n  - {manifest: b/manifest.yaml}\n"

	cases := map[string]struct{ from, to, says string }{
		"another format":         {"repository: 1", "repository: 2", "2 is no format"},
		"no format":              {"repository: 1\n", "", "0 is no format"},
		"an unknown key":         {"plugins:", "owner: me\nplugins:", "field owner not found"},
		"an unknown entry key":   {"name: a}", "name: a, tag: x}", "field tag not found"},
		"a path up":              {"a/manifest.yaml", "../a/manifest.yaml", "without .."},
		"an absolute path":       {"a/manifest.yaml", "/a/manifest.yaml", "without .."},
		"another file":           {"a/manifest.yaml", "a/plugin.yaml", "a manifest.yaml"},
		"a path twice":           {"b/manifest.yaml", "a/manifest.yaml", "a is listed twice"},
		"a name twice":           {"{manifest: b/manifest.yaml}", "{manifest: b/manifest.yaml, name: a}", "the name a is listed twice"},
		"a logo up":              {"name: a}", "name: a, logo: ../x.svg}", "logo"},
		"an unknown placeholder": {"{path}/v{version}", "{kind}/v{version}", "{kind} is no placeholder"},
		"no version":             {"{path}/v{version}", "{path}/latest", "needs {version} once"},
		"no plugins":             {"plugins:\n  - {manifest: a/manifest.yaml, name: a}\n  - {manifest: b/manifest.yaml}\n", "plugins: []\n", "plugins lists none"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(good, c.from) {
				t.Fatalf("the fixture has no %q", c.from)
			}

			_, err := Parse([]byte(strings.Replace(good, c.from, c.to, 1)))
			if !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("got %v, want %q", err, c.says)
			}
		})
	}

	r, err := Parse([]byte(good))
	if err != nil || r.Plugins[1].Path() != "b" {
		t.Fatalf("%+v %v", r, err)
	}

	if one, err := Parse([]byte("repository: 1\nplugins: [{manifest: manifest.yaml}]\n")); err != nil || one.Tags != "v{version}" {
		t.Fatalf("one plugin at the root: %+v %v", one, err)
	}

	if many, err := Parse([]byte("repository: 1\nplugins: [{manifest: a/manifest.yaml}]\n")); err != nil || many.Tags != "{name}-v{version}" {
		t.Fatalf("a plugin in a directory: %+v %v", many, err)
	}
}

func TestAnUntrustedURLIsRefusedBeforeGitRuns(t *testing.T) {
	url, _ := monorepo(t)

	for _, u := range []string{url, "file://" + url, "-uhack", "ext::sh -c touch% x", "./plugins.git"} {
		if _, err := List(context.Background(), u, Options{Untrusted: true}); !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "not a remote git may fetch from") {
			t.Errorf("%s: %v", u, err)
		}

		if _, _, err := AtWith(context.Background(), u, "step/go-test", "0.1.0", Options{Untrusted: true}); !errors.Is(err, ErrRepository) {
			t.Errorf("At %s: %v", u, err)
		}
	}

	if _, err := List(context.Background(), url, Options{}); err != nil {
		t.Fatalf("a trusted local repository: %v", err)
	}
}

// git reaches an ssh remote through an ssh that sees nothing of the host's keys, config or agent.
func TestAnUntrustedSSHRemoteGetsNoneOfTheHostsSSH(t *testing.T) {
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "ssh.log")

	fake := "#!/bin/sh\n{ printf '%s\\n' \"$@\"; env; } > " + log + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SSH_AUTH_SOCK", "/host/agent")
	t.Setenv("GIT_CONFIG_GLOBAL", "/host/gitconfig")

	opt := Options{Untrusted: true, env: []string{"PATH=" + bin + ":" + os.Getenv("PATH")}}
	if _, err := List(context.Background(), "ssh://git@plugins.example.invalid/o/r.git", opt); err == nil {
		t.Fatal("the stand-in ssh fetched something")
	}

	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	got := string(data)
	for _, want := range []string{"IdentitiesOnly=yes\n", "IdentityFile=/dev/null\n", "BatchMode=yes\n", "-F\n/dev/null\n", "git@plugins.example.invalid\n",
		"GIT_CONFIG_GLOBAL=/dev/null\n", "GIT_CONFIG_NOSYSTEM=1\n", "GIT_ALLOW_PROTOCOL=https:ssh\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh saw no %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, "SSH_AUTH_SOCK") || strings.Contains(got, "/host/gitconfig") {
		t.Errorf("ssh saw the host's agent or config:\n%s", got)
	}
}

// An untrusted https remote is fetched with the caller's credential alone, within the size cap.
func TestAnUntrustedHTTPSRemoteTakesTheCallersCredential(t *testing.T) {
	url, commits := monorepo(t)

	execPath, err := exec.CommandContext(context.Background(), "git", "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}

	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(url), "GIT_HTTP_EXPORT_ALL=1"},
	}

	var (
		mu    sync.Mutex
		seen  int
		creds []string
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++

		creds = append(creds, r.Header.Get("Authorization"))
		mu.Unlock()

		if user, pass, ok := r.BasicAuth(); !ok || user != "x-access-token" || pass != "s3cret" {
			w.Header().Set("WWW-Authenticate", `Basic realm="plugins"`)
			http.Error(w, "who are you", http.StatusUnauthorized)

			return
		}

		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	remote := srv.URL + "/plugins.git"
	opt := Options{Untrusted: true, Credential: gitsafe.Credential{Token: "s3cret"}, env: []string{"GIT_SSL_CAINFO=" + ca}}

	got, err := List(context.Background(), remote, opt)
	if err != nil || len(got) != 2 || got[0].Versions[0].Commit != commits["0.2.0"] {
		t.Fatalf("%+v %v", got, err)
	}

	if _, err := List(context.Background(), remote, Options{Untrusted: true, env: opt.env}); err == nil {
		t.Fatal("fetched with no credential")
	}

	opt.MaxBytes = 1 << 10
	if _, err := List(context.Background(), remote, opt); !errors.Is(err, ErrRepository) || !strings.Contains(err.Error(), "over the size limit") {
		t.Fatal(err)
	}

	opt.MaxBytes = 0

	leaked := func() bool {
		mu.Lock()
		defer mu.Unlock()

		return slices.ContainsFunc(creds, func(c string) bool { return c != "" && c != "Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0" })
	}

	// a credential for another host is never offered here
	mu.Lock()
	creds = nil
	mu.Unlock()

	other := opt
	other.Credential.Host = "git.example.com"

	if _, err := List(context.Background(), remote, other); err == nil {
		t.Fatal("fetched with another host's credential")
	}

	mu.Lock()
	gotToken := slices.Contains(creds, "Basic eC1hY2Nlc3MtdG9rZW46czNjcmV0")
	mu.Unlock()

	if gotToken || leaked() {
		t.Fatalf("this host got another host's token: %q", creds)
	}

	// a redirect elsewhere is never followed, so the second server neither serves nor sees the token
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.RequestURI(), http.StatusFound) //nolint:gosec // the test's own servers
	}))
	t.Cleanup(elsewhere.Close)

	mu.Lock()
	before := seen
	mu.Unlock()

	if _, err := List(context.Background(), elsewhere.URL+"/plugins.git", opt); err == nil {
		t.Fatal("a redirect was followed")
	}

	mu.Lock()
	after := seen
	mu.Unlock()

	if after != before {
		t.Fatalf("the redirect reached the second server %d times", after-before)
	}
}
