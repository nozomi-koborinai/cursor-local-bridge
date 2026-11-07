// Package daemon supervises runs. Each run is owned by one goroutine that
// applies lifecycle transitions, writes them to the store, and performs their
// effects. The daemon serves the ipc protocol on a unix socket.
package daemon

import (
	"context"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
)

// Session is one agent conversation, as provided by a backend package.
type Session interface {
	Start(ctx context.Context, opts backend.StartOptions) (sessionID string, err error)
	Prompt(ctx context.Context, text string) error
	Close() error
}

// Backends maps each backend kind to a constructor for its sessions.
type Backends map[backend.Kind]func(backend.Sink) Session
