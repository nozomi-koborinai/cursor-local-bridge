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
	Stopped            State = "stopped"
)

var states = map[State]bool{
	Queued: true, Running: true, AwaitingPermission: true,
	Finished: true, Failed: true, Stopped: true,
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

// Permission is a tool call the agent wants to run and is waiting on.
type Permission struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	ToolCall json.RawMessage `json:"toolCall,omitempty"`
	Options  json.RawMessage `json:"options,omitempty"`
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

// FollowUp queues a prompt, or resumes a finished run with it.
type FollowUp struct{ Prompt string }

// Stop ends the run and releases its worktree.
type Stop struct{}

// BackendFailed reports that the agent process failed or exited.
type BackendFailed struct{ Err string }

func (Started) event()             {}
func (TurnEnded) event()           {}
func (PermissionRequested) event() {}
func (PermissionAnswered) event()  {}
func (FollowUp) event()            {}
func (Stop) event()                {}
func (BackendFailed) event()       {}

type Effect interface{ effect() }

// StartBackend starts an agent process. Resume continues the stored session.
type StartBackend struct{ Resume bool }

type SendPrompt struct{ Prompt string }

// AnswerPermission unblocks the agent's permission request with ID.
type AnswerPermission struct {
	ID      string
	Approve bool
}

// CloseBackend cancels any turn in flight and terminates the agent process.
type CloseBackend struct{}

type RemoveWorktree struct{}

// Notify delivers the completion webhook for a run that entered State.
type Notify struct{ State State }

func (StartBackend) effect()     {}
func (SendPrompt) effect()       {}
func (AnswerPermission) effect() {}
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
// late report from a closed agent cannot revive a run. Commands that the
// state rejects return an error wrapping ErrNotAllowed and leave r unchanged.
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
		r.Permissions = nil
		next, effects := sendNextOrFinish(r)
		return next, effects, nil

	case PermissionRequested:
		if r.State != Running && r.State != AwaitingPermission {
			return r, nil, nil
		}
		r.State = AwaitingPermission
		r.Permissions = append(clone(r.Permissions), e.Permission)
		return r, nil, nil

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
			return Run{State: Stopped}, []Effect{CloseBackend{}, RemoveWorktree{}, Notify{State: Stopped}}, nil
		case Finished:
			return Run{State: Stopped}, []Effect{RemoveWorktree{}, Notify{State: Stopped}}, nil
		default:
			return r, []Effect{RemoveWorktree{}}, nil
		}

	case BackendFailed:
		if !r.State.Active() {
			return r, nil, nil
		}
		return Run{State: Failed, Error: e.Err}, []Effect{CloseBackend{}, Notify{State: Failed}}, nil
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

func notAllowed(action string, s State) error {
	return fmt.Errorf("%w: cannot %s a %s run", ErrNotAllowed, action, s)
}

func clone[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return append([]T(nil), s...)
}
