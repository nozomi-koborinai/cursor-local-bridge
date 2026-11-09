package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

func TestPermissionRequestNotifiesAndLogsWithStatus(t *testing.T) {
	options := json.RawMessage(`[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]`)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "`npm test`",
			ToolCallID: "call-1",
			Kind:       "execute",
			Command:    "npm test",
			ToolCall:   json.RawMessage(`{"toolCallId":"call-1","kind":"execute"}`),
			Options:    options,
		})
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("denied")
		}
		s.sink.Event(chunk("permission: approved"))
		return nil
	}})

	run := h.launch("check")
	waiting := h.waitStatus(run.ID, "awaiting_permission")
	p := waiting.PendingPermission
	if p == nil || p.ID != "perm-1" || p.ToolCallID != "call-1" || p.Kind != "execute" || p.Command != "npm test" {
		t.Fatalf("pending = %+v", p)
	}
	events := h.logs(run.ID)
	if !slices.Contains(types(events), "permission_requested") || !slices.Contains(statuses(events), "awaiting_permission") {
		t.Fatalf("events at awaiting_permission = %v", types(events))
	}
	body := dataOf(events, "permission_requested")
	if strings.Contains(body, "retries") {
		t.Fatalf("permission_requested = %s", body)
	}
	var logged struct {
		ID      string          `json:"id"`
		Command string          `json:"command"`
		Options json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &logged); err != nil {
		t.Fatal(err)
	}
	if logged.ID != "perm-1" || logged.Command != "npm test" || string(logged.Options) != string(options) {
		t.Fatalf("permission_requested = %s", body)
	}

	hook := h.hooks.next(t)
	hook.Timestamp = time.Time{}
	if hook.Event != webhook.EventPermissionRequested || hook.RunID != run.ID || hook.Status != "awaiting_permission" ||
		hook.Summary != "" || hook.Reminder != 0 || hook.Permission == nil {
		t.Fatalf("webhook = %+v", hook)
	}
	if hook.Permission.ID != "perm-1" || hook.Permission.Kind != "execute" || hook.Permission.Command != "npm test" ||
		hook.Permission.Title != "`npm test`" || string(hook.Permission.Options) != string(options) {
		t.Fatalf("permission = %+v", hook.Permission)
	}

	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	if done := h.waitStatus(run.ID, "finished"); done.Summary != "permission: approved" {
		t.Fatalf("summary = %q", done.Summary)
	}
	finished := h.hooks.next(t)
	if finished.Event != webhook.EventStatusChange || finished.Status != "finished" {
		t.Fatalf("finished webhook = %+v", finished)
	}
	loggedHook := dataOf(h.logs(run.ID), "webhook")
	if loggedHook != `{"event":"permissionRequested","status":"awaiting_permission","permission":"perm-1","delivered":true}` {
		t.Fatalf("webhook event = %s", loggedHook)
	}
}

func TestPermissionReminderRepeatsUntilAnswered(t *testing.T) {
	const interval = 150 * time.Millisecond
	h := newHarness(t, &fakeAgent{onPrompt: askPermission})
	h.stop()
	h.webhook = func() (*webhook.Sender, error) {
		s := h.hooks.sender()
		s.Reminder = interval
		return s, nil
	}
	h.serve(h.newDaemon())

	run := h.launch("[permission]")
	h.waitStatus(run.ID, "awaiting_permission")
	var reminders []webhook.Payload
	requested := 0
	for requested < 1 || len(reminders) < 2 {
		got := h.hooks.next(t)
		switch got.Event {
		case webhook.EventPermissionRequested:
			if got.Permission == nil || got.Permission.ID != "perm-1" {
				t.Fatalf("permission webhook = %+v", got)
			}
			requested++
		case webhook.EventPermissionReminder:
			reminders = append(reminders, got)
		default:
			t.Fatalf("webhook = %+v", got)
		}
	}
	if len(reminders) < 2 || reminders[0].Reminder != 1 || reminders[1].Reminder != 2 ||
		reminders[0].Permission == nil || reminders[0].Permission.ID != "perm-1" || reminders[0].Status != "awaiting_permission" {
		t.Fatalf("reminders = %+v", reminders)
	}

	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	h.waitStatus(run.ID, "finished")
	quiet := time.Now().Add(4 * interval)
	for time.Now().Before(quiet) {
		select {
		case got := <-h.hooks.payloads:
			if got.Event == webhook.EventPermissionReminder && got.Reminder > reminders[len(reminders)-1].Reminder+1 {
				t.Fatalf("reminder %d arrived after the permission was answered", got.Reminder)
			}
		case <-time.After(interval):
		}
	}
	later := len(reminderNumbers(drainHooks(h, interval)))
	if later != 0 {
		t.Fatalf("%d reminders arrived after the timer should have stopped", later)
	}
}

func TestSameToolCallSupersedesPendingPermission(t *testing.T) {
	sendSecond := make(chan struct{})
	firstErr := make(chan error, 1)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		go func() {
			_, err := s.sink.Permission(ctx, backend.PermissionRequest{
				Title: "first", ToolCallID: "call-1", Kind: "execute", Command: "npm test",
			})
			firstErr <- err
		}()
		<-sendSecond
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title: "second", ToolCallID: "call-1", Kind: "execute", Command: "go test",
		})
		if err != nil {
			return err
		}
		if !approved {
			s.sink.Event(chunk("permission: denied"))
			return nil
		}
		s.sink.Event(chunk("permission: approved"))
		return nil
	}})

	run := h.launch("replace")
	if waiting := h.waitStatus(run.ID, "awaiting_permission"); waiting.PendingPermission == nil || waiting.PendingPermission.ID != "perm-1" {
		t.Fatalf("pending = %+v", waiting.PendingPermission)
	}
	close(sendSecond)
	replaced := waitPending(t, h, run.ID, "perm-2")
	if replaced.Command != "go test" || replaced.ToolCallID != "call-1" {
		t.Fatalf("replacement = %+v", replaced)
	}
	if err := <-firstErr; err == nil {
		t.Fatal("superseded permission returned no error")
	}
	events := h.logs(run.ID)
	if got := dataOf(events, "permission_cancelled"); got != `{"id":"perm-1","reason":"superseded","supersededBy":"perm-2"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	if done := h.waitStatus(run.ID, "finished"); done.Summary != "permission: approved" || done.PendingPermission != nil {
		t.Fatalf("finished = %+v", done)
	}
}

func TestHigherRetrySupersedesEarlierPermissions(t *testing.T) {
	sendNext := make(chan struct{})
	firstErr := make(chan error, 1)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		go func() {
			_, err := s.sink.Permission(ctx, backend.PermissionRequest{Title: "first", ToolCallID: "call-1", Kind: "execute", Command: "one"})
			firstErr <- err
		}()
		<-sendNext
		s.sink.Event(transportErrorEvent())
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{Title: "second", ToolCallID: "call-2", Kind: "fetch", URL: "https://example.com/hook"})
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("denied")
		}
		s.sink.Event(chunk("permission: approved"))
		return nil
	}})

	run := h.launch("retry")
	h.waitStatus(run.ID, "awaiting_permission")
	close(sendNext)
	replaced := waitPending(t, h, run.ID, "perm-2")
	if replaced.ToolCallID != "call-2" || replaced.URL != "https://example.com/hook" {
		t.Fatalf("replacement = %+v", replaced)
	}
	if err := <-firstErr; err == nil {
		t.Fatal("permission from before the transport error was still open")
	}
	if got := dataOf(h.logs(run.ID), "permission_cancelled"); got != `{"id":"perm-1","reason":"superseded","supersededBy":"perm-2"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "permission: approved" || strings.Contains(done.Summary, "RetriableError") {
		t.Fatalf("summary = %q", done.Summary)
	}
}

func TestEqualRetryDoesNotSupersede(t *testing.T) {
	sendNext := make(chan struct{})
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		s.sink.Event(transportErrorEvent())
		go func() {
			_, _ = s.sink.Permission(ctx, backend.PermissionRequest{Title: "first", ToolCallID: "call-1"})
		}()
		<-sendNext
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{Title: "second", ToolCallID: "call-2"})
		if err != nil {
			return err
		}
		if !approved {
			return errors.New("second denied")
		}
		s.sink.Event(chunk("permission: approved"))
		return nil
	}})

	run := h.launch("pair")
	if waiting := h.waitStatus(run.ID, "awaiting_permission"); waiting.PendingPermission == nil || waiting.PendingPermission.ID != "perm-1" {
		t.Fatalf("pending = %+v", waiting.PendingPermission)
	}
	close(sendNext)
	events := h.waitLog(run.ID, func(ev []ipc.Event) bool {
		return countType(ev, "permission_requested") >= 2
	})
	if slices.Contains(types(events), "permission_cancelled") {
		t.Fatalf("equal retries superseded a permission: %s", dataOf(events, "permission_cancelled"))
	}
	answered := h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	if answered.Status != "awaiting_permission" || answered.PendingPermission == nil || answered.PendingPermission.ID != "perm-2" {
		t.Fatalf("after first approve = %+v", answered)
	}
	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})
	if done := h.waitStatus(run.ID, "finished"); done.Summary != "permission: approved" {
		t.Fatalf("summary = %q", done.Summary)
	}
}

func TestCancelledRequestRecordsPermissionCancelled(t *testing.T) {
	cancels := make(chan context.CancelFunc, 1)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		pctx, cancel := context.WithCancel(ctx)
		cancels <- cancel
		_, err := s.sink.Permission(pctx, backend.PermissionRequest{Title: "withdrawn", ToolCallID: "call-1"})
		if err != nil {
			s.sink.Event(chunk("permission: cancelled"))
			return nil
		}
		s.sink.Event(chunk("permission: answered"))
		return nil
	}})

	run := h.launch("withdraw")
	h.waitStatus(run.ID, "awaiting_permission")
	cancel := <-cancels
	cancel()
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "permission: cancelled" || done.PendingPermission != nil {
		t.Fatalf("finished = %+v", done)
	}
	events := h.logs(run.ID)
	if got := dataOf(events, "permission_cancelled"); got != `{"id":"perm-1","reason":"cancelled"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
	if !slices.Contains(statuses(events), "running") {
		t.Fatalf("statuses = %v, want a return to running", statuses(events))
	}
}

func TestTurnEndCancelsPendingPermission(t *testing.T) {
	release := make(chan struct{})
	errs := make(chan error, 1)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		go func() {
			_, err := s.sink.Permission(ctx, backend.PermissionRequest{Title: "orphan", ToolCallID: "call-1"})
			errs <- err
		}()
		<-release
		s.sink.Event(chunk("done"))
		return nil
	}})

	run := h.launch("abandon")
	h.waitStatus(run.ID, "awaiting_permission")
	close(release)
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "done" || done.PendingPermission != nil {
		t.Fatalf("finished = %+v", done)
	}
	if err := <-errs; err == nil {
		t.Fatal("abandoned permission returned no error")
	}
	if got := dataOf(h.logs(run.ID), "permission_cancelled"); got != `{"id":"perm-1","reason":"turn_ended"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
}

func TestStopCancelsPendingPermission(t *testing.T) {
	permErr := make(chan error, 1)
	release := make(chan struct{})
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		_, err := s.sink.Permission(ctx, backend.PermissionRequest{Title: "stop me", ToolCallID: "call-1"})
		permErr <- err
		<-release
		return nil
	}})

	run := h.launch("stop")
	h.waitStatus(run.ID, "awaiting_permission")
	stopped := h.must(ipc.Request{Method: ipc.MethodStop, RunID: run.ID})
	if stopped.Status != "stopped" || stopped.PendingPermission != nil {
		t.Fatalf("stopped = %+v", stopped)
	}
	if err := <-permErr; err == nil {
		t.Fatal("stopped permission returned no error")
	}
	close(release)
	events := h.logs(run.ID)
	if got := dataOf(events, "permission_cancelled"); got != `{"id":"perm-1","reason":"run_ended"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
	var prev string
	for _, e := range events {
		if e.Type == "status" && prev == "permission_cancelled" {
			var body struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(e.Data, &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != "stopped" {
				t.Fatalf("status after permission_cancelled = %s", body.Status)
			}
			return
		}
		prev = e.Type
	}
	t.Fatalf("permission_cancelled was not committed with stopped: %v", types(events))
}

func TestBackendFailureCancelsPendingPermission(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		go func() {
			_, _ = s.sink.Permission(ctx, backend.PermissionRequest{Title: "doomed", ToolCallID: "call-1"})
		}()
		<-release
		return errors.New("agent exited")
	}})

	run := h.launch("fail")
	h.waitStatus(run.ID, "awaiting_permission")
	close(release)
	failed := h.waitStatus(run.ID, "failed")
	if failed.PendingPermission != nil || !strings.Contains(failed.Error, "agent exited") {
		t.Fatalf("failed = %+v", failed)
	}
	if got := dataOf(h.logs(run.ID), "permission_cancelled"); got != `{"id":"perm-1","reason":"run_ended"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
}

func TestTransportErrorIsStoredApartFromSummary(t *testing.T) {
	const bad = "RetriableError: http/2 stream closed"
	h := newHarness(t, &fakeAgent{onPrompt: func(_ context.Context, s *fakeSession, _ string) error {
		s.sink.Event(chunk("before"))
		s.sink.Event(transportErrorEvent())
		s.sink.Event(chunk("after"))
		return nil
	}})
	run := h.launch("go")
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "after" || strings.Contains(done.Summary, "RetriableError") {
		t.Fatalf("summary = %q", done.Summary)
	}
	events := h.logs(run.ID)
	if !slices.Contains(types(events), backend.EventTransportError) {
		t.Fatalf("types = %v", types(events))
	}
	if !strings.Contains(dataOf(events, backend.EventTransportError), bad) {
		t.Fatalf("transport_error = %s", dataOf(events, backend.EventTransportError))
	}
}

func transportErrorEvent() backend.Event {
	return backend.Event{
		Type: backend.EventTransportError,
		Data: json.RawMessage(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"RetriableError: http/2 stream closed"}}`),
	}
}

func waitPending(t *testing.T, h *harness, id, permID string) *ipc.Permission {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last ipc.Run
	for time.Now().Before(deadline) {
		last = h.must(ipc.Request{Method: ipc.MethodStatus, RunID: id})
		if last.PendingPermission != nil && last.PendingPermission.ID == permID {
			return last.PendingPermission
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending = %+v, want %s", last.PendingPermission, permID)
	return nil
}

func countType(events []ipc.Event, typ string) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func drainHooks(h *harness, d time.Duration) []webhook.Payload {
	var out []webhook.Payload
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case p := <-h.hooks.payloads:
			out = append(out, p)
		case <-timer.C:
			return out
		}
	}
}

func reminderNumbers(payloads []webhook.Payload) []int {
	var out []int
	for _, p := range payloads {
		if p.Event == webhook.EventPermissionReminder {
			out = append(out, p.Reminder)
		}
	}
	return out
}
