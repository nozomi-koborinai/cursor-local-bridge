// Package store keeps runs and their events in SQLite, the source of truth
// for every run.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// RunID identifies a run. It is also the suffix of the run's branch name.
type RunID string

type Run struct {
	ID        RunID
	Repo      string
	BaseRef   string
	Branch    string
	Worktree  string
	Backend   backend.Kind
	Model     string
	SessionID string
	// AgentVersion is the CLI version of the run's most recent agent
	// process, or "" when that process's version probe failed or no process
	// has started.
	AgentVersion string
	// Summary is the agent's final reply message from its most recent turn.
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

var ErrNotFound = errors.New("run not found")

type Store struct {
	db *sql.DB
}

const runColumns = `id, repo, base_ref, branch, worktree, backend, model, session_id, agent_version, state, prompts, permissions, error, stopped, summary, created_at, updated_at`

// Open opens or creates the database at path and applies pending migrations.
func Open(path string) (*Store, error) {
	// SQLite gives the -wal and -shm files the mode of the database file, so
	// creating it owner-only first keeps prompts and summaries private.
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	_ = f.Close()
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateRun inserts r. It fails if r.ID exists.
func (s *Store) CreateRun(ctx context.Context, r Run) error {
	row, err := encodeRun(r)
	if err != nil {
		return fmt.Errorf("create run %s: %w", r.ID, err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO runs (`+runColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Repo, r.BaseRef, r.Branch, r.Worktree, string(r.Backend), r.Model, r.SessionID, r.AgentVersion,
		string(r.Lifecycle.State), row.prompts, row.permissions, r.Lifecycle.Error, r.Lifecycle.Stopped, r.Summary,
		row.created, row.updated,
	)
	if err != nil {
		return fmt.Errorf("create run %s: %w", r.ID, err)
	}
	return nil
}

// GetRun returns the run with id or an error wrapping ErrNotFound.
func (s *Store) GetRun(ctx context.Context, id RunID) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("get run %s: %w", id, ErrNotFound)
	}
	return r, err
}

// ListRuns returns every run, newest first.
func (s *Store) ListRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM runs ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return out, nil
}

// NewEvent is an event to append to a run's log. The store assigns its Seq.
type NewEvent struct {
	Time time.Time
	Type string
	Data json.RawMessage
}

// UpdateRun overwrites every mutable column of the stored run with r and
// appends events to its log in the same transaction, so a reader that sees
// the new row also sees the events. It returns the appended events.
func (s *Store) UpdateRun(ctx context.Context, r Run, events ...NewEvent) ([]Event, error) {
	row, err := encodeRun(r)
	if err != nil {
		return nil, fmt.Errorf("update run %s: %w", r.ID, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update run %s: %w", r.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
		UPDATE runs SET
			repo = ?, base_ref = ?, branch = ?, worktree = ?, backend = ?, model = ?, session_id = ?,
			agent_version = ?,
			state = ?, prompts = ?, permissions = ?, error = ?, stopped = ?, summary = ?, updated_at = ?
		WHERE id = ?`,
		r.Repo, r.BaseRef, r.Branch, r.Worktree, string(r.Backend), r.Model, r.SessionID, r.AgentVersion,
		string(r.Lifecycle.State), row.prompts, row.permissions, r.Lifecycle.Error, r.Lifecycle.Stopped, r.Summary,
		row.updated, r.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update run %s: %w", r.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("update run %s: %w", r.ID, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("update run %s: %w", r.ID, ErrNotFound)
	}
	var appended []Event
	for _, e := range events {
		stored, err := insertEvent(ctx, tx, r.ID, e)
		if err != nil {
			return nil, fmt.Errorf("update run %s: %w", r.ID, err)
		}
		appended = append(appended, stored)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update run %s: %w", r.ID, err)
	}
	return appended, nil
}

// AppendEvent stores an event with the next sequence number for id.
func (s *Store) AppendEvent(ctx context.Context, id RunID, at time.Time, typ string, data json.RawMessage) (Event, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("append event %s: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()

	e, err := insertEvent(ctx, tx, id, NewEvent{Time: at, Type: typ, Data: data})
	if err != nil {
		return Event{}, fmt.Errorf("append event %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("append event %s: %w", id, err)
	}
	return e, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, id RunID, e NewEvent) (Event, error) {
	stored, err := eventData(e.Data)
	if err != nil {
		return Event{}, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE run_id = ?`, id).Scan(&seq); err != nil {
		return Event{}, err
	}
	when := formatTime(e.Time)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (run_id, seq, time, type, data) VALUES (?, ?, ?, ?, ?)`,
		id, seq, when, e.Type, stored,
	); err != nil {
		return Event{}, err
	}
	parsed, err := parseTime(when)
	if err != nil {
		return Event{}, err
	}
	return Event{RunID: id, Seq: seq, Time: parsed, Type: e.Type, Data: json.RawMessage(stored)}, nil
}

// Events returns the events of id with Seq greater than afterSeq, in order.
func (s *Store) Events(ctx context.Context, id RunID, afterSeq int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, time, type, data FROM events
		WHERE run_id = ? AND seq > ?
		ORDER BY seq ASC`, id, afterSeq)
	if err != nil {
		return nil, fmt.Errorf("events %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var (
			ev      Event
			when    string
			payload string
		)
		if err := rows.Scan(&ev.Seq, &when, &ev.Type, &payload); err != nil {
			return nil, fmt.Errorf("events %s: %w", id, err)
		}
		parsed, err := parseTime(when)
		if err != nil {
			return nil, fmt.Errorf("events %s: %w", id, err)
		}
		ev.RunID = id
		ev.Time = parsed
		ev.Data = json.RawMessage(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events %s: %w", id, err)
	}
	return out, nil
}

func migrate(db *sql.DB) error {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	// user_version counts applied files in lexical order, not the numeric
	// prefix in their names, so a renamed prefix cannot skip a file.
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := applyMigration(db, version, body); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func applyMigration(db *sql.DB, version int, body []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(string(body)); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

type encodedRun struct {
	prompts     string
	permissions string
	created     string
	updated     string
}

func encodeRun(r Run) (encodedRun, error) {
	prompts, err := encodeJSONSlice(r.Lifecycle.Prompts)
	if err != nil {
		return encodedRun{}, fmt.Errorf("prompts: %w", err)
	}
	permissions, err := encodeJSONSlice(r.Lifecycle.Permissions)
	if err != nil {
		return encodedRun{}, fmt.Errorf("permissions: %w", err)
	}
	return encodedRun{
		prompts:     prompts,
		permissions: permissions,
		created:     formatTime(r.CreatedAt),
		updated:     formatTime(r.UpdatedAt),
	}, nil
}

func encodeJSONSlice[T any](v []T) (string, error) {
	if len(v) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeJSONSlice[T any](s string) ([]T, error) {
	if s == "" || s == "[]" || s == "null" {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func eventData(data json.RawMessage) (string, error) {
	if len(data) == 0 {
		return "null", nil
	}
	if !json.Valid(data) {
		return "", errors.New("event data is not valid JSON")
	}
	return string(data), nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRun(sc rowScanner) (Run, error) {
	var (
		r                                      Run
		backendText, stateText                 string
		prompts, permissions, created, updated string
	)
	err := sc.Scan(
		&r.ID, &r.Repo, &r.BaseRef, &r.Branch, &r.Worktree,
		&backendText, &r.Model, &r.SessionID, &r.AgentVersion, &stateText,
		&prompts, &permissions, &r.Lifecycle.Error, &r.Lifecycle.Stopped, &r.Summary,
		&created, &updated,
	)
	if err != nil {
		return Run{}, err
	}
	kind, err := backend.ParseKind(backendText)
	if err != nil {
		return Run{}, fmt.Errorf("run %s: %w", r.ID, err)
	}
	r.Backend = kind
	state, err := lifecycle.ParseState(stateText)
	if err != nil {
		return Run{}, fmt.Errorf("run %s: %w", r.ID, err)
	}
	r.Lifecycle.State = state
	r.Lifecycle.Prompts, err = decodeJSONSlice[string](prompts)
	if err != nil {
		return Run{}, fmt.Errorf("run %s prompts: %w", r.ID, err)
	}
	r.Lifecycle.Permissions, err = decodeJSONSlice[lifecycle.Permission](permissions)
	if err != nil {
		return Run{}, fmt.Errorf("run %s permissions: %w", r.ID, err)
	}
	r.CreatedAt, err = parseTime(created)
	if err != nil {
		return Run{}, fmt.Errorf("run %s created_at: %w", r.ID, err)
	}
	r.UpdatedAt, err = parseTime(updated)
	if err != nil {
		return Run{}, fmt.Errorf("run %s updated_at: %w", r.ID, err)
	}
	return r, nil
}

// formatTime uses a fixed nanosecond width. Go's RFC3339Nano layout drops
// trailing zeros, and the shorter strings sort ahead of earlier instants.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
