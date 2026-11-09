// Package acp drives "agent acp", the Cursor CLI's Agent Client Protocol
// server, over stdio.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	acpsdk "github.com/coder/acp-go-sdk"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
)

const (
	cursorLogin      = "cursor_login"
	authRequiredCode = -32000
	cancelWait       = time.Second
	exitGrace        = proc.StderrWait + time.Second
)

// authTimeout bounds authenticate, which the CLI never answers when it has no
// "agent login" credentials.
var authTimeout = 30 * time.Second

var (
	errClosed      = errors.New("acp session closed")
	errAuthTimeout = errors.New("authenticate timed out")
)

// answerKind never names an "always" option: an answer covers one request,
// and a standing rule would let the agent skip later approvals.
var answerKind = map[bool]acpsdk.PermissionOptionKind{
	true:  acpsdk.PermissionOptionKindAllowOnce,
	false: acpsdk.PermissionOptionKindRejectOnce,
}

// Session is one "agent acp" process bound to one ACP session.
type Session struct {
	agentPath string
	sink      backend.Sink

	life      context.Context
	end       context.CancelCauseFunc
	closeOnce sync.Once

	replaying atomic.Bool

	mu    sync.Mutex
	proc  *proc.Process
	stdin io.Closer
	conn  *acpsdk.ClientSideConnection
	id    acpsdk.SessionId
}

func New(agentPath string, sink backend.Sink) *Session {
	life, end := context.WithCancelCause(context.Background())
	return &Session{agentPath: agentPath, sink: sink, life: life, end: end}
}

// Start runs the agent, authenticates with the existing "agent login"
// credentials, and opens a new session or loads opts.ResumeSessionID. It
// returns the session ID.
func (s *Session) Start(ctx context.Context, opts backend.StartOptions) (string, error) {
	id, err := s.start(ctx, opts)
	if err != nil {
		_ = s.Close()
		return "", err
	}
	return string(id), nil
}

func (s *Session) start(ctx context.Context, opts backend.StartOptions) (acpsdk.SessionId, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return "", err
	}
	var args []string
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	cmd := exec.Command(s.agentPath, append(args, "acp")...)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	p, err := proc.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("start agent acp: %w", err)
	}
	conn := acpsdk.NewClientSideConnection(client{s}, stdin, p.Stdout)

	s.mu.Lock()
	closed := s.life.Err() != nil
	if !closed {
		s.proc, s.stdin, s.conn = p, stdin, conn
	}
	s.mu.Unlock()
	if closed {
		_ = stdin.Close()
		p.Stop()
		_ = p.Stdout.Close()
		return "", errClosed
	}
	go func() {
		err := p.Wait()
		if err == nil {
			err = errors.New("exit status 0")
		}
		s.end(exited{p: p, status: err})
	}()

	ctx, cancel := s.bind(ctx)
	defer cancel()

	init, err := conn.Initialize(ctx, acpsdk.InitializeRequest{
		ProtocolVersion: acpsdk.ProtocolVersionNumber,
		ClientCapabilities: acpsdk.ClientCapabilities{
			Meta: map[string]any{"parameterizedModelPicker": true},
		},
		ClientInfo: &acpsdk.Implementation{Name: "clb", Version: clientVersion()},
	})
	if err != nil {
		return "", s.failure(conn, "initialize", err)
	}
	if offersAuth(init.AuthMethods, cursorLogin) {
		if err := s.authenticate(ctx, conn); err != nil {
			return "", err
		}
	}

	var configOptions []acpsdk.SessionConfigOption
	id := acpsdk.SessionId(opts.ResumeSessionID)
	if id == "" {
		resp, err := conn.NewSession(ctx, acpsdk.NewSessionRequest{Cwd: dir, McpServers: []acpsdk.McpServer{}})
		if err != nil {
			return "", s.failure(conn, "session/new", err)
		}
		id = resp.SessionId
		configOptions = resp.ConfigOptions
	} else {
		// The SDK returns from LoadSession only after the handlers for every
		// notification received before the response have run, so the flag
		// covers the whole replay.
		s.replaying.Store(true)
		resp, err := conn.LoadSession(ctx, acpsdk.LoadSessionRequest{SessionId: id, Cwd: dir, McpServers: []acpsdk.McpServer{}})
		s.replaying.Store(false)
		if err != nil {
			return "", s.failure(conn, "session/load", err)
		}
		configOptions = resp.ConfigOptions
	}
	s.reportModel(configOptions)
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
	return id, nil
}

func (s *Session) reportModel(options []acpsdk.SessionConfigOption) {
	mr, ok := s.sink.(backend.ModelReporter)
	if !ok {
		return
	}
	if m := sessionModel(options); m != "" {
		mr.SetModel(m)
	}
}

func sessionModel(options []acpsdk.SessionConfigOption) string {
	var model string
	var params []string
	for _, opt := range options {
		sel := opt.Select
		if sel == nil {
			continue
		}
		id := string(sel.Id)
		val := strings.TrimSpace(string(sel.CurrentValue))
		if id == "mode" || val == "" {
			continue
		}
		if id == "model" || (sel.Category != nil && *sel.Category == acpsdk.SessionConfigOptionCategoryModel) {
			model = val
			continue
		}
		if sel.Category != nil {
			switch *sel.Category {
			case acpsdk.SessionConfigOptionCategoryThoughtLevel, "model_config":
				params = append(params, id+"="+val)
			}
		}
	}
	if model == "" {
		return ""
	}
	if len(params) == 0 || strings.Contains(model, "[") {
		return model
	}
	return model + "[" + strings.Join(params, ",") + "]"
}

func (s *Session) authenticate(ctx context.Context, conn *acpsdk.ClientSideConnection) error {
	ctx, cancel := context.WithTimeoutCause(ctx, authTimeout, errAuthTimeout)
	defer cancel()
	_, err := conn.Authenticate(ctx, acpsdk.AuthenticateRequest{MethodId: cursorLogin})
	if err == nil {
		return nil
	}
	if errors.Is(context.Cause(ctx), errAuthTimeout) {
		return fmt.Errorf("agent acp did not answer authenticate within %s; run 'agent login' and try again", authTimeout)
	}
	return s.failure(conn, "authenticate", err)
}

// Prompt sends text and returns when the agent ends its turn.
func (s *Session) Prompt(ctx context.Context, text string) error {
	s.mu.Lock()
	conn, id := s.conn, s.id
	s.mu.Unlock()
	if id == "" {
		return errors.New("acp: no session; Start has not succeeded")
	}

	// session/cancel names no request, so an agent that reads it after the
	// next session/prompt cancels that turn instead. When ctx ends, cancel
	// and wait for this turn's reply before returning.
	turn, endTurn := context.WithCancel(context.Background())
	defer endTurn()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: id})
			select {
			case <-turn.Done():
			case <-time.After(cancelWait):
				endTurn()
			}
		case <-turn.Done():
		}
	}()
	bound, cancel := s.bind(turn)
	defer cancel()
	resp, err := conn.Prompt(bound, acpsdk.PromptRequest{
		SessionId: id,
		Prompt:    []acpsdk.ContentBlock{acpsdk.TextBlock(text)},
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return s.failure(conn, "session/prompt", err)
	}
	data, err := json.Marshal(map[string]acpsdk.StopReason{"stopReason": resp.StopReason})
	if err != nil {
		return err
	}
	s.sink.Event(backend.Event{Type: "turn_ended", Data: data})
	return nil
}

// Close cancels a turn in flight and terminates the agent process. It is safe
// to call more than once and concurrently with Prompt.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.end(errClosed)
		s.mu.Lock()
		p, stdin, conn, id := s.proc, s.stdin, s.conn, s.id
		s.mu.Unlock()
		if p == nil {
			return
		}
		if id != "" {
			sent := make(chan struct{})
			go func() {
				_ = conn.Cancel(context.Background(), acpsdk.CancelNotification{SessionId: id})
				close(sent)
			}()
			select {
			case <-sent:
			case <-time.After(cancelWait):
			}
		}
		_ = stdin.Close()
		p.Stop()
		_ = p.Stdout.Close()
	})
	return nil
}

func (s *Session) bind(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(s.life, func() { cancel(context.Cause(s.life)) })
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// failure explains err from a request to the agent. When the agent dies, the
// SDK reports a bare disconnect before the exit status and stderr are known.
func (s *Session) failure(conn *acpsdk.ClientSideConnection, op string, err error) error {
	select {
	case <-conn.Done():
		select {
		case <-s.life.Done():
		case <-time.After(exitGrace):
		}
	default:
	}
	cause := context.Cause(s.life)
	var exit exited
	if errors.As(cause, &exit) {
		return exit.p.Explain(fmt.Errorf("%w before it answered %s", exit, op))
	}
	if cause != nil {
		return fmt.Errorf("%s: %w", op, cause)
	}
	var reqErr *acpsdk.RequestError
	if errors.As(err, &reqErr) && reqErr.Code == authRequiredCode {
		return fmt.Errorf("agent acp %s: %w; run 'agent login' and try again", op, err)
	}
	return fmt.Errorf("agent acp %s: %w", op, err)
}

// exited ends a session whose agent process exited on its own.
type exited struct {
	p      *proc.Process
	status error
}

func (e exited) Error() string { return fmt.Sprintf("agent acp exited (%v)", e.status) }

func offersAuth(methods []acpsdk.AuthMethod, id string) bool {
	for _, m := range methods {
		if m.Agent != nil && m.Agent.Id == id {
			return true
		}
	}
	return false
}

func clientVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		return bi.Main.Version
	}
	return ""
}

type client struct{ s *Session }

var _ acpsdk.Client = client{}

func (c client) SessionUpdate(_ context.Context, n acpsdk.SessionNotification) error {
	if c.s.replaying.Load() {
		return nil
	}
	data, err := json.Marshal(n.Update)
	if err != nil {
		return err
	}
	var head struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	ev := backend.Event{Type: head.SessionUpdate, Data: data}
	if chunk := n.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
		text := chunk.Content.Text.Text
		if backend.IsTransportError(text) {
			ev.Type = backend.EventTransportError
		} else {
			ev.Text = text
		}
	}
	if upd := n.Update.ConfigOptionUpdate; upd != nil {
		c.s.reportModel(upd.ConfigOptions)
	}
	c.s.sink.Event(ev)
	return nil
}

func (c client) RequestPermission(ctx context.Context, req acpsdk.RequestPermissionRequest) (acpsdk.RequestPermissionResponse, error) {
	cancelled := acpsdk.RequestPermissionResponse{Outcome: acpsdk.RequestPermissionOutcome{
		Cancelled: &acpsdk.RequestPermissionOutcomeCancelled{},
	}}
	asked, err := permissionRequest(req)
	if err != nil {
		return cancelled, nil
	}
	ctx, cancel := c.s.bind(ctx)
	defer cancel()
	approve, err := c.s.sink.Permission(ctx, asked)
	if err != nil {
		return cancelled, nil
	}
	for _, o := range req.Options {
		if o.Kind == answerKind[approve] {
			return acpsdk.RequestPermissionResponse{Outcome: acpsdk.RequestPermissionOutcome{
				Selected: &acpsdk.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
			}}, nil
		}
	}
	return cancelled, nil
}

func permissionRequest(req acpsdk.RequestPermissionRequest) (backend.PermissionRequest, error) {
	toolCall, err := json.Marshal(req.ToolCall)
	if err != nil {
		return backend.PermissionRequest{}, err
	}
	var title string
	if req.ToolCall.Title != nil {
		title = *req.ToolCall.Title
	}
	var kind string
	if req.ToolCall.Kind != nil {
		kind = string(*req.ToolCall.Kind)
	}
	var options json.RawMessage
	if len(req.Options) > 0 {
		options, err = json.Marshal(req.Options)
		if err != nil {
			return backend.PermissionRequest{}, err
		}
	}
	command, url := commandAndURL(req.ToolCall.RawInput)
	return backend.PermissionRequest{
		Title:      title,
		ToolCallID: string(req.ToolCall.ToolCallId),
		Kind:       kind,
		Command:    command,
		URL:        url,
		ToolCall:   toolCall,
		Options:    options,
	}, nil
}

func commandAndURL(raw any) (command, url string) {
	fields := rawObject(raw)
	if fields == nil {
		return "", ""
	}
	command, _ = fields["command"].(string)
	url, _ = fields["url"].(string)
	return command, url
}

func rawObject(raw any) map[string]any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		return decodeObject([]byte(v))
	case []byte:
		return decodeObject(v)
	case string:
		return decodeObject([]byte(v))
	default:
		return nil
	}
}

func decodeObject(b []byte) map[string]any {
	if len(b) == 0 {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(b, &fields) != nil {
		return nil
	}
	return fields
}

func (client) ReadTextFile(context.Context, acpsdk.ReadTextFileRequest) (acpsdk.ReadTextFileResponse, error) {
	return acpsdk.ReadTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsReadTextFile)
}

func (client) WriteTextFile(context.Context, acpsdk.WriteTextFileRequest) (acpsdk.WriteTextFileResponse, error) {
	return acpsdk.WriteTextFileResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodFsWriteTextFile)
}

func (client) CreateTerminal(context.Context, acpsdk.CreateTerminalRequest) (acpsdk.CreateTerminalResponse, error) {
	return acpsdk.CreateTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalCreate)
}

func (client) KillTerminal(context.Context, acpsdk.KillTerminalRequest) (acpsdk.KillTerminalResponse, error) {
	return acpsdk.KillTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalKill)
}

func (client) TerminalOutput(context.Context, acpsdk.TerminalOutputRequest) (acpsdk.TerminalOutputResponse, error) {
	return acpsdk.TerminalOutputResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalOutput)
}

func (client) ReleaseTerminal(context.Context, acpsdk.ReleaseTerminalRequest) (acpsdk.ReleaseTerminalResponse, error) {
	return acpsdk.ReleaseTerminalResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalRelease)
}

func (client) WaitForTerminalExit(context.Context, acpsdk.WaitForTerminalExitRequest) (acpsdk.WaitForTerminalExitResponse, error) {
	return acpsdk.WaitForTerminalExitResponse{}, acpsdk.NewMethodNotFound(acpsdk.ClientMethodTerminalWaitForExit)
}
