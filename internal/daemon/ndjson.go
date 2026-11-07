package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
)

// The NDJSON log of a run is derived from its events in the store. The run's
// goroutine appends each event after storing it, and Recover rewrites the
// file from the store, so a crash between the two writes heals at restart.

func (d *Daemon) logPath(id store.RunID) string {
	return filepath.Join(d.cfg.LogDir(), string(id)+".ndjson")
}

func ndjsonLine(e store.Event) ([]byte, error) {
	line, err := json.Marshal(toIPCEvent(e))
	return append(line, '\n'), err
}

func (d *Daemon) appendLog(e store.Event) error {
	line, err := ndjsonLine(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d.cfg.LogDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(d.logPath(e.RunID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	return errors.Join(err, f.Close())
}

// writeLog replaces the run's NDJSON log with the events in the store.
func (d *Daemon) writeLog(ctx context.Context, id store.RunID) error {
	events, err := d.store.Events(ctx, id, 0)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range events {
		line, err := ndjsonLine(e)
		if err != nil {
			return err
		}
		buf.Write(line)
	}
	if err := os.MkdirAll(d.cfg.LogDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d.cfg.LogDir(), string(id)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), d.logPath(id))
}
