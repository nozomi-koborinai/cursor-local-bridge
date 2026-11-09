// Package backend is the contract between the daemon and the code that drives
// a Cursor CLI agent process.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

type Command string

const (
	CommandACP        Command = "acp"         // agent acp
	CommandCreateChat Command = "create-chat" // agent create-chat
	CommandPrint      Command = "print"       // agent -p
)

type AgentProcess struct {
	PID     int
	Command Command
	// Version is what the CLI printed for --version just before the process
	// started. It is empty exactly when VersionErr is set.
	Version    string
	VersionErr string
}

// Stderr is one line an agent process wrote to stderr. See proc.StderrLine
// for pieces and Capped.
type Stderr struct {
	PID    int
	Line   string
	Capped bool
}

type StartOptions struct {
	// Dir is the working directory of the agent, the run's worktree.
	Dir string
	// Model is passed to the CLI as --model when not empty.
	Model           string
	ResumeSessionID string
	// ProcFile, when not empty, records the running agent process so a new
	// daemon can stop it if this daemon crashes.
	ProcFile string
}

// EventTransportError is the Event.Type for a transport error (such as
// RetriableError on a closed HTTP/2 stream) that arrived inside an
// agent_message_chunk. Its Text is always empty so it never enters the run
// summary.
const EventTransportError = "transport_error"

// IsTransportError reports whether an agent message chunk text is a transport
// error diagnostic rather than assistant reply text.
func IsTransportError(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "RetriableError") {
		return true
	}
	if !strings.Contains(text, "RetriableError") {
		return false
	}
	for _, marker := range []string{"stream closed", "http/2", "http2", "CANCEL"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Event is one item of agent output, stored in the run's event log as is.
type Event struct {
	// Type is the protocol's own discriminator, such as the ACP
	// "sessionUpdate" value or the stream-json "type" value.
	Type string
	Data json.RawMessage
	// Text is the agent's reply text carried by this event, if any. The
	// daemon uses it to build the run summary.
	Text string
}

const (
	OptionAllowOnce    = "allow_once"
	OptionAllowAlways  = "allow_always"
	OptionRejectOnce   = "reject_once"
	OptionRejectAlways = "reject_always"
)

// PermissionRequest describes a tool call the agent wants approved.
type PermissionRequest struct {
	Title      string
	ToolCallID string
	Kind       string
	Command    string
	URL        string
	ToolCall   json.RawMessage
	// Options is the agent's permission choices as a JSON array of optionId,
	// name, and kind.
	Options json.RawMessage
}

type PermissionDecision struct {
	Approved bool
	Option   string
	OptionID string
}

// Sink receives what a session produces. Implementations are safe for
// concurrent use.
type Sink interface {
	Event(Event)
	// Permission blocks until the request is answered or ctx ends.
	Permission(ctx context.Context, req PermissionRequest) (approve bool, err error)
	// AgentProcess reports a process that StartAgent started. It comes
	// before any Stderr of that process.
	AgentProcess(AgentProcess)
	// Stderr reports stderr of a process that StartAgent started, in order.
	// It must not block on anything that waits for that process to exit.
	Stderr(Stderr)
}

// ModelReporter is an optional Sink extension that records the model and
// effort resolved for the session.
type ModelReporter interface {
	SetModel(model string)
}

// ProcRecorder is an optional Sink extension that names the directory where
// StartAgent records running agent processes for crash recovery.
type ProcRecorder interface {
	ProcDir() string
}

// DecisionSink is an optional extension of Sink that returns the full
// PermissionDecision, including the chosen option kind or optionId.
type DecisionSink interface {
	Sink
	PermissionDecision(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

// AskPermission calls s.PermissionDecision when s implements DecisionSink, or
// falls back to s.Permission with allow_once or reject_once.
func AskPermission(ctx context.Context, s Sink, req PermissionRequest) (PermissionDecision, error) {
	if ds, ok := s.(DecisionSink); ok {
		return ds.PermissionDecision(ctx, req)
	}
	approve, err := s.Permission(ctx, req)
	if err != nil {
		return PermissionDecision{}, err
	}
	if approve {
		return PermissionDecision{Approved: true, Option: OptionAllowOnce}, nil
	}
	return PermissionDecision{Approved: false, Option: OptionRejectOnce}, nil
}
