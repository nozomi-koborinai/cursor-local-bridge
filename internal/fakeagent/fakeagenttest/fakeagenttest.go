// Package fakeagenttest builds the fake Cursor CLI and reads what it recorded.
package fakeagenttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Invocation is one start of the fake agent.
type Invocation struct {
	Pid  int      `json:"pid"`
	Args []string `json:"args"`
}

// Build compiles the fake agent into dir as "agent" and returns its path.
func Build(dir string) (string, error) {
	path := filepath.Join(dir, "agent")
	out, err := exec.Command("go", "build", "-o", path, "github.com/nozomi-koborinai/cursor-local-bridge/internal/fakeagent").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build fake agent: %w\n%s", err, out)
	}
	return path, nil
}

// Invocations returns the starts recorded in stateDir, oldest first.
func Invocations(stateDir string) ([]Invocation, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, "invocations.log"))
	if err != nil {
		return nil, err
	}
	var all []Invocation
	for line := range bytes.Lines(b) {
		var inv Invocation
		if err := json.Unmarshal(line, &inv); err != nil {
			return nil, err
		}
		all = append(all, inv)
	}
	return all, nil
}
