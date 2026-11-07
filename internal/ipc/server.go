package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// Listen listens on a unix socket at path that only the current user can
// use. It replaces a socket file left by a daemon that is gone, and fails if
// a daemon still answers on path.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := removeStale(path); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &peerListener{ln: ln, uid: os.Getuid()}, nil
}

func removeStale(path string) error {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket", path)
	}
	if c, err := net.Dial("unix", path); err == nil {
		_ = c.Close()
		return fmt.Errorf("another daemon is listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// peerListener closes connections from other users before Accept returns.
type peerListener struct {
	ln  *net.UnixListener
	uid int
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.ln.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if uid, err := peerUID(c); err == nil && uid == l.uid {
			return c, nil
		}
		_ = c.Close()
	}
}

func (l *peerListener) Close() error   { return l.ln.Close() }
func (l *peerListener) Addr() net.Addr { return l.ln.Addr() }

// Handler answers one request. It calls send once per response line. A
// returned error ends the call with an error response, CodeInternal unless
// the error is an *Error. ctx ends when the server stops or the client hangs
// up.
type Handler func(ctx context.Context, req Request, send func(Response) error) error

// Serve answers requests on ln with h until ctx ends. It closes ln and
// returns once every call in flight has returned.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			_ = ln.Close()
			return err
		}
		wg.Go(func() { serveConn(ctx, conn, h) })
	}
}

func serveConn(ctx context.Context, conn net.Conn, h Handler) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	enc := json.NewEncoder(conn)
	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = enc.Encode(Response{Error: &Error{Code: CodeInvalid, Message: "read request: " + err.Error()}})
		return
	}
	go func() {
		// The decoder can leave the request's trailing newline unread, so
		// only EOF or a read error means the client hung up.
		_, _ = io.Copy(io.Discard, conn)
		cancel()
	}()

	send := func(r Response) error { return enc.Encode(r) }
	if err := h(ctx, req, send); err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = &Error{Code: CodeInternal, Message: err.Error()}
		}
		_ = send(Response{Error: e})
	}
}
