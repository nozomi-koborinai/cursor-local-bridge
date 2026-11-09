// Package daemon supervises runs. Each run is owned by one goroutine that
// applies lifecycle transitions, writes them to the store, and performs their
// effects. The daemon serves the ipc protocol on a unix socket.
package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/worktree"
)

// Session is one agent conversation, as provided by a backend package.
type Session interface {
	Start(ctx context.Context, opts backend.StartOptions) (sessionID string, err error)
	Prompt(ctx context.Context, text string) error
	Close() error
}

type Backends map[backend.Kind]func(backend.Sink) Session

type Options struct {
	Store    *store.Store
	Backends Backends
	Config   config.Config
	// Webhook is called once per delivery, so a config change applies without
	// a restart. A nil func, or a nil sender with a nil error, sends nothing
	// and records no webhook event.
	Webhook func() (*webhook.Sender, error)
	// KeepAwake holds a sleep assertion until ctx ends when cfg.KeepAwake is
	// true and at least one run is active. A nil func disables keep-awake.
	KeepAwake func(ctx context.Context) error
	Logger    *slog.Logger
}

type Daemon struct {
	store     *store.Store
	backends  Backends
	cfg       config.Config
	webhook   func() (*webhook.Sender, error)
	keepAwake func(ctx context.Context) error
	log       *slog.Logger

	// ctx ends when Serve returns. Run goroutines, agent sessions, and
	// webhook deliveries live inside it.
	ctx    context.Context
	cancel context.CancelFunc
	// writes carries the run goroutines' store calls. The end of ctx does
	// not cancel it, so a transition in progress writes both the run's row
	// and its events.
	writes context.Context
	wg     sync.WaitGroup

	mu         sync.Mutex
	actors     map[store.RunID]*actor
	activeRuns map[store.RunID]bool
	awakeStop  context.CancelFunc
	awakeGen   uint64
}

func New(opts Options) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		store:      opts.Store,
		backends:   opts.Backends,
		cfg:        opts.Config,
		webhook:    opts.Webhook,
		keepAwake:  opts.KeepAwake,
		log:        cmp.Or(opts.Logger, slog.New(slog.DiscardHandler)),
		ctx:        ctx,
		cancel:     cancel,
		writes:     context.WithoutCancel(ctx),
		actors:     map[store.RunID]*actor{},
		activeRuns: map[store.RunID]bool{},
	}
}

func (d *Daemon) setRunActive(id store.RunID, active bool) {
	if !d.cfg.KeepAwake || d.keepAwake == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	if active {
		d.activeRuns[id] = true
		if d.awakeStop == nil {
			ctx, cancel := context.WithCancel(d.ctx)
			d.awakeStop = cancel
			d.awakeGen++
			gen := d.awakeGen
			d.wg.Go(func() {
				err := d.keepAwake(ctx)
				if err != nil && ctx.Err() == nil {
					d.log.Warn("keep-awake exited", "err", err)
				}
				d.mu.Lock()
				if d.awakeGen == gen {
					d.awakeStop = nil
				}
				d.mu.Unlock()
			})
		}
		return
	}
	delete(d.activeRuns, id)
	if len(d.activeRuns) == 0 && d.awakeStop != nil {
		d.awakeStop()
		d.awakeStop = nil
		d.awakeGen++
	}
}

// Serve answers ipc calls on ln until ctx ends. Before it returns, it closes
// every live Session and waits for the goroutines of every run. Runs that
// were active stay active in the store until the next Recover.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	err := ipc.Serve(ctx, ln, d.handle)
	d.cancel()
	d.wg.Wait()
	return err
}

// Recover fails every run that was active when the previous daemon stopped,
// so that its webhook fires, and rewrites every run's NDJSON log from the
// store. Call it before Serve. Calling it again changes nothing.
//
// A run's agent runs in its own process group, so it and its commands can
// outlive a daemon crash. Recover ends the processes working in the
// worktree of each run it fails.
func (d *Daemon) Recover(ctx context.Context) error {
	runs, err := d.store.ListRuns(ctx)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Lifecycle.State.Active() {
			proc.EndUnder(r.Worktree)
			restarted := lifecycle.BackendFailed{Err: "daemon restarted while the run was active"}
			if _, err := d.command(ctx, r.ID, restarted); err != nil {
				return fmt.Errorf("fail run %s: %w", r.ID, err)
			}
		}
		if err := d.rewriteLog(ctx, r.ID); err != nil {
			return fmt.Errorf("rewrite log of run %s: %w", r.ID, err)
		}
	}
	return nil
}

func (d *Daemon) handle(ctx context.Context, req ipc.Request, send func(ipc.Response) error) error {
	var err error
	switch req.Method {
	case ipc.MethodList:
		err = d.list(ctx, send)
	case ipc.MethodLogs:
		err = d.logs(ctx, store.RunID(req.RunID), req.Follow, send)
	default:
		var run store.Run
		if run, err = d.runCall(ctx, req); err == nil {
			out := d.toIPCRun(run)
			err = send(ipc.Response{Run: &out})
		}
	}
	if err != nil {
		return ipcError(err)
	}
	return nil
}

func (d *Daemon) runCall(ctx context.Context, req ipc.Request) (store.Run, error) {
	id := store.RunID(req.RunID)
	switch req.Method {
	case ipc.MethodLaunch:
		return d.launch(ctx, req)
	case ipc.MethodStatus:
		return d.store.GetRun(ctx, id)
	case ipc.MethodFollowUp:
		if strings.TrimSpace(req.Prompt) == "" {
			return store.Run{}, invalid("the prompt is empty")
		}
		run, err := d.store.GetRun(ctx, id)
		if err != nil {
			return store.Run{}, err
		}
		if err := d.rejectUntrustedPrint(run.Backend); err != nil {
			return store.Run{}, err
		}
		return d.command(ctx, id, lifecycle.FollowUp{Prompt: req.Prompt})
	case ipc.MethodStop:
		return d.command(ctx, id, lifecycle.Stop{})
	case ipc.MethodApprove:
		return d.command(ctx, id, lifecycle.PermissionAnswered{Approve: true})
	case ipc.MethodDeny:
		return d.command(ctx, id, lifecycle.PermissionAnswered{Approve: false})
	}
	return store.Run{}, invalid(fmt.Sprintf("unknown method %q", req.Method))
}

func (d *Daemon) launch(ctx context.Context, req ipc.Request) (store.Run, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return store.Run{}, invalid("the prompt is empty")
	}
	if !filepath.IsAbs(req.Repo) {
		return store.Run{}, invalid(fmt.Sprintf("repo %q is not an absolute path", req.Repo))
	}
	repo, err := worktree.Root(ctx, req.Repo)
	if err != nil {
		return store.Run{}, invalid(err.Error())
	}
	ref := cmp.Or(req.Ref, "HEAD")
	if strings.HasPrefix(ref, "-") {
		return store.Run{}, invalid(fmt.Sprintf("ref %q starts with %q", ref, "-"))
	}
	if _, err := worktree.ResolveCommit(ctx, repo, ref); err != nil {
		return store.Run{}, invalid(fmt.Sprintf("ref %q does not name a commit", ref))
	}
	kind := d.cfg.Backend
	if req.Backend != "" {
		if kind, err = backend.ParseKind(req.Backend); err != nil {
			return store.Run{}, invalid(err.Error())
		}
	}
	if d.backends[kind] == nil {
		return store.Run{}, invalid(fmt.Sprintf("backend %q is not available", kind))
	}
	if err := d.rejectUntrustedPrint(kind); err != nil {
		return store.Run{}, err
	}
	id, err := newRunID()
	if err != nil {
		return store.Run{}, err
	}
	now := time.Now().UTC()
	lc, effects := lifecycle.Launch(req.Prompt)
	run := store.Run{
		ID:        id,
		Repo:      repo,
		BaseRef:   ref,
		Branch:    "clb/" + string(id),
		Worktree:  filepath.Join(d.cfg.WorktreeDir, string(id)),
		Backend:   kind,
		Model:     d.configuredModel(req.Model),
		Lifecycle: lc,
		CreatedAt: now,
		UpdatedAt: now,
	}
	a := d.actor(id)
	return a.call(ctx, func() error { return a.create(run, effects) })
}

func (d *Daemon) configuredModel(requested string) string {
	if m := strings.TrimSpace(requested); m != "" {
		return m
	}
	if d.cfg.Home != "" {
		if loaded, err := config.Load(d.cfg.Home); err == nil && strings.TrimSpace(loaded.Model) != "" {
			return strings.TrimSpace(loaded.Model)
		}
	}
	return strings.TrimSpace(d.cfg.Model)
}

func (d *Daemon) command(ctx context.Context, id store.RunID, e lifecycle.Event) (store.Run, error) {
	if _, err := d.store.GetRun(ctx, id); err != nil {
		return store.Run{}, err
	}
	a := d.actor(id)
	return a.call(ctx, func() error {
		_, err := a.apply(e, nil)
		return err
	})
}

func (d *Daemon) list(ctx context.Context, send func(ipc.Response) error) error {
	runs, err := d.store.ListRuns(ctx)
	if err != nil {
		return err
	}
	out := make([]ipc.Run, 0, len(runs))
	for _, r := range runs {
		out = append(out, d.toIPCRun(r))
	}
	return send(ipc.Response{Runs: out})
}

const logPollInterval = 200 * time.Millisecond

func (d *Daemon) logs(ctx context.Context, id store.RunID, follow bool, send func(ipc.Response) error) error {
	var after int64
	for {
		run, err := d.store.GetRun(ctx, id)
		if err != nil {
			return err
		}
		events, err := d.store.Events(ctx, id, after)
		if err != nil {
			return err
		}
		for _, e := range events {
			ev := toIPCEvent(e)
			if err := send(ipc.Response{Event: &ev}); err != nil {
				return err
			}
			after = e.Seq
		}
		switch {
		case !follow:
			return nil
		case len(events) > 0:
			continue
		case !run.Lifecycle.State.Active():
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(logPollInterval):
		}
	}
}

func (d *Daemon) actor(id store.RunID) *actor {
	d.mu.Lock()
	defer d.mu.Unlock()
	a, ok := d.actors[id]
	if !ok {
		a = &actor{d: d, id: id, inbox: make(chan func(), 64)}
		d.actors[id] = a
		d.wg.Go(a.loop)
	}
	return a
}

// rewriteLog runs on the run's goroutine, or under d.mu when the run has
// none, so that no append races the rewrite.
func (d *Daemon) rewriteLog(ctx context.Context, id store.RunID) error {
	d.mu.Lock()
	a, ok := d.actors[id]
	if !ok {
		defer d.mu.Unlock()
		return d.writeLog(ctx, id)
	}
	d.mu.Unlock()
	_, err := a.call(ctx, func() error { return d.writeLog(d.ctx, id) })
	return err
}

func (d *Daemon) toIPCRun(r store.Run) ipc.Run {
	out := ipc.Run{
		ID:            string(r.ID),
		Status:        string(r.Lifecycle.State),
		Repo:          r.Repo,
		BaseRef:       r.BaseRef,
		Branch:        r.Branch,
		Worktree:      r.Worktree,
		Backend:       string(r.Backend),
		Model:         cmp.Or(r.Model, config.CLIDefaultModel()),
		SessionID:     r.SessionID,
		Summary:       r.Summary,
		Error:         r.Lifecycle.Error,
		QueuedPrompts: len(r.Lifecycle.Prompts),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
	if len(r.Lifecycle.Permissions) > 0 {
		p := r.Lifecycle.Permissions[0]
		out.PendingPermission = &ipc.Permission{
			ID:         p.ID,
			Title:      p.Title,
			ToolCallID: p.ToolCallID,
			Kind:       p.Kind,
			Command:    p.Command,
			URL:        p.URL,
			ToolCall:   p.ToolCall,
			Options:    p.Options,
		}
	}
	return out
}

func toIPCEvent(e store.Event) ipc.Event {
	return ipc.Event{RunID: string(e.RunID), Seq: e.Seq, Time: e.Time, Type: e.Type, Data: e.Data}
}

func ipcError(err error) *ipc.Error {
	var e *ipc.Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, lifecycle.ErrNotAllowed):
		return &ipc.Error{Code: ipc.CodeConflict, Message: err.Error()}
	case errors.Is(err, store.ErrNotFound):
		return &ipc.Error{Code: ipc.CodeNotFound, Message: err.Error()}
	}
	return &ipc.Error{Code: ipc.CodeInternal, Message: err.Error()}
}

func invalid(msg string) error {
	return &ipc.Error{Code: ipc.CodeInvalid, Message: msg}
}

const untrustedPrint = `the print backend needs "trustWorktree": true in config.json; agent -p refuses an untrusted workspace, and each run uses a new worktree`

func (d *Daemon) rejectUntrustedPrint(kind backend.Kind) error {
	if kind != backend.Print || d.cfg.TrustWorktree {
		return nil
	}
	return invalid(untrustedPrint)
}

func newRunID() (store.RunID, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return store.RunID("run-" + hex.EncodeToString(b)), nil
}
