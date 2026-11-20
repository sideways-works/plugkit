package gitsafe

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestOnlyHTTPSAndSSHRemotesAreAccepted(t *testing.T) {
	for _, u := range []string{"https://github.com/o/r", "https://git.example.com:8443/o/r.git", "ssh://git@host/o/r.git", "git@github.com:o/r.git", "host.example:o/r"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}

	for _, u := range []string{
		"", "-uhack", "--upload-pack=touch /x", "ext::sh -c touch% /x", "fd::3", "https://host/o/r::x",
		"file:///etc/passwd", "/srv/repo.git", "./repo", "repo", "../repo", "http://host/o/r", "git://host/o/r",
		"ssh://-oProxyCommand=touch%20x/r", "git@-oProxyCommand=x:r", "git@host:-r", "https://", "https://host/o r", `C:\repo`,
	} {
		if err := CheckURL(u); !errors.Is(err, ErrURL) {
			t.Errorf("%q accepted: %v", u, err)
		}
	}
}

func TestTheEnvironmentKeepsTheHostsGitOut(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/home/me", "GIT_CONFIG_GLOBAL=/home/me/.gitconfig", "GIT_ASKPASS=/x", "SSH_AUTH_SOCK=/agent", "LANG=C"}

	env := Env(base, "/scratch/home", "https://github.com/o/r", Credential{})
	for _, want := range []string{"PATH=/usr/bin", "LANG=C", "HOME=/scratch/home", "GIT_CONFIG_KEY_0=http.followRedirects", "GIT_CONFIG_VALUE_0=false", "GIT_ALLOW_PROTOCOL=https:ssh", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_SSH_COMMAND=" + SSHCommand} {
		if !slices.Contains(env, want) {
			t.Errorf("no %s in %q", want, env)
		}
	}

	for _, gone := range []string{"HOME=/home/me", "GIT_ASKPASS=/x", "SSH_AUTH_SOCK=/agent", "GIT_CONFIG_GLOBAL=/home/me/.gitconfig"} {
		if slices.Contains(env, gone) {
			t.Errorf("%s kept", gone)
		}
	}

	if !slices.Contains(env, "GIT_CONFIG_COUNT=1") {
		t.Error("a credential helper with no credential")
	}

	env = Env(base, "/scratch/home", "https://git.example.com:8443/o/r", Credential{Token: "s3cret"})
	if !slices.Contains(env, envToken+"=s3cret") || !slices.Contains(env, envHost+"=git.example.com:8443") || !slices.Contains(env, "GIT_CONFIG_KEY_1=credential.helper") {
		t.Fatalf("the credential: %q", env)
	}

	if env := Env(base, "/scratch/home", "ssh://git@host/o/r", Credential{Token: "s3cret"}); slices.Contains(env, envToken+"=s3cret") {
		t.Fatal("an ssh remote got the https token")
	}
}

// The helper answers git with the token only for https on the credential's host.
func TestTheHelperAnswersForItsHostAlone(t *testing.T) {
	ask := func(request string) string {
		t.Helper()

		// git runs a "!" helper as a shell command with the action after it
		cmd := exec.CommandContext(t.Context(), "sh", "-c", strings.TrimPrefix(helper, "!")+" get")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), envHost + "=git.example.com:8443", envUser + "=me", envToken + "=s3cret"}
		cmd.Stdin = strings.NewReader(request)

		out, err := cmd.Output()
		if err != nil {
			if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() < 0 {
				t.Fatal(err)
			}
		}

		return string(out)
	}

	if got := ask("protocol=https\nhost=git.example.com:8443\npath=o/r\n\n"); got != "username=me\npassword=s3cret\n" {
		t.Fatalf("its own host: %q", got)
	}

	for _, request := range []string{
		"protocol=https\nhost=evil.example\n\n", "protocol=https\nhost=git.example.com\n\n",
		"protocol=http\nhost=git.example.com:8443\n\n", "host=git.example.com:8443\n\n", "protocol=https\n\n",
	} {
		if got := ask(request); got != "" {
			t.Errorf("%q got %q", request, got)
		}
	}
}
