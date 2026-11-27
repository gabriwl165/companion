package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/repocompanion/companion/internal/config"
	"github.com/repocompanion/companion/internal/usage"
	"github.com/spf13/cobra"
)

func parseUsageSince(value string, now time.Time) (time.Time, error) {
	var d time.Duration
	var err error
	if strings.HasSuffix(value, "d") {
		days := strings.TrimSuffix(value, "d")
		if days == "" || strings.Trim(days, "0123456789") != "" {
			return time.Time{}, fmt.Errorf("invalid --since %q: use a positive duration or integer days", value)
		}
		var n uint64
		n, err = strconv.ParseUint(days, 10, 64)
		if err == nil && n <= uint64(math.MaxInt64/int64(24*time.Hour)) {
			d = time.Duration(n) * 24 * time.Hour
		} else {
			err = fmt.Errorf("days out of range")
		}
	} else {
		d, err = time.ParseDuration(value)
	}
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("invalid --since %q: use a positive duration or integer days", value)
	}
	return now.Add(-d), nil
}

func (a *App) usageCmd() *cobra.Command {
	var since string
	cmd := &cobra.Command{
		Use: "usage", Short: "Show local MCP tool execution history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var cutoff time.Time
			if cmd.Flags().Changed("since") {
				var err error
				cutoff, err = parseUsageSince(since, time.Now())
				if err != nil {
					return err
				}
			}
			home, err := config.Home()
			if err != nil {
				return err
			}
			report, err := usage.NewStore(home).Report(cutoff)
			if err != nil {
				return err
			}
			if report.IncompleteTail {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: usage history has an incomplete trailing record; results may be incomplete")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Local MCP tool executions")
			if cutoff.IsZero() {
				fmt.Fprintln(out, "Period: all history")
			} else {
				fmt.Fprintf(out, "Period: since %s\n", usageTime(cutoff))
			}
			if report.StartedAt.IsZero() {
				fmt.Fprintln(out, "No recorded tool executions.")
				return nil
			}
			fmt.Fprintf(out, "First recorded: %s\n", usageTime(report.StartedAt))
			if len(report.Clients) == 0 {
				fmt.Fprintln(out, "No tool executions in this period.")
				return nil
			}
			fmt.Fprintln(out, "\nClients")
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CLIENT\tCALLS\tERRORS\tLAST USED")
			for _, row := range report.Clients {
				fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", row.Client, row.Calls, row.Errors, usageTime(row.LastUsed))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintln(out, "\nTools by client")
			w = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "CLIENT\tTOOL\tCALLS\tERRORS\tLAST USED")
			for _, row := range report.Tools {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", row.Client, row.Tool, row.Calls, row.Errors, usageTime(row.LastUsed))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "show executions from the last duration or integer days (e.g. 24h, 7d)")
	return cmd
}

func usageTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
