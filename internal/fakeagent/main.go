// Command fakeagent stands in for the Cursor CLI "agent" binary in tests. It
// speaks ACP in "acp" mode, prints a chat ID in "create-chat" mode, and writes
// stream-json in "-p" mode. Markers in the prompt pick a script; see scriptFor.
//
// Chats persist in $FAKEAGENT_STATE_DIR, where every start also appends its
// argv to invocations.log. FAKEAGENT_LOGGED_OUT=1 mimics a CLI that has no
// "agent login" credentials. "--model invalid-model acp" mimics a CLI that
// rejects the model: it exits during authenticate.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if err := logInvocation(argv); err != nil {
		return err
	}
	fs := flag.NewFlagSet("fakeagent", flag.ContinueOnError)
	var o printOptions
	printMode := fs.Bool("p", false, "print mode")
	fs.StringVar(&o.model, "model", "", "model")
	fs.StringVar(&o.format, "output-format", "", "print output format")
	fs.StringVar(&o.resume, "resume", "", "chat to continue")
	fs.StringVar(&o.workspace, "workspace", "", "workspace directory")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	args := fs.Args()
	switch {
	case *printMode:
		return runPrint(o, strings.Join(args, " "))
	case len(args) == 1 && args[0] == "acp":
		return runACP(o.model)
	case len(args) == 1 && args[0] == "create-chat":
		return createChat()
	}
	return errors.New("usage: fakeagent [--model M] acp | create-chat | -p --output-format stream-json [--model M] [--resume ID] [--workspace DIR] [--] PROMPT")
}

func loggedOut() bool { return os.Getenv("FAKEAGENT_LOGGED_OUT") == "1" }

type script int

const (
	scriptEcho script = iota
	scriptPermission
	scriptSlow
	scriptCrash
	scriptBackground
)

func scriptFor(prompt string) script {
	switch {
	case strings.Contains(prompt, "[crash]"):
		return scriptCrash
	case strings.Contains(prompt, "[slow]"):
		return scriptSlow
	case strings.Contains(prompt, "[permission]"):
		return scriptPermission
	case strings.Contains(prompt, "[background]"):
		return scriptBackground
	}
	return scriptEcho
}

func crash() {
	fmt.Fprintln(os.Stderr, "fakeagent: crashing on purpose")
	os.Exit(3)
}
