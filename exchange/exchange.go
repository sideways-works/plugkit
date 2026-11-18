// Package exchange is the host's side of rounds: a plugin that talks to the outside answers with
// HTTP requests as data, and the host checks each, sends it with the call's token on it, retries
// it when it may pass, and calls the plugin again with every response so far. The plugin never
// holds a token, nor can it send one anywhere the host doesn't allow.
package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"time"

	"github.com/sideways-works/plugkit/plugin"
	"github.com/sideways-works/plugkit/redact"
)

// The caps on one call: rounds, requests in a round and in the whole call,
// and the size of any request or response body.
const (
	MaxRounds           = 10
	MaxRequestsPerRound = 20
	MaxRequests         = 100
	MaxBody             = 10 << 20
)

// ErrRefused is a request the host won't send, or a round it won't take: the call fails.
var ErrRefused = errors.New("request refused")

// RefusedError is ErrRefused said of a kind of plugin: RefusedError("connector") reads "connector request
// refused".
type RefusedError string

func (r RefusedError) Error() string {
	if r == "" {
		return ErrRefused.Error()
	}

	return string(r) + " " + ErrRefused.Error()
}

func (r RefusedError) Is(target error) bool { return target == ErrRefused }

// Caller calls the plugin once.
type Caller interface {
	Call(ctx context.Context, call string, req, answer any) error
}

type Exchange struct {
	// Name is the plugin's and Noun its kind ("connector"), for messages.
	Name, Noun string
	Plugin     Caller
	// Request is every round's request but its work directory, which is the call's.
	Request plugin.Request
	// Config is set in every call's config, over the request's own.
	Config map[string]any
	// Purposes are the one purpose whose token each call's requests may carry; a call they leave
	// out sends no requests.
	Purposes map[string]string
	// Auth is where a token goes; RequestHeaders the headers a request may set as literals, and
	// ResponseHeaders those of a response the plugin is shown.
	Auth            *Auth
	RequestHeaders  []string
	ResponseHeaders []string
	// Hosts are the origins (scheme://host[:port]) requests may go to (Allowed).
	Hosts  []string
	Tokens map[string]string
	// HTTP sends the requests; its redirect policy is replaced by one
	// that never leaves the request's origin.
	HTTP *http.Client
	// Pause is the first pause before a retry (a second), MaxWait the longest a retry waits for
	// a host that asks it to (90 seconds).
	Pause, MaxWait time.Duration
}

func (e *Exchange) who() string {
	if e.Noun == "" {
		return e.Name
	}

	return e.Noun + " " + e.Name
}

// Allowed is the origins requests may go to: the code remote's, and each API host both the
// plugin's describe declares and the config confirms. noun is the plugin's kind, for messages.
func Allowed(noun, remoteOrigin string, declared, confirmed []string) ([]string, error) {
	out := []string{}

	if remoteOrigin != "" {
		o, err := NormalOrigin(remoteOrigin)
		if err != nil {
			return nil, err
		}

		out = append(out, o)
	}

	in := map[string]bool{}

	for _, h := range confirmed {
		o, err := NormalOrigin(h)
		if err != nil {
			return nil, err
		}

		in[o] = true
	}

	for _, h := range declared {
		o, err := NormalOrigin(h)
		if err != nil {
			return nil, fmt.Errorf("%w: describe's api host: %w", RefusedError(noun), err)
		}

		if in[o] {
			out = append(out, o)
		}
	}

	return out, nil
}

// Call runs call with args in rounds in the work directory work, decoding the plugin's answer into answer.
func (e *Exchange) Call(ctx context.Context, call, work string, args, answer any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("%s %s: args: %w", e.who(), call, err)
	}

	if err := os.MkdirAll(work, 0o750); err != nil {
		return fmt.Errorf("%s %s: %w", e.who(), call, err)
	}

	for _, t := range e.Tokens {
		redact.Add(t)
	}

	c := callState{e: e, call: call, work: work, ids: map[string]bool{}}
	req := Request{Request: e.Request, Args: raw}
	req.Work = work

	if len(e.Config) > 0 {
		cfg := maps.Clone(req.Config)
		if cfg == nil {
			cfg = map[string]any{}
		}

		maps.Copy(cfg, e.Config)
		req.Config = cfg
	}

	for round := 1; ; round++ {
		r, err := e.round(ctx, call, req)
		if err != nil {
			return err
		}

		switch {
		case len(r.Done) > 0 && len(r.Requests) > 0:
			return c.refuse("round %d answers and asks for requests at once", round)
		case len(r.Done) > 0:
			if err := json.Unmarshal(r.Done, answer); err != nil {
				return fmt.Errorf("%s %s: unreadable answer: %w", e.who(), call, err)
			}

			return nil
		case len(r.Requests) == 0:
			return c.refuse("round %d neither answers nor asks for anything", round)
		case round >= MaxRounds:
			return c.refuse("it still asks for requests after %d rounds", MaxRounds)
		}

		got, err := c.round(ctx, r.Requests)
		if err != nil {
			return err
		}

		req.Responses = append(req.Responses, got...)
	}
}

func (e *Exchange) round(ctx context.Context, call string, req Request) (Round, error) {
	var r Round
	if err := e.Plugin.Call(ctx, call, req, &r); err != nil {
		return r, fmt.Errorf("%s: %w", e.who(), err)
	}

	return r, nil
}

type callState struct {
	e    *Exchange
	call string
	work string
	ids  map[string]bool
	sent int
}

func (c *callState) refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s %s: %s", RefusedError(c.e.Noun), c.e.Name, c.call, fmt.Sprintf(format, args...))
}

// round checks every request of a round before it sends any, then sends
// them in order.
func (c *callState) round(ctx context.Context, reqs []HTTPRequest) ([]HTTPResponse, error) {
	if len(reqs) > MaxRequestsPerRound {
		return nil, c.refuse("%d requests in one round, at most %d", len(reqs), MaxRequestsPerRound)
	}

	if c.sent+len(reqs) > MaxRequests {
		return nil, c.refuse("more than %d requests in one call", MaxRequests)
	}

	for _, r := range reqs {
		if problem := c.problem(r); problem != "" {
			return nil, c.refuse("request %q: %s", r.ID, problem)
		}

		c.ids[r.ID] = true
	}

	out := make([]HTTPResponse, 0, len(reqs))

	for _, r := range reqs {
		c.sent++

		resp, err := c.send(ctx, r, fmt.Sprintf("response-%d.body", c.sent))
		if err != nil {
			return nil, err
		}

		out = append(out, resp)
	}

	return out, nil
}
