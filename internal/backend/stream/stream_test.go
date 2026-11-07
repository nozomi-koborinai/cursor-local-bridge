package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent/fakeagenttest"
)

var _ interface {
	Start(ctx context.Context, opts backend.StartOptions) (string, error)
	Prompt(ctx context.Context, text string) error
	Close() error
} = (*Session)(nil)

var fakeAgent string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "clb-stream-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fakeAgent, err = fakeagenttest.Build(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type recorder struct {
	t      *testing.T
	mu     sync.Mutex
	events []backend.Event
}

func (r *recorder) Event(e backend.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) Permission(context.Context, backend.PermissionRequest) (bool, error) {
	r.t.Error("print mode asked for a permission")
	return false, errors.New("unexpected permission request")
}

func (r *recorder) snapshot() []backend.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]backend.Event(nil), r.events...)
}

func (r *recorder) types() []string {
	var types []string
	for _, e := range r.snapshot() {
		types = append(types, e.Type)
	}
	return types
}

func (r *recorder) text() string {
	var b strings.Builder
	for _, e := range r.snapshot() {
		b.WriteString(e.Text)
	}
	return b.String()
}

func (r *recorder) waitText(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(r.text(), substr) {
		if time.Now().After(deadline) {
			t.Fatalf("text %q never contained %q", r.text(), substr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func useState(t *testing.T) string {
	dir := t.TempDir()
	t.Setenv("FAKEAGENT_STATE_DIR", dir)
	return dir
}

func invocations(t *testing.T, state string) []fakeagenttest.Invocation {
	t.Helper()
	invs, err := fakeagenttest.Invocations(state)
	if err != nil {
		t.Fatal(err)
	}
	return invs
}

func start(t *testing.T, agentPath string, sink backend.Sink, opts backend.StartOptions) (*Session, string) {
	t.Helper()
	s := New(agentPath, sink)
	t.Cleanup(func() { _ = s.Close() })
	id, err := s.Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, id
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("agent process %d still exists: kill(0) = %v", pid, err)
	}
}

func TestStartCreatesChatAndPromptResumesIt(t *testing.T) {
	state := useState(t)
	dir := t.TempDir()
	sink := &recorder{t: t}
	began := time.Now()
	s, id := start(t, fakeAgent, sink, backend.StartOptions{Dir: dir, Model: "gpt-5"})
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("Start took %s; it must not wait for create-chat to exit", took)
	}
	invs := invocations(t, state)
	if len(invs) != 1 || !reflect.DeepEqual(invs[0].Args, []string{"create-chat"}) {
		t.Fatalf("invocations = %+v, want one create-chat", invs)
	}
	assertGone(t, invs[0].Pid)

	if err := s.Prompt(context.Background(), "hello there"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if got, want := sink.types(), []string{"system", "assistant", "result"}; !reflect.DeepEqual(got, want) {
		t.Errorf("event types = %q, want %q", got, want)
	}
	if got := sink.text(); got != "echo: hello there" {
		t.Errorf("text = %q, want %q", got, "echo: hello there")
	}
	var init struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(sink.snapshot()[0].Data, &init); err != nil || init.SessionID != id {
		t.Errorf("init event = %s, want session_id %q", sink.snapshot()[0].Data, id)
	}
	invs = invocations(t, state)
	want := []string{"-p", "--output-format", "stream-json", "--model", "gpt-5", "--resume", id, "--workspace", dir, "--", "hello there"}
	if len(invs) != 2 || !reflect.DeepEqual(invs[1].Args, want) {
		t.Errorf("invocations = %+v, want a second one with args %q", invs, want)
	}
}

func TestStartAdoptsResumeSessionID(t *testing.T) {
	state := useState(t)
	dir := t.TempDir()
	_, id := start(t, fakeAgent, &recorder{t: t}, backend.StartOptions{Dir: dir})

	sink := &recorder{t: t}
	s, resumed := start(t, fakeAgent, sink, backend.StartOptions{Dir: dir, ResumeSessionID: id})
	if resumed != id {
		t.Fatalf("Start = %q, want %q", resumed, id)
	}
	if n := len(invocations(t, state)); n != 1 {
		t.Fatalf("%d invocations after resuming, want only the first create-chat", n)
	}
	if err := s.Prompt(context.Background(), "again"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := sink.text(); got != "echo: again" {
		t.Errorf("text = %q, want %q", got, "echo: again")
	}
	invs := invocations(t, state)
	want := []string{"-p", "--output-format", "stream-json", "--resume", id, "--workspace", dir, "--", "again"}
	if !reflect.DeepEqual(invs[len(invs)-1].Args, want) {
		t.Errorf("args = %q, want %q", invs[len(invs)-1].Args, want)
	}
}

func TestCloseStopsPromptInFlight(t *testing.T) {
	state := useState(t)
	sink := &recorder{t: t}
	s, _ := start(t, fakeAgent, sink, backend.StartOptions{Dir: t.TempDir()})

	prompted := make(chan error, 1)
	go func() { prompted <- s.Prompt(context.Background(), "[slow]") }()
	sink.waitText(t, "tick 1")
	invs := invocations(t, state)
	pid := invs[len(invs)-1].Pid

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-prompted:
		if err == nil {
			t.Error("Prompt reported a completed turn after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt still running 5s after Close")
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertGone(t, pid)
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := s.Prompt(context.Background(), "after close"); err == nil {
		t.Error("Prompt after Close succeeded")
	}
}

func TestPromptHonorsContext(t *testing.T) {
	useState(t)
	sink := &recorder{t: t}
	s, _ := start(t, fakeAgent, sink, backend.StartOptions{Dir: t.TempDir()})

	ctx, cancel := context.WithCancel(context.Background())
	prompted := make(chan error, 1)
	go func() { prompted <- s.Prompt(ctx, "[slow]") }()
	sink.waitText(t, "tick 1")
	cancel()
	select {
	case err := <-prompted:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Prompt = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prompt still running 5s after its context ended")
	}
}

func TestPromptFailures(t *testing.T) {
	tests := []struct {
		name      string
		prompt    string
		loggedOut bool
		want      []string
	}{
		{"crash", "[crash]", false, []string{"exit status 3", "crashing on purpose"}},
		{"logged out", "hi", true, []string{"exit status 1", "agent login"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useState(t)
			s, _ := start(t, fakeAgent, &recorder{t: t}, backend.StartOptions{Dir: t.TempDir()})
			if tt.loggedOut {
				t.Setenv("FAKEAGENT_LOGGED_OUT", "1")
			}
			err := s.Prompt(context.Background(), tt.prompt)
			if err == nil {
				t.Fatal("Prompt succeeded")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error = %q, want it to contain %q", err, w)
				}
			}
		})
	}
}

func TestPromptForwardsNonJSONAndReportsErrorResult(t *testing.T) {
	script := filepath.Join(t.TempDir(), "agent")
	body := "#!/bin/sh\necho 'warming up'\necho '{\"type\":\"result\",\"subtype\":\"error\",\"is_error\":true,\"result\":\"model refused\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := &recorder{t: t}
	s, _ := start(t, script, sink, backend.StartOptions{Dir: t.TempDir(), ResumeSessionID: "chat-1"})

	err := s.Prompt(context.Background(), "hi")
	if err == nil || !strings.Contains(err.Error(), "model refused") {
		t.Errorf("Prompt = %v, want the result's error text", err)
	}
	events := sink.snapshot()
	if len(events) != 2 || events[0].Type != "stdout" || string(events[0].Data) != `"warming up"` || events[1].Type != "result" {
		t.Errorf("events = %+v, want a stdout event with the line as a JSON string, then the result", events)
	}
}
