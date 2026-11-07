package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/lifecycle"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/worktree"
)

const (
	eventStatus              = "status"
	eventPrompt              = "prompt"
	eventPermissionRequested = "permission_requested"
	eventPermissionAnswered  = "permission_answered"
	eventWebhook             = "webhook"
)

// Thought chunks, including print-mode "thinking", and available-command
// updates repeat on every turn and bloat the run log. Backends still
// forward them.
var unstoredEvents = map[string]bool{
	"agent_thought_chunk":       true,
	"available_commands_update": true,
	"thinking":                  true,
}

type statusData struct {
	Status lifecycle.State `json:"status"`
	Error  string          `json:"error,omitempty"`
}

type promptData struct {
	Prompt string `json:"prompt"`
}

type permissionAnsweredData struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

type webhookData struct {
	Status    string `json:"status"`
	Delivered bool   `json:"delivered"`
	Error     string `json:"error,omitempty"`
}

// actor owns one run. Its goroutine runs every function sent to inbox and is
// the only writer of the run's row, events, and NDJSON log. The fields after
// inbox belong to that goroutine.
type actor struct {
	d  *Daemon
	id store.RunID

	// sweepMu guards sweeper, the newest generation. Only its goroutine
	// sweeps the worktree after Close, so a generation whose Close outlasted
	// closeWait cannot end the processes of the one that replaced it.
	sweepMu sync.Mutex
	sweeper *generation

	inbox chan func()

	gen *generation
	// closing is the last closed generation. Its agent may still be
	// shutting down.
	closing *generation
	perms   int
}

// generation is one agent session of a run. Everything a session reports
// carries its generation, so reports from a closed session are dropped.
type generation struct {
	ctx     context.Context
	cancel  context.CancelFunc
	session Session
	closed  chan struct{}
	turn    turnSummary
	answers map[string]chan<- bool
}

func (a *actor) loop() {
	for {
		select {
		case f := <-a.inbox:
			if a.d.ctx.Err() != nil {
				return
			}
			f()
		case <-a.d.ctx.Done():
			return
		}
	}
}

func (a *actor) post(f func()) bool {
	select {
	case a.inbox <- f:
		return true
	case <-a.d.ctx.Done():
		return false
	}
}

// postFrom queues a report from g, which runs only while g is still the
// run's generation. Posting gives up once g is closed, because the run's
// goroutine is not reading inbox while it waits for g's agent to exit.
func (a *actor) postFrom(g *generation, f func()) bool {
	report := func() {
		if g == a.gen {
			f()
		}
	}
	select {
	case a.inbox <- report:
		return true
	case <-g.ctx.Done():
		return false
	}
}

var errStopping = errors.New("the daemon is shutting down")

func (a *actor) call(ctx context.Context, f func() error) (store.Run, error) {
	type result struct {
		run store.Run
		err error
	}
	done := make(chan result, 1)
	job := func() {
		if err := f(); err != nil {
			done <- result{err: err}
			return
		}
		run, err := a.d.store.GetRun(a.d.writes, a.id)
		done <- result{run, err}
	}
	select {
	case a.inbox <- job:
	case <-ctx.Done():
		return store.Run{}, ctx.Err()
	case <-a.d.ctx.Done():
		return store.Run{}, errStopping
	}
	select {
	case r := <-done:
		return r.run, r.err
	case <-ctx.Done():
		return store.Run{}, ctx.Err()
	case <-a.d.ctx.Done():
		return store.Run{}, errStopping
	}
}

func (a *actor) create(run store.Run, effects []lifecycle.Effect) error {
	if err := a.d.store.CreateRun(a.d.writes, run); err != nil {
		return err
	}
	a.appendEvent(eventStatus, statusData{Status: run.Lifecycle.State})
	a.d.log.Info("run launched", "run", run.ID, "repo", run.Repo, "backend", run.Backend)
	return a.perform(run, effects)
}

func (a *actor) apply(e lifecycle.Event, edit func(*store.Run)) (store.Run, error) {
	run, err := a.d.store.GetRun(a.d.writes, a.id)
	if err != nil {
		return run, err
	}
	next, effects, err := lifecycle.Apply(run.Lifecycle, e)
	if err != nil {
		return run, err
	}
	changed := next.State != run.Lifecycle.State
	run.Lifecycle = next
	if edit != nil {
		edit(&run)
	}
	run.UpdatedAt = time.Now().UTC()
	var status []store.NewEvent
	if changed {
		data, err := json.Marshal(statusData{Status: next.State, Error: next.Error})
		if err != nil {
			return run, err
		}
		status = []store.NewEvent{{Time: run.UpdatedAt, Type: eventStatus, Data: data}}
	}
	appended, err := a.d.store.UpdateRun(a.d.writes, run, status...)
	if err != nil {
		return run, err
	}
	for _, e := range appended {
		a.appendNDJSON(e)
	}
	if changed {
		a.d.log.Info("run status", "run", a.id, "status", next.State, "error", next.Error)
	}
	return run, a.perform(run, effects)
}

func (a *actor) record(e lifecycle.Event, edit func(*store.Run)) {
	if _, err := a.apply(e, edit); err != nil {
		a.d.log.Error("apply run event", "run", a.id, "event", fmt.Sprintf("%T", e), "err", err)
	}
}

func (a *actor) fail(err error) {
	a.record(lifecycle.BackendFailed{Err: err.Error()}, nil)
}

func (a *actor) perform(run store.Run, effects []lifecycle.Effect) error {
	var errs []error
	for _, eff := range effects {
		switch eff := eff.(type) {
		case lifecycle.StartBackend:
			a.startBackend(run, eff.Resume)
		case lifecycle.SendPrompt:
			a.sendPrompt(eff.Prompt)
		case lifecycle.AnswerPermission:
			a.answerPermission(eff)
		case lifecycle.CloseBackend:
			a.closeBackend()
		case lifecycle.RemoveWorktree:
			a.awaitClosed()
			proc.EndUnder(run.Worktree)
			if err := worktree.Remove(a.d.ctx, run.Repo, run.Worktree); err != nil {
				errs = append(errs, fmt.Errorf("remove worktree: %w", err))
			}
		case lifecycle.Notify:
			a.notify(run, eff.State)
		default:
			panic(fmt.Sprintf("daemon: unhandled effect %T", eff))
		}
	}
	return errors.Join(errs...)
}

// startBackend creates the worktree on the run's goroutine, so a stop can
// never interleave with it, then starts the session in the background.
func (a *actor) startBackend(run store.Run, resume bool) {
	a.awaitClosed()
	if err := worktree.Add(a.d.ctx, run.Repo, run.Worktree, run.Branch, run.BaseRef); err != nil {
		a.fail(fmt.Errorf("create worktree: %w", err))
		return
	}
	newSession := a.d.backends[run.Backend]
	if newSession == nil {
		a.fail(fmt.Errorf("backend %q is not available", run.Backend))
		return
	}
	ctx, cancel := context.WithCancel(a.d.ctx)
	g := &generation{ctx: ctx, cancel: cancel, closed: make(chan struct{}), answers: map[string]chan<- bool{}}
	g.session = newSession(sink{a: a, g: g})
	a.gen = g
	a.sweepMu.Lock()
	a.sweeper = g
	a.sweepMu.Unlock()

	opts := backend.StartOptions{Dir: run.Worktree, Model: run.Model}
	if resume {
		opts.ResumeSessionID = run.SessionID
	}
	a.d.wg.Go(func() {
		defer close(g.closed)
		sessionID, err := g.session.Start(ctx, opts)
		a.postFrom(g, func() { a.started(sessionID, err) })
		<-ctx.Done()
		if err := g.session.Close(); err != nil {
			a.d.log.Warn("close agent session", "run", a.id, "err", err)
		}
		a.sweep(g, run.Worktree)
	})
}

// sweep ends the processes left in dir unless a newer generation than g has
// started. It holds sweepMu while it sweeps, so startBackend cannot start the
// next session until the sweep is over.
func (a *actor) sweep(g *generation, dir string) {
	a.sweepMu.Lock()
	defer a.sweepMu.Unlock()
	if a.sweeper == g {
		proc.EndUnder(dir)
	}
}

func (a *actor) started(sessionID string, err error) {
	if err != nil {
		a.fail(fmt.Errorf("start agent: %w", err))
		return
	}
	a.record(lifecycle.Started{}, func(r *store.Run) { r.SessionID = sessionID })
}

func (a *actor) sendPrompt(prompt string) {
	g := a.gen
	a.appendEvent(eventPrompt, promptData{Prompt: prompt})
	g.turn.Reset()
	a.d.wg.Go(func() {
		err := g.session.Prompt(g.ctx, prompt)
		a.postFrom(g, func() { a.promptDone(g, err) })
	})
}

func (a *actor) promptDone(g *generation, err error) {
	if err != nil {
		a.fail(fmt.Errorf("prompt agent: %w", err))
		return
	}
	summary := g.turn.String()
	a.record(lifecycle.TurnEnded{}, func(r *store.Run) { r.Summary = summary })
}

func (a *actor) agentEvent(g *generation, e backend.Event) {
	if a.d.cfg.StoreAllEvents || !unstoredEvents[e.Type] {
		a.appendEvent(e.Type, e.Data)
	}
	g.turn.add(e)
}

// turnSummary is the turn's message text. A tool call ends the block in
// progress, so the text before it and the text after it stay separate. Other
// events, including thought chunks, stay in the current block.
type turnSummary struct {
	blocks []string
	cur    strings.Builder
}

func (s *turnSummary) add(e backend.Event) {
	if e.Type == "tool_call" || e.Type == "tool_call_update" {
		s.endBlock()
		return
	}
	s.cur.WriteString(e.Text)
}

func (s *turnSummary) endBlock() {
	if s.cur.Len() == 0 {
		return
	}
	s.blocks = append(s.blocks, s.cur.String())
	s.cur.Reset()
}

func (s *turnSummary) String() string {
	if s.cur.Len() == 0 {
		return strings.Join(s.blocks, "\n")
	}
	blocks := make([]string, len(s.blocks)+1)
	copy(blocks, s.blocks)
	blocks[len(s.blocks)] = s.cur.String()
	return strings.Join(blocks, "\n")
}

func (s *turnSummary) Reset() {
	s.blocks = nil
	s.cur.Reset()
}

func (a *actor) permissionAsked(g *generation, req backend.PermissionRequest, answer chan<- bool) {
	p := lifecycle.Permission{ID: a.nextPermissionID(), Title: req.Title, ToolCall: req.ToolCall, Options: req.Options}
	run, err := a.apply(lifecycle.PermissionRequested{Permission: p}, nil)
	if err != nil || run.Lifecycle.State != lifecycle.AwaitingPermission {
		answer <- false
		return
	}
	g.answers[p.ID] = answer
	a.appendEvent(eventPermissionRequested, p)
}

// nextPermissionID counts from the stored events, so IDs stay unique across
// daemon restarts.
func (a *actor) nextPermissionID() string {
	if a.perms == 0 {
		events, err := a.d.store.Events(a.d.writes, a.id, 0)
		if err != nil {
			a.d.log.Error("count permissions", "run", a.id, "err", err)
		}
		for _, e := range events {
			if e.Type == eventPermissionRequested {
				a.perms++
			}
		}
	}
	a.perms++
	return fmt.Sprintf("perm-%d", a.perms)
}

func (a *actor) answerPermission(eff lifecycle.AnswerPermission) {
	if a.gen != nil {
		if answer, ok := a.gen.answers[eff.ID]; ok {
			answer <- eff.Approve
			delete(a.gen.answers, eff.ID)
		}
	}
	a.appendEvent(eventPermissionAnswered, permissionAnsweredData{ID: eff.ID, Approved: eff.Approve})
}

func (a *actor) closeBackend() {
	if a.gen != nil {
		a.gen.cancel()
		a.closing = a.gen
		a.gen = nil
	}
}

// closeWait exceeds the longest Close plus one sweep of the worktree.
// proc.Stop sends SIGKILL after its grace period, and a sweep gives up after
// its last round. A resumed session must not start while the sweep could
// still end it.
var closeWait = 15 * time.Second

// awaitClosed waits for the last closed session's agent to exit and for the
// sweep of the worktree, so that neither is still running when the worktree
// is removed or when a resumed session starts there.
func (a *actor) awaitClosed() {
	g := a.closing
	if g == nil {
		return
	}
	a.closing = nil
	timer := time.NewTimer(closeWait)
	defer timer.Stop()
	select {
	case <-g.closed:
	case <-timer.C:
		a.d.log.Warn("agent session still closing; continuing", "run", a.id)
	case <-a.d.ctx.Done():
	}
}

func (a *actor) notify(run store.Run, state lifecycle.State) {
	if a.d.webhook == nil {
		return
	}
	p := webhook.Payload{
		Event:     webhook.EventStatusChange,
		Timestamp: time.Now().UTC(),
		RunID:     string(run.ID),
		Status:    string(state),
		Repo:      run.Repo,
		Branch:    run.Branch,
		Summary:   run.Summary,
		Error:     run.Lifecycle.Error,
	}
	a.d.wg.Go(func() {
		err := a.d.webhook.Send(a.d.ctx, p)
		a.post(func() { a.delivered(p.Status, err) })
	})
}

func (a *actor) delivered(status string, err error) {
	data := webhookData{Status: status, Delivered: err == nil}
	if err != nil {
		data.Error = err.Error()
		a.d.log.Warn("deliver webhook", "run", a.id, "status", status, "err", err)
	}
	a.appendEvent(eventWebhook, data)
}

func (a *actor) appendEvent(typ string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		a.d.log.Error("encode event", "run", a.id, "type", typ, "err", err)
		return
	}
	e, err := a.d.store.AppendEvent(a.d.writes, a.id, time.Now().UTC(), typ, raw)
	if err != nil {
		a.d.log.Error("append event", "run", a.id, "type", typ, "err", err)
		return
	}
	a.appendNDJSON(e)
}

func (a *actor) appendNDJSON(e store.Event) {
	if err := a.d.appendLog(e); err != nil {
		a.d.log.Warn("append to NDJSON log", "run", a.id, "err", err)
	}
}

type sink struct {
	a *actor
	g *generation
}

func (s sink) Event(e backend.Event) {
	s.a.postFrom(s.g, func() { s.a.agentEvent(s.g, e) })
}

var errSessionClosed = errors.New("the agent session was closed")

func (s sink) Permission(ctx context.Context, req backend.PermissionRequest) (bool, error) {
	answer := make(chan bool, 1)
	if !s.a.postFrom(s.g, func() { s.a.permissionAsked(s.g, req, answer) }) {
		return false, errSessionClosed
	}
	select {
	case approve := <-answer:
		return approve, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-s.g.ctx.Done():
		return false, errSessionClosed
	}
}
