// Package lifecycle is the run state machine. It performs no IO. Callers feed
// it events and execute the effects it returns, in order.
package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

type PermissionOption string

const (
	OptionAllowOnce    PermissionOption = "allow_once"
	OptionAllowAlways  PermissionOption = "allow_always"
	OptionRejectOnce   PermissionOption = "reject_once"
	OptionRejectAlways PermissionOption = "reject_always"
)

func ParsePermissionOption(s string) (opt PermissionOption, allow, ok bool) {
	norm := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	switch norm {
	case "allow_once", "once":
		return OptionAllowOnce, true, true
	case "allow_always", "always":
		return OptionAllowAlways, true, true
	case "reject_once", "reject", "deny":
		return OptionRejectOnce, false, true
	case "reject_always":
		return OptionRejectAlways, false, true
	}
	return "", false, false
}

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
	// Error is non-empty exactly when State is Failed.
	Error string
	// Stopped reports that Stop has been run on a failed run, so FollowUp
	// cannot resume it even though State stays Failed.
	Stopped bool
}

// noReason is the error of a failure whose cause has no message, so a failed
// run always says something.
const noReason = "failed without an error message"

type Event interface{ event() }

// Started reports that the agent process is ready for a prompt.
type Started struct{}

// TurnEnded reports that the agent finished the prompt it was given.
type TurnEnded struct{}

// PermissionRequested reports that the agent is blocked on a permission.
type PermissionRequested struct{ Permission Permission }

// PermissionAnswered answers one or more pending permissions on a run. When
// PermissionID and PermissionIDs are empty and All is false, it answers the
// oldest pending permission.
type PermissionAnswered struct {
	Approve       bool
	Option        string
	PermissionID  string
	PermissionIDs []string
	All           bool
}

// PermissionCancelled reports that the agent withdrew or cancelled the
// permission request with ID (for example via $/cancel_request or stream cancel).
type PermissionCancelled struct {
	ID     string
	Reason CancelReason
}

// FollowUp queues a prompt, or resumes a finished or failed run with it.
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
	ID       string
	Approve  bool
	Option   string
	OptionID string
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
		return answerPermissions(r, e)

	case FollowUp:
		switch r.State {
		case Queued, Running, AwaitingPermission:
			r.Prompts = append(clone(r.Prompts), e.Prompt)
			return r, nil, nil
		case Finished:
			return Run{State: Queued, Prompts: []string{e.Prompt}}, []Effect{StartBackend{Resume: true}}, nil
		case Failed:
			if r.Stopped {
				return r, nil, notAllowed("follow up on", Stopped)
			}
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
		case Failed:
			r.Stopped = true
			return r, []Effect{RemoveWorktree{}}, nil
		default:
			return r, []Effect{RemoveWorktree{}}, nil
		}

	case BackendFailed:
		if !r.State.Active() {
			return r, nil, nil
		}
		cancels := cancelAll(r.Permissions, CancelRunEnded)
		return Run{State: Failed, Error: reason(e.Err)}, append(cancels, CloseBackend{}, Notify{State: Failed}), nil

	case WorktreeRemoved:
		if r.State == Failed {
			r.Stopped = true
			return r, nil, nil
		}
		if r.State != Stopping {
			return r, nil, nil
		}
		return Run{State: Stopped}, []Effect{Notify{State: Stopped}}, nil

	case WorktreeRemoveFailed:
		if r.State != Stopping {
			return r, nil, nil
		}
		return Run{State: Failed, Error: reason(e.Err), Stopped: true}, []Effect{Notify{State: Failed}}, nil
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

type offeredOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

func answerPermissions(r Run, e PermissionAnswered) (Run, []Effect, error) {
	if r.State != AwaitingPermission {
		return r, nil, notAllowed("answer a permission for", r.State)
	}
	ids := targetPermissionIDs(e.PermissionID, e.PermissionIDs)
	if e.All && len(ids) > 0 {
		return r, nil, fmt.Errorf("%w: cannot combine --all with a permission id", ErrNotAllowed)
	}
	var targets []Permission
	switch {
	case e.All:
		targets = r.Permissions
	case len(ids) == 0:
		targets = r.Permissions[:1]
	default:
		for _, id := range ids {
			if !slices.ContainsFunc(r.Permissions, func(p Permission) bool { return p.ID == id }) {
				return r, nil, fmt.Errorf("%w: permission %q is not pending", ErrNotAllowed, id)
			}
		}
		for _, p := range r.Permissions {
			if slices.Contains(ids, p.ID) {
				targets = append(targets, p)
			}
		}
	}
	rawOption := strings.TrimSpace(e.Option)
	effects := make([]Effect, 0, len(targets))
	for _, p := range targets {
		ans, err := resolvePermissionAnswer(p, e.Approve, rawOption)
		if err != nil {
			return r, nil, err
		}
		effects = append(effects, ans)
	}
	nextPerms := clone(r.Permissions)
	for _, p := range targets {
		nextPerms, _ = withoutPermission(nextPerms, p.ID)
	}
	r.Permissions = nextPerms
	if len(r.Permissions) == 0 {
		r.State = Running
	}
	return r, effects, nil
}

func targetPermissionIDs(single string, many []string) []string {
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	add(single)
	for _, id := range many {
		add(id)
	}
	return out
}

func resolvePermissionAnswer(p Permission, approve bool, rawOption string) (AnswerPermission, error) {
	var offered []offeredOption
	if len(p.Options) > 0 {
		if err := json.Unmarshal(p.Options, &offered); err != nil {
			return AnswerPermission{}, fmt.Errorf("%w: invalid options on permission %q", ErrNotAllowed, p.ID)
		}
	}
	if rawOption == "" {
		wantKind := string(OptionRejectOnce)
		if approve {
			wantKind = string(OptionAllowOnce)
		}
		for _, o := range offered {
			if o.Kind == wantKind {
				return AnswerPermission{ID: p.ID, Approve: approve, Option: o.Kind, OptionID: o.OptionID}, nil
			}
		}
		return AnswerPermission{ID: p.ID, Approve: approve}, nil
	}

	kind, allow, knownKind := ParsePermissionOption(rawOption)
	if !approve && knownKind && allow {
		return AnswerPermission{}, fmt.Errorf("%w: cannot deny with option %q", ErrNotAllowed, rawOption)
	}
	if len(offered) == 0 {
		if !knownKind {
			return AnswerPermission{}, fmt.Errorf("%w: unknown permission option %q", ErrNotAllowed, rawOption)
		}
		return AnswerPermission{ID: p.ID, Approve: allow, Option: string(kind)}, nil
	}
	if knownKind {
		for _, o := range offered {
			if o.Kind == string(kind) {
				return AnswerPermission{ID: p.ID, Approve: allow, Option: o.Kind, OptionID: o.OptionID}, nil
			}
		}
		return AnswerPermission{}, fmt.Errorf("%w: permission %q does not offer option %q", ErrNotAllowed, p.ID, rawOption)
	}
	for _, o := range offered {
		if o.OptionID == rawOption {
			optKind, optAllow, ok := ParsePermissionOption(o.Kind)
			if !ok {
				optKind = PermissionOption(o.Kind)
				optAllow = approve
			}
			if !approve && optAllow {
				return AnswerPermission{}, fmt.Errorf("%w: cannot deny with option %q", ErrNotAllowed, rawOption)
			}
			return AnswerPermission{ID: p.ID, Approve: optAllow, Option: string(optKind), OptionID: o.OptionID}, nil
		}
	}
	return AnswerPermission{}, fmt.Errorf("%w: permission %q does not offer option %q", ErrNotAllowed, p.ID, rawOption)
}

func reason(err string) string {
	if strings.TrimSpace(err) == "" {
		return noReason
	}
	return err
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
