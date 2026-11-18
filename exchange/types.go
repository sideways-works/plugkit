package exchange

import (
	"encoding/json"

	"github.com/sideways-works/plugkit/plugin"
)

// Request is one round of a call. A plugin keeps no state between rounds: each carries the
// call's arguments again and every response of every round before it.
type Request struct {
	plugin.Request

	Args      json.RawMessage `json:"args,omitempty"`
	Responses []HTTPResponse  `json:"responses,omitempty"`
}

// Round is a plugin's answer to one round: requests for the host to send, or Done with the
// call's answer.
type Round struct {
	Requests []HTTPRequest   `json:"requests,omitempty"`
	Done     json.RawMessage `json:"done,omitempty"`
}

type HTTPRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Auth    *HTTPAuth         `json:"auth,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// HTTPAuth names the purpose whose token a request carries; it must be the call's.
type HTTPAuth struct {
	Purpose string `json:"purpose"`
}

// HTTPResponse is the host's answer to one request, after its retries; the body is a file in
// the call's work directory.
type HTTPResponse struct {
	ID       string            `json:"id"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyFile string            `json:"body_file,omitempty"`
}

// Auth puts a token in Header, after Scheme and a space when there is one
// (Authorization: Bearer <token>, PRIVATE-TOKEN: <token>).
type Auth struct {
	Header string `json:"header"`
	Scheme string `json:"scheme,omitempty"`
}
