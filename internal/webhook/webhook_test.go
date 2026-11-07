package webhook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	if err := s.Send(t.Context(), payload); err != nil {
		t.Fatal(err)
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
	if err := s.Send(t.Context(), Payload{Event: EventStatusChange, RunID: "run-1", Status: "finished"}); err != nil {
		t.Fatal(err)
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
	err := s.Send(t.Context(), Payload{Event: EventStatusChange})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("err = %v", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("requests = %d", got)
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
	err := s.Send(ctx, Payload{Event: EventStatusChange})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("requests = %d", got)
	}
}
