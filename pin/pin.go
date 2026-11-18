// Package pin names a plugin's release and how its files are checked: a sha256 per platform, or
// the sha256 of the release's SHA256SUMS. A file that doesn't match its pin never runs.
package pin

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"runtime"
	"strings"
)

// Pin is one release of a plugin.
type Pin struct {
	// Name is the plugin's, for messages.
	Name string
	// Repository is the URL a person would clone (https or ssh); owner/name is GitHub's shorthand.
	Repository string
	// Path is a monorepo plugin's directory (tools/lint): its tags are <path>/vX.Y.Z, a
	// repository plugin's vX.Y.Z.
	Path    string
	Version string
	// Program names the release's files (<program>-<os>-<arch>, <program>.mjs); empty derives it
	// from Path (kind/name gives name-kind) or else the repository's name.
	Program string
	SHA256  Checksums
	// Script is the checksum of the release's <program>.mjs, the one file a Node plugin is
	// released as, the same on every platform.
	Script string
	Sums   string
	// URL overrides where files are fetched from; %s is replaced by the repository, the tag and
	// the file's name, in that order.
	URL string
}

// Tag is the git tag of the release.
func (p Pin) Tag() string {
	if p.Path == "" {
		return p.Version
	}

	return strings.Trim(p.Path, "/") + "/" + p.Version
}

func (p Pin) ProgramName() string {
	if p.Program != "" {
		return p.Program
	}

	if p.Path != "" {
		dir, name := path.Split(strings.Trim(p.Path, "/"))
		if dir == "" {
			return name
		}

		return name + "-" + path.Base(dir)
	}

	return strings.TrimSuffix(path.Base(strings.TrimSuffix(p.Repository, "/")), ".git")
}

// Binary is the release's file for platform.
func (p Pin) Binary(platform string) string { return p.ProgramName() + "-" + platform }

// ScriptFile is the release's one file of a Node plugin.
func (p Pin) ScriptFile() string { return p.ProgramName() + ".mjs" }

// Checksums are a release's SHA-256 by platform (linux-amd64, darwin-arm64); AnyPlatform's
// holds for whichever platform runs it.
type Checksums map[string]string

const AnyPlatform = "*"

func (c Checksums) For(platform string) (string, bool) {
	if sum, ok := c[platform]; ok {
		return sum, true
	}

	sum, ok := c[AnyPlatform]

	return sum, ok
}

// Platform is the platform this program runs on, as checksums name it.
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Listed reads a SHA256SUMS file: each file's checksum, in lower case.
func Listed(sums []byte) map[string]string {
	out := map[string]string{}

	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		sum, file, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if ok {
			out[strings.TrimPrefix(strings.TrimSpace(file), "*")] = strings.ToLower(sum)
		}
	}

	return out
}

// FileSHA256 is a file's SHA-256 in hex.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // a pinned file the caller names
	if err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}
	defer f.Close() //nolint:errcheck // read only

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("checksum: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// The kinds of git server a release's files come from.
const (
	GitHub = "github"
	GitLab = "gitlab"
	Git    = "git"
)

var ErrRepository = errors.New("repository")

// Repo is where a pin's repository lives: the server's kind and web origin, and the project's
// path on it (owner/name, or group/subgroup/name).
type Repo struct {
	Forge   string
	Origin  string
	Project string
}

// ParseRepository reads owner/name as GitHub, and an https, ssh or scp-form (git@host:path) URL as
// the forge its host is: github.com and gitlab.com by name, any other by forges, else plain git.
func ParseRepository(repo string, forges map[string]string) (Repo, error) {
	origin, host, project, err := split(repo)
	if err != nil {
		return Repo{}, err
	}

	project = strings.TrimSuffix(strings.Trim(project, "/"), ".git")
	if project == "" || strings.Contains(project, "..") {
		return Repo{}, fmt.Errorf("%w %q: no project path", ErrRepository, repo)
	}

	forge := forges[host]
	if forge == "" {
		switch host {
		case "github.com":
			forge = GitHub
		case "gitlab.com":
			forge = GitLab
		default:
			forge = Git
		}
	}

	return Repo{Forge: forge, Origin: origin, Project: project}, nil
}

// split is a repository's web origin, host and project path: an http(s) URL keeps its scheme and
// port, an ssh or scp-form one is https on the default port.
func split(repo string) (string, string, string, error) {
	if u, err := url.Parse(repo); err == nil && u.Scheme != "" && u.Host != "" {
		host := strings.ToLower(u.Hostname())

		switch u.Scheme {
		case "https", "http":
			return u.Scheme + "://" + strings.ToLower(u.Host), host, u.Path, nil
		case "ssh", "git+ssh", "ssh+git":
			return "https://" + host, host, u.Path, nil
		}

		return "", "", "", fmt.Errorf("%w %q: scheme %q", ErrRepository, repo, u.Scheme)
	}

	if strings.Contains(repo, "://") {
		return "", "", "", fmt.Errorf("%w %q: no host", ErrRepository, repo)
	}

	// scp form: [user@]host:path, with no slash before the colon
	if at := strings.Index(repo, ":"); at > 0 && !strings.Contains(repo[:at], "/") {
		host := repo[:at]
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}

		host = strings.ToLower(host)

		return "https://" + host, host, repo[at+1:], nil
	}

	if owner, name, ok := strings.Cut(repo, "/"); ok && owner != "" && name != "" && !strings.Contains(name, "/") {
		return "https://github.com", "github.com", repo, nil
	}

	return "", "", "", fmt.Errorf("%w %q: neither owner/name nor a URL to clone", ErrRepository, repo)
}
