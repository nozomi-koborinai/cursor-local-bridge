// Package stream drives the Cursor CLI in print mode, one
// "agent -p --output-format stream-json" process per prompt, continuing the
// chat with --resume.
package stream

import (
	"context"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

// Session is one chat driven through print mode.
type Session struct {
	agentPath string
	sink      backend.Sink
}

// New returns a session that runs agentPath and reports to sink.
func New(agentPath string, sink backend.Sink) *Session {
	return &Session{agentPath: agentPath, sink: sink}
}

// Start creates a chat with "agent create-chat", or adopts
// opts.ResumeSessionID, and returns the chat ID. It starts no long-lived
// process.
func (s *Session) Start(ctx context.Context, opts backend.StartOptions) (string, error) {
	panic("not implemented")
}

// Prompt runs one print-mode process for text, forwards each stream-json line
// as an event, and returns when the process exits.
func (s *Session) Prompt(ctx context.Context, text string) error {
	panic("not implemented")
}

// Close terminates a print-mode process in flight. It is safe to call more
// than once and concurrently with Prompt.
func (s *Session) Close() error {
	panic("not implemented")
}
