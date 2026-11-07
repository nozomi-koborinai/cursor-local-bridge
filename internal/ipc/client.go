package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
)

// Call sends req to the daemon listening on socketPath and passes each
// response to fn until the daemon closes the connection. An error response
// ends the call with that *Error. Canceling ctx closes the connection.
func Call(ctx context.Context, socketPath string, req Request, fn func(Response) error) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("daemon is not running at %s; start it with clb daemon", socketPath)
		}
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return callError(ctx, err)
	}
	dec := json.NewDecoder(conn)
	for replies := 0; ; replies++ {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			if errors.Is(err, io.EOF) && (replies > 0 || req.Method == MethodLogs) {
				return nil
			}
			return callError(ctx, err)
		}
		if resp.Error != nil {
			return resp.Error
		}
		if err := fn(resp); err != nil {
			return err
		}
	}
}

func callError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, io.EOF) {
		return errors.New("daemon closed the connection without replying")
	}
	return fmt.Errorf("talk to daemon: %w", err)
}
