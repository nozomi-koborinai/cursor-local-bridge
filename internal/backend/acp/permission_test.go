package acp

import (
	"context"
	"encoding/json"
	"testing"

	acpsdk "github.com/coder/acp-go-sdk"
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
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	asked := sink.permissions()
	if len(asked) != 1 || asked[0].Title != title {
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
