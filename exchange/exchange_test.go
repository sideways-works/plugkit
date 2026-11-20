package exchange

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sideways-works/plugkit/plugin"
)

const (
	callChanges  = "changes"
	callStatus   = "status"
	callIdentify = "identify"
	callLinks    = "links"

	purposeRead    = "read"
	purposeChanges = "changes"
	purposeReport  = "report"
	purposeStore   = "store"
)

var purposes = map[string]string{callChanges: purposeChanges, callStatus: purposeReport, "comment": purposeReport}

type script struct {
	Rounds [][]HTTPRequest `json:"rounds"`
	Both   bool            `json:"both,omitempty"`
	Empty  bool            `json:"empty,omitempty"`
	Remote bool            `json:"remote,omitempty"`
}

type seen struct {
	ID      string            `json:"id"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body"`
}

// TestMain lets the test binary act as a plugin when asked, so the
// exchange calls a real executable, round by round, keeping no state.
func TestMain(m *testing.M) {
	if os.Getenv("CONNECTOR_TEST_SERVE") == "" {
		os.Exit(runTests(m))
	}

	handle := func(in []byte) (any, error) {
		var req Request
		if err := json.Unmarshal(in, &req); err != nil {
			return nil, fmt.Errorf("request: %w", err)
		}

		var s script
		if err := json.Unmarshal(req.Args, &s); err != nil {
			return nil, fmt.Errorf("args: %w", err)
		}

		// the round to answer is the one whose requests have no
		// responses yet
		next, sum := 0, 0
		for next < len(s.Rounds) && sum+len(s.Rounds[next]) <= len(req.Responses) {
			sum += len(s.Rounds[next])
			next++
		}

		if next < len(s.Rounds) {
			return Round{Requests: s.Rounds[next]}, nil
		}

		var out []seen

		if s.Remote {
			remote, _ := req.Config["remote"].(string) //nolint:errcheck // absent is the answer

			out = append(out, seen{ID: "remote", Body: remote})
		}

		for _, r := range req.Responses {
			body, err := os.ReadFile(filepath.Join(req.Work, r.BodyFile))
			if err != nil {
				return nil, fmt.Errorf("body: %w", err)
			}

			out = append(out, seen{ID: r.ID, Status: r.Status, Headers: r.Headers, Body: string(body)})
		}

		done, err := json.Marshal(out)
		if err != nil {
			return nil, fmt.Errorf("answer: %w", err)
		}

		switch {
		case s.Both:
			return Round{Done: done, Requests: []HTTPRequest{{ID: "late", Method: "GET", URL: "http://x"}}}, nil
		case s.Empty:
			return Round{}, nil
		}

		return Round{Done: done}, nil
	}

	handlers := map[string]plugin.Handler{}
	for _, c := range []string{callChanges, callStatus, callIdentify, callLinks, "comment"} {
		handlers[c] = handle
	}

	os.Exit(plugin.Serve(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, handlers))
}

const (
	changesToken = "changes-token-value-5d1c"
	reportToken  = "report-token-value-77ab"
)

// fakeConnectorPath is the fake connector's script, written once before any test runs: a test
// that wrote it while a parallel test forked could find it busy ("text file busy"), as the fork
// still held the file open for writing until its exec.
var fakeConnectorPath string

// runTests writes the fake connector, runs the tests and removes it.
func runTests(m *testing.M) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	dir, err := os.MkdirTemp("", "exchange-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}()

	fakeConnectorPath = filepath.Join(dir, "fake-connector")
	if err := os.WriteFile(fakeConnectorPath, []byte("#!/bin/sh\nCONNECTOR_TEST_SERVE=1 exec '"+self+"' \"$@\"\n"), 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	return m.Run()
}

func fakeConnector(t *testing.T) plugin.Plugin {
	t.Helper()

	return plugin.Plugin{Name: "fake-connector", Path: fakeConnectorPath}
}

type host struct {
	*httptest.Server

	mu   sync.Mutex
	got  []*http.Request
	answ func(w http.ResponseWriter, r *http.Request, n int)
}

func newHost(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, n int)) *host {
	t.Helper()

	h := &host{answ: answer}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)

	return h
}

func (h *host) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.got = append(h.got, r.Clone(r.Context()))
	n := len(h.got)
	h.mu.Unlock()

	if h.answ != nil {
		h.answ(w, r, n)

		return
	}

	w.Header().Set("Link", "<next>")
	w.Header().Set("X-Other", "dropped")
	write(w, "body of "+r.URL.Path)
}

func write(w http.ResponseWriter, body string) {
	if _, err := io.WriteString(w, body); err != nil { //nolint:gosec // a test server's own text
		return
	}
}

func (h *host) hits() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.got)
}

func (h *host) request(i int) *http.Request {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.got[i]
}

func exchange(t *testing.T, h *host) *Exchange {
	t.Helper()

	hosts, err := Allowed("connector", h.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	return &Exchange{
		Name: "fake-connector", Noun: "connector", Plugin: fakeConnector(t), Request: plugin.Request{Protocol: 1},
		Purposes: purposes, Auth: &Auth{Header: "Authorization", Scheme: "Bearer"},
		RequestHeaders: []string{"Accept"}, ResponseHeaders: []string{"Link"}, Hosts: hosts,
		Tokens: map[string]string{purposeChanges: changesToken, purposeReport: reportToken},
		Pause:  time.Millisecond, MaxWait: 20 * time.Millisecond,
	}
}

func run(t *testing.T, e *Exchange, call string, s script) ([]seen, error) {
	t.Helper()

	var out []seen

	err := e.Call(t.Context(), call, t.TempDir(), s, &out)

	return out, err
}

func get(id, u string, auth string) HTTPRequest {
	r := HTTPRequest{ID: id, Method: "GET", URL: u}
	if auth != "" {
		r.Auth = &HTTPAuth{Purpose: auth}
	}

	return r
}

func TestExchangeRounds(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	e := exchange(t, h)

	first := get("page-1", h.URL+"/pulls?page=1", purposeChanges)
	first.Headers = map[string]string{"accept": "application/json"}

	out, err := run(t, e, callChanges, script{Rounds: [][]HTTPRequest{
		{first, get("anon", h.URL+"/open", "")},
		{get("page-2", h.URL+"/pulls?page=2", purposeChanges)},
	}})
	if err != nil {
		t.Fatal(err)
	}

	if len(out) != 3 || out[0].ID != "page-1" || out[2].ID != "page-2" || out[2].Body != "body of /pulls" {
		t.Fatalf("responses = %+v", out)
	}

	if out[0].Headers["Link"] != "<next>" || out[0].Headers["X-Other"] != "" || len(out[0].Headers) != 1 {
		t.Errorf("headers = %v, want only Link", out[0].Headers)
	}

	if got := h.request(0).Header.Get("Authorization"); got != "Bearer "+changesToken {
		t.Errorf("page-1 Authorization = %q", got)
	}

	if got := h.request(0).Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q", got)
	}

	if got := h.request(1).Header.Get("Authorization"); got != "" {
		t.Errorf("a request without auth carried %q", got)
	}
}

func TestExchangeCustomAuthHeader(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	e := exchange(t, h)
	e.Auth = &Auth{Header: "PRIVATE-TOKEN"}

	if _, err := run(t, e, callStatus, script{Rounds: [][]HTTPRequest{{get("s", h.URL+"/s", purposeReport)}}}); err != nil {
		t.Fatal(err)
	}

	r := h.request(0)
	if r.Header.Get("Private-Token") != reportToken || r.Header.Get("Authorization") != "" {
		t.Errorf("headers = %v", r.Header)
	}
}

func TestExchangeRefusals(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	other := newHost(t, nil)

	// the subtests run at once; none may have sent anything, which is
	// known once they all ended
	t.Cleanup(func() {
		for _, x := range []*host{h, other} {
			for i := range x.hits() {
				t.Errorf("a refused request went out: %s %s", x.request(i).Method, x.request(i).URL)
			}
		}
	})

	big := get("big", h.URL+"/b", "")
	big.Method, big.Body = "POST", strings.Repeat("x", MaxBody+1)

	with := func(name, value string) HTTPRequest {
		r := get("h", h.URL+"/h", "")
		r.Headers = map[string]string{name: value}

		return r
	}

	method := get("m", h.URL+"/m", "")
	method.Method = "CONNECT"

	https := "https" + strings.TrimPrefix(h.URL, "http")
	userinfo := strings.Replace(h.URL, "http://", "http://user:pw@", 1)

	tooMany := make([]HTTPRequest, MaxRequestsPerRound+1)
	for i := range tooMany {
		tooMany[i] = get(fmt.Sprintf("r%d", i), h.URL+"/r", "")
	}

	cases := []struct {
		name    string
		call    string
		reqs    []HTTPRequest
		declare []string
		auth    *Auth
		tokens  map[string]string
	}{
		{name: "another host", reqs: []HTTPRequest{get("a", other.URL+"/x", purposeChanges)}},
		{name: "another scheme on the same host", reqs: []HTTPRequest{get("a", https+"/x", "")}},
		{name: "a scheme that isn't http", reqs: []HTTPRequest{get("a", "file:///etc/passwd", "")}},
		{name: "a user in the url", reqs: []HTTPRequest{get("a", userinfo+"/x", "")}},
		{name: "an undeclared header", reqs: []HTTPRequest{with("X-Foo", "1")}},
		{name: "Authorization, declared", reqs: []HTTPRequest{with("Authorization", "Bearer x")}, declare: []string{"Authorization"}},
		{name: "authorization in lower case", reqs: []HTTPRequest{with("authorization", "x")}, declare: []string{"authorization"}},
		{name: "Cookie", reqs: []HTTPRequest{with("Cookie", "a=b")}, declare: []string{"Cookie"}},
		{name: "Host", reqs: []HTTPRequest{with("Host", "evil.example")}, declare: []string{"Host"}},
		{name: "a Proxy- header", reqs: []HTTPRequest{with("proxy-authorization", "x")}, declare: []string{"Proxy-Authorization"}},
		{
			name: "the token's own header as a literal", reqs: []HTTPRequest{with("Private-Token", "x")},
			declare: []string{"PRIVATE-TOKEN"}, auth: &Auth{Header: "PRIVATE-TOKEN"},
		},
		{name: "another call's purpose", reqs: []HTTPRequest{get("a", h.URL+"/x", purposeReport)}},
		{name: "the read purpose", reqs: []HTTPRequest{get("a", h.URL+"/x", purposeRead)}},
		{name: "the store purpose", reqs: []HTTPRequest{get("a", h.URL+"/x", purposeStore)}},
		{
			name: "a purpose with no token", call: callStatus,
			reqs: []HTTPRequest{get("a", h.URL+"/x", purposeReport)}, tokens: map[string]string{purposeChanges: changesToken},
		},
		{name: "identify asking for a request", call: callIdentify, reqs: []HTTPRequest{get("a", h.URL+"/x", "")}},
		{name: "links asking for a request", call: callLinks, reqs: []HTTPRequest{get("a", h.URL+"/x", "")}},
		{name: "an id with a path in it", reqs: []HTTPRequest{get("../x", h.URL+"/x", "")}},
		{name: "an empty id", reqs: []HTTPRequest{get("", h.URL+"/x", "")}},
		{name: "an id twice in a round", reqs: []HTTPRequest{get("a", h.URL+"/x", ""), get("a", h.URL+"/y", "")}},
		{name: "a method not allowed", reqs: []HTTPRequest{method}},
		{name: "a body over the cap", reqs: []HTTPRequest{big}},
		{name: "too many requests in a round", reqs: tooMany},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			e := exchange(t, h)
			if c.declare != nil {
				e.RequestHeaders = c.declare
			}

			if c.auth != nil {
				e.Auth = c.auth
			}

			if c.tokens != nil {
				e.Tokens = c.tokens
			}

			call := c.call
			if call == "" {
				call = callChanges
			}

			_, err := run(t, e, call, script{Rounds: [][]HTTPRequest{c.reqs}})
			if !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want refused", err)
			}

			if strings.Contains(err.Error(), changesToken) || strings.Contains(err.Error(), reportToken) {
				t.Errorf("the error carries a token: %v", err)
			}
		})
	}
}

func TestExchangeRoundShape(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	one := [][]HTTPRequest{{get("a", h.URL+"/a", "")}}

	for name, s := range map[string]script{
		"answers and asks": {Rounds: one, Both: true},
		"neither":          {Rounds: one, Empty: true},
		"an id reused":     {Rounds: [][]HTTPRequest{{get("a", h.URL+"/a", "")}, {get("a", h.URL+"/b", "")}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := run(t, exchange(t, h), callChanges, s); !errors.Is(err, ErrRefused) {
				t.Fatalf("err = %v, want refused", err)
			}
		})
	}
}

func TestExchangeCaps(t *testing.T) {
	t.Parallel()

	rounds := func(h *host, n, per int) script {
		var s script

		for r := range n {
			var reqs []HTTPRequest
			for i := range per {
				reqs = append(reqs, get(fmt.Sprintf("r%d-%d", r, i), h.URL+"/x", ""))
			}

			s.Rounds = append(s.Rounds, reqs)
		}

		return s
	}

	t.Run("rounds", func(t *testing.T) {
		t.Parallel()

		h := newHost(t, nil)

		if _, err := run(t, exchange(t, h), callChanges, rounds(h, MaxRounds-1, 1)); err != nil {
			t.Fatalf("%d rounds of requests and one answer: %v", MaxRounds-1, err)
		}
	})

	t.Run("one round too many", func(t *testing.T) {
		t.Parallel()

		h := newHost(t, nil)

		if _, err := run(t, exchange(t, h), callChanges, rounds(h, MaxRounds, 1)); !errors.Is(err, ErrRefused) {
			t.Fatalf("%d rounds of requests: err = %v, want refused", MaxRounds, err)
		}
	})

	t.Run("requests", func(t *testing.T) {
		t.Parallel()

		h := newHost(t, nil)

		if _, err := run(t, exchange(t, h), callChanges, rounds(h, MaxRequests/MaxRequestsPerRound, MaxRequestsPerRound)); err != nil {
			t.Fatalf("%d requests: %v", MaxRequests, err)
		}
	})

	t.Run("one request too many", func(t *testing.T) {
		t.Parallel()

		h := newHost(t, nil)
		s := rounds(h, MaxRequests/MaxRequestsPerRound+1, MaxRequestsPerRound)
		s.Rounds[len(s.Rounds)-1] = s.Rounds[len(s.Rounds)-1][:1]

		if _, err := run(t, exchange(t, h), callChanges, s); !errors.Is(err, ErrRefused) {
			t.Fatalf("%d requests: err = %v, want refused", MaxRequests+1, err)
		}

		if h.hits() != MaxRequests {
			t.Errorf("%d requests went out, want %d", h.hits(), MaxRequests)
		}
	})
}

func TestExchangeResponseCap(t *testing.T) {
	t.Parallel()

	h := newHost(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		write(w, strings.Repeat("x", MaxBody+1))
	})

	_, err := run(t, exchange(t, h), callChanges, script{Rounds: [][]HTTPRequest{{get("a", h.URL+"/a", "")}}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want refused", err)
	}
}

func TestAllowedAPIHosts(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name                string
		declared, confirmed bool
	}{
		{name: "declared only", declared: true},
		{name: "confirmed only", confirmed: true},
		{name: "both", declared: true, confirmed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			h, api := newHost(t, nil), newHost(t, nil)

			var declared, confirmed []string
			if c.declared {
				declared = []string{api.URL}
			}

			if c.confirmed {
				confirmed = []string{api.URL + "/"}
			}

			hosts, err := Allowed("connector", h.URL, declared, confirmed)
			if err != nil {
				t.Fatal(err)
			}

			e := exchange(t, h)
			e.Hosts = hosts
			sent := c.declared && c.confirmed

			_, err = run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("a", api.URL+"/x", purposeChanges)}}})

			switch {
			case sent && (err != nil || api.hits() != 1):
				t.Fatalf("err = %v, hits %d", err, api.hits())
			case !sent && (!errors.Is(err, ErrRefused) || api.hits() != 0):
				t.Fatalf("err = %v, hits %d, want refused", err, api.hits())
			}
		})
	}
}

func TestExchangeRedirects(t *testing.T) {
	t.Parallel()

	other := newHost(t, nil)

	h := newHost(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/there", http.StatusFound)
		case "/away":
			http.Redirect(w, r, other.URL+"/stolen", http.StatusTemporaryRedirect)
		default:
			write(w, "arrived")
		}
	})

	e := exchange(t, h)
	e.Auth = &Auth{Header: "PRIVATE-TOKEN"}

	out, err := run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("a", h.URL+"/same", purposeChanges)}}})
	if err != nil || len(out) != 1 || out[0].Body != "arrived" {
		t.Fatalf("same origin: %v %+v", err, out)
	}

	if got := h.request(1).Header.Get("Private-Token"); got != changesToken {
		t.Errorf("the redirect on the origin lost the token: %q", got)
	}

	_, err = run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("b", h.URL+"/away", purposeChanges)}}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("away: err = %v, want refused", err)
	}

	if other.hits() != 0 {
		t.Errorf("the other host got %d requests", other.hits())
	}

	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/plain", http.StatusFound) //nolint:gosec // the redirect under test
	}))
	t.Cleanup(tls.Close)

	te := exchange(t, h)
	te.HTTP = tls.Client()

	if te.Hosts, err = Allowed("connector", tls.URL, nil, nil); err != nil {
		t.Fatal(err)
	}

	_, err = run(t, te, callChanges, script{Rounds: [][]HTTPRequest{{get("c", tls.URL+"/x", purposeChanges)}}})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("https to http: err = %v, want refused", err)
	}
}

func TestExchangeRetries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		answer func(w http.ResponseWriter, n int)
		status int
		hits   int
	}{
		{name: "a server error passes", status: 200, hits: 2, answer: func(w http.ResponseWriter, n int) {
			if n == 1 {
				w.WriteHeader(http.StatusBadGateway)
			}
		}},
		{name: "429 waits as asked", status: 200, hits: 2, answer: func(w http.ResponseWriter, n int) {
			if n == 1 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
			}
		}},
		{name: "a 403 rate limit", status: 200, hits: 2, answer: func(w http.ResponseWriter, n int) {
			if n == 1 {
				w.Header().Set("Ratelimit-Remaining", "0")
				w.WriteHeader(http.StatusForbidden)
			}
		}},
		{name: "a plain 403 is the answer", status: 403, hits: 1, answer: func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusForbidden)
		}},
		{name: "404 is the answer", status: 404, hits: 1, answer: func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{name: "a server error that stays", status: 503, hits: tries, answer: func(w http.ResponseWriter, _ int) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			h := newHost(t, func(w http.ResponseWriter, _ *http.Request, n int) { c.answer(w, n) })

			out, err := run(t, exchange(t, h), callChanges, script{Rounds: [][]HTTPRequest{{get("a", h.URL+"/a", purposeChanges)}}})
			if err != nil {
				t.Fatal(err)
			}

			if out[0].Status != c.status || h.hits() != c.hits {
				t.Errorf("status %d after %d hits, want %d after %d", out[0].Status, h.hits(), c.status, c.hits)
			}
		})
	}

	t.Run("no answer at all", func(t *testing.T) {
		t.Parallel()

		h := newHost(t, nil)
		e := exchange(t, h)
		h.Close()

		_, err := run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("a", h.URL+"/a", purposeChanges)}}})
		if err == nil || errors.Is(err, ErrRefused) {
			t.Fatalf("err = %v, want a failure that isn't a refusal", err)
		}

		if strings.Contains(err.Error(), changesToken) {
			t.Errorf("the error carries the token: %v", err)
		}
	})
}

func TestThePluginHoldsNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", changesToken)

	h := newHost(t, nil)
	e := exchange(t, h)
	work := t.TempDir()

	var out []seen
	if err := e.Call(t.Context(), callChanges, work, script{Rounds: [][]HTTPRequest{{get("a", h.URL+"/a", purposeChanges)}}}, &out); err != nil {
		t.Fatal(err)
	}

	err := filepath.WalkDir(work, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path) //nolint:gosec // the test's own directory
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		if strings.Contains(string(data), changesToken) {
			t.Errorf("%s holds the token", path)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOrigin(t *testing.T) {
	t.Parallel()

	for remote, want := range map[string]string{
		"https://GitHub.com/o/r.git":         "https://github.com",
		"https://git.example:443/o/r":        "https://git.example",
		"http://127.0.0.1:3000/o/r":          "http://127.0.0.1:3000",
		"ssh://git@git.example:2222/o/r.git": "https://git.example",
		"git@git.example:o/r.git":            "https://git.example",
		"git.example:o/r":                    "https://git.example",
		"git://git.example/o/r":              "https://git.example",
		"https://[::1]:8443/o/r":             "https://[::1]:8443",
		"file:///srv/git/r.git":              "",
		"/srv/git/r.git":                     "",
		"./r":                                "",
	} {
		got, err := Origin(remote)
		if (err != nil) != (want == "") || got != want {
			t.Errorf("Origin(%q) = %q, %v; want %q", remote, got, err, want)
		}
	}
}

func TestEveryCallGetsTheExchangesConfig(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	e := exchange(t, h)
	remote := "https://git.example.test/team/app.git"
	e.Config = map[string]any{"remote": remote}
	e.Request.Config = map[string]any{"api": "x"}

	out, err := run(t, e, callLinks, script{Remote: true})
	if err != nil || len(out) != 1 || out[0].Body != remote {
		t.Fatalf("%+v %v", out, err)
	}

	if _, ok := e.Request.Config["remote"]; ok {
		t.Error("the plugin's own block was changed")
	}
}

func TestARefusalSaysWhoseRequestItRefused(t *testing.T) {
	t.Parallel()

	h := newHost(t, nil)
	e := exchange(t, h)

	_, err := run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("a", "file:///etc/passwd", "")}}})
	if !errors.Is(err, ErrRefused) || err.Error() != `connector request refused: fake-connector changes: request "a": scheme "file"` {
		t.Fatalf("%v", err)
	}

	e.Noun = ""

	_, err = run(t, e, callChanges, script{Rounds: [][]HTTPRequest{{get("a", "file:///etc/passwd", "")}}})
	if !errors.Is(err, ErrRefused) || err.Error() != `request refused: fake-connector changes: request "a": scheme "file"` {
		t.Fatalf("%v", err)
	}

	if _, err := Allowed("connector", "", []string{"ftp://x"}, nil); err == nil || err.Error() != `connector request refused: describe's api host: no web origin: "ftp://x"` {
		t.Fatalf("%v", err)
	}
}
