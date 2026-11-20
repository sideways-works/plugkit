package gitsafe

import (
	"errors"
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

	env := Env(base, "/scratch/home", Credential{})
	for _, want := range []string{"PATH=/usr/bin", "LANG=C", "HOME=/scratch/home", "GIT_ALLOW_PROTOCOL=https:ssh", "GIT_TERMINAL_PROMPT=0",
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

	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "GIT_CONFIG_COUNT") }) {
		t.Error("a credential helper with no credential")
	}

	env = Env(base, "/scratch/home", Credential{Token: "s3cret"})
	if !slices.Contains(env, envToken+"=s3cret") || !slices.Contains(env, envUser+"=x-access-token") || !slices.Contains(env, "GIT_CONFIG_KEY_0=credential.helper") {
		t.Fatalf("the credential: %q", env)
	}
}
