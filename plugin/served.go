package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sideways-works/plugkit/redact"
)

var ErrClosed = errors.New("plugin closed")

var errEnded = errors.New("the process ended")

// The restart pause after a process that ended within quickDeath of its start: it doubles with
// each such end in a row, up to maxPause, so a plugin that can't start isn't started in a loop.
const (
	quickDeath = time.Second
	firstPause = 100 * time.Millisecond
	maxPause   = 5 * time.Second
	closeGrace = 5 * time.Second
)

// Served is a plugin in serve mode: one process answering many calls at once, started on the
// first call and started again on the next call after it ended. A call in flight when the
// process ends fails; it is never sent again, since the plugin may have acted on it.
type Served struct {
	Plugin Plugin

	mu     sync.Mutex
	proc   *process
	closed bool
	quick  int
	lastID atomic.Uint64
}

type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time
	stderr  *stderrLines

	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[uint64]chan LineAnswer
	ended   chan struct{}
	why     error
}

// Call sends one call to the process, starting it if none runs, and decodes the answer's body
// into answer. Plugin.Timeout bounds the call, not the process.
func (s *Served) Call(ctx context.Context, call string, req, answer any) error {
	if s.Plugin.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, s.Plugin.Timeout)
		defer cancel()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrFailed, s.Plugin.Name, call, err)
	}

	p, err := s.running(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrFailed, s.Plugin.Name, call, err)
	}

	id := s.lastID.Add(1)

	got, err := p.ask(ctx, LineRequest{ID: id, Call: call, Body: body})
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrFailed, s.Plugin.Name, call, err)
	}

	switch {
	case got.CantTell:
		return &cantTellError{plugin: s.Plugin.Name, call: call, message: redact.Redact(got.Error)}
	case got.Error != "":
		return fmt.Errorf("%w: %s %s: %s", ErrFailed, s.Plugin.Name, call, redact.Redact(got.Error))
	case answer == nil:
		return nil
	}

	if err := json.Unmarshal(got.Body, answer); err != nil {
		return fmt.Errorf("%w: %s %s: unreadable answer: %w", ErrFailed, s.Plugin.Name, call, err)
	}

	return nil
}

// Close ends the process: its stdin closes, so it answers what it has and exits, and after a
// grace period it is killed. Calls after Close fail with ErrClosed.
func (s *Served) Close() error {
	s.mu.Lock()
	s.closed = true
	p := s.proc
	s.mu.Unlock()

	if p == nil {
		return nil
	}

	p.writeMu.Lock()
	_ = p.stdin.Close() //nolint:errcheck // closing is the request to end; the process may be gone already
	p.writeMu.Unlock()

	select {
	case <-p.ended:
	case <-time.After(closeGrace):
		_ = p.cmd.Process.Kill() //nolint:errcheck // it may have ended meanwhile

		<-p.ended
	}

	return nil
}

func (s *Served) running(ctx context.Context) (*process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, ErrClosed
	}

	if s.proc != nil {
		select {
		case <-s.proc.ended:
		default:
			return s.proc, nil
		}

		if time.Since(s.proc.started) < quickDeath {
			s.quick++
		} else {
			s.quick = 0
		}

		if s.quick > 0 {
			pause := min(firstPause<<(s.quick-1), maxPause)

			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("waiting to start again: %w", ctx.Err())
			case <-time.After(pause):
			}
		}
	}

	p, err := s.start()
	if err != nil {
		return nil, err
	}

	s.proc = p

	return p, nil
}

func (s *Served) start() (*process, error) {
	cmd := exec.Command(s.Plugin.Path, CallServe) //nolint:gosec,noctx // a plugin the host pins by checksum; it outlives any one call
	cmd.Dir = s.Plugin.Dir
	cmd.Env = s.Plugin.env()

	p := &process{
		cmd: cmd, pending: map[uint64]chan LineAnswer{}, ended: make(chan struct{}),
		stderr: &stderrLines{log: s.Plugin.Log, prefix: s.Plugin.Name + " " + CallServe},
	}
	cmd.Stderr = p.stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	p.stdin, p.started = stdin, time.Now()

	go p.read(stdout)

	return p, nil
}

// read hands each answer to its call until stdout ends, then fails every call still waiting.
// A line that is no answer ends the process: what it says next can't be trusted either.
func (p *process) read(stdout io.Reader) {
	in := bufio.NewReader(stdout)

	var why error

	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			var a LineAnswer
			if jerr := json.Unmarshal(line, &a); jerr != nil {
				why = fmt.Errorf("unreadable answer line: %w", jerr)
				_ = p.cmd.Process.Kill() //nolint:errcheck // it may have ended already

				break
			}

			p.mu.Lock()
			ch, ok := p.pending[a.ID]
			delete(p.pending, a.ID)
			p.mu.Unlock()

			if ok {
				ch <- a
			}
		}

		if err != nil {
			break
		}
	}

	_, _ = io.Copy(io.Discard, stdout) //nolint:errcheck // draining, so Wait can return

	if err := p.cmd.Wait(); why == nil {
		why = errEnded
		if err != nil {
			why = fmt.Errorf("%w: %w", errEnded, err)
		}
	}

	if t := p.stderr.tail(); t != "" {
		why = fmt.Errorf("%w: %s", why, redact.Redact(t))
	}

	p.mu.Lock()
	p.why = why
	p.mu.Unlock()
	close(p.ended)
}

func (p *process) ask(ctx context.Context, req LineRequest) (LineAnswer, error) {
	ch := make(chan LineAnswer, 1)

	p.mu.Lock()
	select {
	case <-p.ended:
		why := p.why
		p.mu.Unlock()

		return LineAnswer{}, why
	default:
	}

	p.pending[req.ID] = ch
	p.mu.Unlock()

	forget := func() {
		p.mu.Lock()
		delete(p.pending, req.ID)
		p.mu.Unlock()
	}

	data, err := json.Marshal(req)
	if err != nil {
		forget()

		return LineAnswer{}, fmt.Errorf("request: %w", err)
	}

	p.writeMu.Lock()
	_, err = p.stdin.Write(append(data, '\n'))
	p.writeMu.Unlock()

	if err != nil {
		forget()

		return LineAnswer{}, fmt.Errorf("request: %w", err)
	}

	select {
	case a := <-ch:
		return a, nil
	case <-p.ended:
		select {
		case a := <-ch:
			return a, nil
		default:
		}

		p.mu.Lock()
		defer p.mu.Unlock()

		return LineAnswer{}, p.why
	case <-ctx.Done():
		forget()

		return LineAnswer{}, fmt.Errorf("waiting for the answer: %w", ctx.Err())
	}
}

// stderrLines logs a serve-mode process's stderr a line at a time and keeps its last lines for
// the error of a call it fails.
type stderrLines struct {
	log    io.Writer
	prefix string

	mu      sync.Mutex
	partial string
	last    []string
}

const keptLines = 15

func (s *stderrLines) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	text := s.partial + string(b)

	lines := strings.Split(text, "\n")
	s.partial = lines[len(lines)-1]

	for _, line := range lines[:len(lines)-1] {
		logLines(s.log, s.prefix, line)

		s.last = append(s.last, line)
		if len(s.last) > keptLines {
			s.last = s.last[1:]
		}
	}

	return len(b), nil
}

func (s *stderrLines) tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	lines := s.last
	if s.partial != "" {
		lines = append(slices.Clip(lines), s.partial)
	}

	return strings.Join(lines, "\n")
}
