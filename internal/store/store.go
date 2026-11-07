// Package store keeps runs and their events in SQLite, the source of truth
// for every run.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
)

// RunID identifies a run. It is also the suffix of the run's branch name.
type RunID string

// Run is one stored run.
type Run struct {
	ID        RunID
	Repo      string
	BaseRef   string
	Branch    string
	Worktree  string
	Backend   backend.Kind
	Model     string
	SessionID string
	// Summary is the agent's reply text from its most recent turn.
	Summary   string
	Lifecycle lifecycle.Run
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Event is one entry of a run's event log. Seq starts at 1 for each run.
type Event struct {
	RunID RunID
	Seq   int64
	Time  time.Time
	Type  string
	Data  json.RawMessage
}

// ErrNotFound reports a run ID with no stored run.
var ErrNotFound = errors.New("run not found")

// Store is a SQLite database opened by Open.
type Store struct{}

// Open opens or creates the database at path and applies pending migrations.
func Open(path string) (*Store, error) {
	panic("not implemented")
}

func (s *Store) Close() error {
	panic("not implemented")
}

// CreateRun inserts r. It fails if r.ID exists.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	panic("not implemented")
}

// GetRun returns the run with id or an error wrapping ErrNotFound.
func (s *Store) GetRun(ctx context.Context, id RunID) (Run, error) {
	panic("not implemented")
}

// ListRuns returns every run, newest first.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	panic("not implemented")
}

// UpdateRun overwrites every mutable column of the stored run with r.
func (s *Store) UpdateRun(ctx context.Context, r Run) error {
	panic("not implemented")
}

// AppendEvent stores an event with the next sequence number for id.
func (s *Store) AppendEvent(ctx context.Context, id RunID, at time.Time, typ string, data json.RawMessage) (Event, error) {
	panic("not implemented")
}

// Events returns the events of id with Seq greater than afterSeq, in order.
func (s *Store) Events(ctx context.Context, id RunID, afterSeq int64) ([]Event, error) {
	panic("not implemented")
}
