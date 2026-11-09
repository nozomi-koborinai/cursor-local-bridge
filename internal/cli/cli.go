// Package cli is the clb command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/acp"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/backend/stream"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/config"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/daemon"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/keepawake"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/store"
	"github.com/nozomi-koborinai/cursor-local-bridge/internal/webhook"
)

// Execute runs the command line and returns the process exit code.
func Execute(version string) int {
	if err := newRoot(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "clb:", err)
		return 1
	}
	return 0
}

type app struct {
	home    string
	homeErr error
}

func (a *app) absHome() (string, error) {
	if a.home == "" {
		return "", fmt.Errorf("find the clb home: %w; pass --home", a.homeErr)
	}
	return filepath.Abs(a.home)
}

func (a *app) config() (config.Config, error) {
	home, err := a.absHome()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(home)
}

func newRoot(version string) *cobra.Command {
	a := &app{}
	a.home, a.homeErr = config.DefaultHome()
	root := &cobra.Command{
		Use:           "clb",
		Short:         "Run local Cursor CLI agents with a cloud-agent-shaped lifecycle",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.home, "home", a.home, "directory with the config, database, socket, logs, and worktrees")
	root.AddCommand(
		a.daemonCmd(),
		a.launchCmd(),
		a.runCmd("status", "Print a run", ipc.MethodStatus),
		a.followUpCmd(),
		a.runCmd("stop", "Stop a run. A worktree run loses its worktree and keeps its branch. A run without a worktree keeps its directory", ipc.MethodStop),
		a.cleanCmd(),
		a.permissionAnswerCmd("approve", "Approve a run's pending permission(s)", ipc.MethodApprove),
		a.permissionAnswerCmd("deny", "Deny a run's pending permission(s)", ipc.MethodDeny),
		a.permissionsCmd(),
		a.listCmd(),
		a.logsCmd(),
		a.webhookCmd(),
	)
	return root
}

func (a *app) daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the daemon that supervises runs, until SIGINT or SIGTERM",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			// A second signal kills the daemon if shutdown hangs.
			context.AfterFunc(ctx, stop)
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			slog.SetDefault(log)
			return runDaemon(ctx, cfg, log)
		},
	}
}

func runDaemon(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	ln, err := ipc.Listen(cfg.SocketPath)
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath()), 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	home := cfg.Home
	d := daemon.New(daemon.Options{
		Store: st,
		Backends: daemon.Backends{
			backend.ACP:   func(s backend.Sink) daemon.Session { return acp.New(cfg.AgentPath, s) },
			backend.Print: func(s backend.Sink) daemon.Session { return stream.New(cfg.AgentPath, s, cfg.TrustWorktree) },
		},
		Config: cfg,
		Webhook: func() (*webhook.Sender, error) {
			loaded, err := config.Load(home)
			if err != nil {
				return nil, err
			}
			return newSender(loaded.Webhook), nil
		},
		KeepAwake: keepawake.Default(),
		Logger:    log,
	})
	if err := d.Recover(ctx); err != nil {
		return err
	}
	log.Info("daemon listening", "socket", cfg.SocketPath, "home", cfg.Home)
	if err := d.Serve(ctx, ln); err != nil {
		return err
	}
	log.Info("daemon stopped")
	return nil
}

func newSender(w config.Webhook) *webhook.Sender {
	if w.URL == "" {
		return nil
	}
	hdr := make(http.Header, len(w.Headers))
	for k, v := range w.Headers {
		hdr.Set(k, v)
	}
	var reminder time.Duration
	if d, err := w.ReminderDuration(); err == nil {
		reminder = d
	}
	return &webhook.Sender{
		URL:      w.URL,
		Secret:   []byte(w.Secret.Reveal()),
		Header:   hdr,
		Client:   &http.Client{Timeout: 10 * time.Second},
		Backoff:  time.Second,
		Attempts: 3,
		Reminder: reminder,
	}
}

func (a *app) launchCmd() *cobra.Command {
	var repo string
	req := ipc.Request{Method: ipc.MethodLaunch}
	cmd := &cobra.Command{
		Use:   "launch [flags] PROMPT",
		Short: "Start a run on a new branch and worktree of a repository",
		Long:  "Start a run on a new branch and worktree of a repository. Pass --no-worktree to run in --repo directly, with no worktree and no branch. A PROMPT of - reads the prompt from stdin.",
		Args:  cobra.ExactArgs(1),
	}
	format := addOutputFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		var err error
		if req.Prompt, err = promptArg(cmd, args[0]); err != nil {
			return err
		}
		if req.Repo, err = filepath.Abs(repo); err != nil {
			return err
		}
		return a.printRun(cmd, req, *format)
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "repository or directory to work in")
	cmd.Flags().BoolVar(&req.NoWorktree, "no-worktree", false, "run directly in --repo without creating a git worktree or branch")
	cmd.Flags().StringVar(&req.Ref, "ref", "", "commit, branch, or tag to start the run's branch from (default HEAD)")
	cmd.Flags().StringVar(&req.Model, "model", "", "model for the agent (default from the config)")
	cmd.Flags().StringVar(&req.Backend, "backend", "", "acp or print (default from the config)")
	return cmd
}

func (a *app) cleanCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:     "clean",
		Aliases: []string{"gc"},
		Short:   "Remove worktrees and branches of finished, failed, and stopped runs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var clean *ipc.CleanResult
			err := a.call(cmd, ipc.Request{Method: ipc.MethodClean, DryRun: dryRun}, func(r ipc.Response) error {
				clean = r.Clean
				return nil
			})
			if err != nil {
				return err
			}
			if clean == nil {
				return errors.New("the daemon replied without a clean result")
			}
			return writeIndented(cmd.OutOrStdout(), clean)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be removed without deleting anything")
	return cmd
}

func (a *app) followUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "followup ID PROMPT",
		Short: "Send a follow-up prompt, queued behind the turn in flight or resuming a finished or failed run",
		Long:  "Send a follow-up prompt, queued behind the turn in flight or resuming a finished or failed run. A PROMPT of - reads the prompt from stdin.",
		Args:  cobra.ExactArgs(2),
	}
	format := addOutputFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		prompt, err := promptArg(cmd, args[1])
		if err != nil {
			return err
		}
		return a.printRun(cmd, ipc.Request{Method: ipc.MethodFollowUp, RunID: args[0], Prompt: prompt}, *format)
	}
	return cmd
}

func (a *app) runCmd(name, short string, method ipc.Method) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name + " ID",
		Short: short,
		Args:  cobra.ExactArgs(1),
	}
	format := addOutputFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return a.printRun(cmd, ipc.Request{Method: method, RunID: args[0]}, *format)
	}
	return cmd
}

func (a *app) permissionAnswerCmd(name, short string, method ipc.Method) *cobra.Command {
	var (
		permissionIDs []string
		idAliases     []string
		all           bool
		option        string
		always        bool
		list          bool
	)
	cmd := &cobra.Command{
		Use:   name + " [flags] ID [PERMISSION_ID]",
		Short: short,
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids := append([]string(nil), permissionIDs...)
			ids = append(ids, idAliases...)
			if len(args) == 2 {
				ids = append(ids, args[1])
			}
			if list {
				if len(ids) > 0 || all || option != "" || always {
					return errors.New("invalid: --list cannot be combined with permission target or option flags")
				}
				return a.printPermissions(cmd, args[0])
			}
			if always {
				if option != "" {
					return errors.New("invalid: cannot combine --always with --option")
				}
				if method == ipc.MethodApprove {
					option = "allow-always"
				} else {
					option = "reject-always"
				}
			}
			if all && len(ids) > 0 {
				return errors.New("invalid: cannot combine --all with a permission id")
			}
			return a.printRun(cmd, ipc.Request{
				Method:        method,
				RunID:         args[0],
				PermissionIDs: ids,
				All:           all,
				Option:        option,
			}, outputJSON)
		},
	}
	cmd.Flags().StringSliceVarP(&permissionIDs, "permission", "p", nil, "pending permission id to answer (default oldest)")
	cmd.Flags().StringSliceVar(&idAliases, "id", nil, "pending permission id to answer (alias for --permission)")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "answer every pending permission on the run")
	cmd.Flags().StringVarP(&option, "option", "o", "", "permission option to select (allow-once, allow-always, reject, or optionId)")
	cmd.Flags().BoolVar(&always, "always", false, "select the standing allow-always or reject-always option")
	cmd.Flags().BoolVarP(&list, "list", "l", false, "print the run's pending permissions without answering")
	return cmd
}

func (a *app) permissionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "permissions ID",
		Short: "Print a run's pending permissions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.printPermissions(cmd, args[0])
		},
	}
}

func (a *app) printPermissions(cmd *cobra.Command, runID string) error {
	var run *ipc.Run
	err := a.call(cmd, ipc.Request{Method: ipc.MethodStatus, RunID: runID}, func(r ipc.Response) error {
		run = r.Run
		return nil
	})
	if err != nil {
		return err
	}
	if run == nil {
		return errors.New("the daemon replied without a run")
	}
	perms := run.PendingPermissions
	if perms == nil {
		perms = []ipc.Permission{}
	}
	return writeIndented(cmd.OutOrStdout(), perms)
}

func (a *app) listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print every run, newest first",
		Args:  cobra.NoArgs,
	}
	format := addOutputFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		runs := []ipc.Run{}
		err := a.call(cmd, ipc.Request{Method: ipc.MethodList}, func(r ipc.Response) error {
			runs = append(runs, r.Runs...)
			return nil
		})
		if err != nil {
			return err
		}
		return writeRuns(cmd.OutOrStdout(), *format, runs, time.Now())
	}
	return cmd
}

func (a *app) logsCmd() *cobra.Command {
	var follow bool
	var types []string
	cmd := &cobra.Command{
		Use:   "logs ID",
		Short: "Print a run's events, one JSON object per line",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			enc := json.NewEncoder(cmd.OutOrStdout())
			req := ipc.Request{Method: ipc.MethodLogs, RunID: args[0], Follow: follow}
			return a.call(cmd, req, func(r ipc.Response) error {
				if r.Event == nil || !eventWanted(types, r.Event.Type) {
					return nil
				}
				return enc.Encode(r.Event)
			})
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events until the run is no longer active")
	cmd.Flags().StringArrayVar(&types, "type", nil, "print only events of this type (repeatable)")
	return cmd
}

func (a *app) call(cmd *cobra.Command, req ipc.Request, fn func(ipc.Response) error) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	return ipc.Call(cmd.Context(), cfg.SocketPath, req, fn)
}

func (a *app) printRun(cmd *cobra.Command, req ipc.Request, format outputFormat) error {
	var run *ipc.Run
	err := a.call(cmd, req, func(r ipc.Response) error {
		run = r.Run
		return nil
	})
	if err != nil {
		return err
	}
	if run == nil {
		return errors.New("the daemon replied without a run")
	}
	return writeRun(cmd.OutOrStdout(), format, *run, time.Now())
}

func promptArg(cmd *cobra.Command, arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", fmt.Errorf("read the prompt from stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func writeIndented(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
