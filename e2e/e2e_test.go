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
		launched := c.run(t, "launch", "--repo", repo, "--model", "grok-4.6[effort=high,fast=false]", "hello")
		if launched.Status != "queued" || launched.Branch != "clb/"+launched.ID || launched.Model != "grok-4.6[effort=high,fast=false]" {
			t.Errorf("launch printed %+v, want a queued run on branch clb/<id> with requested model", launched)
		}
		hello = c.waitStatus(t, launched.ID, "finished")
		if hello.Summary != "echo: hello" || hello.Model != "grok-4.6[effort=high,fast=false]" {
			t.Errorf("finished run = %+v, want summary %q and model %q", hello, "echo: hello", "grok-4.6[effort=high,fast=false]")
		}
		if hello.AgentVersion != "2026.01.01-fake" {
			t.Errorf("agentVersion = %q, want 2026.01.01-fake", hello.AgentVersion)
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
		if got := d.header.Get("Authorization"); got != "Bearer "+secret {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+secret)
		}
		if got := d.header.Get("X-Clb-Event"); got != "statusChange" {
			t.Errorf("X-Clb-Event = %q, want statusChange", got)
		}
		if d.header.Get("X-Clb-Delivery") == "" {
			t.Error("X-Clb-Delivery is empty")
		}
		want := payload{
			Event: "statusChange", ID: hello.ID, Status: "finished", Repo: repo, Branch: hello.Branch,
			AgentVersion: "2026.01.01-fake", Summary: "echo: hello",
		}
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
		if got := runs[len(runs)-1].Model; got != "grok-4.6[effort=high,fast=false]" {
			t.Errorf("list model = %q, want %q", got, "grok-4.6[effort=high,fast=false]")
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

	step("a crashed run fails and followup resumes it on the same worktree", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "[crash] oops")
		failed := c.waitStatus(t, launched.ID, "failed")
		if !strings.Contains(failed.Error, "crashing on purpose") {
			t.Errorf("error = %q, want crash stderr", failed.Error)
		}
		checkSigned(t, hooks.wait(t, launched.ID, "failed", 1)[0])
		if err := os.WriteFile(filepath.Join(failed.Worktree, "wip.txt"), []byte("kept\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		resumed := c.run(t, "followup", launched.ID, "recover from crash")
		if resumed.Status != "queued" || resumed.Worktree != failed.Worktree {
			t.Errorf("followup on failed run = %+v", resumed)
		}
		done := c.waitStatus(t, launched.ID, "finished")
		if done.Summary != "echo: recover from crash" || done.SessionID != failed.SessionID {
			t.Errorf("resumed run = %+v", done)
		}
		if b, err := os.ReadFile(filepath.Join(done.Worktree, "wip.txt")); err != nil || string(b) != "kept\n" {
			t.Errorf("wip.txt in worktree = %q, %v", b, err)
		}
		checkSigned(t, hooks.wait(t, launched.ID, "finished", 1)[0])
	})

	step("a crash records stderr, the version, and the failure", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "[crash] now")
		failed := c.waitStatus(t, launched.ID, "failed")
		if failed.AgentVersion != "2026.01.01-fake" {
			t.Errorf("agentVersion = %q", failed.AgentVersion)
		}
		if !strings.Contains(failed.Error, "exit status 3") || !strings.Contains(failed.Error, "crashing on purpose") {
			t.Errorf("error = %q", failed.Error)
		}
		text := string(c.output(t, "", "status", "-o", "text", failed.ID))
		for _, want := range []string{failed.ID, "failed", failed.AgentVersion, "crashing on purpose", "clb logs --type stderr " + failed.ID} {
			if !strings.Contains(text, want) {
				t.Errorf("status -o text missing %q:\n%s", want, text)
			}
		}
		stderrEvents := parseEvents(t, c.output(t, "", "logs", "--type", "stderr", failed.ID))
		if len(stderrEvents) == 0 {
			t.Fatal("clb logs --type stderr printed nothing")
		}
		var saw bool
		for _, e := range stderrEvents {
			if e.Type != "stderr" {
				t.Errorf("filtered log included type %s", e.Type)
			}
			if strings.Contains(string(e.Data), "crashing on purpose") {
				saw = true
			}
		}
		if !saw {
			t.Fatalf("stderr events did not include the crash line: %s", c.output(t, "", "logs", "--type", "stderr", failed.ID))
		}
		followed := c.output(t, "", "logs", "--follow", "--type", "stderr", failed.ID)
		if !bytes.Contains(followed, []byte("crashing on purpose")) {
			t.Errorf("--follow --type stderr =\n%s", followed)
		}
		body := hooks.wait(t, failed.ID, "failed", 1)[0]
		checkSigned(t, body)
		if body.payload.Error != failed.Error || body.payload.AgentVersion != failed.AgentVersion {
			t.Errorf("webhook = %+v, want error %q and version %q", body.payload, failed.Error, failed.AgentVersion)
		}
	})

	step("a stderr prompt is logged and the turn still finishes", func(t *testing.T) {
		launched := c.run(t, "launch", "--repo", repo, "[stderr] hello")
		done := c.waitStatus(t, launched.ID, "finished")
		if done.Summary != "echo: [stderr] hello" || done.AgentVersion != "2026.01.01-fake" {
			t.Errorf("run summary %q version %q", done.Summary, done.AgentVersion)
		}
		events := parseEvents(t, c.output(t, "", "logs", "--type", "stderr", done.ID))
		var lines []string
		for _, e := range events {
			if e.Type != "stderr" {
				t.Errorf("filtered log included type %s", e.Type)
			}
			var d struct {
				Line string `json:"line"`
			}
			decode(t, e.Data, &d)
			lines = append(lines, d.Line)
		}
		if len(lines) < 3 || lines[0] != "diag: [stderr] hello" || len(lines[1]) != 4<<10 || lines[2] != "x" {
			t.Fatalf("stderr lines = %#v", lines)
		}
		procs := parseEvents(t, c.output(t, "", "logs", "--type", "agent_process", done.ID))
		if len(procs) != 1 || !strings.Contains(string(procs[0].Data), `"version":"2026.01.01-fake"`) {
			t.Fatalf("agent_process events = %+v", procs)
		}
	})

	var plain ipc.Run
	step("launch --no-worktree leaves the directory in place", func(t *testing.T) {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		note := filepath.Join(dir, "note.txt")
		if err := os.WriteFile(note, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		plain = c.run(t, "launch", "--repo", dir, "--no-worktree", "hello")
		if plain.Status != "queued" || plain.Repo != dir || plain.BaseRef != "" || plain.Branch != "" || plain.Worktree != "" {
			t.Fatalf("launch printed %+v, want a queued run in %s with no checkout", plain, dir)
		}
		finished := c.waitStatus(t, plain.ID, "finished")
		if finished.Summary != "echo: hello" || finished.Repo != dir {
			t.Fatalf("finished = %+v", finished)
		}
		status := c.output(t, "", "status", plain.ID)
		for _, key := range []string{`"baseRef"`, `"branch"`, `"worktree"`} {
			if bytes.Contains(status, []byte(key)) {
				t.Errorf("status included %s:\n%s", key, status)
			}
		}
		delivered := hooks.wait(t, plain.ID, "finished", 1)[0]
		checkSigned(t, delivered)
		if bytes.Contains(delivered.body, []byte(`"branch"`)) || delivered.payload.Repo != dir {
			t.Fatalf("webhook = %s", delivered.body)
		}
		if stopped := c.run(t, "stop", plain.ID); stopped.Status != "stopped" {
			t.Errorf("stop printed status %s, want stopped", stopped.Status)
		}
		if body, err := os.ReadFile(note); err != nil || string(body) != "keep\n" {
			t.Fatalf("note after stop = %q, %v", body, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatal(err)
		}
	})

	var active ipc.Run
	step("clean --dry-run reports leftover checkouts and deletes nothing", func(t *testing.T) {
		active = c.run(t, "launch", "--repo", repo, "[slow] count")
		c.waitStatus(t, active.ID, "running")
		before := gitBranches(t, repo)
		var report cleanReport
		decode(t, c.output(t, "", "clean", "--dry-run"), &report)
		if !report.DryRun {
			t.Fatalf("dryRun = false: %+v", report)
		}
		found, ok := findCleaned(report.Runs, hello.ID)
		if !ok || found.Status != "finished" || found.Branch != hello.Branch || found.Worktree != hello.Worktree || found.Repo != repo {
			t.Fatalf("dry-run entry for %s = %+v, ok %v", hello.ID, found, ok)
		}
		if _, ok := findCleaned(report.Runs, plain.ID); ok {
			t.Fatalf("dry-run included the directory run %s", plain.ID)
		}
		if _, ok := findCleaned(report.Runs, active.ID); ok {
			t.Fatalf("dry-run included the running run %s", active.ID)
		}
		if _, err := os.Stat(hello.Worktree); err != nil {
			t.Fatalf("worktree after dry-run: %v", err)
		}
		if got := gitBranches(t, repo); got != before {
			t.Fatalf("branches after dry-run:\n%s\nwant:\n%s", got, before)
		}
	})

	step("clean and gc remove finished worktrees and branches", func(t *testing.T) {
		var report cleanReport
		decode(t, c.output(t, "", "clean"), &report)
		if report.DryRun {
			t.Fatalf("clean ran as a dry-run: %+v", report)
		}
		if _, err := os.Stat(hello.Worktree); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("worktree after clean: %v, want it gone", err)
		}
		if gitBranchExists(t, repo, hello.Branch) {
			t.Errorf("branch %s still exists", hello.Branch)
		}
		if still := c.waitStatus(t, active.ID, "running"); still.Worktree == "" {
			t.Fatalf("running run lost its worktree: %+v", still)
		}
		if _, err := os.Stat(active.Worktree); err != nil {
			t.Fatalf("running worktree after clean: %v", err)
		}
		if !gitBranchExists(t, repo, active.Branch) {
			t.Errorf("clean deleted the running run's branch %s", active.Branch)
		}
		for _, line := range strings.Split(gitBranches(t, repo), "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "*"))
			if strings.HasPrefix(line, "clb/") && line != active.Branch {
				t.Errorf("clean left %s", line)
			}
		}
		decode(t, c.output(t, "", "gc"), &report)
		if report.DryRun || len(report.Runs) != 0 {
			t.Fatalf("gc = %+v, want no runs", report)
		}
		if stopped := c.run(t, "stop", active.ID); stopped.Status != "stopped" {
			t.Errorf("stop printed status %s, want stopped", stopped.Status)
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

func TestStallWatchdogAndResume(t *testing.T) {
	bin := t.TempDir()
	c := &clb{path: buildCLB(t, bin), home: shortTempDir(t)}
	agentPath, err := fakeagenttest.Build(bin)
	if err != nil {
		t.Fatal(err)
	}
	hooks := newReceiver(t)
	writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
		"agentPath":    agentPath,
		"stallTimeout": "300ms",
		"keepAwake":    true,
		"webhook": map[string]any{
			"url":    hooks.url,
			"secret": secret,
		},
	})
	state := t.TempDir()
	repo := gitRepo(t)
	startDaemon(t, c, state)

	launched := c.run(t, "launch", "--repo", repo, "[stall] freeze")
	failed := c.waitStatus(t, launched.ID, "failed")
	if failed.Error != "run stalled: no agent activity for 300ms" {
		t.Fatalf("failed error = %q", failed.Error)
	}
	d := hooks.wait(t, launched.ID, "failed", 1)[0]
	checkSigned(t, d)
	if d.payload.Error != "run stalled: no agent activity for 300ms" {
		t.Fatalf("webhook error = %q", d.payload.Error)
	}

	resumed := c.run(t, "followup", launched.ID, "continue after stall")
	if resumed.Status != "queued" || resumed.Worktree != launched.Worktree {
		t.Fatalf("followup after stall = %+v", resumed)
	}
	done := c.waitStatus(t, launched.ID, "finished")
	if done.Summary != "echo: continue after stall" {
		t.Fatalf("done summary = %q", done.Summary)
	}
	checkSigned(t, hooks.wait(t, launched.ID, "finished", 1)[0])
}

func TestPermissionGranularityE2E(t *testing.T) {
	bin := t.TempDir()
	c := &clb{path: buildCLB(t, bin), home: shortTempDir(t)}
	agentPath, err := fakeagenttest.Build(bin)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(c.home, "config.json"), map[string]any{
		"agentPath": agentPath,
	})
	startDaemon(t, c, t.TempDir())
	repo := gitRepo(t)

	multi := c.run(t, "launch", "--repo", repo, "[permissions] check")
	var waiting ipc.Run
	eventually(t, "two simultaneous pending permissions", func() bool {
		waiting = c.run(t, "status", multi.ID)
		return len(waiting.PendingPermissions) == 2
	})
	if waiting.PendingPermission == nil || waiting.PendingPermission.ID != "perm-1" {
		t.Fatalf("pendingPermission = %+v", waiting.PendingPermission)
	}

	var listed []ipc.Permission
	decode(t, c.output(t, "", "permissions", multi.ID), &listed)
	if len(listed) != 2 || listed[0].ID != "perm-1" || listed[1].ID != "perm-2" {
		t.Fatalf("clb permissions = %+v", listed)
	}
	var listedViaApprove []ipc.Permission
	decode(t, c.output(t, "", "approve", "--list", multi.ID), &listedViaApprove)
	if len(listedViaApprove) != 2 || listedViaApprove[0].ID != "perm-1" || listedViaApprove[1].ID != "perm-2" {
		t.Fatalf("clb approve --list = %+v", listedViaApprove)
	}

	afterPerm2 := c.run(t, "approve", "--permission", "perm-2", "--option", "allow-always", multi.ID)
	if afterPerm2.Status != "awaiting_permission" || len(afterPerm2.PendingPermissions) != 1 || afterPerm2.PendingPermission.ID != "perm-1" {
		t.Fatalf("after approving perm-2 = %+v", afterPerm2)
	}

	afterPerm1 := c.run(t, "approve", "--id", "perm-1", "--option", "reject", multi.ID)
	if afterPerm1.Status != "running" || len(afterPerm1.PendingPermissions) != 0 {
		t.Fatalf("after rejecting perm-1 = %+v", afterPerm1)
	}
	doneMulti := c.waitStatus(t, multi.ID, "finished")
	if want := "permissions: call-1=reject-once call-2=allow-always"; doneMulti.Summary != want {
		t.Fatalf("summary = %q, want %q", doneMulti.Summary, want)
	}

	denyAll := c.run(t, "launch", "--repo", repo, "[permissions] deny all")
	eventually(t, "two pending permissions for deny --all", func() bool {
		return len(c.run(t, "status", denyAll.ID).PendingPermissions) == 2
	})
	c.run(t, "deny", "--all", denyAll.ID)
	doneDenyAll := c.waitStatus(t, denyAll.ID, "finished")
	if want := "permissions: call-1=reject-once call-2=reject-once"; doneDenyAll.Summary != want {
		t.Fatalf("deny --all summary = %q, want %q", doneDenyAll.Summary, want)
	}

	approveAll := c.run(t, "launch", "--repo", repo, "[permissions] approve all always")
	eventually(t, "two pending permissions for approve --all --always", func() bool {
		return len(c.run(t, "status", approveAll.ID).PendingPermissions) == 2
	})
	c.run(t, "approve", "--all", "--always", approveAll.ID)
	doneApproveAll := c.waitStatus(t, approveAll.ID, "finished")
	if want := "permissions: call-1=allow-always call-2=allow-always"; doneApproveAll.Summary != want {
		t.Fatalf("approve --all --always summary = %q, want %q", doneApproveAll.Summary, want)
	}
}

func TestAutoApprovalPolicyE2E(t *testing.T) {
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
		},
	})
	writeJSON(t, filepath.Join(c.home, "policy.json"), map[string]any{
		"allow": map[string]any{
			"commands": []string{"rg *"},
		},
	})
	startDaemon(t, c, t.TempDir())
	repo := gitRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, ".clb"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(repo, ".clb", "policy.json"), map[string]any{
		"allow": map[string]any{
			"fetch": []string{"docs.example.com"},
		},
	})

	// 1. User-level ~/.clb/policy.json + repo-level <repo>/.clb/policy.json auto-approve
	// "rg pattern src" and "https://docs.example.com/guide", while compound command with &&
	// asks the user via awaiting_permission.
	globalRun := c.run(t, "launch", "--repo", repo, "[policy] global")
	waiting := c.waitStatus(t, globalRun.ID, "awaiting_permission")
	if waiting.PendingPermission == nil || waiting.PendingPermission.ID != "perm-3" {
		t.Fatalf("pending = %+v, want perm-3", waiting.PendingPermission)
	}
	c.run(t, "deny", globalRun.ID)
	done := c.waitStatus(t, globalRun.ID, "finished")
	if done.Summary != "policy: allow,allow,deny" {
		t.Fatalf("global summary = %q, want %q", done.Summary, "policy: allow,allow,deny")
	}
	hooks.wait(t, globalRun.ID, "finished", 1)
	if asked := hooks.matching(globalRun.ID, "awaiting_permission"); len(asked) != 1 || asked[0].payload.Permission == nil || asked[0].payload.Permission.ID != "perm-3" {
		t.Fatalf("awaiting_permission webhooks = %+v, want only perm-3", asked)
	}

	var answered []string
	for _, e := range c.logs(t, globalRun.ID) {
		if e.Type == "permission_answered" {
			answered = append(answered, string(e.Data))
		}
	}
	wantAnswered := []string{
		`{"id":"perm-1","approved":true,"auto":true,"rule":"allow.commands:rg *"}`,
		`{"id":"perm-2","approved":true,"auto":true,"rule":"allow.fetch:docs.example.com"}`,
		`{"id":"perm-3","approved":false,"option":"reject_once","optionId":"reject-once"}`,
	}
	if !slices.Equal(answered, wantAnswered) {
		t.Fatalf("permission_answered events = %v, want %v", answered, wantAnswered)
	}

	// 2. Per-run --policy overrides the user and repo policies (omitting docs.example.com).
	perRunPolicy := filepath.Join(t.TempDir(), "per-run-policy.json")
	writeJSON(t, perRunPolicy, map[string]any{
		"allow": map[string]any{
			"commands": []string{"rg *"},
		},
	})
	overrideRun := c.run(t, "launch", "--repo", repo, "--policy", perRunPolicy, "[policy] override")
	waitingFetch := c.waitStatus(t, overrideRun.ID, "awaiting_permission")
	if waitingFetch.PendingPermission == nil || waitingFetch.PendingPermission.ID != "perm-2" {
		t.Fatalf("override pending = %+v, want perm-2", waitingFetch.PendingPermission)
	}
	c.run(t, "deny", overrideRun.ID)
	waitingChain := c.waitStatus(t, overrideRun.ID, "awaiting_permission")
	if waitingChain.PendingPermission == nil || waitingChain.PendingPermission.ID != "perm-3" {
		t.Fatalf("override second pending = %+v, want perm-3", waitingChain.PendingPermission)
	}
	c.run(t, "deny", overrideRun.ID)
	overrideDone := c.waitStatus(t, overrideRun.ID, "finished")
	if overrideDone.Summary != "policy: allow,deny,deny" {
		t.Fatalf("override summary = %q, want %q", overrideDone.Summary, "policy: allow,deny,deny")
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

type cleanReport struct {
	DryRun bool             `json:"dryRun"`
	Runs   []ipc.CleanedRun `json:"runs"`
}

func findCleaned(runs []ipc.CleanedRun, id string) (ipc.CleanedRun, bool) {
	for _, run := range runs {
		if run.ID == id {
			return run, true
		}
	}
	return ipc.CleanedRun{}, false
}

func gitBranches(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "branch", "--list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git branch: %v\n%s", err, out)
	}
	return string(out)
}

func gitBranchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	err := cmd.Run()
	if err == nil {
		return true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git show-ref %s: %v", branch, err)
	return false
}

type payload struct {
	Event        string    `json:"event"`
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	Repo         string    `json:"repo"`
	Branch       string    `json:"branch"`
	AgentVersion string    `json:"agentVersion"`
	Summary      string    `json:"summary"`
	Error        string    `json:"error"`
	Permission   *permBody `json:"permission"`
	Reminder     int       `json:"reminder"`
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
