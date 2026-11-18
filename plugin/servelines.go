package plugin

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// LineRequest is one call in serve mode, a line on the plugin's stdin.
type LineRequest struct {
	ID   uint64          `json:"id"`
	Call string          `json:"call"`
	Body json.RawMessage `json:"body,omitempty"`
}

// LineAnswer is the answer to the request of the same ID, a line on the plugin's stdout: Body,
// or Error with CantTell set when the plugin can't tell.
type LineAnswer struct {
	ID       uint64          `json:"id"`
	Body     json.RawMessage `json:"body,omitempty"`
	Error    string          `json:"error,omitempty"`
	CantTell bool            `json:"cant_tell,omitempty"`
}

// ServeLines is serve mode's loop: it answers each request line with a handler, several at once,
// each answer a whole line as soon as it is ready, until stdin ends and every call has answered.
// The exit code is 1 when an answer could not be written.
func ServeLines(stdin io.Reader, stdout, stderr io.Writer, handlers map[string]Handler) int {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed atomic.Bool
	)

	write := func(a LineAnswer) {
		data, err := json.Marshal(a)
		if err != nil {
			say(stderr, fmt.Sprintf("answer %d: %v", a.ID, err))
			failed.Store(true)

			return
		}

		mu.Lock()
		defer mu.Unlock()

		if _, err := stdout.Write(append(data, '\n')); err != nil {
			failed.Store(true)
		}
	}

	in := bufio.NewReader(stdin)

	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var req LineRequest
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				say(stderr, "unreadable request: "+jerr.Error())
			} else {
				wg.Go(func() { write(answerLine(req, handlers)) })
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				say(stderr, err.Error())
				failed.Store(true)
			}

			break
		}
	}

	wg.Wait()

	if failed.Load() {
		return 1
	}

	return 0
}

func answerLine(req LineRequest, handlers map[string]Handler) LineAnswer {
	h, ok := handlers[req.Call]
	if !ok {
		return LineAnswer{ID: req.ID, Error: fmt.Sprintf("no call %q", req.Call)}
	}

	answer, err := h(req.Body)
	if err != nil {
		return LineAnswer{ID: req.ID, Error: err.Error(), CantTell: errors.Is(err, ErrCantTell)}
	}

	body, err := json.Marshal(answer)
	if err != nil {
		return LineAnswer{ID: req.ID, Error: "answer: " + err.Error()}
	}

	return LineAnswer{ID: req.ID, Body: body}
}
