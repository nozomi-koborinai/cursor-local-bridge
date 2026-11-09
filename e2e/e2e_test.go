//go:build e2e

// Package e2e drives the clb binary end to end. The fake agent stands in for
// the Cursor CLI, and an httptest server receives the webhooks.
package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent/fakeagenttest"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

const (
	timeout = 30 * time.Second
	secret  = "e2e webhook secret"
)

func TestCLB(t *testing.T) {
	bin := t.TempDir()
	c := &clb{path: buildCLB(t, bin), home: shortTempDir(t)}
	agentPath, err := fakeagenttest.Build(bin)
	if err != nil {
		t.Fatal(err)
	}
	hooks := newReceiver(t)
	writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
		"agentPath": agentPath,
		"webhook": map[string]any{
			"url":    hooks.url,
			"secret": secret,
			"headers": map[string]string{
				"Authorization": "Bearer e2e-sender-key",
			},
		},
	})
	state := t.TempDir()
	repo := gitRepo(t)
	daemon := startDaemon(t, c, state)

	step := func(name string, f func(t *testing.T)) {
		if !t.Run(name, f) {
			t.FailNow()
		}
	}
	var hello, approved, denied, slow ipc.Run

	step("launch runs to finished", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "hello")
		if launched.Status != "queued" || launched.Branch != "clb/"+launched.ID {
			t.Errorf("launch printed %+v, want a queued run on branch clb/<id>", launched)
		}
		hello = c.waitStatus(t, launched.ID, "finished")
		if hello.Summary != "echo: hello" {
			t.Errorf("summary = %q, want %q", hello.Summary, "echo: hello")
		}
	})

	step("logs show the streamed events", func(t *testing.T) {
		eventually(t, "the webhook outcome in the logs", func() bool {
			return slices.ContainsFunc(c.logs(t, hello.ID), isType("webhook"))
		})
		out := c.output(t, "", "logs", hello.ID)
		events := parseEvents(t, out)
		counts := map[string]int{}
		for _, e := range events {
			counts[e.Type]++
		}
		if counts["agent_message_chunk"] < 3 || counts["tool_call"] < 1 || counts["prompt"] != 1 {
			t.Errorf("event counts = %v, want at least 3 agent_message_chunk, a tool_call, and a prompt", counts)
		}
		if counts["agent_thought_chunk"] != 0 || counts["available_commands_update"] != 0 {
			t.Errorf("event counts = %v, want no agent_thought_chunk or available_commands_update", counts)
		}
		if got := statuses(t, events); !slices.Equal(got, []string{"queued", "running", "finished"}) {
			t.Errorf("statuses = %v, want [queued running finished]", got)
		}
		var file []byte
		if !poll(func() bool {
			file, _ = os.ReadFile(filepath.Join(c.home, "logs", hello.ID+".ndjson"))
			return bytes.Equal(file, out)
		}) {
			t.Errorf("NDJSON log:\n%s\nwant the lines clb logs printed:\n%s", file, out)
		}
	})

	step("a signed webhook reports the finish", func(t *testing.T) {
		d := hooks.wait(t, hello.ID, "finished", 1)[0]
		checkSigned(t, d)
		if got := d.header.Get("Authorization"); got != "Bearer e2e-sender-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer e2e-sender-key")
		}
		if got := d.header.Get("X-Clb-Event"); got != "statusChange" {
			t.Errorf("X-Clb-Event = %q, want statusChange", got)
		}
		if d.header.Get("X-Clb-Delivery") == "" {
			t.Error("X-Clb-Delivery is empty")
		}
		want := payload{Event: "statusChange", ID: hello.ID, Status: "finished", Repo: repo, Branch: hello.Branch, Summary: "echo: hello"}
		if d.payload != want {
			t.Errorf("payload = %+v, want %+v", d.payload, want)
		}
	})

	step("a follow-up resumes the session in a new agent process", func(t *testing.T) {
		before := invocations(t, state)
		queued := c.runStdin(t, "tell me more\n", "followup", hello.ID, "-")
		if queued.Status != "queued" {
			t.Errorf("followup printed status %s, want queued", queued.Status)
		}
		done := c.waitStatus(t, hello.ID, "finished")
		if done.Summary != "echo: tell me more" {
			t.Errorf("summary = %q, want %q", done.Summary, "echo: tell me more")
		}
		if done.SessionID != hello.SessionID {
			t.Errorf("session = %q, want the first turn's %q", done.SessionID, hello.SessionID)
		}
		d := hooks.wait(t, hello.ID, "finished", 2)[1]
		checkSigned(t, d)
		if d.payload.Summary != "echo: tell me more" {
			t.Errorf("webhook summary = %q, want %q", d.payload.Summary, "echo: tell me more")
		}
		if started := len(invocations(t, state)) - len(before); started != 1 {
			t.Errorf("agent processes started for the follow-up = %d, want 1", started)
		}
		var transcript strings.Builder
		for _, e := range c.logs(t, hello.ID) {
			if e.Type != "agent_message_chunk" {
				continue
			}
			var chunk struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			decode(t, e.Data, &chunk)
			transcript.WriteString(chunk.Content.Text)
		}
		if got, want := transcript.String(), "reading fileecho: helloreading fileecho: tell me more"; got != want {
			t.Errorf("full transcript from logs = %q, want %q", got, want)
		}
	})

	step("approve and deny answer permission requests", func(t *testing.T) {
		for _, tc := range []struct {
			command, summary string
			run              *ipc.Run
		}{
			{"approve", "permission: approved", &approved},
			{"deny", "permission: denied", &denied},
		} {
			*tc.run = c.run(t, "launch", "--repo", repo, "[permission] "+tc.command)
			waiting := c.waitStatus(t, tc.run.ID, "awaiting_permission")
			if p := waiting.PendingPermission; p == nil || p.Title != "Run fake command" || p.ID != "perm-1" ||
				p.ToolCallID != "perm-1" || p.Kind != "execute" || p.Command != "true" {
				t.Errorf("pending permission = %+v, want the fake command", p)
			}
			asked := hooks.wait(t, tc.run.ID, "awaiting_permission", 1)[0]
			checkSigned(t, asked)
			if asked.header.Get("X-Clb-Event") != "permissionRequested" || asked.payload.Event != "permissionRequested" ||
				asked.payload.Permission == nil || asked.payload.Permission.ID != "perm-1" ||
				asked.payload.Permission.Kind != "execute" || asked.payload.Permission.Command != "true" ||
				asked.payload.Reminder != 0 {
				t.Errorf("permission webhook = %s", asked.body)
			}
			c.run(t, tc.command, tc.run.ID)
			if done := c.waitStatus(t, tc.run.ID, "finished"); done.Summary != tc.summary {
				t.Errorf("after %s: summary = %q, want %q", tc.command, done.Summary, tc.summary)
			}
		}
	})

	step("stop ends a run mid-turn", func(t *testing.T) {
		before := len(invocations(t, state))
		slow = c.run(t, "launch", "--repo", repo, "[slow] count")
		c.waitStatus(t, slow.ID, "running")
		eventually(t, "a tick event", func() bool {
			return slices.ContainsFunc(c.logs(t, slow.ID), func(e ipc.Event) bool {
				return e.Type == "agent_message_chunk" && strings.Contains(string(e.Data), "tick")
			})
		})
		started := invocations(t, state)
		if len(started) != before+1 {
			t.Fatalf("agent processes started for the run = %d, want 1", len(started)-before)
		}
		pid := started[before].Pid

		follow := c.start(t, nil, "logs", "--follow", slow.ID)
		t.Cleanup(follow.stop)
		eventually(t, "clb logs --follow to print", func() bool { return follow.stdout.Len() > 0 })

		if stopped := c.run(t, "stop", slow.ID); stopped.Status != "stopped" {
			t.Errorf("stop printed status %s, want stopped", stopped.Status)
		}
		if _, err := os.Stat(slow.Worktree); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("worktree after stop: %v, want it gone", err)
		}
		checkSigned(t, hooks.wait(t, slow.ID, "stopped", 1)[0])
		eventually(t, "the agent process to exit", func() bool {
			return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
		})
		follow.wait(t)
		if got := statuses(t, parseEvents(t, follow.stdout.Bytes())); !slices.Contains(got, "stopped") {
			t.Errorf("clb logs --follow printed statuses %v, want stopped among them", got)
		}
	})

	step("list shows every run, newest first", func(t *testing.T) {
		var runs []ipc.Run
		decode(t, c.output(t, "", "list"), &runs)
		var ids []string
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		if want := []string{slow.ID, denied.ID, approved.ID, hello.ID}; !slices.Equal(ids, want) {
			t.Errorf("list = %v, want %v", ids, want)
		}
	})

	step("a finished run leaves no background command running", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "[background] start a dev server")
		c.waitStatus(t, launched.ID, "finished")
		b, err := os.ReadFile(filepath.Join(state, "background.pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("background process %d still runs 10s after the run finished", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})

	step("stop commits an uncommitted file onto the run branch", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "hello")
		finished := c.waitStatus(t, launched.ID, "finished")
		if err := os.WriteFile(filepath.Join(finished.Worktree, "note.txt"), []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if stopped := c.run(t, "stop", launched.ID); stopped.Status != "stopped" {
			t.Errorf("stop printed status %s, want stopped", stopped.Status)
		}
		cmd := exec.Command("git", "-C", repo, "log", "-1", "--name-only", "--format=%s", finished.Branch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git log: %v\n%s", err, out)
		}
		if !strings.Contains(string(out), "note.txt") {
			t.Fatalf("git log = %q, want note.txt", out)
		}
	})

	step("SIGTERM stops the daemon cleanly", func(t *testing.T) {
		if err := daemon.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		daemon.wait(t)
		if strings.Contains(daemon.stderr.String(), "DATA RACE") {
			t.Error("the daemon reported a data race")
		}
	})
}

func TestTransportErrorAndPermissionReminder(t *testing.T) {
	bin := t.TempDir()
	c := &clb{path: buildCLB(t, bin), home: shortTempDir(t)}
	agentPath, err := fakeagenttest.Build(bin)
	if err != nil {
		t.Fatal(err)
	}
	hooks := newReceiver(t)
	writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
		"agentPath": agentPath,
		"webhook": map[string]any{
			"url":      hooks.url,
			"secret":   secret,
			"reminder": "200ms",
		},
	})
	startDaemon(t, c, t.TempDir())
	repo := gitRepo(t)

	launched := c.run(t, "launch", "--repo", repo, "[transport] hello")
	done := c.waitStatus(t, launched.ID, "finished")
	if done.Summary != "after" || strings.Contains(done.Summary, "RetriableError") {
		t.Fatalf("summary = %q", done.Summary)
	}
	var sawTransport bool
	for _, e := range c.logs(t, launched.ID) {
		if e.Type == "transport_error" && strings.Contains(string(e.Data), "RetriableError: http/2 stream closed") {
			sawTransport = true
		}
	}
	if !sawTransport {
		t.Fatal("logs have no transport_error event")
	}

	waitingRun := c.run(t, "launch", "--repo", repo, "[permission] remind")
	waiting := c.waitStatus(t, waitingRun.ID, "awaiting_permission")
	if waiting.PendingPermission == nil || waiting.PendingPermission.Command != "true" {
		t.Fatalf("pending = %+v", waiting.PendingPermission)
	}
	var reminders []delivery
	deadline := time.Now().Add(timeout)
	for len(reminders) < 2 && time.Now().Before(deadline) {
		for _, d := range hooks.matching(waitingRun.ID, "awaiting_permission") {
			if d.payload.Event == "permissionReminder" && !slices.ContainsFunc(reminders, func(got delivery) bool {
				return got.payload.Reminder == d.payload.Reminder
			}) {
				reminders = append(reminders, d)
			}
		}
		if len(reminders) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(reminders) < 2 || reminders[0].payload.Reminder != 1 || reminders[1].payload.Reminder != 2 {
		t.Fatalf("reminders = %+v", reminders)
	}
	checkSigned(t, reminders[0])
	if reminders[0].header.Get("X-Clb-Event") != "permissionReminder" || reminders[0].payload.Permission == nil ||
		reminders[0].payload.Permission.ID != "perm-1" {
		t.Fatalf("reminder = %s", reminders[0].body)
	}
	c.run(t, "approve", waitingRun.ID)
	if finished := c.waitStatus(t, waitingRun.ID, "finished"); finished.Summary != "permission: approved" {
		t.Fatalf("summary = %q", finished.Summary)
	}
}

type clb struct {
	path string
	home string
}

func (c *clb) command(args ...string) *exec.Cmd {
	cmd := exec.Command(c.path, args...)
	// The race runtime sleeps for a second before each exit unless told
	// otherwise, and the test polls clb many times. A race still makes clb
	// exit with status 66.
	race := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	cmd.Env = append(os.Environ(), "CLB_HOME="+c.home, "GORACE="+race)
	return cmd
}

func (c *clb) output(t *testing.T, stdin string, args ...string) []byte {
	t.Helper()
	cmd := c.command(args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("clb %s: %v\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return out
}

func (c *clb) run(t *testing.T, args ...string) ipc.Run {
	t.Helper()
	return c.runStdin(t, "", args...)
}

func (c *clb) runStdin(t *testing.T, stdin string, args ...string) ipc.Run {
	t.Helper()
	var r ipc.Run
	decode(t, c.output(t, stdin, args...), &r)
	return r
}

func (c *clb) logs(t *testing.T, id string) []ipc.Event {
	t.Helper()
	return parseEvents(t, c.output(t, "", "logs", id))
}

func (c *clb) waitStatus(t *testing.T, id, want string) ipc.Run {
	t.Helper()
	var r ipc.Run
	poll(func() bool {
		r = c.run(t, "status", id)
		return r.Status == want || r.Status == "failed"
	})
	if r.Status != want {
		t.Fatalf("run %s is %s (error %q), want %s", id, r.Status, r.Error, want)
	}
	return r
}

type process struct {
	cmd    *exec.Cmd
	stdout syncBuffer
	stderr syncBuffer
	done   chan struct{}
	err    error
}

func (c *clb) start(t *testing.T, env []string, args ...string) *process {
	t.Helper()
	p := &process{cmd: c.command(args...), done: make(chan struct{})}
	p.cmd.Env = append(p.cmd.Env, env...)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	return p
}

func (p *process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		t.Fatalf("%s did not exit within %s", p.cmd, timeout)
	}
	if p.err != nil {
		t.Fatalf("%s: %v\n%s", p.cmd, p.err, p.stderr.String())
	}
}

func (p *process) stop() {
	if p.exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(timeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func startDaemon(t *testing.T, c *clb, stateDir string) *process {
	t.Helper()
	d := c.start(t, []string{"FAKEAGENT_STATE_DIR=" + stateDir}, "daemon")
	t.Cleanup(func() {
		d.stop()
		if t.Failed() {
			t.Logf("daemon stderr:\n%s", d.stderr.String())
		}
	})
	ready := poll(func() bool { return d.exited() || c.command("list").Run() == nil })
	if d.exited() {
		t.Fatalf("the daemon exited: %v", d.err)
	}
	if !ready {
		t.Fatalf("the daemon did not answer within %s", timeout)
	}
	return d
}

type payload struct {
	Event      string    `json:"event"`
	ID         string    `json:"id"`
	Status     string    `json:"status"`
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	Summary    string    `json:"summary"`
	Error      string    `json:"error"`
	Permission *permBody `json:"permission"`
	Reminder   int       `json:"reminder"`
}

type permBody struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	Kind    string          `json:"kind"`
	Command string          `json:"command"`
	URL     string          `json:"url"`
	Options json.RawMessage `json:"options"`
}

type delivery struct {
	header  http.Header
	body    []byte
	payload payload
}

type receiver struct {
	url string

	mu         sync.Mutex
	deliveries []delivery
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := delivery{header: req.Header.Clone(), body: body}
		_ = json.Unmarshal(body, &d.payload)
		r.mu.Lock()
		r.deliveries = append(r.deliveries, d)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *receiver) wait(t *testing.T, id, status string, n int) []delivery {
	t.Helper()
	var got []delivery
	if !poll(func() bool {
		got = r.matching(id, status)
		return len(got) >= n
	}) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var all strings.Builder
		for _, d := range r.deliveries {
			all.Write(d.body)
			all.WriteByte('\n')
		}
		t.Fatalf("got %d %s webhooks for %s, want %d; every delivery:\n%s", len(got), status, id, n, all.String())
	}
	return got
}

func (r *receiver) matching(id, status string) []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []delivery
	for _, d := range r.deliveries {
		if d.payload.ID == id && d.payload.Status == status {
			out = append(out, d)
		}
	}
	return out
}

func checkSigned(t *testing.T, d delivery) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(d.body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := d.header.Get("X-Clb-Signature"); got != want {
		t.Errorf("X-Clb-Signature = %q, want %q for body %s", got, want, d.body)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func poll(ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return true
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	if !poll(ok) {
		t.Fatalf("timed out after %s waiting for %s", timeout, what)
	}
}

func buildCLB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "clb")
	out, err := exec.Command("go", "build", "-race", "-o", path, "github.com/nozomi-koborinai/cursor-local-bridge/cmd/clb").CombinedOutput()
	if err != nil {
		t.Fatalf("build clb: %v\n%s", err, out)
	}
	return path
}

// shortTempDir returns a new directory with a short path. macOS limits unix
// socket paths to 104 bytes, and t.TempDir paths are long there.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "clb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{
			"-c", "user.name=clb", "-c", "user.email=clb@example.invalid",
			"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
		}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("e2e\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "README.md")
	git("commit", "-q", "-m", "initial commit")
	return dir
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func invocations(t *testing.T, stateDir string) []fakeagenttest.Invocation {
	t.Helper()
	all, err := fakeagenttest.Invocations(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func parseEvents(t *testing.T, out []byte) []ipc.Event {
	t.Helper()
	var events []ipc.Event
	for line := range bytes.Lines(out) {
		var e ipc.Event
		decode(t, line, &e)
		events = append(events, e)
	}
	return events
}

func statuses(t *testing.T, events []ipc.Event) []string {
	t.Helper()
	var out []string
	for _, e := range events {
		if e.Type != "status" {
			continue
		}
		var d struct {
			Status string `json:"status"`
		}
		decode(t, e.Data, &d)
		out = append(out, d.Status)
	}
	return out
}

func isType(typ string) func(ipc.Event) bool {
	return func(e ipc.Event) bool { return e.Type == typ }
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}
