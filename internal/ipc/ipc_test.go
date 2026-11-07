package ipc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallStreamsEveryResponse(t *testing.T) {
	path := serve(t, listen(t), func(_ context.Context, req Request, send func(Response) error) error {
		for i := range 3 {
			if err := send(Response{Event: &Event{RunID: req.RunID, Seq: int64(i + 1)}}); err != nil {
				return err
			}
		}
		return nil
	})

	var seqs []int64
	err := Call(context.Background(), path, Request{Method: MethodLogs, RunID: "run-1"}, func(r Response) error {
		seqs = append(seqs, r.Event.Seq)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 3 || seqs[0] != 1 || seqs[2] != 3 {
		t.Fatalf("seqs = %v, want [1 2 3]", seqs)
	}
}

func TestCallReturnsErrorResponses(t *testing.T) {
	path := serve(t, listen(t), func(_ context.Context, req Request, _ func(Response) error) error {
		if req.Method == MethodStatus {
			return &Error{Code: CodeNotFound, Message: "run x not found"}
		}
		return errors.New("disk on fire")
	})

	var ipcErr *Error
	err := Call(context.Background(), path, Request{Method: MethodStatus, RunID: "x"}, ignore)
	if !errors.As(err, &ipcErr) || ipcErr.Code != CodeNotFound {
		t.Fatalf("status err = %v, want code %s", err, CodeNotFound)
	}
	err = Call(context.Background(), path, Request{Method: MethodList}, ignore)
	if !errors.As(err, &ipcErr) || ipcErr.Code != CodeInternal || ipcErr.Message != "disk on fire" {
		t.Fatalf("list err = %v, want internal: disk on fire", err)
	}
}

func TestCallFailsWhenNoReplyArrives(t *testing.T) {
	path := serve(t, listen(t), noReply)

	if err := Call(context.Background(), path, Request{Method: MethodStatus}, ignore); err == nil {
		t.Fatal("status with no reply succeeded")
	}
	if err := Call(context.Background(), path, Request{Method: MethodLogs}, ignore); err != nil {
		t.Fatalf("logs with no events: %v", err)
	}
}

func TestCallWithoutDaemon(t *testing.T) {
	dir := socketDir(t)
	missing := filepath.Join(dir, "missing.sock")
	stale := filepath.Join(dir, "stale.sock")
	leaveStaleSocket(t, stale)

	for _, path := range []string{missing, stale} {
		err := Call(context.Background(), path, Request{Method: MethodList}, ignore)
		if err == nil || !strings.Contains(err.Error(), "daemon is not running at "+path) {
			t.Errorf("Call(%s) err = %v, want daemon is not running", filepath.Base(path), err)
		}
	}
}

func TestListen(t *testing.T) {
	t.Run("socket and its directory are private", func(t *testing.T) {
		path := serve(t, listen(t), noReply)
		assertMode(t, path, 0o600)
		assertMode(t, filepath.Dir(path), 0o700)
	})
	t.Run("refuses while another daemon answers", func(t *testing.T) {
		path := serve(t, listen(t), noReply)
		if ln, err := Listen(path); err == nil {
			_ = ln.Close()
			t.Fatal("second Listen succeeded")
		}
	})
	t.Run("replaces a stale socket", func(t *testing.T) {
		path := filepath.Join(socketDir(t), "clb.sock")
		leaveStaleSocket(t, path)
		ln, err := Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		_ = ln.Close()
	})
	t.Run("keeps a file that is not a socket", func(t *testing.T) {
		path := filepath.Join(socketDir(t), "clb.sock")
		if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ln, err := Listen(path); err == nil {
			_ = ln.Close()
			t.Fatal("Listen replaced a regular file")
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != "keep me" {
			t.Fatalf("file changed: %q, %v", b, err)
		}
	})
}

func TestPeerCheck(t *testing.T) {
	var calls atomic.Int32
	h := func(_ context.Context, _ Request, send func(Response) error) error {
		calls.Add(1)
		return send(Response{Runs: []Run{}})
	}

	same := serve(t, listen(t), h)
	if err := Call(context.Background(), same, Request{Method: MethodList}, ignore); err != nil {
		t.Fatalf("same user was refused: %v", err)
	}

	ln := listen(t)
	ln.(*peerListener).uid = os.Getuid() + 1
	other := serve(t, ln, h)
	if err := Call(context.Background(), other, Request{Method: MethodList}, ignore); err == nil {
		t.Fatal("a connection from another user got a reply")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

func TestClientHangupCancelsTheCall(t *testing.T) {
	ended := make(chan struct{})
	path := serve(t, listen(t), func(ctx context.Context, _ Request, send func(Response) error) error {
		if err := send(Response{Event: &Event{Seq: 1}}); err != nil {
			return err
		}
		<-ctx.Done()
		close(ended)
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	err := Call(ctx, path, Request{Method: MethodLogs, Follow: true}, func(Response) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("handler still running after the client hung up")
	}
}

// socketDir returns a short directory, because macOS limits unix socket
// paths to 104 bytes and t.TempDir is long there.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "clb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := Listen(filepath.Join(socketDir(t), "sub", "clb.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// serve runs Serve on ln until the test ends and returns the socket path.
func serve(t *testing.T, ln net.Listener, h Handler) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, h) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

func noReply(context.Context, Request, func(Response) error) error { return nil }

func ignore(Response) error { return nil }

func leaveStaleSocket(t *testing.T, path string) {
	t.Helper()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	_ = ln.Close()
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %o, want %o", path, got, want)
	}
}
