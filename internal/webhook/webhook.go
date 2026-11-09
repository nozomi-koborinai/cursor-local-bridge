// Package webhook delivers signed run notifications over HTTP.
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
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
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

	userAgent       = "clb-webhook/1"
	clbHeaderPrefix = "X-Clb-"
)

const (
	EventStatusChange        = "statusChange"
	EventPermissionRequested = "permissionRequested"
	EventPermissionReminder  = "permissionReminder"
)

// reservedHeaders are canonical names CheckHeaders rejects.
// Content-Type and User-Agent are written on every delivery.
// Host, Content-Length, and Transfer-Encoding are dropped or rewritten
// by net/http, so a configured value would never be sent.
var reservedHeaders = map[string]string{
	"Content-Type":      "is set by clb and cannot be overridden",
	"User-Agent":        "is set by clb and cannot be overridden",
	"Host":              "is dropped or rewritten by the HTTP client and cannot be set",
	"Content-Length":    "is dropped or rewritten by the HTTP client and cannot be set",
	"Transfer-Encoding": "is dropped or rewritten by the HTTP client and cannot be set",
}

// CheckHeaders rejects headers that clb will not send. A nil map is valid.
// The error names the header and omits the value.
func CheckHeaders(headers map[string]string) error {
	seen := make(map[string]string, len(headers))
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		if !validHeaderToken(name) {
			return fmt.Errorf("header name %q is not a valid HTTP token", name)
		}
		canon := textproto.CanonicalMIMEHeaderKey(name)
		if reason, ok := reservedHeaders[canon]; ok {
			return fmt.Errorf("header %q %s", name, reason)
		}
		if strings.HasPrefix(canon, clbHeaderPrefix) {
			return fmt.Errorf("header %q is in clb's namespace and cannot be set", name)
		}
		if ValueHasControl(headers[name]) {
			return fmt.Errorf("header %q value has a control character such as CR or LF", name)
		}
		if prev, ok := seen[canon]; ok {
			return fmt.Errorf("headers %q and %q name the same header", prev, name)
		}
		seen[canon] = name
	}
	return nil
}

func validHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isTChar(name[i]) {
			return false
		}
	}
	return true
}

func isTChar(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z':
		return true
	default:
		return strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
	}
}

// ValueHasControl reports whether v contains a byte that cannot be sent in an
// HTTP header value. Tab is allowed. CR, LF, and NUL are not.
func ValueHasControl(v string) bool {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if (b < 0x20 && b != '\t') || b == 0x7f {
			return true
		}
	}
	return false
}

// Permission is the tool call a permissionRequested or permissionReminder
// payload describes.
type Permission struct {
	ID      string          `json:"id"`
	Title   string          `json:"title,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	Command string          `json:"command,omitempty"`
	URL     string          `json:"url,omitempty"`
	Options json.RawMessage `json:"options,omitempty"`
}

// Payload is the JSON body of a webhook request.
type Payload struct {
	Event        string      `json:"event"`
	Timestamp    time.Time   `json:"timestamp"`
	RunID        string      `json:"id"`
	Status       string      `json:"status"`
	Repo         string      `json:"repo"`
	Branch       string      `json:"branch"`
	AgentVersion string      `json:"agentVersion,omitempty"`
	Summary      string      `json:"summary,omitempty"`
	Error        string      `json:"error,omitempty"`
	Permission   *Permission `json:"permission,omitempty"`
	Reminder     int         `json:"reminder,omitempty"`
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

type Sender struct {
	URL    string
	Secret []byte
	// Header holds extra headers sent on every delivery. The names are
	// already canonical.
	Header http.Header
	Client *http.Client
	// Backoff is the wait before the first retry. It doubles per retry.
	Backoff time.Duration
	// Attempts is the total number of tries, at least 1.
	Attempts int
	// Reminder is how long a pending permission waits before each
	// permissionReminder. Zero sends no reminders.
	Reminder time.Duration
}

// Send posts p and retries on network errors and 5xx responses until
// Attempts runs out or ctx ends. A 2xx response is success. Other responses
// fail without retry. The int is the status code of the last response
// received, or 0 when none arrived.
func (s *Sender) Send(ctx context.Context, p Payload) (int, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("webhook payload: %w", err)
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, fmt.Errorf("webhook delivery id: %w", err)
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
	var status int
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			if err := wait(ctx, backoff); err != nil {
				return status, err
			}
			if backoff > 0 {
				backoff *= 2
			}
		}
		code, err := post(ctx, client, s.URL, body, s.Secret, sig, p.Event, delivery, s.Header)
		if code != 0 {
			status = code
		}
		if err == nil {
			return status, nil
		}
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		if !retryable(err) {
			return status, err
		}
		last = err
	}
	return status, last
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

func post(ctx context.Context, client *http.Client, endpoint string, body, secret []byte, sig, event, delivery string, extra http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("webhook POST: %w", withoutURL(err))
	}
	maps.Copy(req.Header, extra)
	if len(secret) > 0 && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+string(secret))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(EventHeader, event)
	req.Header.Set(DeliveryHeader, delivery)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("webhook POST: %w", withoutURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, fmt.Errorf("webhook POST: %w", err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, &statusError{code: resp.StatusCode}
}

// withoutURL drops the request URL that net/http puts in its errors. A URL
// can carry a token in its path or query, and delivery errors reach the
// daemon log, the run's events, and command output.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
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
