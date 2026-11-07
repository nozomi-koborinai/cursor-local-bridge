// Package webhook delivers signed run status notifications over HTTP.
package webhook

import (
	"context"
	"net/http"
	"time"
)

const (
	// SignatureHeader carries "sha256=" and the hex HMAC-SHA256 of the body,
	// keyed with the configured secret.
	SignatureHeader = "X-Clb-Signature"
	// EventHeader carries the payload's Event value.
	EventHeader = "X-Clb-Event"
	// DeliveryHeader carries an ID that stays the same across retries of one
	// delivery, so receivers can drop duplicates.
	DeliveryHeader = "X-Clb-Delivery"
)

// EventStatusChange is the only event clb sends.
const EventStatusChange = "statusChange"

// Payload is the JSON body of a webhook request.
type Payload struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	RunID     string    `json:"id"`
	Status    string    `json:"status"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Summary   string    `json:"summary,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// Sign returns the SignatureHeader value for body.
func Sign(secret, body []byte) string {
	panic("not implemented")
}

// Verify reports whether signature is the SignatureHeader value for body. It
// compares in constant time.
func Verify(secret, body []byte, signature string) bool {
	panic("not implemented")
}

// Sender posts payloads to one URL.
type Sender struct {
	URL    string
	Secret []byte
	Client *http.Client
	// Backoff is the wait before the first retry. It doubles per retry.
	Backoff time.Duration
	// Attempts is the total number of tries, at least 1.
	Attempts int
}

// Send posts p and retries on network errors and 5xx responses until
// Attempts runs out or ctx ends. A 2xx response is success. Other responses
// fail without retry.
func (s *Sender) Send(ctx context.Context, p Payload) error {
	panic("not implemented")
}
