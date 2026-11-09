package acp

import (
	"context"
	"encoding/json"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

func TestRequestPermissionForwardsOptions(t *testing.T) {
	options := []acpsdk.PermissionOption{
		{OptionId: "allow-once", Name: "Allow once", Kind: acpsdk.PermissionOptionKindAllowOnce},
		{OptionId: "allow-always", Name: "Always allow", Kind: acpsdk.PermissionOptionKindAllowAlways},
		{OptionId: "reject-once", Name: "Reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
	}
	sink := &recorder{answer: func() (bool, error) { return true, nil }}
	s := New(fakeAgent, sink)
	t.Cleanup(func() { _ = s.Close() })

	title := "`echo`"
	_, err := client{s}.RequestPermission(context.Background(), acpsdk.RequestPermissionRequest{
		Options: options,
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: "call-1",
			Title:      &title,
			Kind:       acpsdk.Ptr(acpsdk.ToolKindExecute),
			RawInput:   map[string]any{"command": "npm test"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := sink.permissions()
	if len(asked) != 1 || asked[0].Title != title || asked[0].ToolCallID != "call-1" ||
		asked[0].Kind != "execute" || asked[0].Command != "npm test" || asked[0].URL != "" {
		t.Fatalf("asked = %+v", asked)
	}
	want, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	if string(asked[0].Options) != string(want) {
		t.Fatalf("options = %s, want %s", asked[0].Options, want)
	}
}

func TestRequestPermissionReadsURL(t *testing.T) {
	sink := &recorder{answer: func() (bool, error) { return false, nil }}
	s := New(fakeAgent, sink)
	t.Cleanup(func() { _ = s.Close() })

	_, err := client{s}.RequestPermission(context.Background(), acpsdk.RequestPermissionRequest{
		Options: []acpsdk.PermissionOption{
			{OptionId: "reject-once", Name: "Reject", Kind: acpsdk.PermissionOptionKindRejectOnce},
		},
		ToolCall: acpsdk.ToolCallUpdate{
			ToolCallId: "call-2",
			Kind:       acpsdk.Ptr(acpsdk.ToolKindFetch),
			RawInput:   json.RawMessage(`{"url":"https://example.com/hook"}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := sink.permissions()
	if len(asked) != 1 || asked[0].ToolCallID != "call-2" || asked[0].Kind != "fetch" ||
		asked[0].URL != "https://example.com/hook" || asked[0].Command != "" {
		t.Fatalf("asked = %+v", asked)
	}
}

type decisionRecorder struct {
	recorder
	decision func(backend.PermissionRequest) (backend.PermissionDecision, error)
}

func (r *decisionRecorder) PermissionDecision(ctx context.Context, req backend.PermissionRequest) (backend.PermissionDecision, error) {
	r.mu.Lock()
	r.asked = append(r.asked, req)
	fn := r.decision
	r.mu.Unlock()
	if fn == nil {
		<-ctx.Done()
		return backend.PermissionDecision{}, ctx.Err()
	}
	return fn(req)
}

func TestRequestPermissionSelectsExplicitDecisionOption(t *testing.T) {
	allowOnce := acpsdk.PermissionOption{OptionId: "allow-once", Kind: acpsdk.PermissionOptionKindAllowOnce}
	allowAlways := acpsdk.PermissionOption{OptionId: "allow-always", Kind: acpsdk.PermissionOptionKindAllowAlways}
	rejectOnce := acpsdk.PermissionOption{OptionId: "reject-once", Kind: acpsdk.PermissionOptionKindRejectOnce}
	rejectAlways := acpsdk.PermissionOption{OptionId: "reject-always", Kind: acpsdk.PermissionOptionKindRejectAlways}
	options := []acpsdk.PermissionOption{allowOnce, allowAlways, rejectOnce, rejectAlways}

	for _, tc := range []struct {
		name     string
		decision backend.PermissionDecision
		want     string
	}{
		{
			name:     "allow_always kind",
			decision: backend.PermissionDecision{Approved: true, Option: backend.OptionAllowAlways},
			want:     "allow-always",
		},
		{
			name:     "reject_once kind",
			decision: backend.PermissionDecision{Approved: false, Option: backend.OptionRejectOnce},
			want:     "reject-once",
		},
		{
			name:     "reject_always kind",
			decision: backend.PermissionDecision{Approved: false, Option: backend.OptionRejectAlways},
			want:     "reject-always",
		},
		{
			name:     "explicit optionId",
			decision: backend.PermissionDecision{Approved: true, OptionID: "allow-always"},
			want:     "allow-always",
		},
		{
			name:     "missing optionId cancels",
			decision: backend.PermissionDecision{Approved: true, OptionID: "unknown-option"},
			want:     "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &decisionRecorder{decision: func(backend.PermissionRequest) (backend.PermissionDecision, error) {
				return tc.decision, nil
			}}
			s := New(fakeAgent, sink)
			t.Cleanup(func() { _ = s.Close() })

			resp, err := client{s}.RequestPermission(context.Background(), acpsdk.RequestPermissionRequest{Options: options})
			if err != nil {
				t.Fatalf("RequestPermission: %v", err)
			}
			var got string
			if sel := resp.Outcome.Selected; sel != nil {
				got = string(sel.OptionId)
			}
			if got != tc.want {
				t.Fatalf("selected = %q, want %q", got, tc.want)
			}
			if tc.want == "" && resp.Outcome.Cancelled == nil {
				t.Fatalf("outcome = %+v, want cancelled", resp.Outcome)
			}
		})
	}
}
