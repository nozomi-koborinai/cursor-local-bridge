package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type printOptions struct {
	model, format, resume, workspace string
	trust                            bool
}

func createChat() error {
	c, err := newChat("")
	if err != nil {
		return err
	}
	fmt.Println(c.ID)
	// The real CLI keeps running after it prints the ID, so callers must not
	// wait for it to exit.
	time.Sleep(2 * time.Minute)
	return nil
}

func runPrint(o printOptions, prompt string) error {
	if !o.trust {
		refuseUntrusted(o.workspace)
	}
	if o.format != "stream-json" {
		return fmt.Errorf("unsupported --output-format %q", o.format)
	}
	if loggedOut() {
		fmt.Fprintln(os.Stderr, "Error: Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY environment variable.")
		os.Exit(1)
	}
	var (
		c   *chat
		err error
	)
	if o.resume != "" {
		c, err = loadChat(o.resume)
	} else {
		c, err = newChat(o.workspace)
	}
	if err != nil {
		return err
	}

	out := json.NewEncoder(os.Stdout)
	emit := func(line map[string]any) error {
		line["session_id"] = c.ID
		return out.Encode(line)
	}
	assistant := func(text string) error {
		return emit(map[string]any{
			"type": "assistant",
			"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "text", "text": text}},
			},
		})
	}

	if err := emit(map[string]any{"type": "system", "subtype": "init", "model": o.model, "cwd": o.workspace}); err != nil {
		return err
	}
	if err := emit(map[string]any{"type": "thinking", "subtype": "delta", "text": "thinking"}); err != nil {
		return err
	}
	if err := emit(map[string]any{"type": "thinking", "subtype": "completed"}); err != nil {
		return err
	}
	var reply string
	switch scriptFor(prompt) {
	case scriptCrash:
		if err := assistant("about to crash"); err != nil {
			return err
		}
		crash()
	case scriptSlow:
		for n := 1; n <= 2400; n++ {
			reply = fmt.Sprintf("tick %d", n)
			if err := assistant(reply); err != nil {
				return err
			}
			time.Sleep(50 * time.Millisecond)
		}
	default:
		reply = "echo: " + prompt
		if err := assistant(reply); err != nil {
			return err
		}
	}
	if err := c.record(prompt, reply); err != nil {
		return err
	}
	return emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply})
}

func refuseUntrusted(dir string) {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	fmt.Fprintf(os.Stderr, `⚠ Workspace Trust Required

  Cursor Agent can execute code and access files in this directory.
  Do you trust the contents of this directory?

    %s

  To proceed, you can either:
    • Run 'agent' interactively to decide
    • Pass --trust, --yolo, or -f if you trust this directory
`, dir)
	os.Exit(1)
}
