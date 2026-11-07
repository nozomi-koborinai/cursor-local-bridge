// Package acp drives "agent acp", the Cursor CLI's Agent Client Protocol
// server, over stdio.
package acp

import (
	"context"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

// Session is one "agent acp" process bound to one ACP session.
type Session struct {
	agentPath string
	sink      backend.Sink
}

// New returns a session that runs agentPath and reports to sink.
func New(agentPath string, sink backend.Sink) *Session {
	return &Session{agentPath: agentPath, sink: sink}
}

// Start runs the agent, authenticates with the existing "agent login"
// credentials, and opens a new session or loads opts.ResumeSessionID. It
// returns the session ID.
func (s *Session) Start(ctx context.Context, opts backend.StartOptions) (string, error) {
	panic("not implemented")
}

// Prompt sends text and returns when the agent ends its turn.
func (s *Session) Prompt(ctx context.Context, text string) error {
	panic("not implemented")
}

// Close cancels a turn in flight and terminates the agent process. It is safe
// to call more than once and concurrently with Prompt.
func (s *Session) Close() error {
	panic("not implemented")
}
