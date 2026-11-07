package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	dir, err := os.MkdirTemp("", "clb-acp-test-")
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
	mu     sync.Mutex
	events []backend.Event
	asked  []backend.PermissionRequest
	answer func() (bool, error)
}

func (r *recorder) Event(e backend.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) Permission(ctx context.Context, req backend.PermissionRequest) (bool, error) {
	r.mu.Lock()
	r.asked = append(r.asked, req)
	answer := r.answer
	r.mu.Unlock()
	if answer == nil {
		<-ctx.Done()
		return false, ctx.Err()
	}
	return answer()
}

func (r *recorder) snapshot() []backend.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]backend.Event(nil), r.events...)
}

func (r *recorder) permissions() []backend.PermissionRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]backend.PermissionRequest(nil), r.asked...)
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

func start(t *testing.T, sink backend.Sink, opts backend.StartOptions) (*Session, string) {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	s := New(fakeAgent, sink)
	t.Cleanup(func() { _ = s.Close() })
	id, err := s.Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s, id
}

func lastPid(t *testing.T, state string) int {
	t.Helper()
	invs, err := fakeagenttest.Invocations(state)
	if err != nil || len(invs) == 0 {
		t.Fatalf("invocations: %v, %d recorded", err, len(invs))
	}
	return invs[len(invs)-1].Pid
}

func assertGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("agent process %d still exists: kill(0) = %v", pid, err)
	}
}

func TestPromptStreamsReply(t *testing.T) {
	useState(t)
	sink := &recorder{}
	s, id := start(t, sink, backend.StartOptions{})
	if id == "" {
		t.Fatal("Start returned an empty session ID")
	}

	if err := s.Prompt(context.Background(), "hello there"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if got := sink.text(); got != "echo: hello there" {
		t.Errorf("text = %q, want %q", got, "echo: hello there")
	}
	events := sink.snapshot()
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	if counts["tool_call"] != 1 || counts["agent_message_chunk"] < 3 {
		t.Errorf("event types = %v, want one tool_call and at least 3 agent_message_chunk", counts)
	}
	last := events[len(events)-1]
	var end struct{ StopReason string }
	if err := json.Unmarshal(last.Data, &end); last.Type != "turn_ended" || err != nil || end.StopReason != "end_turn" {
		t.Errorf("last event = %s %s, want turn_ended with stopReason end_turn", last.Type, last.Data)
	}
	if asked := sink.permissions(); len(asked) != 0 {
		t.Errorf("asked %d permissions for a plain prompt", len(asked))
	}
}

func TestPermissionAnswerReachesAgent(t *testing.T) {
	tests := []struct {
		name   string
		answer func() (bool, error)
		reply  string
	}{
		{"approve", func() (bool, error) { return true, nil }, "permission: approved"},
		{"deny", func() (bool, error) { return false, nil }, "permission: denied"},
		{"sink error", func() (bool, error) { return true, errors.New("no answer") }, "permission: cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useState(t)
			sink := &recorder{answer: tt.answer}
			s, _ := start(t, sink, backend.StartOptions{})

			if err := s.Prompt(context.Background(), "[permission] run it"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			if got := sink.text(); got != tt.reply {
				t.Errorf("text = %q, want %q", got, tt.reply)
			}
			asked := sink.permissions()
			if len(asked) != 1 || asked[0].Title != "Run fake command" {
				t.Fatalf("asked = %+v, want one request titled %q", asked, "Run fake command")
			}
			var toolCall struct{ ToolCallID string }
			if err := json.Unmarshal(asked[0].ToolCall, &toolCall); err != nil || toolCall.ToolCallID != "perm-1" {
				t.Errorf("ToolCall = %s, want the agent's tool call", asked[0].ToolCall)
			}
		})
	}
}

func TestCloseStopsPromptInFlight(t *testing.T) {
	state := useState(t)
	sink := &recorder{}
	s, _ := start(t, sink, backend.StartOptions{})
	pid := lastPid(t, state)

	prompted := make(chan error, 1)
	go func() { prompted <- s.Prompt(context.Background(), "[slow]") }()
	sink.waitText(t, "tick 1")

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
}

func TestPromptHonorsContext(t *testing.T) {
	useState(t)
	sink := &recorder{}
	s, _ := start(t, sink, backend.StartOptions{})

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

	if err := s.Prompt(context.Background(), "again"); err != nil {
		t.Fatalf("Prompt after a cancelled turn: %v", err)
	}
	sink.waitText(t, "echo: again")
}

func TestCrashFailsPrompt(t *testing.T) {
	useState(t)
	sink := &recorder{}
	s, _ := start(t, sink, backend.StartOptions{})

	err := s.Prompt(context.Background(), "[crash]")
	if err == nil {
		t.Fatal("Prompt succeeded although the agent exited")
	}
	if msg := err.Error(); !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "crashing on purpose") {
		t.Errorf("error = %q, want the exit status and the agent's stderr", msg)
	}
	sink.waitText(t, "about to crash")
}

func TestResumeDropsReplayedHistory(t *testing.T) {
	useState(t)
	dir := t.TempDir()
	first, id := start(t, &recorder{}, backend.StartOptions{Dir: dir})
	if err := first.Prompt(context.Background(), "first"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sink := &recorder{}
	second, resumed := start(t, sink, backend.StartOptions{Dir: dir, ResumeSessionID: id})
	if resumed != id {
		t.Fatalf("resumed session %q, want %q", resumed, id)
	}
	if events := sink.snapshot(); len(events) != 0 {
		t.Fatalf("Start delivered %d replayed events: %+v", len(events), events)
	}

	if err := second.Prompt(context.Background(), "second"); err != nil {
		t.Fatalf("Prompt after resume: %v", err)
	}
	if got := sink.text(); got != "echo: second" {
		t.Errorf("text = %q, want %q", got, "echo: second")
	}
}

func TestModelReachesCommandLine(t *testing.T) {
	state := useState(t)
	start(t, &recorder{}, backend.StartOptions{Model: "gpt-5"})

	invs, err := fakeagenttest.Invocations(state)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "gpt-5", "acp"}
	if len(invs) != 1 || !reflect.DeepEqual(invs[0].Args, want) {
		t.Errorf("invocations = %+v, want one with args %q", invs, want)
	}
}

func TestStartFailsWhenLoggedOut(t *testing.T) {
	state := useState(t)
	t.Setenv("FAKEAGENT_LOGGED_OUT", "1")
	defer func(d time.Duration) { authTimeout = d }(authTimeout)
	authTimeout = 200 * time.Millisecond

	s := New(fakeAgent, &recorder{})
	_, err := s.Start(context.Background(), backend.StartOptions{Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "agent login") {
		t.Fatalf("Start = %v, want an error that says to run agent login", err)
	}
	assertGone(t, lastPid(t, state))
}
