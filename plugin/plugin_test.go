package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretValue = "a-secret-value-81fe"

var errBroke = errors.New("it broke")

// TestMain lets the test binary act as a plugin when asked, so calls reach a real executable.
func TestMain(m *testing.M) {
	if os.Getenv("PLUGKIT_TEST_PLUGIN") == "" {
		os.Exit(m.Run())
	}

	os.Exit(Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, testHandlers()))
}

type echo struct {
	N     int `json:"n"`
	Sleep int `json:"sleep"`
}

func testHandlers() map[string]Handler {
	return map[string]Handler{
		"describe": func([]byte) (any, error) { return map[string]string{"name": "fake"}, nil },
		"env":      func([]byte) (any, error) { return os.Environ(), nil },
		"cant": func([]byte) (any, error) {
			return nil, fmt.Errorf("%w: no evidence here", ErrCantTell)
		},
		"fail": func([]byte) (any, error) {
			fmt.Fprintln(os.Stderr, "about to fail with "+secretValue)

			return nil, errBroke
		},
		"log": func([]byte) (any, error) {
			fmt.Fprintln(os.Stderr, "a note\nand "+secretValue)

			return map[string]any{}, nil
		},
		"echo": func(in []byte) (any, error) {
			var e echo
			if err := json.Unmarshal(in, &e); err != nil {
				return nil, fmt.Errorf("request: %w", err)
			}

			time.Sleep(time.Duration(e.Sleep) * time.Millisecond)

			return e, nil
		},
		"pid": func([]byte) (any, error) { return os.Getpid(), nil },
		"crash": func([]byte) (any, error) {
			fmt.Fprintln(os.Stderr, "crashing")
			os.Exit(2)

			return nil, nil
		},
		"garbage": func([]byte) (any, error) {
			if _, err := fmt.Fprintln(os.Stdout, "this is no answer"); err != nil {
				return nil, fmt.Errorf("stdout: %w", err)
			}

			return map[string]any{}, nil
		},
		"hang": func([]byte) (any, error) {
			time.Sleep(2 * time.Second)

			return map[string]any{}, nil
		},
	}
}

func fakePath(t *testing.T) string {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "fake-plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nPLUGKIT_TEST_PLUGIN=1 exec '"+self+"' \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func fake(t *testing.T) Plugin {
	t.Helper()

	return Plugin{Name: "fake", Path: fakePath(t)}
}

func TestCallAnswers(t *testing.T) {
	var d map[string]string
	if err := fake(t).Call(t.Context(), "describe", Request{Work: t.TempDir()}, &d); err != nil || d["name"] != "fake" {
		t.Fatalf("describe: %v %v", d, err)
	}
}

func TestAPluginGetsNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", secretValue)
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	t.Setenv("LC_ALL", "C")

	var env []string
	if err := fake(t).Call(t.Context(), "env", Request{}, &env); err != nil {
		t.Fatal(err)
	}

	for _, kv := range env {
		if strings.Contains(kv, secretValue) || strings.HasPrefix(kv, "HTTPS_PROXY=") {
			t.Errorf("the plugin got %s", kv)
		}
	}

	if !slices.Contains(env, "LC_ALL=C") {
		t.Errorf("the plugin lacks its locale: %q", env)
	}

	p := fake(t)
	p.Env = Env([]string{"GITHUB_TOKEN"}, map[string]string{"B": "2", "A": "1"})

	if err := p.Call(t.Context(), "env", Request{}, &env); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(env, "GITHUB_TOKEN="+secretValue) || !slices.Contains(env, "A=1") || !slices.Contains(env, "B=2") {
		t.Errorf("an environment the host gives isn't kept as given: %q", env)
	}

	if set := Env(nil, map[string]string{"B": "2", "A": "1"}); !slices.Equal(set[len(set)-2:], []string{"A=1", "B=2"}) {
		t.Errorf("set's variables come last, in name order: %q", set)
	}
}

func TestExitCodes(t *testing.T) {
	t.Setenv("PLUGKIT_TEST_TOKEN", secretValue)

	p := fake(t)

	err := p.Call(t.Context(), "cant", Request{}, nil)
	if !errors.Is(err, ErrCantTell) || CantTellMessage(err) != "can't tell: no evidence here" {
		t.Fatalf("cant: %v (%q)", err, CantTellMessage(err))
	}

	err = p.Call(t.Context(), "fail", Request{}, nil)
	if !errors.Is(err, ErrFailed) || errors.Is(err, ErrCantTell) || !strings.Contains(err.Error(), "it broke") {
		t.Fatalf("fail: %v", err)
	}

	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("a failure carries the secret: %v", err)
	}

	if err := p.Call(t.Context(), "no-such-call", Request{}, nil); !errors.Is(err, ErrFailed) || errors.Is(err, ErrCantTell) {
		t.Fatalf("an unknown call: %v", err)
	}

	if CantTellMessage(err) != "" {
		t.Fatal("a failure has a can't-tell message")
	}
}

func TestAPluginsStderrReachesTheLog(t *testing.T) {
	t.Setenv("PLUGKIT_TEST_TOKEN", secretValue)

	var log bytes.Buffer

	p := fake(t)
	p.Log = &log

	if err := p.Call(t.Context(), "log", Request{}, nil); err != nil {
		t.Fatal(err)
	}

	if got, want := log.String(), "fake log: a note\nfake log: and ***\n"; got != want {
		t.Fatalf("log %q, want %q", got, want)
	}
}

func TestReadLinesStaysInTheWorkDirectory(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "a.jsonl"), []byte("{\"n\":1}\n\n{\"n\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadLines(work, []string{"a.jsonl"}, []echo(nil))
	if err != nil || len(got) != 2 || got[1].N != 2 {
		t.Fatalf("got %v, %v", got, err)
	}

	for _, f := range []string{"../x.jsonl", "/etc/passwd"} {
		if _, err := ReadLines[echo](work, []string{f}, nil); !errors.Is(err, ErrFailed) {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func served(t *testing.T) *Served {
	t.Helper()

	s := &Served{Plugin: fake(t)}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	return s
}

func TestServedAnswersConcurrentCalls(t *testing.T) {
	s := served(t)

	var wg sync.WaitGroup

	errs := make(chan string, 40)

	for i := range 40 {
		wg.Go(func() {
			var got echo
			if err := s.Call(t.Context(), "echo", echo{N: i, Sleep: (40 - i) % 7 * 5}, &got); err != nil {
				errs <- err.Error()

				return
			}

			if got.N != i {
				errs <- fmt.Sprintf("call %d got the answer of %d", i, got.N)
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	var pids []int

	for range 3 {
		var pid int
		if err := s.Call(t.Context(), "pid", nil, &pid); err != nil {
			t.Fatal(err)
		}

		pids = append(pids, pid)
	}

	if pids[0] != pids[1] || pids[1] != pids[2] {
		t.Fatalf("one process answers every call, got %v", pids)
	}
}

func TestServedRestartsAfterACrash(t *testing.T) {
	s := served(t)

	var before int
	if err := s.Call(t.Context(), "pid", nil, &before); err != nil {
		t.Fatal(err)
	}

	slow := make(chan error, 1)

	go func() { slow <- s.Call(t.Context(), "echo", echo{Sleep: 3000}, nil) }()

	time.Sleep(100 * time.Millisecond)

	err := s.Call(t.Context(), "crash", nil, nil)
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "crashing") {
		t.Fatalf("the crashing call: %v", err)
	}

	select {
	case err := <-slow:
		if !errors.Is(err, ErrFailed) {
			t.Fatalf("a call in flight when the process died: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a call in flight when the process died still waits")
	}

	var after int
	if err := s.Call(t.Context(), "pid", nil, &after); err != nil {
		t.Fatalf("the call after a crash: %v", err)
	}

	if after == before {
		t.Fatal("no new process after the crash")
	}
}

func TestServedDropsAProcessThatSpeaksNoProtocol(t *testing.T) {
	s := served(t)

	if err := s.Call(t.Context(), "garbage", nil, nil); !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "unreadable answer line") {
		t.Fatalf("garbage: %v", err)
	}

	var pid int
	if err := s.Call(t.Context(), "pid", nil, &pid); err != nil {
		t.Fatalf("after garbage: %v", err)
	}
}

func TestServedAnswersAndErrors(t *testing.T) {
	t.Setenv("PLUGKIT_TEST_TOKEN", secretValue)

	var log bytes.Buffer

	s := served(t)
	s.Plugin.Log = &log

	if err := s.Call(t.Context(), "cant", nil, nil); !errors.Is(err, ErrCantTell) || CantTellMessage(err) != "can't tell: no evidence here" {
		t.Fatalf("cant: %v", err)
	}

	err := s.Call(t.Context(), "fail", nil, nil)
	if !errors.Is(err, ErrFailed) || errors.Is(err, ErrCantTell) || !strings.Contains(err.Error(), "it broke") {
		t.Fatalf("fail: %v", err)
	}

	if err := s.Call(t.Context(), "no-such-call", nil, nil); !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), `no call "no-such-call"`) {
		t.Fatalf("an unknown call: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if err := s.Call(ctx, "hang", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call past its deadline: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.Call(t.Context(), "pid", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("a call after close: %v", err)
	}

	if !strings.Contains(log.String(), "fake serve: about to fail with ***\n") || strings.Contains(log.String(), secretValue) {
		t.Fatalf("log: %q", log.String())
	}
}

func TestServeLines(t *testing.T) {
	in := strings.Join([]string{
		`{"id":1,"call":"echo","body":{"n":1,"sleep":50}}`,
		`not a request`,
		`{"id":2,"call":"nope"}`,
		`{"id":3,"call":"echo","body":{"n":3}}`,
	}, "\n")

	var out, errOut bytes.Buffer

	if code := ServeLines(strings.NewReader(in), &out, &errOut, testHandlers()); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}

	var answers []LineAnswer

	for line := range strings.SplitSeq(strings.TrimSpace(out.String()), "\n") {
		var a LineAnswer
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("%q: %v", line, err)
		}

		answers = append(answers, a)
	}

	if len(answers) != 3 || answers[len(answers)-1].ID != 1 {
		t.Fatalf("answers, the slow one last: %+v", answers)
	}

	for _, a := range answers {
		switch a.ID {
		case 2:
			if a.Error != `no call "nope"` {
				t.Errorf("unknown call: %+v", a)
			}
		default:
			if string(a.Body) != fmt.Sprintf(`{"n":%d,"sleep":%d}`, a.ID, map[uint64]int{1: 50, 3: 0}[a.ID]) {
				t.Errorf("answer %d: %s", a.ID, a.Body)
			}
		}
	}

	if !strings.Contains(errOut.String(), "unreadable request") {
		t.Fatalf("stderr: %q", errOut.String())
	}
}
