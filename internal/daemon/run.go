package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
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
	eventPermissionCancelled = "permission_cancelled"
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

type permissionCancelledData struct {
	ID           string `json:"id"`
	Reason       string `json:"reason"`
	SupersededBy string `json:"supersededBy,omitempty"`
}

type webhookData struct {
	Event      string `json:"event,omitempty"`
	Status     string `json:"status"`
	Permission string `json:"permission,omitempty"`
	Reminder   int    `json:"reminder,omitempty"`
	Delivered  bool   `json:"delivered"`
	Error      string `json:"error,omitempty"`
}

type permResult struct {
	approve bool
	err     error
}

type permWait struct {
	mu        sync.Mutex
	id        string
	withdrawn bool
	answer    chan permResult
}

func (w *permWait) alreadyWithdrawn() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.withdrawn
}

func (w *permWait) claim(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.id = id
	return w.withdrawn
}

func (w *permWait) withdraw() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.withdrawn {
		return "", false
	}
	w.withdrawn = true
	return w.id, w.id != ""
}

type permReminder struct {
	timer *time.Timer
	n     int
	perm  lifecycle.Permission
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
	closing     *generation
	state       lifecycle.State
	stall       *time.Timer
	perms       int
	permRetries map[string]int
	carried     []store.NewEvent
}

// generation is one agent session of a run. Everything a session reports
// carries its generation, so reports from a closed session are dropped.
type generation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	session   Session
	closed    chan struct{}
	model     string
	turn      turnSummary
	retries   int
	answers   map[string]chan<- permResult
	reminders map[string]*permReminder
}

func (a *actor) loop() {
	defer a.stopStall()
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
	a.state = run.Lifecycle.State
	a.d.setRunActive(a.id, run.Lifecycle.State.Active())
	a.appendEvent(eventStatus, statusData{Status: run.Lifecycle.State})
	a.d.log.Info("run launched", "run", run.ID, "repo", run.Repo, "backend", run.Backend)
	err := a.perform(run, effects)
	a.syncStall(true)
	return err
}

func (a *actor) apply(e lifecycle.Event, edit func(*store.Run)) (store.Run, error) {
	run, err := a.d.store.GetRun(a.d.writes, a.id)
	if err != nil {
		return run, err
	}
	return a.applyTo(run, e, edit)
}

func (a *actor) applyTo(run store.Run, e lifecycle.Event, edit func(*store.Run)) (store.Run, error) {
	a.hydrateRetries(&run.Lifecycle)
	prev := run.Lifecycle
	next, effects, err := lifecycle.Apply(run.Lifecycle, e)
	if err != nil {
		return run, err
	}
	a.rememberRetries(next)
	changed := next.State != prev.State
	a.state = next.State
	run.Lifecycle = next
	if edit != nil {
		edit(&run)
	}
	at := time.Now().UTC()
	events, err := transitionEvents(effects, next, changed, at)
	if err != nil {
		return run, err
	}
	if next.State == lifecycle.Stopping {
		a.stopStall()
		a.carried = append(a.carried, events...)
		return run, a.perform(run, effects)
	}
	run.UpdatedAt = at
	events = append(a.carried, events...)
	a.carried = nil
	appended, err := a.d.store.UpdateRun(a.d.writes, run, events...)
	if err != nil {
		return run, err
	}
	a.d.setRunActive(a.id, next.State.Active())
	for _, ev := range appended {
		a.appendNDJSON(ev)
	}
	if changed {
		a.d.log.Info("run status", "run", a.id, "status", next.State, "error", next.Error)
	}
	err = a.perform(run, effects)
	a.syncStall(changed || len(effects) > 0)
	return run, err
}

func (a *actor) hydrateRetries(r *lifecycle.Run) {
	if len(a.permRetries) == 0 {
		return
	}
	for i := range r.Permissions {
		r.Permissions[i].Retries = a.permRetries[r.Permissions[i].ID]
	}
}

func (a *actor) rememberRetries(r lifecycle.Run) {
	if len(r.Permissions) == 0 {
		a.permRetries = nil
		return
	}
	m := make(map[string]int, len(r.Permissions))
	for _, p := range r.Permissions {
		m[p.ID] = p.Retries
	}
	a.permRetries = m
}

func transitionEvents(effects []lifecycle.Effect, next lifecycle.Run, changed bool, at time.Time) ([]store.NewEvent, error) {
	var cancels, answered []store.NewEvent
	var requested *store.NewEvent
	for _, eff := range effects {
		switch eff := eff.(type) {
		case lifecycle.CancelPermission:
			ev, err := newEvent(at, eventPermissionCancelled, permissionCancelledData{
				ID:           eff.ID,
				Reason:       string(eff.Reason),
				SupersededBy: eff.SupersededBy,
			})
			if err != nil {
				return nil, err
			}
			cancels = append(cancels, ev)
		case lifecycle.AnswerPermission:
			ev, err := newEvent(at, eventPermissionAnswered, permissionAnsweredData{ID: eff.ID, Approved: eff.Approve})
			if err != nil {
				return nil, err
			}
			answered = append(answered, ev)
		case lifecycle.NotifyPermission:
			ev, err := newEvent(at, eventPermissionRequested, eff.Permission)
			if err != nil {
				return nil, err
			}
			requested = &ev
		}
	}
	var out []store.NewEvent
	out = append(out, cancels...)
	if changed && next.State != lifecycle.Stopping {
		ev, err := newEvent(at, eventStatus, statusData{Status: next.State, Error: next.Error})
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if requested != nil {
		out = append(out, *requested)
	}
	out = append(out, answered...)
	return out, nil
}

func newEvent(at time.Time, typ string, data any) (store.NewEvent, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return store.NewEvent{}, err
	}
	return store.NewEvent{Time: at, Type: typ, Data: raw}, nil
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
		case lifecycle.CancelPermission:
			a.cancelPermission(eff)
		case lifecycle.NotifyPermission:
			a.notifyPermission(eff.Permission)
		case lifecycle.CloseBackend:
			a.closeBackend()
		case lifecycle.RemoveWorktree:
			if err := a.removeWorktree(run); err != nil {
				errs = append(errs, err)
			}
		case lifecycle.Notify:
			a.notify(run, eff.State)
		default:
			panic(fmt.Sprintf("daemon: unhandled effect %T", eff))
		}
	}
	return errors.Join(errs...)
}

// Removal runs under a.d.writes, so a stop that races shutdown still removes
// the worktree. removeTimeout keeps such a removal from holding up Serve
// indefinitely.
const removeTimeout = time.Minute

// removeWorktree removes the run's worktree and applies the outcome in the
// same job, so that no other command acts on the run in between. It returns
// the removal's error, if any, for the caller of the command.
func (a *actor) removeWorktree(run store.Run) error {
	a.awaitClosed()
	proc.EndUnder(run.Worktree)
	ctx, cancel := context.WithTimeout(a.d.writes, removeTimeout)
	err := worktree.Remove(ctx, run.Repo, run.Worktree)
	cancel()
	var outcome lifecycle.Event = lifecycle.WorktreeRemoved{}
	if err != nil {
		err = fmt.Errorf("remove worktree: %w", err)
		outcome = lifecycle.WorktreeRemoveFailed{Err: err.Error()}
	}
	if _, applyErr := a.applyTo(run, outcome, nil); applyErr != nil {
		return errors.Join(err, applyErr)
	}
	return err
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
	g := &generation{ctx: ctx, cancel: cancel, closed: make(chan struct{}), answers: map[string]chan<- permResult{}}
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
		a.postFrom(g, func() { a.started(g, sessionID, err) })
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

func (a *actor) started(g *generation, sessionID string, err error) {
	if err != nil {
		a.fail(fmt.Errorf("start agent: %w", err))
		return
	}
	model := cmp.Or(g.model, config.CLIDefaultModel())
	a.record(lifecycle.Started{}, func(r *store.Run) {
		r.SessionID = sessionID
		r.Model = mergeModelLabel(r.Model, model)
	})
}

func (a *actor) setModel(g *generation, model string) {
	g.model = model
	run, err := a.d.store.GetRun(a.d.writes, a.id)
	if err != nil {
		a.d.log.Error("get run for model update", "run", a.id, "err", err)
		return
	}
	next := mergeModelLabel(run.Model, model)
	if next == run.Model {
		return
	}
	run.Model = next
	run.UpdatedAt = time.Now().UTC()
	if _, err := a.d.store.UpdateRun(a.d.writes, run); err != nil {
		a.d.log.Error("update run model", "run", a.id, "err", err)
	}
}

func mergeModelLabel(current, reported string) string {
	reported = strings.TrimSpace(reported)
	if reported == "" {
		return current
	}
	if current == "" {
		return reported
	}
	if !strings.Contains(current, "[") && strings.HasPrefix(reported, current+"[") {
		return reported
	}
	return current
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
	if a.state == lifecycle.Queued || a.state == lifecycle.Running {
		a.resetStall(g)
	}
	if e.Type == backend.EventTransportError {
		g.retries++
	}
	if a.d.cfg.StoreAllEvents || !unstoredEvents[e.Type] {
		a.appendEvent(e.Type, e.Data)
	}
	g.turn.add(e)
}

func (a *actor) syncStall(reset bool) {
	switch a.state {
	case lifecycle.Queued, lifecycle.Running:
		if reset && a.gen != nil {
			a.resetStall(a.gen)
		}
	default:
		a.stopStall()
	}
}

func (a *actor) resetStall(g *generation) {
	a.stopStall()
	d := a.d.cfg.StallTimeout.Duration()
	if d <= 0 || g == nil {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(d, func() {
		a.postFrom(g, func() {
			if a.stall != timer {
				return
			}
			a.fail(fmt.Errorf("run stalled: no agent activity for %s", d))
		})
	})
	a.stall = timer
}

func (a *actor) stopStall() {
	if a.stall != nil {
		a.stall.Stop()
		a.stall = nil
	}
}

type turnSummary struct {
	lastMessage    string
	currentMessage strings.Builder
}

func (s *turnSummary) add(e backend.Event) {
	if e.Type == "tool_call" || e.Type == "tool_call_update" || e.Type == backend.EventTransportError {
		s.endMessage()
		return
	}
	if e.Type == "assistant" {
		s.endMessage()
	}
	s.currentMessage.WriteString(e.Text)
}

func (s *turnSummary) endMessage() {
	if s.currentMessage.Len() == 0 {
		return
	}
	s.lastMessage = s.currentMessage.String()
	s.currentMessage.Reset()
}

func (s *turnSummary) String() string {
	if s.currentMessage.Len() > 0 {
		return s.currentMessage.String()
	}
	return s.lastMessage
}

func (s *turnSummary) Reset() {
	s.lastMessage = ""
	s.currentMessage.Reset()
}

func (a *actor) permissionAsked(g *generation, req backend.PermissionRequest, w *permWait) {
	if w.alreadyWithdrawn() {
		return
	}
	p := lifecycle.Permission{
		ID:         a.nextPermissionID(),
		Title:      req.Title,
		ToolCallID: req.ToolCallID,
		Kind:       req.Kind,
		Command:    req.Command,
		URL:        req.URL,
		ToolCall:   req.ToolCall,
		Options:    req.Options,
		Retries:    g.retries,
	}
	run, err := a.apply(lifecycle.PermissionRequested{Permission: p}, nil)
	if err != nil || !hasPermission(run.Lifecycle, p.ID) {
		sendResult(w.answer, permResult{approve: false})
		return
	}
	if w.claim(p.ID) {
		a.record(lifecycle.PermissionCancelled{ID: p.ID, Reason: lifecycle.CancelWithdrawn}, nil)
		return
	}
	g.answers[p.ID] = w.answer
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
	a.stopReminder(eff.ID)
	if a.gen == nil {
		return
	}
	if ch, ok := a.gen.answers[eff.ID]; ok {
		sendResult(ch, permResult{approve: eff.Approve})
		delete(a.gen.answers, eff.ID)
	}
}

func (a *actor) cancelPermission(eff lifecycle.CancelPermission) {
	a.stopReminder(eff.ID)
	if a.gen == nil {
		return
	}
	ch := a.gen.answers[eff.ID]
	delete(a.gen.answers, eff.ID)
	sendResult(ch, permResult{err: errPermissionCancelled})
}

func (a *actor) closeBackend() {
	a.stopStall()
	if a.gen != nil {
		a.stopReminders(a.gen)
		a.gen.cancel()
		a.closing = a.gen
		a.gen = nil
	}
}

func (a *actor) stopReminders(g *generation) {
	for id, rem := range g.reminders {
		rem.timer.Stop()
		delete(g.reminders, id)
	}
}

func (a *actor) stopReminder(id string) {
	if a.gen == nil {
		return
	}
	rem := a.gen.reminders[id]
	if rem == nil {
		return
	}
	rem.timer.Stop()
	delete(a.gen.reminders, id)
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
		sender, err := a.d.webhook()
		if err == nil && sender == nil {
			return
		}
		if err == nil {
			_, err = sender.Send(a.d.ctx, p)
		}
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

func (a *actor) notifyPermission(p lifecycle.Permission) {
	run, err := a.d.store.GetRun(a.d.writes, a.id)
	if err != nil {
		a.d.log.Error("permission webhook", "run", a.id, "err", err)
	} else {
		a.sendPermissionWebhook(webhook.EventPermissionRequested, run, p, 0)
	}
	a.armReminder(p)
}

func (a *actor) reminderInterval() time.Duration {
	if a.d.webhook == nil {
		return 0
	}
	sender, err := a.d.webhook()
	if err != nil || sender == nil {
		return 0
	}
	return sender.Reminder
}

func (a *actor) armReminder(p lifecycle.Permission) {
	g := a.gen
	if g == nil {
		return
	}
	interval := a.reminderInterval()
	if interval <= 0 {
		return
	}
	a.scheduleReminder(g, p, interval, 0)
}

func (a *actor) scheduleReminder(g *generation, p lifecycle.Permission, interval time.Duration, n int) {
	if g.reminders == nil {
		g.reminders = map[string]*permReminder{}
	}
	if old := g.reminders[p.ID]; old != nil {
		old.timer.Stop()
	}
	timer := time.AfterFunc(interval, func() {
		a.postFrom(g, func() { a.fireReminder(g, p.ID) })
	})
	g.reminders[p.ID] = &permReminder{timer: timer, n: n, perm: p}
}

func (a *actor) fireReminder(g *generation, id string) {
	if g != a.gen {
		return
	}
	rem := g.reminders[id]
	if rem == nil {
		return
	}
	run, err := a.d.store.GetRun(a.d.writes, a.id)
	if err != nil || !hasPermission(run.Lifecycle, id) {
		a.stopReminder(id)
		return
	}
	interval := a.reminderInterval()
	if interval <= 0 {
		a.stopReminder(id)
		return
	}
	n := rem.n + 1
	perm := rem.perm
	a.sendPermissionWebhook(webhook.EventPermissionReminder, run, perm, n)
	a.scheduleReminder(g, perm, interval, n)
}

func (a *actor) sendPermissionWebhook(event string, run store.Run, p lifecycle.Permission, reminder int) {
	if a.d.webhook == nil {
		return
	}
	payload := webhook.Payload{
		Event:     event,
		Timestamp: time.Now().UTC(),
		RunID:     string(run.ID),
		Status:    string(run.Lifecycle.State),
		Repo:      run.Repo,
		Branch:    run.Branch,
		Permission: &webhook.Permission{
			ID:      p.ID,
			Title:   p.Title,
			Kind:    p.Kind,
			Command: p.Command,
			URL:     p.URL,
			Options: p.Options,
		},
		Reminder: reminder,
	}
	a.d.wg.Go(func() {
		sender, err := a.d.webhook()
		if err == nil && sender == nil {
			return
		}
		if err == nil {
			_, err = sender.Send(a.d.ctx, payload)
		}
		a.post(func() {
			data := webhookData{
				Event:      event,
				Status:     payload.Status,
				Permission: p.ID,
				Reminder:   reminder,
				Delivered:  err == nil,
			}
			if err != nil {
				data.Error = err.Error()
				a.d.log.Warn("deliver webhook", "run", a.id, "status", payload.Status, "err", err)
			}
			a.appendEvent(eventWebhook, data)
		})
	})
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

func (s sink) SetModel(model string) {
	s.a.postFrom(s.g, func() { s.a.setModel(s.g, model) })
}

var (
	errSessionClosed       = errors.New("the agent session was closed")
	errPermissionCancelled = errors.New("permission cancelled")
)

func (s sink) Permission(ctx context.Context, req backend.PermissionRequest) (bool, error) {
	w := &permWait{answer: make(chan permResult, 1)}
	if !s.a.postFrom(s.g, func() { s.a.permissionAsked(s.g, req, w) }) {
		return false, errSessionClosed
	}
	select {
	case res := <-w.answer:
		return res.approve, res.err
	case <-ctx.Done():
		if id, ok := w.withdraw(); ok {
			s.a.postFrom(s.g, func() {
				s.a.record(lifecycle.PermissionCancelled{ID: id, Reason: lifecycle.CancelWithdrawn}, nil)
			})
		}
		return false, ctx.Err()
	case <-s.g.ctx.Done():
		return false, errSessionClosed
	}
}

func hasPermission(r lifecycle.Run, id string) bool {
	for _, p := range r.Permissions {
		if p.ID == id {
			return true
		}
	}
	return false
}

func sendResult(ch chan<- permResult, res permResult) {
	if ch == nil {
		return
	}
	select {
	case ch <- res:
	default:
	}
}
