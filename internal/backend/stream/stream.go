// Package stream drives the Cursor CLI in print mode, one
// "agent -p --output-format stream-json" process per prompt, continuing the
// chat with --resume.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/proc"
)

const createChatTimeout = 30 * time.Second

var errClosed = errors.New("stream session closed")

// Session is one chat driven through print mode.
type Session struct {
	agentPath string
	sink      backend.Sink
	// trust passes --trust to agent -p. --force, --yolo, and -f also get past
	// the trust prompt, and they turn approvals off, so they are not used.
	trust bool

	dir, model, chatID string

	mu      sync.Mutex
	closed  bool
	running *proc.Process
}

func New(agentPath string, sink backend.Sink, trust bool) *Session {
	return &Session{agentPath: agentPath, sink: sink, trust: trust}
}

// Start creates a chat with "agent create-chat", or adopts
// opts.ResumeSessionID, and returns the chat ID. It starts no long-lived
// process.
func (s *Session) Start(ctx context.Context, opts backend.StartOptions) (string, error) {
	dir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return "", err
	}
	s.dir, s.model, s.chatID = dir, opts.Model, opts.ResumeSessionID
	if s.chatID == "" {
		if s.chatID, err = s.createChat(ctx); err != nil {
			return "", err
		}
	}
	return s.chatID, nil
}

// createChat returns the first line "agent create-chat" prints. The CLI keeps
// running after printing it, so the process is stopped instead of awaited.
func (s *Session) createChat(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, createChatTimeout)
	defer cancel()
	cmd := exec.Command(s.agentPath, "create-chat")
	cmd.Dir = s.dir
	p, err := s.run(cmd)
	if err != nil {
		return "", err
	}
	defer s.finish(p)

	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(p.Stdout).ReadString('\n')
		line <- strings.TrimSpace(l)
	}()
	select {
	case id := <-line:
		if id != "" {
			return id, nil
		}
		if s.isClosed() {
			return "", errClosed
		}
		p.Stop()
		if err := p.Wait(); err != nil {
			return "", p.Explain(fmt.Errorf("agent create-chat printed no chat ID (%w)", err))
		}
		return "", p.Explain(errors.New("agent create-chat printed no chat ID"))
	case <-ctx.Done():
		return "", fmt.Errorf("agent create-chat: %w", ctx.Err())
	}
}

// Prompt runs one print-mode process for text, forwards each stream-json line
// as an event, and returns when the process exits.
func (s *Session) Prompt(ctx context.Context, text string) error {
	if s.chatID == "" {
		return errors.New("stream: no chat; Start has not succeeded")
	}
	args := []string{"-p"}
	if s.trust {
		args = append(args, "--trust")
	}
	args = append(args, "--output-format", "stream-json")
	if s.model != "" {
		args = append(args, "--model", s.model)
	}
	args = append(args, "--resume", s.chatID, "--workspace", s.dir, "--", text)
	cmd := exec.Command(s.agentPath, args...)
	cmd.Dir = s.dir
	p, err := s.run(cmd)
	if err != nil {
		return err
	}
	defer s.finish(p)
	// Stopping the group once the agent exits also ends children that would
	// otherwise hold stdout open.
	go func() {
		select {
		case <-ctx.Done():
		case <-p.Done():
		}
		p.Stop()
	}()

	res, readErr := s.forward(p.Stdout)
	exitErr := p.Wait()
	switch {
	case s.isClosed():
		return errClosed
	case ctx.Err() != nil:
		return ctx.Err()
	case exitErr != nil:
		return p.Explain(fmt.Errorf("agent -p failed (%w)", exitErr))
	case readErr != nil:
		return fmt.Errorf("read agent -p output: %w", readErr)
	case res == nil:
		return p.Explain(errors.New("agent -p exited without a result line"))
	case res.IsError:
		return p.Explain(fmt.Errorf("agent -p reported an error: %s", res.Result))
	}
	return nil
}

// Close terminates a print-mode process in flight. It is safe to call more
// than once and concurrently with Prompt.
func (s *Session) Close() error {
	s.mu.Lock()
	s.closed = true
	p := s.running
	s.mu.Unlock()
	if p != nil {
		p.Stop()
	}
	return nil
}

func (s *Session) run(cmd *exec.Cmd) (*proc.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errClosed
	}
	p, err := proc.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}
	s.running = p
	return p, nil
}

func (s *Session) finish(p *proc.Process) {
	p.Stop()
	_ = p.Stdout.Close()
	s.mu.Lock()
	s.running = nil
	s.mu.Unlock()
}

func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type result struct {
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

func (s *Session) forward(stdout io.Reader) (*result, error) {
	var last *result
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if ev, res, ok := parseLine(line); ok {
			s.sink.Event(ev)
			if res != nil {
				last = res
			}
		}
		if errors.Is(err, io.EOF) {
			return last, nil
		}
		if err != nil {
			return last, err
		}
	}
}

func parseLine(line []byte) (backend.Event, *result, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return backend.Event{}, nil, false
	}
	var head struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &head) != nil || head.Type == "" {
		data, _ := json.Marshal(string(line))
		return backend.Event{Type: "stdout", Data: data}, nil, true
	}
	ev := backend.Event{Type: head.Type, Data: line}
	var res *result
	switch head.Type {
	case "assistant":
		ev.Text = assistantText(head.Message)
	case "result":
		res = &result{}
		if json.Unmarshal(line, res) != nil {
			res = &result{IsError: true, Result: "unreadable result line"}
		}
	}
	return ev, res, true
}

func assistantText(message json.RawMessage) string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(message, &m) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}
