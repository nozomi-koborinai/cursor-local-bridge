// Package ipc is the wire protocol between the clb CLI and the daemon. A
// client connects to the daemon's unix socket, writes one Request as a JSON
// line, and reads Response JSON lines until the daemon closes the connection.
// The client keeps its side open until then. Closing it early cancels the
// call.
package ipc

import (
	"encoding/json"
	"time"
)

// Method names a daemon operation.
type Method string

const (
	MethodLaunch   Method = "launch"
	MethodStatus   Method = "status"
	MethodFollowUp Method = "followup"
	MethodStop     Method = "stop"
	MethodList     Method = "list"
	MethodLogs     Method = "logs"
	MethodApprove  Method = "approve"
	MethodDeny     Method = "deny"
)

// Request is one call. Fields a method does not use stay empty.
type Request struct {
	Method  Method `json:"method"`
	RunID   string `json:"runId,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Model   string `json:"model,omitempty"`
	Backend string `json:"backend,omitempty"`
	// Follow keeps a logs call open until the run is no longer active.
	Follow bool `json:"follow,omitempty"`
}

// Response is one reply line. Logs calls get one line per event. Every other
// call gets exactly one line.
type Response struct {
	Run   *Run   `json:"run,omitempty"`
	Runs  []Run  `json:"runs,omitempty"`
	Event *Event `json:"event,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error codes.
const (
	CodeInvalid  = "invalid"
	CodeNotFound = "not_found"
	CodeConflict = "conflict"
	CodeInternal = "internal"
)

// Error is a failed call.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Run is the public view of a run.
type Run struct {
	ID                string      `json:"id"`
	Status            string      `json:"status"`
	Repo              string      `json:"repo"`
	BaseRef           string      `json:"baseRef"`
	Branch            string      `json:"branch"`
	Worktree          string      `json:"worktree"`
	Backend           string      `json:"backend"`
	Model             string      `json:"model,omitempty"`
	SessionID         string      `json:"sessionId,omitempty"`
	Summary           string      `json:"summary,omitempty"`
	Error             string      `json:"error,omitempty"`
	QueuedPrompts     int         `json:"queuedPrompts"`
	PendingPermission *Permission `json:"pendingPermission,omitempty"`
	CreatedAt         time.Time   `json:"createdAt"`
	UpdatedAt         time.Time   `json:"updatedAt"`
}

// Permission is the oldest unanswered permission request of a run.
type Permission struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	ToolCall json.RawMessage `json:"toolCall,omitempty"`
}

// Event is one entry of a run's event log. The daemon writes the same JSON,
// one object per line, to the run's NDJSON log file.
type Event struct {
	RunID string          `json:"runId"`
	Seq   int64           `json:"seq"`
	Time  time.Time       `json:"time"`
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data"`
}
