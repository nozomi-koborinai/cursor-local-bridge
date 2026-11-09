// Package lifecycle is the run state machine. It performs no IO. Callers feed
// it events and execute the effects it returns, in order.
package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
)

// State is the externally visible status of a run.
type State string

const (
	Queued             State = "queued"
	Running            State = "running"
	AwaitingPermission State = "awaiting_permission"
	Finished           State = "finished"
	Failed             State = "failed"
	// Stopping lasts from a Stop until its RemoveWorktree reports back.
	Stopping State = "stopping"
	Stopped  State = "stopped"
)

var states = map[State]bool{
	Queued: true, Running: true, AwaitingPermission: true,
	Finished: true, Failed: true, Stopping: true, Stopped: true,
}

func ParseState(s string) (State, error) {
	if !states[State(s)] {
		return "", fmt.Errorf("unknown run state %q", s)
	}
	return State(s), nil
}

// Active reports whether an agent process is attached to the run.
func (s State) Active() bool {
	return s == Queued || s == Running || s == AwaitingPermission
}

// CancelReason says why a pending permission was dropped.
type CancelReason string

const (
	CancelWithdrawn  CancelReason = "cancelled"
	CancelSuperseded CancelReason = "superseded"
	CancelTurnEnded  CancelReason = "turn_ended"
	CancelRunEnded   CancelReason = "run_ended"
)

// Permission is a tool call the agent wants to run and is waiting on.
type Permission struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	ToolCallID string          `json:"toolCallId,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Command    string          `json:"command,omitempty"`
	URL        string          `json:"url,omitempty"`
	ToolCall   json.RawMessage `json:"toolCall,omitempty"`
	Options    json.RawMessage `json:"options,omitempty"`
	Retries    int             `json:"-"`
}

// Run is the lifecycle part of a run record.
type Run struct {
	State State
	// Prompts holds prompts not yet sent to the agent, oldest first. While
	// queued it holds the prompt that starts the turn.
	Prompts []string
	// Permissions holds unanswered permission requests, oldest first. It is
	// non-empty exactly when State is AwaitingPermission.
	Permissions []Permission
	Error       string
}

type Event interface{ event() }

// Started reports that the agent process is ready for a prompt.
type Started struct{}

// TurnEnded reports that the agent finished the prompt it was given.
type TurnEnded struct{}

// PermissionRequested reports that the agent is blocked on a permission.
type PermissionRequested struct{ Permission Permission }

// PermissionAnswered approves or denies the oldest pending permission.
type PermissionAnswered struct{ Approve bool }

// PermissionCancelled reports that the agent withdrew or cancelled the
// permission request with ID (for example via $/cancel_request or stream cancel).
type PermissionCancelled struct {
	ID     string
	Reason CancelReason
}

// FollowUp queues a prompt, or resumes a finished run with it.
type FollowUp struct{ Prompt string }

// Stop ends the run and releases its worktree. The run is stopping until the
// removal reports back.
type Stop struct{}

// BackendFailed reports that the agent process failed or exited.
type BackendFailed struct{ Err string }

// WorktreeRemoved reports that a RemoveWorktree effect removed the worktree.
type WorktreeRemoved struct{}

// WorktreeRemoveFailed reports that a RemoveWorktree effect could not
// remove the worktree.
type WorktreeRemoveFailed struct{ Err string }

func (Started) event()              {}
func (TurnEnded) event()            {}
func (PermissionRequested) event()  {}
func (PermissionAnswered) event()   {}
func (PermissionCancelled) event()  {}
func (FollowUp) event()             {}
func (Stop) event()                 {}
func (BackendFailed) event()        {}
func (WorktreeRemoved) event()      {}
func (WorktreeRemoveFailed) event() {}

type Effect interface{ effect() }

// StartBackend starts an agent process. Resume continues the stored session.
type StartBackend struct{ Resume bool }

type SendPrompt struct{ Prompt string }

// AnswerPermission unblocks the agent's permission request with ID.
type AnswerPermission struct {
	ID      string
	Approve bool
}

// CancelPermission unblocks a pending permission request as cancelled and
// records a permission_cancelled event in the run log.
type CancelPermission struct {
	ID           string
	Reason       CancelReason
	SupersededBy string
}

// NotifyPermission delivers the permissionRequested webhook and arms the
// reminder timer for Permission.
type NotifyPermission struct {
	Permission Permission
}

// CloseBackend cancels any turn in flight and terminates the agent process.
type CloseBackend struct{}

// RemoveWorktree removes the run's worktree. The caller applies
// WorktreeRemoved or WorktreeRemoveFailed right after it.
type RemoveWorktree struct{}

// Notify delivers the completion webhook for a run that entered State.
type Notify struct{ State State }

func (StartBackend) effect()     {}
func (SendPrompt) effect()       {}
func (AnswerPermission) effect() {}
func (CancelPermission) effect() {}
func (NotifyPermission) effect() {}
func (CloseBackend) effect()     {}
func (RemoveWorktree) effect()   {}
func (Notify) effect()           {}

// ErrNotAllowed reports a command that the run's current state rejects.
var ErrNotAllowed = errors.New("not allowed")

func Launch(prompt string) (Run, []Effect) {
	return Run{State: Queued, Prompts: []string{prompt}}, []Effect{StartBackend{}}
}

// Apply returns the run after e and the effects to perform. Backend events
// that arrive after the run left the state they belong to are ignored, so a
// late report from a closed agent cannot revive a run. A removal report for
// a run that is not stopping, as after a stop of a failed run, changes
// nothing. Commands that the state rejects return an error wrapping
// ErrNotAllowed and leave r unchanged.
func Apply(r Run, e Event) (Run, []Effect, error) {
	switch e := e.(type) {
	case Started:
		if r.State != Queued {
			return r, nil, nil
		}
		next, effects := sendNextOrFinish(r)
		return next, effects, nil

	case TurnEnded:
		if r.State != Running && r.State != AwaitingPermission {
			return r, nil, nil
		}
		cancels := cancelAll(r.Permissions, CancelTurnEnded)
		r.Permissions = nil
		next, effects := sendNextOrFinish(r)
		return next, append(cancels, effects...), nil

	case PermissionRequested:
		if r.State != Running && r.State != AwaitingPermission {
			return r, nil, nil
		}
		p := e.Permission
		var effects []Effect
		kept := make([]Permission, 0, len(r.Permissions))
		for _, old := range r.Permissions {
			if superseded(p, old) {
				effects = append(effects, CancelPermission{ID: old.ID, Reason: CancelSuperseded, SupersededBy: p.ID})
				continue
			}
			kept = append(kept, old)
		}
		r.Permissions = append(kept, p)
		r.State = AwaitingPermission
		effects = append(effects, NotifyPermission{Permission: p})
		return r, effects, nil

	case PermissionCancelled:
		if r.State != Running && r.State != AwaitingPermission {
			return r, nil, nil
		}
		next, ok := withoutPermission(r.Permissions, e.ID)
		if !ok {
			return r, nil, nil
		}
		r.Permissions = next
		if len(r.Permissions) == 0 && r.State == AwaitingPermission {
			r.State = Running
		}
		return r, []Effect{CancelPermission{ID: e.ID, Reason: e.Reason}}, nil

	case PermissionAnswered:
		if r.State != AwaitingPermission {
			return r, nil, notAllowed("answer a permission for", r.State)
		}
		head := r.Permissions[0]
		r.Permissions = clone(r.Permissions[1:])
		if len(r.Permissions) == 0 {
			r.State = Running
		}
		return r, []Effect{AnswerPermission{ID: head.ID, Approve: e.Approve}}, nil

	case FollowUp:
		switch r.State {
		case Queued, Running, AwaitingPermission:
			r.Prompts = append(clone(r.Prompts), e.Prompt)
			return r, nil, nil
		case Finished:
			return Run{State: Queued, Prompts: []string{e.Prompt}}, []Effect{StartBackend{Resume: true}}, nil
		default:
			return r, nil, notAllowed("follow up on", r.State)
		}

	case Stop:
		switch r.State {
		case Queued, Running, AwaitingPermission:
			cancels := cancelAll(r.Permissions, CancelRunEnded)
			return Run{State: Stopping}, append(cancels, CloseBackend{}, RemoveWorktree{}), nil
		case Finished:
			return Run{State: Stopping}, []Effect{RemoveWorktree{}}, nil
		default:
			return r, []Effect{RemoveWorktree{}}, nil
		}

	case BackendFailed:
		if !r.State.Active() {
			return r, nil, nil
		}
		cancels := cancelAll(r.Permissions, CancelRunEnded)
		return Run{State: Failed, Error: e.Err}, append(cancels, CloseBackend{}, Notify{State: Failed}), nil

	case WorktreeRemoved:
		if r.State != Stopping {
			return r, nil, nil
		}
		return Run{State: Stopped}, []Effect{Notify{State: Stopped}}, nil

	case WorktreeRemoveFailed:
		if r.State != Stopping {
			return r, nil, nil
		}
		return Run{State: Failed, Error: e.Err}, []Effect{Notify{State: Failed}}, nil
	}
	panic(fmt.Sprintf("lifecycle: unhandled event %T", e))
}

func sendNextOrFinish(r Run) (Run, []Effect) {
	if len(r.Prompts) == 0 {
		return Run{State: Finished}, []Effect{CloseBackend{}, Notify{State: Finished}}
	}
	prompt := r.Prompts[0]
	r.State = Running
	r.Prompts = clone(r.Prompts[1:])
	return r, []Effect{SendPrompt{Prompt: prompt}}
}

func superseded(next, old Permission) bool {
	if next.ToolCallID != "" && next.ToolCallID == old.ToolCallID {
		return true
	}
	return next.Retries > old.Retries
}

func cancelAll(perms []Permission, reason CancelReason) []Effect {
	if len(perms) == 0 {
		return nil
	}
	out := make([]Effect, 0, len(perms))
	for _, p := range perms {
		out = append(out, CancelPermission{ID: p.ID, Reason: reason})
	}
	return out
}

func withoutPermission(perms []Permission, id string) ([]Permission, bool) {
	next := make([]Permission, 0, len(perms))
	found := false
	for _, p := range perms {
		if p.ID == id {
			found = true
			continue
		}
		next = append(next, p)
	}
	if !found {
		return perms, false
	}
	if len(next) == 0 {
		return nil, true
	}
	return next, true
}

func notAllowed(action string, s State) error {
	return fmt.Errorf("%w: cannot %s a %s run", ErrNotAllowed, action, s)
}

func clone[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return append([]T(nil), s...)
}
