// Package backend is the contract between the daemon and the code that drives
// a Cursor CLI agent process.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
)

// Kind names a way of driving the Cursor CLI.
type Kind string

const (
	// ACP drives "agent acp" over the Agent Client Protocol on stdio.
	ACP Kind = "acp"
	// Print runs "agent -p --output-format stream-json" once per prompt and
	// continues the chat with --resume. It cannot ask for permissions.
	Print Kind = "print"
)

func ParseKind(s string) (Kind, error) {
	switch k := Kind(s); k {
	case ACP, Print:
		return k, nil
	}
	return "", fmt.Errorf("unknown backend %q (want %q or %q)", s, ACP, Print)
}

type StartOptions struct {
	// Dir is the working directory of the agent, the run's worktree.
	Dir string
	// Model is passed to the CLI as --model when not empty.
	Model           string
	ResumeSessionID string
}

// Event is one item of agent output, stored in the run's event log as is.
type Event struct {
	// Type is the protocol's own discriminator, such as the ACP
	// "sessionUpdate" value or the stream-json "type" value.
	Type string
	Data json.RawMessage
	// Text is the agent's reply text carried by this event, if any. The
	// daemon joins it into the run summary.
	Text string
}

// PermissionRequest describes a tool call the agent wants approved.
type PermissionRequest struct {
	Title    string
	ToolCall json.RawMessage
}

// Sink receives what a session produces. Implementations are safe for
// concurrent use.
type Sink interface {
	Event(Event)
	// Permission blocks until the request is answered or ctx ends.
	Permission(ctx context.Context, req PermissionRequest) (approve bool, err error)
}
