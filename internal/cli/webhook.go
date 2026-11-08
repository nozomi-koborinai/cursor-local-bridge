package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

// valueInput is one secret the setup command reads. The source is the env
// var when it is non-empty, and otherwise a hidden prompt or a stdin line.
type valueInput struct {
	env    string
	prompt string
	empty  string
	after  func(string) string
}

var webhookInputs = []valueInput{
	{
		env:    "CLB_WEBHOOK_URL",
		prompt: "Webhook URL (input hidden): ",
		empty:  "webhook URL is empty",
	},
	{
		env:    "CLB_WEBHOOK_KEY",
		prompt: "Key (input hidden): ",
		empty:  "webhook key is empty",
		after:  stripBearerPrefix,
	},
}

type webhookSetupResult struct {
	Config        string             `json:"config"`
	WebhookHost   string             `json:"webhookHost"`
	SecretCreated bool               `json:"secretCreated"`
	Test          *webhookTestResult `json:"test,omitempty"`
}

type webhookTestResult struct {
	HTTPStatus int `json:"httpStatus"`
}

func (a *app) webhookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "webhook",
		Short: "Set up or test the webhook that receives run status",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(a.webhookSetupCmd(), a.webhookTestCmd())
	return cmd
}

func (a *app) webhookSetupCmd() *cobra.Command {
	var test bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Save the webhook URL and key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runWebhookSetup(cmd, test)
		},
	}
	cmd.Flags().BoolVar(&test, "test", false, "send one test delivery after saving")
	return cmd
}

func (a *app) webhookTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Send one signed test delivery",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := a.deliverTest(cmd)
			if err != nil {
				return err
			}
			return writeIndented(cmd.OutOrStdout(), webhookTestResult{HTTPStatus: status})
		},
	}
}

func (a *app) runWebhookSetup(cmd *cobra.Command, test bool) error {
	values, err := readWebhookValues(cmd, webhookInputs)
	if err != nil {
		return err
	}
	rawURL, key := values[0], values[1]
	if err := webhook.CheckHeaders(map[string]string{"Authorization": "Bearer " + key}); err != nil {
		return err
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return errors.New("webhook.url must be an http or https URL with a host")
	}
	home, err := a.absHome()
	if err != nil {
		return err
	}
	created, err := config.SaveWebhook(home, rawURL, "Bearer "+key)
	if err != nil {
		return err
	}
	result := webhookSetupResult{
		Config:        filepath.Join(home, "config.json"),
		WebhookHost:   parsed.Host,
		SecretCreated: created,
	}
	if test {
		status, err := a.deliverTest(cmd)
		if err != nil {
			return fmt.Errorf("saved %s, but the test delivery failed: %w", result.Config, err)
		}
		result.Test = &webhookTestResult{HTTPStatus: status}
	}
	return writeIndented(cmd.OutOrStdout(), result)
}

func (a *app) deliverTest(cmd *cobra.Command) (int, error) {
	cfg, err := a.config()
	if err != nil {
		return 0, err
	}
	sender := newSender(cfg.Webhook)
	if sender == nil {
		return 0, errors.New("webhook.url is empty; run clb webhook setup")
	}
	status, err := sender.Send(cmd.Context(), webhook.Payload{
		Event:     webhook.EventStatusChange,
		Timestamp: time.Now().UTC(),
		RunID:     "test",
		Status:    "test",
		Summary:   "Test delivery from clb webhook test. No run changed.",
	})
	return status, err
}

func readWebhookValues(cmd *cobra.Command, inputs []valueInput) ([]string, error) {
	stdin := cmd.InOrStdin()
	tty, terminal := terminalFile(stdin)
	var lines *bufio.Reader
	if !terminal {
		lines = bufio.NewReader(stdin)
	}
	if terminal && envMissing(inputs) {
		// ReadPassword restores the terminal in a defer. A signal kills the
		// process before that defer runs, which leaves the terminal without echo.
		disarm, err := armTerminalRestore(int(tty.Fd()), cmd.ErrOrStderr())
		if err != nil {
			return nil, err
		}
		defer disarm()
	}
	out := make([]string, len(inputs))
	for i, in := range inputs {
		value, err := readWebhookValue(cmd, tty, lines, in)
		if err != nil {
			return nil, err
		}
		if in.after != nil {
			value = in.after(value)
		}
		if value == "" {
			return nil, errors.New(in.empty)
		}
		out[i] = value
	}
	return out, nil
}

func envMissing(inputs []valueInput) bool {
	for _, in := range inputs {
		if os.Getenv(in.env) == "" {
			return true
		}
	}
	return false
}

func readWebhookValue(cmd *cobra.Command, tty *os.File, lines *bufio.Reader, in valueInput) (string, error) {
	if v := os.Getenv(in.env); v != "" {
		return strings.TrimSpace(v), nil
	}
	if tty != nil {
		return readHidden(cmd, tty, in.prompt)
	}
	line, err := lines.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func readHidden(cmd *cobra.Command, tty *os.File, prompt string) (string, error) {
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), prompt); err != nil {
		return "", err
	}
	buf, err := term.ReadPassword(int(tty.Fd()))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf)), nil
}

func stripBearerPrefix(s string) string {
	const prefix = "bearer "
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return strings.TrimSpace(s[len(prefix):])
	}
	return s
}

func terminalFile(r io.Reader) (*os.File, bool) {
	f, ok := r.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil, false
	}
	return f, true
}

func armTerminalRestore(fd int, stderr io.Writer) (func(), error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})
	go func() {
		select {
		case <-sig:
			select {
			case <-stop:
				return
			default:
			}
			_ = term.Restore(fd, state)
			_, _ = fmt.Fprintln(stderr)
			os.Exit(130)
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		signal.Stop(sig)
	}, nil
}
