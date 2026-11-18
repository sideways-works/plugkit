// Package plugin is the plugin protocol: an executable called as `<executable> <call>` with a
// JSON request on stdin and a JSON answer on stdout, or, in serve mode, one long-lived process
// answering JSON lines. Serve, ServeLines and Main are a plugin's side; Plugin and Served the host's.
package plugin

import "errors"

// ExitCantTell is the exit code of a plugin that can't answer for what it was asked: an answer
// of its own, apart from a failure and from an empty answer.
const ExitCantTell = 3

// CallServe starts serve mode.
const CallServe = "serve"

// ErrCantTell is a plugin's answer that it can't tell.
var ErrCantTell = errors.New("can't tell")

var ErrFailed = errors.New("plugin failed")

// Request is what every call's request carries. Work is a directory the call alone uses: an
// answer too large for stdout is written there as files the answer names (ReadLines).
type Request struct {
	Protocol int            `json:"protocol"`
	Work     string         `json:"work"`
	Cache    string         `json:"cache,omitempty"`
	Config   map[string]any `json:"config,omitempty"`
}
