// Package gitsafe runs git against a remote an untrusted caller names, such as a URL a person typed
// into a server: only https and ssh, never an option or a local path, and nothing of the host's
// git config, credential helpers or ssh keys. The one credential is the caller's own.
package gitsafe

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var ErrURL = errors.New("not a remote git may fetch from")

// KnownHosts is the known_hosts file in home that ssh checks a host's key against.
const KnownHosts = "known_hosts"

// SSHCommand is ssh with none of the account's keys, agent, config, known hosts or prompts, and
// the known hosts of home alone, which a host key must be in: ssh reads ~/.ssh even when HOME is
// moved, so the options say it.
func SSHCommand(home string) string {
	return "ssh -o IdentitiesOnly=yes -o IdentityFile=/dev/null -o BatchMode=yes -F /dev/null" +
		" -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile='" + filepath.Join(home, KnownHosts) + "' -o StrictHostKeyChecking=yes"
}

var errHome = errors.New("a home whose path holds a quote")

// Prepare writes home's known_hosts with the caller's lines: without any, ssh knows no host and an
// ssh remote fails closed.
func Prepare(home, knownHosts string) error {
	if strings.ContainsAny(home, "'\n") {
		return fmt.Errorf("%w: %q", errHome, home)
	}

	if err := os.WriteFile(filepath.Join(home, KnownHosts), []byte(knownHosts), 0o600); err != nil {
		return fmt.Errorf("known hosts: %w", err)
	}

	return nil
}

// scp is git's scp-like form, [user@]host:path, whose host has no slash before the colon.
var scp = regexp.MustCompile(`^(?:([A-Za-z0-9._-]+)@)?([A-Za-z0-9.-]+):(.+)$`)

// CheckURL accepts https://host/..., ssh://host/... and user@host:path, and refuses an option, a
// transport helper (ext::, any ::), file:// and a local path.
func CheckURL(u string) error {
	switch {
	case u == "" || strings.HasPrefix(u, "-"):
		return fmt.Errorf("%w: %q starts with a dash or is empty", ErrURL, u)
	case strings.Contains(u, "::"):
		return fmt.Errorf("%w: %q names a transport helper", ErrURL, u)
	case strings.ContainsAny(u, " \t\r\n\\"):
		return fmt.Errorf("%w: %q holds a space or a backslash", ErrURL, u)
	}

	if strings.Contains(u, "://") {
		p, err := url.Parse(u)
		// ssh takes a user name or host starting with a dash for an option
		if err != nil || (p.Scheme != "https" && p.Scheme != "ssh") || p.Hostname() == "" || strings.HasPrefix(p.Host, "-") || strings.HasPrefix(p.Hostname(), "-") ||
			p.User != nil && strings.HasPrefix(p.User.Username(), "-") {
			return fmt.Errorf("%w: %q: only https:// and ssh:// URLs, or user@host:path", ErrURL, u)
		}

		return nil
	}

	m := scp.FindStringSubmatch(u)
	if m == nil || strings.HasPrefix(m[1], "-") || strings.HasPrefix(m[2], "-") || strings.HasPrefix(m[3], "-") {
		return fmt.Errorf("%w: %q: only https:// and ssh:// URLs, or user@host:path", ErrURL, u)
	}

	return nil
}

// Credential is the https credential the caller gives: a token, the user name its host wants with
// it (x-access-token when empty), and the host it is for, host[:port] (the remote's when empty).
// git gets it for that host over https alone, so a remote elsewhere, or a redirect, never sees it.
type Credential struct {
	Host, Username, Token string
}

const (
	envHost  = "PLUGKIT_GIT_HOST"
	envUser  = "PLUGKIT_GIT_USERNAME"
	envToken = "PLUGKIT_GIT_TOKEN" //nolint:gosec // the variable's name, not a credential
	// helper answers git's request for a credential from the environment, so the token is in
	// neither the URL nor any process's arguments, and only when git asks for https on the host the
	// credential is for: git writes protocol= and host= lines, host with its port when it has one.
	helper = `!f() { test "$1" = get || return 0; p=; h=; while IFS= read -r l; do case "$l" in protocol=*) p="${l#protocol=}";; host=*) h="${l#host=}";; "") break;; esac; done; ` +
		`test "$p" = https && test -n "$h" && test "$h" = "$` + envHost + `" && printf 'username=%s\npassword=%s\n' "$` + envUser + `" "$` + envToken + `"; }; f`
)

// Env is git's whole environment for remote, an untrusted URL CheckURL accepted: base without its
// git and ssh variables, home as HOME, only https and ssh allowed, no system or global config, no
// prompt, no redirect followed, ssh with nothing of the host's, and cred, when it has a token, as
// the only credential, for its host alone.
func Env(base []string, home, remote string, cred Credential) []string {
	env := make([]string, 0, len(base)+12)

	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "SSH_") || k == "HOME" || k == "XDG_CONFIG_HOME" || k == envUser || k == envToken {
			continue
		}

		env = append(env, kv)
	}

	env = append(env,
		"HOME="+home, "XDG_CONFIG_HOME="+home,
		"GIT_ALLOW_PROTOCOL=https:ssh", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_SSH_COMMAND="+SSHCommand(home),
	)

	// a redirect could take the fetch, and a credential, to another host
	config := [][2]string{{"http.followRedirects", "false"}}

	if host := credentialHost(remote, cred); cred.Token != "" && host != "" {
		user := cred.Username
		if user == "" {
			user = "x-access-token"
		}

		config = append(config, [2]string{"credential.helper", helper})
		env = append(env, envHost+"="+host, envUser+"="+user, envToken+"="+cred.Token)
	}

	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(config)))
	for i, kv := range config {
		env = append(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+kv[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+kv[1])
	}

	return env
}

// credentialHost is the host git may get cred for: the credential's own, or the remote's, and none
// for a remote that isn't https, whose credential is ssh's.
func credentialHost(remote string, cred Credential) string {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" {
		return ""
	}

	if cred.Host != "" {
		return strings.ToLower(cred.Host)
	}

	return strings.ToLower(u.Host)
}
