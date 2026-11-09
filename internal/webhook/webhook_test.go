package webhook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignRFC4231Case2(t *testing.T) {
	got := Sign([]byte("Jefe"), []byte("what do ya want for nothing?"))
	const want = "sha256=5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	if got != want {
		t.Fatalf("sign = %s", got)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	key := []byte("Jefe")
	body := []byte("what do ya want for nothing?")
	sig := Sign(key, body)
	if !Verify(key, body, sig) {
		t.Fatal("valid signature rejected")
	}
	tampered := append(append([]byte{}, body...), 'x')
	if Verify(key, tampered, sig) {
		t.Fatal("tampered body accepted")
	}
	if Verify([]byte("Jeff"), body, sig) {
		t.Fatal("wrong secret accepted")
	}
	if Verify(key, body, strings.TrimPrefix(sig, "sha256=")) {
		t.Fatal("signature without prefix accepted")
	}
}

func TestSendPostsSignedPayload(t *testing.T) {
	secret := []byte("topsecret")
	payload := Payload{
		Event:     EventStatusChange,
		Timestamp: time.Date(2026, 11, 7, 8, 0, 0, 123, time.UTC),
		RunID:     "run-1",
		Status:    "finished",
		Repo:      "/repo",
		Branch:    "clb/run-1",
		Summary:   "done",
		Error:     "none",
	}
	var (
		mu   sync.Mutex
		got  []byte
		hdr  http.Header
		meth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		got = body
		hdr = r.Header.Clone()
		meth = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: secret, Backoff: time.Millisecond}
	status, err := s.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if meth != http.MethodPost {
		t.Fatalf("method = %s", meth)
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %s", hdr.Get("Content-Type"))
	}
	if hdr.Get("User-Agent") != "clb-webhook/1" {
		t.Fatalf("user agent = %s", hdr.Get("User-Agent"))
	}
	if hdr.Get(EventHeader) != payload.Event {
		t.Fatalf("event = %s", hdr.Get(EventHeader))
	}
	raw, err := hex.DecodeString(hdr.Get(DeliveryHeader))
	if err != nil || len(raw) != 16 {
		t.Fatalf("delivery = %q", hdr.Get(DeliveryHeader))
	}
	if hdr.Get(SignatureHeader) != Sign(secret, got) || !Verify(secret, got, hdr.Get(SignatureHeader)) {
		t.Fatalf("signature = %s", hdr.Get(SignatureHeader))
	}
	var decoded Payload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != payload.Event || decoded.RunID != payload.RunID || decoded.Status != payload.Status ||
		decoded.Repo != payload.Repo || decoded.Branch != payload.Branch ||
		decoded.Summary != payload.Summary || decoded.Error != payload.Error ||
		!decoded.Timestamp.Equal(payload.Timestamp) {
		t.Fatalf("payload = %+v", decoded)
	}
	if decoded.Permission != nil || decoded.Reminder != 0 {
		t.Fatalf("status payload included permission fields: %+v", decoded)
	}
}

func TestSendPermissionPayload(t *testing.T) {
	secret := []byte("topsecret")
	options := json.RawMessage(`[{"optionId":"allow-once","name":"Allow once","kind":"allow_once"}]`)
	payload := Payload{
		Event:     EventPermissionRequested,
		Timestamp: time.Date(2026, 11, 9, 9, 30, 4, 500000000, time.UTC),
		RunID:     "run-0123abcd",
		Status:    "awaiting_permission",
		Repo:      "/path/to/repo",
		Branch:    "clb/run-0123abcd",
		Permission: &Permission{
			ID:      "perm-1",
			Title:   "`npm test`",
			Kind:    "execute",
			Command: "npm test",
			Options: options,
		},
	}
	var mu sync.Mutex
	var got []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		got = body
		hdr = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: secret, Backoff: time.Millisecond, Reminder: 5 * time.Minute}
	if _, err := s.Send(t.Context(), payload); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body := append([]byte(nil), got...)
	header := hdr.Clone()
	mu.Unlock()
	if header.Get(EventHeader) != EventPermissionRequested {
		t.Fatalf("event header = %s", header.Get(EventHeader))
	}
	if !Verify(secret, body, header.Get(SignatureHeader)) {
		t.Fatalf("signature = %s", header.Get(SignatureHeader))
	}
	var decoded Payload
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != EventPermissionRequested || decoded.RunID != payload.RunID || decoded.Status != payload.Status ||
		decoded.Summary != "" || decoded.Error != "" || decoded.Reminder != 0 || decoded.Permission == nil {
		t.Fatalf("payload = %+v", decoded)
	}
	p := decoded.Permission
	if p.ID != "perm-1" || p.Title != "`npm test`" || p.Kind != "execute" || p.Command != "npm test" || p.URL != "" ||
		string(p.Options) != string(options) {
		t.Fatalf("permission = %+v", p)
	}

	payload.Event = EventPermissionReminder
	payload.Reminder = 2
	if _, err := s.Send(t.Context(), payload); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	body = append([]byte(nil), got...)
	header = hdr.Clone()
	mu.Unlock()
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Event != EventPermissionReminder || decoded.Reminder != 2 || header.Get(EventHeader) != EventPermissionReminder {
		t.Fatalf("reminder payload = %+v header %s", decoded, header.Get(EventHeader))
	}
}

func TestSendExtraHeaders(t *testing.T) {
	secret := []byte("topsecret")
	payload := Payload{Event: EventStatusChange, RunID: "run-1", Status: "finished"}
	var (
		mu    sync.Mutex
		got   []byte
		hdr   http.Header
		auths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			http.Error(w, "read", http.StatusInternalServerError)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		auths = append(auths, r.Header.Get("Authorization"))
		if len(auths) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		got = body
		hdr = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	extra := make(http.Header)
	extra.Set("Authorization", "Bearer k-123")
	extra.Set("X-Automation-Key", "k-123")
	extra.Set(SignatureHeader, "forged")
	extra.Set("Content-Type", "text/plain")
	s := Sender{URL: srv.URL, Secret: secret, Header: extra, Backoff: time.Millisecond, Attempts: 2}
	status, err := s.Send(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"Bearer k-123", "Bearer k-123"}; !slices.Equal(auths, want) {
		t.Fatalf("Authorization per attempt = %q, want %q", auths, want)
	}
	if hdr.Get("Authorization") != "Bearer k-123" {
		t.Fatalf("Authorization = %q", hdr.Get("Authorization"))
	}
	if hdr.Get("X-Automation-Key") != "k-123" {
		t.Fatalf("X-Automation-Key = %q", hdr.Get("X-Automation-Key"))
	}
	if hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", hdr.Get("Content-Type"))
	}
	if hdr.Get("User-Agent") != "clb-webhook/1" {
		t.Fatalf("user agent = %q", hdr.Get("User-Agent"))
	}
	if hdr.Get(EventHeader) != payload.Event {
		t.Fatalf("event = %q", hdr.Get(EventHeader))
	}
	raw, err := hex.DecodeString(hdr.Get(DeliveryHeader))
	if err != nil || len(raw) != 16 {
		t.Fatalf("delivery = %q", hdr.Get(DeliveryHeader))
	}
	if hdr.Get(SignatureHeader) == "forged" || !Verify(secret, got, hdr.Get(SignatureHeader)) {
		t.Fatalf("signature = %q", hdr.Get(SignatureHeader))
	}
}

func TestCheckHeaders(t *testing.T) {
	const value = "v4lue-s3cret"
	tests := []struct {
		name    string
		headers map[string]string
		want    []string
	}{
		{name: "nil"},
		{
			name: "valid",
			headers: map[string]string{
				"Authorization":      "Bearer " + value,
				"X-Automation-Key":   value,
				"X-Request-Id":       "id\t1",
				"!#$%&'*+-.^_`|~":    value,
				"Already-Canonical9": value,
			},
		},
		{name: "empty name", headers: map[string]string{"": value}, want: []string{`""`}},
		{name: "space in name", headers: map[string]string{"Bad Name": value}, want: []string{`"Bad Name"`}},
		{name: "crlf", headers: map[string]string{"Authorization": value + "\r\n"}, want: []string{`"Authorization"`}},
		{name: "nul", headers: map[string]string{"X-Request-Id": value + "\x00"}, want: []string{`"X-Request-Id"`}},
		{name: "other control", headers: map[string]string{"X-Request-Id": value + "\x01"}, want: []string{`"X-Request-Id"`}},
		{name: "del", headers: map[string]string{"X-Request-Id": value + "\x7f"}, want: []string{`"X-Request-Id"`}},
		{name: "content-type", headers: map[string]string{"Content-Type": value}, want: []string{`"Content-Type"`}},
		{name: "user-agent", headers: map[string]string{"User-Agent": value}, want: []string{`"User-Agent"`}},
		{name: "host", headers: map[string]string{"Host": value}, want: []string{`"Host"`}},
		{name: "content-length", headers: map[string]string{"Content-Length": value}, want: []string{`"Content-Length"`}},
		{name: "transfer-encoding", headers: map[string]string{"Transfer-Encoding": value}, want: []string{`"Transfer-Encoding"`}},
		{name: "lowercase content-type", headers: map[string]string{"content-type": value}, want: []string{`"content-type"`}},
		{name: "x-clb", headers: map[string]string{"X-Clb-Signature": value}, want: []string{`"X-Clb-Signature"`}},
		{name: "lowercase x-clb", headers: map[string]string{"x-clb-anything": value}, want: []string{`"x-clb-anything"`}},
		{
			name:    "duplicate",
			headers: map[string]string{"Authorization": value, "authorization": value + "-other"},
			want:    []string{`headers "Authorization" and "authorization" name the same header`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckHeaders(tt.headers)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			for _, sub := range tt.want {
				if !strings.Contains(msg, sub) {
					t.Fatalf("err = %v, want substring %s", err, sub)
				}
			}
			if strings.Contains(msg, value) || strings.Contains(msg, value+"-other") {
				t.Fatalf("err = %v includes the header value", err)
			}
		})
	}
}

func TestSendRetriesServerError(t *testing.T) {
	var mu sync.Mutex
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(DeliveryHeader))
		n := len(ids)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: []byte("k"), Backoff: time.Millisecond, Attempts: 4}
	status, err := s.Send(t.Context(), Payload{Event: EventStatusChange, RunID: "run-1", Status: "finished"})
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 {
		t.Fatalf("requests = %d", len(ids))
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("delivery ids = %q", ids)
	}
}

func TestSendDoesNotRetryClientError(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.Error(w, "no", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: []byte("k"), Backoff: time.Millisecond, Attempts: 4}
	status, err := s.Send(t.Context(), Payload{Event: EventStatusChange})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d", status)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("requests = %d", got)
	}
}

func TestSendReturnsLastStatusWhenRetriesRunOut(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: []byte("k"), Backoff: time.Millisecond, Attempts: 3}
	status, err := s.Send(t.Context(), Payload{Event: EventStatusChange})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d", status)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("requests = %d", got)
	}
}

func TestSendStatusIsZeroWhenNoResponseArrives(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	s := Sender{URL: url + "/hooks/path-token?key=query-token", Secret: []byte("k"), Backoff: time.Millisecond, Attempts: 2}
	status, err := s.Send(t.Context(), Payload{Event: EventStatusChange})
	if err == nil {
		t.Fatal("expected error")
	}
	if status != 0 {
		t.Fatalf("status = %d, err = %v", status, err)
	}
	if msg := err.Error(); strings.Contains(msg, "path-token") || strings.Contains(msg, "query-token") {
		t.Fatalf("error shows the URL: %v", err)
	}
}

func TestSendStopsWhenContextCanceled(t *testing.T) {
	var n atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	s := Sender{URL: srv.URL, Secret: []byte("k"), Backoff: 20 * time.Millisecond, Attempts: 5}
	if _, err := s.Send(ctx, Payload{Event: EventStatusChange}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("requests = %d", got)
	}
}
