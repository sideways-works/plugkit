package exchange

import (
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const (
	schemeHTTPS = "https"
	schemeHTTP  = "http"
)

var errNoOrigin = errors.New("no web origin")

// requestID is what a request's id may be made of: it names nothing on
// disk, but it shows in messages.
var requestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var methods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

// neverLiteral are header names no request may set, declared or not: credentials go only where the host
// puts them, and Host would point a request somewhere its URL doesn't say.
var neverLiteral = []string{"Authorization", "Cookie", "Host"}

func (c *callState) problem(r HTTPRequest) string {
	if !requestID.MatchString(r.ID) {
		return "an id is 1 to 64 letters, digits, '.', '_' or '-'"
	}

	if c.ids[r.ID] {
		return "its id is taken by an earlier request of the call"
	}

	if _, ok := c.e.Purposes[c.call]; !ok {
		return c.call + " sends no requests"
	}

	if !slices.Contains(methods, r.Method) {
		return fmt.Sprintf("method %q", r.Method)
	}

	if p := c.e.urlProblem(r.URL); p != "" {
		return p
	}

	if p := c.e.headerProblem(r.Headers); p != "" {
		return p
	}

	if len(r.Body) > MaxBody {
		return fmt.Sprintf("its body is over %d bytes", MaxBody)
	}

	if r.Auth != nil {
		want := c.e.Purposes[c.call]

		switch {
		case r.Auth.Purpose != want:
			return fmt.Sprintf("it asks for the %q token; %s may carry only the %q one", r.Auth.Purpose, c.call, want)
		case c.e.Tokens[want] == "":
			return fmt.Sprintf("the config gives the %q purpose no token", want)
		case c.e.Auth == nil || c.e.Auth.Header == "":
			return "describe says no header for the token"
		}
	}

	return ""
}

// urlProblem says why a request may not go to raw: only http(s), no user
// in the URL, and an origin the exchange allows.
func (e *Exchange) urlProblem(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Sprintf("url: %v", err)
	}

	if u.User != nil {
		return "its url carries a user or password"
	}

	if u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP {
		return fmt.Sprintf("scheme %q", u.Scheme)
	}

	o, err := NormalOrigin(u.Scheme + "://" + u.Host)
	if err != nil {
		return err.Error()
	}

	if !slices.Contains(e.Hosts, o) {
		return o + " is neither the code remote's origin nor an api host the config confirms"
	}

	return ""
}

func (e *Exchange) headerProblem(h map[string]string) string {
	for name := range h {
		canon := textproto.CanonicalMIMEHeaderKey(name)

		switch {
		case slices.Contains(neverLiteral, canon) || strings.HasPrefix(canon, "Proxy-"):
			return fmt.Sprintf("header %s is never set by a %s", name, e.noun())
		case e.Auth != nil && canon == textproto.CanonicalMIMEHeaderKey(e.Auth.Header):
			return fmt.Sprintf("header %s carries the token, which only the core puts on", name)
		case !slices.ContainsFunc(e.RequestHeaders, func(d string) bool { return textproto.CanonicalMIMEHeaderKey(d) == canon }):
			return fmt.Sprintf("header %s isn't in describe's request_headers", name)
		}
	}

	return ""
}

// NormalOrigin is scheme://host[:port] in lower case, the default port
// left out, from an origin or a URL whose path is ignored.
func NormalOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != schemeHTTPS && u.Scheme != schemeHTTP) {
		return "", fmt.Errorf("%w: %q", errNoOrigin, raw)
	}

	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == schemeHTTPS && port == "443") || (u.Scheme == schemeHTTP && port == "80") {
		port = ""
	}

	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}

	return strings.ToLower(u.Scheme) + "://" + host, nil
}

// Origin is a git remote's web origin, where its host's API is: an http(s) remote keeps scheme and port, an
// ssh or scp-form one (git@host:owner/name) is https on the default port, a file remote has none.
func Origin(remote string) (string, error) {
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		switch u.Scheme {
		case schemeHTTPS, schemeHTTP:
			return NormalOrigin(u.Scheme + "://" + u.Host)
		case "ssh", "git", "git+ssh", "ssh+git":
			return NormalOrigin(schemeHTTPS + "://" + u.Hostname())
		}

		return "", fmt.Errorf("%w: remote %q, scheme %q", errNoOrigin, remote, u.Scheme)
	}

	// scp form: [user@]host:path, with no slash before the colon
	if at := strings.Index(remote, ":"); at > 0 && !strings.Contains(remote[:at], "/") && !strings.Contains(remote, "://") {
		host := remote[:at]
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}

		return NormalOrigin(schemeHTTPS + "://" + host)
	}

	return "", fmt.Errorf("%w: remote %q", errNoOrigin, remote)
}

func (e *Exchange) noun() string {
	if e.Noun == "" {
		return "plugin"
	}

	return e.Noun
}
