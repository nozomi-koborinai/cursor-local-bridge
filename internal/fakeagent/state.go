package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type chat struct {
	ID    string `json:"id"`
	Cwd   string `json:"cwd,omitempty"`
	Turns []turn `json:"turns"`
}

type turn struct {
	Prompt string `json:"prompt"`
	Reply  string `json:"reply"`
}

func stateDir() string {
	if dir := os.Getenv("FAKEAGENT_STATE_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "fakeagent")
}

func logInvocation(argv []string) error {
	line, err := json.Marshal(struct {
		Pid  int      `json:"pid"`
		Args []string `json:"args"`
	}{os.Getpid(), argv})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(stateDir(), "invocations.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func chatPath(id string) (string, error) {
	if id == "" || filepath.Base(id) != id {
		return "", fmt.Errorf("invalid chat id %q", id)
	}
	return filepath.Join(stateDir(), "chat-"+id+".json"), nil
}

func newChat(cwd string) (*chat, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	c := &chat{ID: fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), Cwd: cwd}
	return c, c.save()
}

func loadChat(id string) (*chat, error) {
	path, err := chatPath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chat %s not found: %w", id, err)
	}
	var c chat
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *chat) record(prompt, reply string) error {
	c.Turns = append(c.Turns, turn{Prompt: prompt, Reply: reply})
	return c.save()
}

func (c *chat) save() error {
	path, err := chatPath(c.ID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
