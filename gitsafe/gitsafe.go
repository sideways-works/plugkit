// Package gitsafe runs git against a remote an untrusted caller names, such as a URL a person typed
// into a server: only https and ssh, never an option or a local path, and nothing of the host's
// git config, credential helpers or ssh keys. The one credential is the caller's own.
package gitsafe

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var ErrURL = errors.New("not a remote git may fetch from")

// SSHCommand is ssh with none of the host's keys, agent, config or prompts: ssh reads ~/.ssh even
// when HOME is moved, so the options say it.
const SSHCommand = "ssh -o IdentitiesOnly=yes -o IdentityFile=/dev/null -o BatchMode=yes -F /dev/null"

// scp is git's scp-like form, [user@]host:path, whose host has no slash before the colon.
var scp = regexp.MustCompile(`^(?:[A-Za-z0-9._-]+@)?([A-Za-z0-9.-]+):(.+)$`)

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
		if err != nil || (p.Scheme != "https" && p.Scheme != "ssh") || p.Hostname() == "" || strings.HasPrefix(p.Host, "-") || strings.HasPrefix(p.Hostname(), "-") {
			return fmt.Errorf("%w: %q: only https:// and ssh:// URLs, or user@host:path", ErrURL, u)
		}

		return nil
	}

	m := scp.FindStringSubmatch(u)
	if m == nil || strings.HasPrefix(m[1], "-") || strings.HasPrefix(m[2], "-") {
		return fmt.Errorf("%w: %q: only https:// and ssh:// URLs, or user@host:path", ErrURL, u)
	}

	return nil
}

// Credential is the https credential the caller gives: a token, and the user name its host wants
// with it (x-access-token when empty).
type Credential struct {
	Username, Token string
}

const (
	envUser  = "PLUGKIT_GIT_USERNAME"
	envToken = "PLUGKIT_GIT_TOKEN" //nolint:gosec // the variable's name, not a credential
	// helper answers git's request for a credential from the environment, so the token is in
	// neither the URL nor any process's arguments.
	helper = `!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$` + envUser + `" "$` + envToken + `"; }; f`
)

// Env is git's whole environment for an untrusted remote: base without its git and ssh variables,
// home as HOME, only https and ssh allowed, no system or global config, no prompt, ssh with nothing
// of the host's, and cred, when it has a token, as the only credential.
func Env(base []string, home string, cred Credential) []string {
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
		"GIT_SSH_COMMAND="+SSHCommand,
	)

	if cred.Token != "" {
		user := cred.Username
		if user == "" {
			user = "x-access-token"
		}

		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="+helper,
			envUser+"="+user, envToken+"="+cred.Token)
	}

	return env
}
