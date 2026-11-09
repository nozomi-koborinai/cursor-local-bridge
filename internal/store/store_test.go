package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
)

func TestRunRoundTrip(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	created := time.Date(2026, 11, 7, 1, 2, 3, 4, time.FixedZone("JST", 9*3600))
	updated := created.Add(time.Minute)
	tool := json.RawMessage(`{"name":"shell","input":{"cmd":"ls \"x\""}}`)
	in := Run{
		ID:           "run-1",
		Repo:         "/repo",
		BaseRef:      "abc123",
		Branch:       "clb/run-1",
		Worktree:     "/wt/run-1",
		Backend:      backend.ACP,
		Model:        "composer",
		SessionID:    "sess-1",
		AgentVersion: "2026.01.01-fake",
		Summary:      "did the thing",
		Lifecycle: lifecycle.Run{
			State:   lifecycle.AwaitingPermission,
			Prompts: []string{`say "hi"`, "次"},
			Permissions: []lifecycle.Permission{{
				ID:       "perm-1",
				Title:    "Run ls",
				ToolCall: tool,
				Options:  json.RawMessage(`[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]`),
			}},
			Error: "partial",
		},
		CreatedAt: created,
		UpdatedAt: updated,
	}
	if err := s.CreateRun(ctx, in); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.CreateRun(ctx, in); err == nil {
		t.Fatal("duplicate create succeeded")
	}
	got, err := s.GetRun(ctx, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	assertRun(t, got, in)

	in.Repo = "/repo2"
	in.BaseRef = "def456"
	in.Branch = "clb/run-1b"
	in.Worktree = "/wt/run-1b"
	in.Backend = backend.Print
	in.Model = "other"
	in.SessionID = "sess-2"
	in.AgentVersion = "2026.01.08-abcdef0"
	in.Summary = "next"
	in.Lifecycle = lifecycle.Run{State: lifecycle.Failed, Error: "later", Stopped: true}
	in.CreatedAt = updated.Add(2 * time.Hour)
	in.UpdatedAt = updated.Add(time.Hour)
	if _, err := s.UpdateRun(ctx, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = s.GetRun(ctx, in.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	want := in
	want.CreatedAt = created
	assertRun(t, got, want)

	empty := Run{
		ID:        "empty",
		Backend:   backend.Print,
		Lifecycle: lifecycle.Run{State: lifecycle.Queued},
		CreatedAt: created,
		UpdatedAt: created,
	}
	if err := s.CreateRun(ctx, empty); err != nil {
		t.Fatalf("create empty: %v", err)
	}
	got, err = s.GetRun(ctx, empty.ID)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if !reflect.DeepEqual(got.Lifecycle, lifecycle.Run{State: lifecycle.Queued}) {
		t.Fatalf("lifecycle = %#v", got.Lifecycle)
	}
	var prompts, permissions string
	if err := s.db.QueryRowContext(ctx, `SELECT prompts, permissions FROM runs WHERE id = ?`, empty.ID).Scan(&prompts, &permissions); err != nil {
		t.Fatal(err)
	}
	if prompts != "[]" || permissions != "[]" {
		t.Fatalf("stored slices prompts=%s permissions=%s", prompts, permissions)
	}
}

func TestMigrationAddsAgentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clb.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := migrationFS.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	const id = "run-old"
	_, err = db.Exec(`
		INSERT INTO runs (
			id, repo, base_ref, branch, worktree, backend, model, session_id,
			state, prompts, permissions, error, summary, created_at, updated_at
		) VALUES (?, '/repo', 'main', 'clb/run-old', '/wt', 'acp', '', 'sess', 'finished', '[]', '[]', '', 'done', ?, ?)`,
		id, "2026-01-01T00:00:00.000000000Z", "2026-01-01T00:00:00.000000000Z",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got, err := s.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentVersion != "" || got.Summary != "done" || got.Lifecycle.State != lifecycle.Finished {
		t.Fatalf("upgraded run = %+v, want an empty agent version", got)
	}
	got.AgentVersion = "2026.01.01-fake"
	if _, err := s.UpdateRun(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentVersion != "2026.01.01-fake" {
		t.Fatalf("agent version = %q", got.AgentVersion)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

func TestNotFound(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	_, err := s.GetRun(ctx, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("get err = %v", err)
	}
	_, err = s.UpdateRun(ctx, Run{
		ID:        "missing",
		Backend:   backend.ACP,
		Lifecycle: lifecycle.Run{State: lifecycle.Queued},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update err = %v", err)
	}
}

func TestEventsSeqFilterAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clb.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := t.Context()
	at := time.Date(2026, 3, 4, 5, 6, 7, 8, time.UTC)
	for _, id := range []RunID{"run-a", "run-b"} {
		if err := s.CreateRun(ctx, newRun(id, at)); err != nil {
			t.Fatal(err)
		}
	}
	payload := json.RawMessage(`{"k":"v","n":1}`)
	first, err := s.AppendEvent(ctx, "run-a", at, "sessionUpdate", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AppendEvent(ctx, "run-b", at.Add(time.Second), "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.AppendEvent(ctx, "run-a", at.Add(2*time.Second), "sessionUpdate", json.RawMessage(`{"n":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, "run-a", at, "bad", json.RawMessage(`{`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	assertEvent(t, first, Event{RunID: "run-a", Seq: 1, Time: at, Type: "sessionUpdate", Data: payload})
	assertEvent(t, second, Event{RunID: "run-b", Seq: 1, Time: at.Add(time.Second), Type: "other", Data: json.RawMessage("null")})
	assertEvent(t, third, Event{RunID: "run-a", Seq: 2, Time: at.Add(2 * time.Second), Type: "sessionUpdate", Data: json.RawMessage(`{"n":2}`)})

	got, err := s.Events(ctx, "run-a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("afterSeq events = %#v", got)
	}
	assertEvent(t, got[0], third)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("user_version = %d", version)
	}
	got, err = s.Events(ctx, "run-a", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("reopened events = %#v", got)
	}
	assertEvent(t, got[0], first)
	assertEvent(t, got[1], third)
	if string(got[0].Data) != string(payload) {
		t.Fatalf("data = %s", got[0].Data)
	}

	fourth, err := s.AppendEvent(ctx, "run-a", at, "sessionUpdate", json.RawMessage(`{"n":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Seq != 3 {
		t.Fatalf("seq after reopen = %d", fourth.Seq)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, "run-a"); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE run_id = ?`, "run-a").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("events left after delete = %d", left)
	}
}

func TestUpdateRunCommitsTheRowAndItsEventsTogether(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	at := time.Date(2026, 11, 7, 1, 2, 3, 0, time.UTC)
	queued := newRun("run-1", at)
	if err := s.CreateRun(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, queued.ID, at, "status", json.RawMessage(`{"status":"queued"}`)); err != nil {
		t.Fatal(err)
	}

	running := queued
	running.Lifecycle.State = lifecycle.Running
	running.UpdatedAt = at.Add(time.Second)
	status := NewEvent{Time: running.UpdatedAt, Type: "status", Data: json.RawMessage(`{"status":"running"}`)}
	appended, err := s.UpdateRun(ctx, running, status)
	if err != nil {
		t.Fatal(err)
	}
	want := Event{RunID: running.ID, Seq: 2, Time: status.Time, Type: status.Type, Data: status.Data}
	if len(appended) != 1 {
		t.Fatalf("appended = %#v, want one event", appended)
	}
	assertEvent(t, appended[0], want)
	got, err := s.GetRun(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertRun(t, got, running)
	events, err := s.Events(ctx, running.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events after the update = %#v, want the status event", events)
	}
	assertEvent(t, events[0], want)

	if _, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER reject BEFORE INSERT ON events WHEN NEW.type = 'rejected'
		BEGIN SELECT RAISE(ABORT, 'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	stopped := running
	stopped.Lifecycle.State = lifecycle.Stopped
	if _, err := s.UpdateRun(ctx, stopped, NewEvent{Time: at, Type: "rejected", Data: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("an update whose event was rejected succeeded")
	}
	got, err = s.GetRun(ctx, running.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertRun(t, got, running)
	if events, err := s.Events(ctx, running.ID, 0); err != nil || len(events) != 2 {
		t.Fatalf("events after the rejected update = %d, %v; want the 2 from before", len(events), err)
	}
}

func TestListRunsOrder(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	later := base.Add(time.Nanosecond)
	for _, id := range []RunID{"m", "b"} {
		if err := s.CreateRun(ctx, newRun(id, base)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateRun(ctx, newRun("a", later)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []RunID
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	want := []RunID{"a", "m", "b"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestGetRunRejectsCorruptRows(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	if err := s.CreateRun(ctx, newRun("bad", time.Unix(0, 0).UTC())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET backend = 'nope' WHERE id = 'bad'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, "bad"); err == nil {
		t.Fatal("bad backend accepted")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET backend = 'acp', prompts = '{' WHERE id = 'bad'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRun(ctx, "bad"); err == nil {
		t.Fatal("bad prompts accepted")
	}
}

func TestDatabaseFilesAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clb.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateRun(t.Context(), newRun("run-1", time.Now())); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %v, want no group or other access", filepath.Base(p), perm)
		}
	}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "clb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newRun(id RunID, at time.Time) Run {
	return Run{
		ID:        id,
		Repo:      "/repo",
		BaseRef:   "main",
		Branch:    "clb/" + string(id),
		Worktree:  "/wt/" + string(id),
		Backend:   backend.ACP,
		Lifecycle: lifecycle.Run{State: lifecycle.Queued},
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func assertRun(t *testing.T, got, want Run) {
	t.Helper()
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("times got %s %s, want %s %s", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
	got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}
	want.CreatedAt, want.UpdatedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run = %#v, want %#v", got, want)
	}
}

func TestRunWithoutWorktree(t *testing.T) {
	plain := Run{Repo: "/path/to/repo"}
	if plain.HasWorktree() || plain.Dir() != plain.Repo {
		t.Fatalf("plain dir = %q, has worktree %v", plain.Dir(), plain.HasWorktree())
	}
	with := Run{Repo: "/path/to/repo", Worktree: "/path/to/wt"}
	if !with.HasWorktree() || with.Dir() != with.Worktree {
		t.Fatalf("worktree dir = %q, has worktree %v", with.Dir(), with.HasWorktree())
	}

	s := openStore(t)
	now := time.Now().UTC()
	in := Run{
		ID:        "run-plain",
		Repo:      "/path/to/repo",
		Backend:   backend.ACP,
		Lifecycle: lifecycle.Run{State: lifecycle.Finished},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateRun(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(t.Context(), in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != in.Repo || got.BaseRef != "" || got.Branch != "" || got.Worktree != "" || got.HasWorktree() || got.Dir() != in.Repo {
		t.Fatalf("stored = %+v", got)
	}
}

func assertEvent(t *testing.T, got, want Event) {
	t.Helper()
	if !got.Time.Equal(want.Time) {
		t.Fatalf("time %s, want %s", got.Time, want.Time)
	}
	got.Time, want.Time = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event = %#v, want %#v", got, want)
	}
}
