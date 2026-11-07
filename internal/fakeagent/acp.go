package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

const cursorLogin = "cursor_login"

type fakeACP struct {
	mu            sync.Mutex
	conn          *acp.AgentSideConnection
	authenticated bool
	chats         map[acp.SessionId]*chat
}

var (
	_ acp.Agent       = (*fakeACP)(nil)
	_ acp.AgentLoader = (*fakeACP)(nil)
)

func runACP() error {
	a := &fakeACP{chats: map[acp.SessionId]*chat{}}
	conn := acp.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	<-conn.Done()
	return nil
}

func (a *fakeACP) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:         true,
			McpCapabilities:     acp.McpCapabilities{Http: true, Sse: true},
			PromptCapabilities:  acp.PromptCapabilities{Image: true},
			SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}},
		},
		AuthMethods: []acp.AuthMethod{{Agent: &acp.AuthMethodAgent{
			Id:          cursorLogin,
			Name:        "Cursor Login",
			Description: acp.Ptr("Authenticate using existing Cursor login credentials. Run 'agent login' first if not logged in."),
		}}},
	}, nil
}

func (a *fakeACP) Authenticate(ctx context.Context, req acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	if loggedOut() {
		// The real CLI never answers authenticate when it has no credentials.
		<-ctx.Done()
		return acp.AuthenticateResponse{}, ctx.Err()
	}
	if req.MethodId != cursorLogin {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": req.MethodId})
	}
	a.mu.Lock()
	a.authenticated = true
	a.mu.Unlock()
	return acp.AuthenticateResponse{}, nil
}

func (a *fakeACP) requireAuth() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.authenticated {
		return nil
	}
	return acp.NewAuthRequired(map[string]any{
		"message": "Authentication required. Please run 'agent login' first, then call authenticate() with methodId 'cursor_login'.",
	})
}

func (a *fakeACP) NewSession(_ context.Context, req acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if err := a.requireAuth(); err != nil {
		return acp.NewSessionResponse{}, err
	}
	c, err := newChat(req.Cwd)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	a.mu.Lock()
	a.chats[acp.SessionId(c.ID)] = c
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(c.ID)}, nil
}

func (a *fakeACP) LoadSession(ctx context.Context, req acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	if err := a.requireAuth(); err != nil {
		return acp.LoadSessionResponse{}, err
	}
	c, err := loadChat(string(req.SessionId))
	if err != nil {
		return acp.LoadSessionResponse{}, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	for _, t := range c.Turns {
		if err := a.update(ctx, req.SessionId, acp.UpdateUserMessageText(t.Prompt)); err != nil {
			return acp.LoadSessionResponse{}, err
		}
		if err := a.update(ctx, req.SessionId, acp.UpdateAgentMessageText(t.Reply)); err != nil {
			return acp.LoadSessionResponse{}, err
		}
	}
	a.mu.Lock()
	a.chats[req.SessionId] = c
	a.mu.Unlock()
	return acp.LoadSessionResponse{}, nil
}

func (a *fakeACP) Prompt(ctx context.Context, req acp.PromptRequest) (acp.PromptResponse, error) {
	a.mu.Lock()
	c := a.chats[req.SessionId]
	a.mu.Unlock()
	if c == nil {
		return acp.PromptResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown session " + string(req.SessionId)})
	}
	var prompt strings.Builder
	for _, b := range req.Prompt {
		if b.Text != nil {
			prompt.WriteString(b.Text.Text)
		}
	}

	var (
		reply string
		err   error
	)
	switch scriptFor(prompt.String()) {
	case scriptCrash:
		_ = a.update(ctx, req.SessionId, acp.UpdateAgentMessageText("about to crash"))
		crash()
	case scriptSlow:
		reply, err = a.slow(ctx, req.SessionId)
	case scriptPermission:
		reply, err = a.askPermission(ctx, req.SessionId)
	default:
		reply, err = a.echo(ctx, req.SessionId, prompt.String())
	}
	if rerr := c.record(prompt.String(), reply); rerr != nil {
		return acp.PromptResponse{}, rerr
	}
	if ctx.Err() != nil {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	if err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *fakeACP) echo(ctx context.Context, sid acp.SessionId, prompt string) (string, error) {
	// The real CLI sends both of these on a turn, and its first tool_call
	// can arrive with an empty rawInput that a later update fills in.
	if err := a.update(ctx, sid, acp.UpdateAgentThoughtText("thinking")); err != nil {
		return "", err
	}
	commands := acp.SessionUpdate{AvailableCommandsUpdate: &acp.SessionAvailableCommandsUpdate{
		AvailableCommands: []acp.AvailableCommand{{
			Name:        "create_plan",
			Description: "Create a plan",
		}},
	}}
	if err := a.update(ctx, sid, commands); err != nil {
		return "", err
	}
	const toolCallID = "read-1"
	err := a.update(ctx, sid, acp.StartToolCall(toolCallID, "Read fake file",
		acp.WithStartKind(acp.ToolKindRead),
		acp.WithStartStatus(acp.ToolCallStatusCompleted),
		acp.WithStartRawInput(map[string]any{}),
	))
	if err != nil {
		return "", err
	}
	err = a.update(ctx, sid, acp.UpdateToolCall(toolCallID,
		acp.WithUpdateRawInput(map[string]any{"path": "README.md"}),
	))
	if err != nil {
		return "", err
	}
	reply := "echo: " + prompt
	runes := []rune(reply)
	const parts = 3
	for i := range parts {
		if i > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		chunk := string(runes[i*len(runes)/parts : (i+1)*len(runes)/parts])
		if err := a.update(ctx, sid, acp.UpdateAgentMessageText(chunk)); err != nil {
			return "", err
		}
	}
	return reply, nil
}

func (a *fakeACP) askPermission(ctx context.Context, sid acp.SessionId) (string, error) {
	options := []acp.PermissionOption{
		{OptionId: "allow-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
		{OptionId: "allow-always", Name: "Always allow", Kind: acp.PermissionOptionKindAllowAlways},
		{OptionId: "reject-once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
	}
	resp, err := a.connection().RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sid,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "perm-1",
			Title:      acp.Ptr("Run fake command"),
			Kind:       acp.Ptr(acp.ToolKindExecute),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			RawInput:   map[string]any{"command": "true"},
		},
		Options: options,
	})
	if err != nil {
		return "", err
	}
	reply := "permission: cancelled"
	if sel := resp.Outcome.Selected; sel != nil {
		reply = ""
		for _, o := range options {
			if o.OptionId != sel.OptionId {
				continue
			}
			switch o.Kind {
			case acp.PermissionOptionKindAllowOnce, acp.PermissionOptionKindAllowAlways:
				reply = "permission: approved"
			case acp.PermissionOptionKindRejectOnce, acp.PermissionOptionKindRejectAlways:
				reply = "permission: denied"
			}
		}
		if reply == "" {
			return "", fmt.Errorf("unknown permission option %q", sel.OptionId)
		}
	}
	return reply, a.update(ctx, sid, acp.UpdateAgentMessageText(reply))
}

func (a *fakeACP) slow(ctx context.Context, sid acp.SessionId) (string, error) {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	limit := time.After(2 * time.Minute)
	var reply strings.Builder
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return reply.String(), nil
		case <-limit:
			return reply.String(), nil
		case <-tick.C:
			chunk := fmt.Sprintf("tick %d\n", n)
			reply.WriteString(chunk)
			if err := a.update(ctx, sid, acp.UpdateAgentMessageText(chunk)); err != nil {
				return reply.String(), err
			}
		}
	}
}

func (a *fakeACP) connection() *acp.AgentSideConnection {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

func (a *fakeACP) update(ctx context.Context, sid acp.SessionId, u acp.SessionUpdate) error {
	return a.connection().SessionUpdate(ctx, acp.SessionNotification{SessionId: sid, Update: u})
}

func (a *fakeACP) Cancel(context.Context, acp.CancelNotification) error { return nil }

func (a *fakeACP) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

func (a *fakeACP) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}

func (a *fakeACP) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}

func (a *fakeACP) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}

func (a *fakeACP) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}

func (a *fakeACP) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}
