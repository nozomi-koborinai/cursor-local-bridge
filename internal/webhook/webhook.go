// Package webhook delivers signed run status notifications over HTTP.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	userAgent = "clb-webhook/1"
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
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature is the SignatureHeader value for body. It
// compares in constant time.
func Verify(secret, body []byte, signature string) bool {
	return hmac.Equal([]byte(signature), []byte(Sign(secret, body)))
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
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("webhook payload: %w", err)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Errorf("webhook delivery id: %w", err)
	}
	delivery := hex.EncodeToString(buf[:])
	sig := Sign(s.Secret, body)

	attempts := s.Attempts
	if attempts < 1 {
		attempts = 1
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	backoff := s.Backoff
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := wait(ctx, backoff); err != nil {
				return err
			}
			if backoff > 0 {
				backoff *= 2
			}
		}
		err := post(ctx, client, s.URL, body, sig, p.Event, delivery)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryable(err) {
			return err
		}
		last = err
	}
	return last
}

func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func post(ctx context.Context, client *http.Client, endpoint string, body []byte, sig, event, delivery string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(EventHeader, event)
	req.Header.Set(DeliveryHeader, delivery)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("webhook POST: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &statusError{code: resp.StatusCode}
}

type statusError struct {
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("webhook POST: status %d", e.code)
}

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 && se.code < 600
	}
	return true
}
