package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

func TestGlobalPolicyAutoApprovesAllowlistAndAsksOnMetasyntax(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		cmdApproved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "Run rg search",
			ToolCallID: "call-1",
			Kind:       "execute",
			Command:    "rg -n needle internal",
		})
		if err != nil {
			return err
		}
		fetchApproved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "Fetch docs",
			ToolCallID: "call-2",
			Kind:       "fetch",
			URL:        "https://docs.example.com/guide",
		})
		if err != nil {
			return err
		}
		chainApproved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "Run compound shell command",
			ToolCallID: "call-3",
			Kind:       "execute",
			Command:    "cd /path/to/repo && rg needle internal",
		})
		if err != nil {
			return err
		}
		unlistedApproved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "Unlisted command",
			ToolCallID: "call-4",
			Kind:       "execute",
			Command:    "curl https://unlisted.example.org",
		})
		if err != nil {
			return err
		}
		var parts []string
		for _, b := range []bool{cmdApproved, fetchApproved, chainApproved, unlistedApproved} {
			if b {
				parts = append(parts, "allow")
			} else {
				parts = append(parts, "deny")
			}
		}
		s.sink.Event(chunk(strings.Join(parts, ",")))
		return nil
	}})
	writePolicyFile(t, filepath.Join(h.cfg.Home, "policy.json"), `{
		"allow": {
			"commands": ["rg *"],
			"fetch": ["docs.example.com"]
		}
	}`)

	run := h.launch("audit")

	// perm-1 and perm-2 are auto-approved; perm-3 (compound command with &&) asks the user.
	waiting := waitPending(t, h, run.ID, "perm-3")
	if waiting.Command != "cd /path/to/repo && rg needle internal" {
		t.Fatalf("pending = %+v, want perm-3", waiting)
	}
	hook := h.hooks.next(t)
	if hook.Event != webhook.EventPermissionRequested || hook.Permission == nil || hook.Permission.ID != "perm-3" {
		t.Fatalf("webhook = %+v, want permissionRequested for perm-3", hook)
	}
	h.must(ipc.Request{Method: ipc.MethodDeny, RunID: run.ID})

	// perm-4 (unlisted command) also asks the user.
	waiting = waitPending(t, h, run.ID, "perm-4")
	if waiting.Command != "curl https://unlisted.example.org" {
		t.Fatalf("pending = %+v, want perm-4", waiting)
	}
	hook = h.hooks.next(t)
	if hook.Event != webhook.EventPermissionRequested || hook.Permission == nil || hook.Permission.ID != "perm-4" {
		t.Fatalf("webhook = %+v, want permissionRequested for perm-4", hook)
	}
	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: run.ID})

	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "allow,allow,deny,allow" {
		t.Fatalf("summary = %q, want allow,allow,deny,allow", done.Summary)
	}

	events := h.logs(run.ID)
	var answers []string
	for _, e := range events {
		if e.Type == "permission_answered" {
			answers = append(answers, string(e.Data))
		}
	}
	wantAnswers := []string{
		`{"id":"perm-1","approved":true,"auto":true,"rule":"allow.commands:rg *"}`,
		`{"id":"perm-2","approved":true,"auto":true,"rule":"allow.fetch:docs.example.com"}`,
		`{"id":"perm-3","approved":false}`,
		`{"id":"perm-4","approved":true}`,
	}
	if !slices.Equal(answers, wantAnswers) {
		t.Fatalf("permission_answered = %v, want %v", answers, wantAnswers)
	}
	if countType(events, "permission_requested") != 4 {
		t.Fatalf("permission_requested count = %d, want 4", countType(events, "permission_requested"))
	}
}

func TestRepoAndPerRunPolicyPrecedenceAndRestart(t *testing.T) {
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, prompt string) error {
		cmd := "git status"
		if strings.HasPrefix(prompt, "test:") {
			cmd = "go test ./..."
		}
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title:      "Run command",
			ToolCallID: "call-1",
			Kind:       "execute",
			Command:    cmd,
		})
		if err != nil {
			return err
		}
		if approved {
			s.sink.Event(chunk(prompt + ":approved"))
		} else {
			s.sink.Event(chunk(prompt + ":denied"))
		}
		return nil
	}})

	// User-level policy allows "git status", while repo-level .clb/policy.json allows "go test ./...".
	homePolicy := filepath.Join(h.cfg.Home, "policy.json")
	writePolicyFile(t, homePolicy, `{"allow":{"commands":["git status"]}}`)
	repoPolicyDir := filepath.Join(h.repo, ".clb")
	if err := os.MkdirAll(repoPolicyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writePolicyFile(t, filepath.Join(repoPolicyDir, "policy.json"), `{"allow":{"commands":["go test ./..."]}}`)

	first := h.launch("test:merged")
	if done := h.waitStatus(first.ID, "finished"); done.Summary != "test:merged:approved" {
		t.Fatalf("first summary = %q", done.Summary)
	}
	if got := dataOf(h.logs(first.ID), "permission_answered"); got != `{"id":"perm-1","approved":true,"auto":true,"rule":"allow.commands:go test ./..."}` {
		t.Fatalf("first permission_answered = %s", got)
	}

	// Per-run --policy overrides both user-level and repo-level policies:
	// an allowlist with only "pwd" does not auto-approve "git status".
	runPolicy := filepath.Join(t.TempDir(), "run-policy.json")
	writePolicyFile(t, runPolicy, `{"allow":{"commands":["pwd"]}}`)
	second := h.must(ipc.Request{
		Method: ipc.MethodLaunch,
		Repo:   h.repo,
		Prompt: "per-run",
		Policy: runPolicy,
	})
	if second.Policy != runPolicy {
		t.Fatalf("run policy = %q, want %q", second.Policy, runPolicy)
	}
	waiting := h.waitStatus(second.ID, "awaiting_permission")
	if waiting.PendingPermission == nil || waiting.PendingPermission.Command != "git status" {
		t.Fatalf("pending = %+v, want git status awaiting permission", waiting.PendingPermission)
	}
	h.must(ipc.Request{Method: ipc.MethodDeny, RunID: second.ID})
	if done := h.waitStatus(second.ID, "finished"); done.Summary != "per-run:denied" {
		t.Fatalf("second summary = %q", done.Summary)
	}

	// Restart the daemon and send a follow-up on the second run; its per-run
	// policy must still override homePolicy and repoPolicy.
	h.stop()
	d := h.newDaemon()
	if err := d.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.serve(d)

	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: second.ID, Prompt: "after-restart"})
	waiting = h.waitStatus(second.ID, "awaiting_permission")
	if waiting.PendingPermission == nil || waiting.PendingPermission.Command != "git status" {
		t.Fatalf("pending after restart = %+v, want git status awaiting permission", waiting.PendingPermission)
	}
	h.must(ipc.Request{Method: ipc.MethodApprove, RunID: second.ID})
	if done := h.waitStatus(second.ID, "finished"); done.Summary != "after-restart:approved" {
		t.Fatalf("after restart summary = %q", done.Summary)
	}

	// Modify <repo>/.clb/policy.json after first was launched; a post-restart
	// follow-up on first must keep using the repo policy snapshot captured at
	// launch ("go test ./...") and not pick up newly added rules from the
	// working tree.
	writePolicyFile(t, filepath.Join(repoPolicyDir, "policy.json"), `{"allow":{"commands":["rm -rf /tmp/build"]}}`)
	h.must(ipc.Request{Method: ipc.MethodFollowUp, RunID: first.ID, Prompt: "test:after-repo-edit"})
	if done := h.waitStatus(first.ID, "finished"); done.Summary != "test:after-repo-edit:approved" {
		t.Fatalf("first follow-up summary = %q, want test:after-repo-edit:approved", done.Summary)
	}
}

func TestAutoAnsweredPermissionSupersedesStalePendingPermission(t *testing.T) {
	sendSecond := make(chan struct{})
	firstErr := make(chan error, 1)
	h := newHarness(t, &fakeAgent{onPrompt: func(ctx context.Context, s *fakeSession, _ string) error {
		go func() {
			_, err := s.sink.Permission(ctx, backend.PermissionRequest{
				Title: "first", ToolCallID: "call-1", Kind: "execute", Command: "curl https://example.com",
			})
			firstErr <- err
		}()
		<-sendSecond
		approved, err := s.sink.Permission(ctx, backend.PermissionRequest{
			Title: "second", ToolCallID: "call-1", Kind: "execute", Command: "echo ok",
		})
		if err != nil {
			return err
		}
		if approved {
			s.sink.Event(chunk("auto-approved after supersede"))
		}
		return nil
	}})
	writePolicyFile(t, filepath.Join(h.cfg.Home, "policy.json"), `{"allow":{"commands":["echo *"]}}`)

	run := h.launch("supersede")
	h.waitStatus(run.ID, "awaiting_permission")
	close(sendSecond)
	done := h.waitStatus(run.ID, "finished")
	if done.Summary != "auto-approved after supersede" || done.PendingPermission != nil {
		t.Fatalf("done = %+v", done)
	}
	if err := <-firstErr; err == nil {
		t.Fatal("superseded permission returned no error")
	}
	events := h.logs(run.ID)
	if got := dataOf(events, "permission_cancelled"); got != `{"id":"perm-1","reason":"superseded","supersededBy":"perm-2"}` {
		t.Fatalf("permission_cancelled = %s", got)
	}
	if got := dataOf(events, "permission_answered"); got != `{"id":"perm-2","approved":true,"auto":true,"rule":"allow.commands:echo *"}` {
		t.Fatalf("permission_answered = %s", got)
	}
}

func TestLaunchRejectsInvalidPolicyPath(t *testing.T) {
	h := newHarness(t, &fakeAgent{})
	_, err := h.call(ipc.Request{
		Method: ipc.MethodLaunch,
		Repo:   h.repo,
		Prompt: "hi",
		Policy: "relative/policy.json",
	})
	if code := codeOf(err); code != ipc.CodeInvalid {
		t.Fatalf("relative policy err = %v, want invalid", err)
	}

	badPolicy := filepath.Join(t.TempDir(), "bad.json")
	writePolicyFile(t, badPolicy, `{"allow":{"commands":[""]}}`)
	_, err = h.call(ipc.Request{
		Method: ipc.MethodLaunch,
		Repo:   h.repo,
		Prompt: "hi",
		Policy: badPolicy,
	})
	if code := codeOf(err); code != ipc.CodeInvalid {
		t.Fatalf("bad policy err = %v, want invalid", err)
	}
}

func writePolicyFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
