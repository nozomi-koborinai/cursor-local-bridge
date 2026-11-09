package lifecycle

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLaunch(t *testing.T) {
	r, effects := Launch("fix the bug")
	want := Run{State: Queued, Prompts: []string{"fix the bug"}}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("run = %+v, want %+v", r, want)
	}
	if !reflect.DeepEqual(effects, []Effect{StartBackend{}}) {
		t.Fatalf("effects = %#v", effects)
	}
}

func TestApply(t *testing.T) {
	permA := Permission{ID: "perm-a", Title: "Run make"}
	permB := Permission{ID: "perm-b", Title: "Write file"}

	tests := []struct {
		name    string
		from    Run
		event   Event
		want    Run
		effects []Effect
		wantErr bool
	}{
		{
			name:    "started sends the launch prompt",
			from:    Run{State: Queued, Prompts: []string{"p1"}},
			event:   Started{},
			want:    Run{State: Running},
			effects: []Effect{SendPrompt{Prompt: "p1"}},
		},
		{
			name:    "started keeps follow-ups queued behind the first prompt",
			from:    Run{State: Queued, Prompts: []string{"p1", "p2"}},
			event:   Started{},
			want:    Run{State: Running, Prompts: []string{"p2"}},
			effects: []Effect{SendPrompt{Prompt: "p1"}},
		},
		{
			name:  "started after stop is ignored",
			from:  Run{State: Stopped},
			event: Started{},
			want:  Run{State: Stopped},
		},
		{
			name:    "turn end with no follow-up finishes and notifies",
			from:    Run{State: Running},
			event:   TurnEnded{},
			want:    Run{State: Finished},
			effects: []Effect{CloseBackend{}, Notify{State: Finished}},
		},
		{
			name:    "turn end sends the next follow-up",
			from:    Run{State: Running, Prompts: []string{"p2", "p3"}},
			event:   TurnEnded{},
			want:    Run{State: Running, Prompts: []string{"p3"}},
			effects: []Effect{SendPrompt{Prompt: "p2"}},
		},
		{
			name:    "turn end drops permissions the agent abandoned",
			from:    Run{State: AwaitingPermission, Permissions: []Permission{permA}},
			event:   TurnEnded{},
			want:    Run{State: Finished},
			effects: []Effect{CancelPermission{ID: "perm-a", Reason: CancelTurnEnded}, CloseBackend{}, Notify{State: Finished}},
		},
		{
			name:    "turn end with abandoned permissions sends the next follow-up",
			from:    Run{State: AwaitingPermission, Prompts: []string{"p2"}, Permissions: []Permission{permA}},
			event:   TurnEnded{},
			want:    Run{State: Running},
			effects: []Effect{CancelPermission{ID: "perm-a", Reason: CancelTurnEnded}, SendPrompt{Prompt: "p2"}},
		},
		{
			name:  "turn end after stop is ignored",
			from:  Run{State: Stopped},
			event: TurnEnded{},
			want:  Run{State: Stopped},
		},
		{
			name:    "permission request blocks the run",
			from:    Run{State: Running, Prompts: []string{"p2"}},
			event:   PermissionRequested{Permission: permA},
			want:    Run{State: AwaitingPermission, Prompts: []string{"p2"}, Permissions: []Permission{permA}},
			effects: []Effect{NotifyPermission{Permission: permA}},
		},
		{
			name:    "concurrent permission requests queue in order",
			from:    Run{State: AwaitingPermission, Permissions: []Permission{permA}},
			event:   PermissionRequested{Permission: permB},
			want:    Run{State: AwaitingPermission, Permissions: []Permission{permA, permB}},
			effects: []Effect{NotifyPermission{Permission: permB}},
		},
		{
			name:  "permission request while finished is ignored",
			from:  Run{State: Finished},
			event: PermissionRequested{Permission: permA},
			want:  Run{State: Finished},
		},
		{
			name:    "approve answers the oldest request and resumes",
			from:    Run{State: AwaitingPermission, Permissions: []Permission{permA}},
			event:   PermissionAnswered{Approve: true},
			want:    Run{State: Running},
			effects: []Effect{AnswerPermission{ID: "perm-a", Approve: true}},
		},
		{
			name:    "deny with another request pending keeps waiting",
			from:    Run{State: AwaitingPermission, Permissions: []Permission{permA, permB}},
			event:   PermissionAnswered{Approve: false},
			want:    Run{State: AwaitingPermission, Permissions: []Permission{permB}},
			effects: []Effect{AnswerPermission{ID: "perm-a", Approve: false}},
		},
		{
			name:    "approve without a pending request is rejected",
			from:    Run{State: Running},
			event:   PermissionAnswered{Approve: true},
			want:    Run{State: Running},
			wantErr: true,
		},
		{
			name:  "follow-up while queued waits behind the launch prompt",
			from:  Run{State: Queued, Prompts: []string{"p1"}},
			event: FollowUp{Prompt: "p2"},
			want:  Run{State: Queued, Prompts: []string{"p1", "p2"}},
		},
		{
			name:  "follow-up while running is queued",
			from:  Run{State: Running},
			event: FollowUp{Prompt: "p2"},
			want:  Run{State: Running, Prompts: []string{"p2"}},
		},
		{
			name:  "follow-up while awaiting permission is queued",
			from:  Run{State: AwaitingPermission, Permissions: []Permission{permA}},
			event: FollowUp{Prompt: "p2"},
			want:  Run{State: AwaitingPermission, Prompts: []string{"p2"}, Permissions: []Permission{permA}},
		},
		{
			name:    "follow-up on a finished run resumes the session",
			from:    Run{State: Finished},
			event:   FollowUp{Prompt: "p2"},
			want:    Run{State: Queued, Prompts: []string{"p2"}},
			effects: []Effect{StartBackend{Resume: true}},
		},
		{
			name:    "follow-up on a failed run resumes the session",
			from:    Run{State: Failed, Error: "boom"},
			event:   FollowUp{Prompt: "p2"},
			want:    Run{State: Queued, Prompts: []string{"p2"}},
			effects: []Effect{StartBackend{Resume: true}},
		},
		{
			name:    "follow-up on a stopped failed run is rejected",
			from:    Run{State: Failed, Error: "boom", Stopped: true},
			event:   FollowUp{Prompt: "p2"},
			want:    Run{State: Failed, Error: "boom", Stopped: true},
			wantErr: true,
		},
		{
			name:    "follow-up on a stopped run is rejected",
			from:    Run{State: Stopped},
			event:   FollowUp{Prompt: "p2"},
			want:    Run{State: Stopped},
			wantErr: true,
		},
		{
			name:    "stop mid-run closes the agent and removes the worktree",
			from:    Run{State: Running, Prompts: []string{"p2"}},
			event:   Stop{},
			want:    Run{State: Stopping},
			effects: []Effect{CloseBackend{}, RemoveWorktree{}},
		},
		{
			name:    "stop while queued closes the starting agent",
			from:    Run{State: Queued, Prompts: []string{"p1"}},
			event:   Stop{},
			want:    Run{State: Stopping},
			effects: []Effect{CloseBackend{}, RemoveWorktree{}},
		},
		{
			name:    "stop while awaiting permission drops the request",
			from:    Run{State: AwaitingPermission, Permissions: []Permission{permA}},
			event:   Stop{},
			want:    Run{State: Stopping},
			effects: []Effect{CancelPermission{ID: "perm-a", Reason: CancelRunEnded}, CloseBackend{}, RemoveWorktree{}},
		},
		{
			name:    "stop on a finished run releases the worktree",
			from:    Run{State: Finished},
			event:   Stop{},
			want:    Run{State: Stopping},
			effects: []Effect{RemoveWorktree{}},
		},
		{
			name:    "stop while stopping retries the removal",
			from:    Run{State: Stopping},
			event:   Stop{},
			want:    Run{State: Stopping},
			effects: []Effect{RemoveWorktree{}},
		},
		{
			name:    "removal while stopping stops the run and notifies",
			from:    Run{State: Stopping},
			event:   WorktreeRemoved{},
			want:    Run{State: Stopped},
			effects: []Effect{Notify{State: Stopped}},
		},
		{
			name:    "failed removal while stopping fails the run and notifies",
			from:    Run{State: Stopping},
			event:   WorktreeRemoveFailed{Err: "remove worktree: locked"},
			want:    Run{State: Failed, Error: "remove worktree: locked", Stopped: true},
			effects: []Effect{Notify{State: Failed}},
		},
		{
			name:  "removal after a stop of a failed run keeps the failure",
			from:  Run{State: Failed, Error: "boom"},
			event: WorktreeRemoved{},
			want:  Run{State: Failed, Error: "boom", Stopped: true},
		},
		{
			name:  "failed removal after a stop of a failed run keeps the failure",
			from:  Run{State: Failed, Error: "boom", Stopped: true},
			event: WorktreeRemoveFailed{Err: "remove worktree: locked"},
			want:  Run{State: Failed, Error: "boom", Stopped: true},
		},
		{
			name:  "removal after a stop of a stopped run changes nothing",
			from:  Run{State: Stopped},
			event: WorktreeRemoved{},
			want:  Run{State: Stopped},
		},
		{
			name:  "failed removal after a stop of a stopped run changes nothing",
			from:  Run{State: Stopped},
			event: WorktreeRemoveFailed{Err: "remove worktree: locked"},
			want:  Run{State: Stopped},
		},
		{
			name:    "follow-up while stopping is rejected",
			from:    Run{State: Stopping},
			event:   FollowUp{Prompt: "p2"},
			want:    Run{State: Stopping},
			wantErr: true,
		},
		{
			name:    "approve while stopping is rejected",
			from:    Run{State: Stopping},
			event:   PermissionAnswered{Approve: true},
			want:    Run{State: Stopping},
			wantErr: true,
		},
		{
			name:  "started while stopping is ignored",
			from:  Run{State: Stopping},
			event: Started{},
			want:  Run{State: Stopping},
		},
		{
			name:  "turn end while stopping is ignored",
			from:  Run{State: Stopping},
			event: TurnEnded{},
			want:  Run{State: Stopping},
		},
		{
			name:  "permission request while stopping is ignored",
			from:  Run{State: Stopping},
			event: PermissionRequested{Permission: permA},
			want:  Run{State: Stopping},
		},
		{
			name:  "backend failure while stopping is ignored",
			from:  Run{State: Stopping},
			event: BackendFailed{Err: "signal: killed"},
			want:  Run{State: Stopping},
		},
		{
			name:    "stop on a stopped run only retries worktree cleanup",
			from:    Run{State: Stopped},
			event:   Stop{},
			want:    Run{State: Stopped},
			effects: []Effect{RemoveWorktree{}},
		},
		{
			name:    "stop on a failed run keeps the failure",
			from:    Run{State: Failed, Error: "boom"},
			event:   Stop{},
			want:    Run{State: Failed, Error: "boom", Stopped: true},
			effects: []Effect{RemoveWorktree{}},
		},
		{
			name:    "backend failure while running fails the run",
			from:    Run{State: Running, Prompts: []string{"p2"}},
			event:   BackendFailed{Err: "agent exited with status 1"},
			want:    Run{State: Failed, Error: "agent exited with status 1"},
			effects: []Effect{CloseBackend{}, Notify{State: Failed}},
		},
		{
			name:  "backend failure while awaiting permission drops the request",
			from:  Run{State: AwaitingPermission, Permissions: []Permission{permA, permB}},
			event: BackendFailed{Err: "agent exited with status 1"},
			want:  Run{State: Failed, Error: "agent exited with status 1"},
			effects: []Effect{
				CancelPermission{ID: "perm-a", Reason: CancelRunEnded},
				CancelPermission{ID: "perm-b", Reason: CancelRunEnded},
				CloseBackend{},
				Notify{State: Failed},
			},
		},
		{
			name:    "backend failure while starting fails the run",
			from:    Run{State: Queued, Prompts: []string{"p1"}},
			event:   BackendFailed{Err: "not logged in"},
			want:    Run{State: Failed, Error: "not logged in"},
			effects: []Effect{CloseBackend{}, Notify{State: Failed}},
		},
		{
			name:  "backend failure after stop is ignored",
			from:  Run{State: Stopped},
			event: BackendFailed{Err: "signal: killed"},
			want:  Run{State: Stopped},
		},
		{
			name:  "backend failure after finish is ignored",
			from:  Run{State: Finished},
			event: BackendFailed{Err: "exit"},
			want:  Run{State: Finished},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, effects, err := Apply(tt.from, tt.event)
			if tt.wantErr != errors.Is(err, ErrNotAllowed) {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("run = %+v, want %+v", got, tt.want)
			}
			if !reflect.DeepEqual(effects, tt.effects) {
				t.Errorf("effects = %#v, want %#v", effects, tt.effects)
			}
		})
	}
}

func TestApplyDoesNotAliasInput(t *testing.T) {
	from := Run{State: Running, Prompts: make([]string, 1, 4)}
	from.Prompts[0] = "p2"
	a, _, _ := Apply(from, FollowUp{Prompt: "a"})
	b, _, _ := Apply(from, FollowUp{Prompt: "b"})
	if a.Prompts[1] != "a" || b.Prompts[1] != "b" {
		t.Fatalf("follow-ups share storage: a=%v b=%v", a.Prompts, b.Prompts)
	}
}

func TestPermissionsNonEmptyExactlyWhenAwaiting(t *testing.T) {
	r, _ := Launch("p1")
	events := []Event{
		Started{},
		PermissionRequested{Permission: Permission{ID: "1"}},
		PermissionRequested{Permission: Permission{ID: "2"}},
		FollowUp{Prompt: "p2"},
		PermissionAnswered{Approve: true},
		PermissionAnswered{Approve: false},
		TurnEnded{},
		PermissionRequested{Permission: Permission{ID: "3"}},
		TurnEnded{},
		FollowUp{Prompt: "p3"},
		Started{},
		Stop{},
		WorktreeRemoved{},
	}
	for i, e := range events {
		var err error
		r, _, err = Apply(r, e)
		if err != nil {
			t.Fatalf("event %d (%T): %v", i, e, err)
		}
		if (r.State == AwaitingPermission) != (len(r.Permissions) > 0) {
			t.Fatalf("event %d (%T): state %s with %d permissions", i, e, r.State, len(r.Permissions))
		}
	}
	if r.State != Stopped {
		t.Fatalf("final state = %s, want stopped", r.State)
	}
}

	t.Run("cancel after the run finished is ignored", func(t *testing.T) {
		from := Run{State: Finished}
		got, effects, err := Apply(from, PermissionCancelled{ID: old.ID, Reason: CancelWithdrawn})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, from) || effects != nil {
			t.Fatalf("run = %+v effects = %#v", got, effects)
		}
	})
}

func TestErrorNonEmptyExactlyWhenFailed(t *testing.T) {
	r, _ := Launch("p1")
	events := []Event{
		Started{},
		PermissionRequested{Permission: Permission{ID: "1"}},
		PermissionAnswered{Approve: true},
		TurnEnded{},
		FollowUp{Prompt: "p2"},
		Started{},
		Stop{},
		WorktreeRemoved{},
	}
	for i, e := range events {
		var err error
		r, _, err = Apply(r, e)
		if err != nil {
			t.Fatalf("event %d (%T): %v", i, e, err)
		}
		if failed := r.State == Failed; failed != (r.Error != "") {
			t.Fatalf("event %d (%T): state %s error %q", i, e, r.State, r.Error)
		}
	}
	if r.State != Stopped || r.Error != "" {
		t.Fatalf("after stop = %+v, want stopped with an empty error", r)
	}
	late, _, err := Apply(r, BackendFailed{Err: "late"})
	if err != nil || late.State != Stopped || late.Error != "" {
		t.Fatalf("failure after stop = %+v, %v", late, err)
	}

	for _, from := range []Run{
		{State: Queued, Prompts: []string{"p"}},
		{State: Running},
		{State: AwaitingPermission, Permissions: []Permission{{ID: "1"}}},
	} {
		for _, cause := range []string{"", " \n\t"} {
			got, _, err := Apply(from, BackendFailed{Err: cause})
			if err != nil || got.State != Failed || got.Error != "failed without an error message" {
				t.Fatalf("BackendFailed %q from %s = %+v, %v", cause, from.State, got, err)
			}
		}
	}
	got, _, err := Apply(Run{State: Stopping}, WorktreeRemoveFailed{})
	if err != nil || got.State != Failed || got.Error != "failed without an error message" {
		t.Fatalf("empty removal failure = %+v, %v", got, err)
	}
	got, _, err = Apply(Run{State: Running}, BackendFailed{Err: "boom"})
	if err != nil || got.Error != "boom" {
		t.Fatalf("message = %+v, %v", got, err)
	}
	got, _, err = Apply(Run{State: Running}, Stop{})
	if err != nil || got.State != Stopping || got.Error != "" {
		t.Fatalf("stop = %+v, %v", got, err)
	}
}

func TestParseState(t *testing.T) {
	for _, s := range []string{"queued", "running", "awaiting_permission", "finished", "failed", "stopping", "stopped"} {
		if got, err := ParseState(s); err != nil || string(got) != s {
			t.Errorf("ParseState(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := ParseState("done"); err == nil {
		t.Error("ParseState accepted an unknown state")
	}
}

func TestPermissionGranularity(t *testing.T) {
	optsAll := json.RawMessage(`[
		{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},
		{"optionId":"allow-always","name":"Always allow","kind":"allow_always"},
		{"optionId":"reject-once","name":"Reject","kind":"reject_once"}
	]`)
	optsOnceOnly := json.RawMessage(`[
		{"optionId":"allow-once","name":"Allow once","kind":"allow_once"},
		{"optionId":"reject-once","name":"Reject","kind":"reject_once"}
	]`)
	perm1 := Permission{ID: "perm-1", Title: "Run test", Options: optsAll}
	perm2 := Permission{ID: "perm-2", Title: "Fetch URL", Options: optsOnceOnly}
	both := Run{State: AwaitingPermission, Permissions: []Permission{perm1, perm2}}

	t.Run("target specific permission id out of order", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, PermissionID: "perm-2"})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: AwaitingPermission, Permissions: []Permission{perm1}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{AnswerPermission{ID: "perm-2", Approve: true, Option: "allow_once", OptionID: "allow-once"}}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})

	t.Run("select allow-always option", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, PermissionID: "perm-1", Option: "allow-always"})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: AwaitingPermission, Permissions: []Permission{perm2}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{AnswerPermission{ID: "perm-1", Approve: true, Option: "allow_always", OptionID: "allow-always"}}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})

	t.Run("select reject option via approve --option reject", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, PermissionID: "perm-1", Option: "reject"})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: AwaitingPermission, Permissions: []Permission{perm2}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{AnswerPermission{ID: "perm-1", Approve: false, Option: "reject_once", OptionID: "reject-once"}}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})

	t.Run("approve all pending permissions", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, All: true})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: Running}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{
			AnswerPermission{ID: "perm-1", Approve: true, Option: "allow_once", OptionID: "allow-once"},
			AnswerPermission{ID: "perm-2", Approve: true, Option: "allow_once", OptionID: "allow-once"},
		}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})

	t.Run("deny all pending permissions", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: false, All: true})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: Running}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{
			AnswerPermission{ID: "perm-1", Approve: false, Option: "reject_once", OptionID: "reject-once"},
			AnswerPermission{ID: "perm-2", Approve: false, Option: "reject_once", OptionID: "reject-once"},
		}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})

	t.Run("rejects unknown permission id", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, PermissionID: "perm-99"})
		if !errors.Is(err, ErrNotAllowed) || !reflect.DeepEqual(got, both) || effects != nil {
			t.Fatalf("got = %+v, effects = %#v, err = %v", got, effects, err)
		}
	})

	t.Run("rejects option not offered on targeted permission", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, PermissionID: "perm-2", Option: "allow-always"})
		if !errors.Is(err, ErrNotAllowed) || !reflect.DeepEqual(got, both) || effects != nil {
			t.Fatalf("got = %+v, effects = %#v, err = %v", got, effects, err)
		}
	})

	t.Run("all-or-nothing when one permission lacks the requested option", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: true, All: true, Option: "allow-always"})
		if !errors.Is(err, ErrNotAllowed) || !reflect.DeepEqual(got, both) || effects != nil {
			t.Fatalf("got = %+v, effects = %#v, err = %v", got, effects, err)
		}
	})

	t.Run("rejects deny with allow option", func(t *testing.T) {
		got, effects, err := Apply(both, PermissionAnswered{Approve: false, Option: "allow-once"})
		if !errors.Is(err, ErrNotAllowed) || !reflect.DeepEqual(got, both) || effects != nil {
			t.Fatalf("got = %+v, effects = %#v, err = %v", got, effects, err)
		}
	})

	t.Run("default approve without allow_once unblocks without selecting allow_always", func(t *testing.T) {
		alwaysOnly := Permission{
			ID:      "perm-always",
			Title:   "Run tool",
			Options: json.RawMessage(`[{"optionId":"allow-always","name":"Always allow","kind":"allow_always"},{"optionId":"reject-once","name":"Reject","kind":"reject_once"}]`),
		}
		from := Run{State: AwaitingPermission, Permissions: []Permission{alwaysOnly}}
		got, effects, err := Apply(from, PermissionAnswered{Approve: true})
		if err != nil {
			t.Fatal(err)
		}
		want := Run{State: Running}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("run = %+v, want %+v", got, want)
		}
		wantEffects := []Effect{AnswerPermission{ID: "perm-always", Approve: true}}
		if !reflect.DeepEqual(effects, wantEffects) {
			t.Fatalf("effects = %#v, want %#v", effects, wantEffects)
		}
	})
}
