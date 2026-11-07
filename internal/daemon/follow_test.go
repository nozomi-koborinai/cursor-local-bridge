package daemon_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"modernc.org/sqlite"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

// A follower reads the run, then its events, and exits once the run is no
// longer active and no events are new. This test reads the same way from a
// second connection at the instant each status event is inserted, which is
// the instant a follower is most likely to miss it.
func TestAFollowerThatSeesTheRunEndHasItsStatusLine(t *testing.T) {
	registerStatusProbe()
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		if prompt == "fail" {
			return errors.New("agent exited with status 3")
		}
		return echo(ctx, s, prompt)
	}})
	reader, err := store.Open(h.cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	type view struct {
		state lifecycle.State
		last  string
	}
	var (
		mu    sync.Mutex
		views []view
	)
	setStatusProbe(t, h.cfg.DBPath(), func(id string) {
		ctx := context.Background()
		run, err := reader.GetRun(ctx, store.RunID(id))
		if err != nil {
			t.Errorf("read run %s: %v", id, err)
			return
		}
		events, err := reader.Events(ctx, store.RunID(id), 0)
		if err != nil {
			t.Errorf("read events of %s: %v", id, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		views = append(views, view{run.Lifecycle.State, lastStatus(events)})
	})

	finished := h.launch("hello")
	h.waitStatus(finished.ID, "finished")
	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: finished.ID, Prompt: "again"})
	h.waitStatus(finished.ID, "finished")
	h.must(ipc.Request{Method: ipc.MethodStop, RunID: finished.ID})
	failed := h.launch("fail")
	h.waitStatus(failed.ID, "failed")

	written := len(statuses(h.logs(finished.ID))) + len(statuses(h.logs(failed.ID)))
	mu.Lock()
	defer mu.Unlock()
	if len(views) != written {
		t.Fatalf("the probe ran %d times for %d status events", len(views), written)
	}
	sawEnded := false
	for _, v := range views {
		if v.state.Active() {
			continue
		}
		sawEnded = true
		if v.last != string(v.state) {
			t.Errorf("a reader saw the run %s while its last status event was %q", v.state, v.last)
		}
	}
	if !sawEnded {
		t.Error("the probe never saw a run that had ended, so it cannot tell a missing status line")
	}
}

var statusProbe struct {
	register sync.Once
	mu       sync.Mutex
	fn       func(runID string)
}

// registerStatusProbe must run before the store under test opens its
// connection, because SQLite adds functions to a connection when it opens.
func registerStatusProbe() {
	statusProbe.register.Do(func() {
		sqlite.MustRegisterScalarFunction("clb_test_status_written", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			statusProbe.mu.Lock()
			fn := statusProbe.fn
			statusProbe.mu.Unlock()
			if fn != nil {
				fn(fmt.Sprint(args[0]))
			}
			return nil, nil
		})
	})
}

// setStatusProbe makes every insert of a status event into the database at
// path call fn with the event's run ID, inside the inserting transaction.
func setStatusProbe(t *testing.T, path string, fn func(runID string)) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`
		CREATE TRIGGER status_written AFTER INSERT ON events WHEN NEW.type = 'status'
		BEGIN SELECT clb_test_status_written(NEW.run_id); END`); err != nil {
		t.Fatal(err)
	}
	statusProbe.mu.Lock()
	statusProbe.fn = fn
	statusProbe.mu.Unlock()
	t.Cleanup(func() {
		statusProbe.mu.Lock()
		statusProbe.fn = nil
		statusProbe.mu.Unlock()
	})
}

func lastStatus(events []store.Event) string {
	last := ""
	for _, e := range events {
		if e.Type != "status" {
			continue
		}
		var s struct{ Status string }
		_ = json.Unmarshal(e.Data, &s)
		last = s.Status
	}
	return last
}
