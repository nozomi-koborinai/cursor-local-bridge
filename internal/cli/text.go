package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/nozomi-koborinai/cursor-local-bridge/internal/ipc"
)

type outputFormat string

const (
	outputJSON outputFormat = "json"
	outputText outputFormat = "text"
)

func (f *outputFormat) Set(s string) error {
	switch outputFormat(s) {
	case outputJSON, outputText:
		*f = outputFormat(s)
		return nil
	default:
		return fmt.Errorf("must be %q or %q", outputJSON, outputText)
	}
}

func (f *outputFormat) String() string {
	if f == nil || *f == "" {
		return string(outputJSON)
	}
	return string(*f)
}

func (f *outputFormat) Type() string { return "json|text" }

func addOutputFlag(cmd *cobra.Command) *outputFormat {
	f := outputJSON
	cmd.Flags().VarP(&f, "output", "o", `output format, "json" or "text"`)
	return &f
}

func writeRun(w io.Writer, f outputFormat, r ipc.Run, now time.Time) error {
	if f == outputText {
		_, err := io.WriteString(w, runText(r, now))
		return err
	}
	return writeIndented(w, r)
}

func writeRuns(w io.Writer, f outputFormat, runs []ipc.Run, now time.Time) error {
	if f == outputText {
		_, err := io.WriteString(w, runsTable(runs, now))
		return err
	}
	return writeIndented(w, runs)
}

func runText(r ipc.Run, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s  (updated %s)\n", printable(r.ID), printable(r.Status), ago(r.UpdatedAt, now))
	field := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		gap := 10 - utf8.RuneCountInString(key)
		if gap < 2 {
			gap = 2
		}
		fmt.Fprintf(&b, "  %s%s%s\n", key, strings.Repeat(" ", gap), printable(value))
	}
	block := func(key, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		fmt.Fprintf(&b, "  %s\n", key)
		for line := range strings.SplitSeq(printable(value), "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	field("repo", r.Repo)
	field("ref", r.BaseRef)
	field("branch", r.Branch)
	field("worktree", r.Worktree)
	field("backend", r.Backend)
	field("model", r.Model)
	field("agent", r.AgentVersion)
	field("session", r.SessionID)
	if r.QueuedPrompts > 0 {
		field("prompts", fmt.Sprintf("%d", r.QueuedPrompts))
	}
	if p := r.PendingPermission; p != nil {
		value := p.ID
		if p.Title != "" {
			if value != "" {
				value += "  "
			}
			value += p.Title
		}
		field("permission", value)
	}
	field("created", r.CreatedAt.UTC().Format(time.RFC3339))
	field("updated", r.UpdatedAt.UTC().Format(time.RFC3339))
	block("summary", r.Summary)
	block("error", r.Error)
	if r.Status == "failed" {
		field("stderr", "clb logs --type stderr "+r.ID)
	}
	return b.String()
}

func runsTable(runs []ipc.Run, now time.Time) string {
	type row struct {
		id, status, agent, updated, note string
	}
	rows := make([]row, len(runs))
	for i, r := range runs {
		rows[i] = row{
			id:      printable(r.ID),
			status:  printable(r.Status),
			agent:   printable(r.AgentVersion),
			updated: ago(r.UpdatedAt, now),
			note:    noteOf(r),
		}
	}
	header := row{id: "ID", status: "STATUS", agent: "AGENT", updated: "UPDATED", note: "NOTE"}
	width := func(pick func(row) string) int {
		n := utf8.RuneCountInString(pick(header))
		for _, r := range rows {
			if w := utf8.RuneCountInString(pick(r)); w > n {
				n = w
			}
		}
		return n
	}
	widths := []int{
		width(func(r row) string { return r.id }),
		width(func(r row) string { return r.status }),
		width(func(r row) string { return r.agent }),
		width(func(r row) string { return r.updated }),
	}
	var b strings.Builder
	write := func(r row) {
		cols := []string{r.id, r.status, r.agent, r.updated, r.note}
		for i, col := range cols {
			if i == len(cols)-1 {
				if col != "" {
					b.WriteString("  ")
					b.WriteString(col)
				}
				break
			}
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(pad(col, widths[i]))
		}
		b.WriteByte('\n')
	}
	write(header)
	for _, r := range rows {
		write(r)
	}
	return b.String()
}

func noteOf(r ipc.Run) string {
	text := r.Summary
	if r.Status == "failed" {
		text = r.Error
	}
	text, _, _ = strings.Cut(text, "\n")
	return cutRunes(printable(text), 60)
}

func cutRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func ago(at, now time.Time) string {
	d := now.Sub(at)
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

func printable(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			b.WriteRune('\uFFFD')
		case r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && (r < 0x80 || r > 0x9f)):
			b.WriteRune(r)
		default:
			fmt.Fprintf(&b, `\u%04X`, r)
		}
		i += size
	}
	return b.String()
}

func eventWanted(types []string, typ string) bool {
	if len(types) == 0 {
		return true
	}
	for _, want := range types {
		if want == typ {
			return true
		}
	}
	return false
}
