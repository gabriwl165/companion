package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repocompanion/companion/internal/usage"
	"github.com/spf13/pflag"
)

func TestParseUsageSince(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		input    string
		duration time.Duration
	}{
		{"30m", 30 * time.Minute}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour},
		{"1ns", time.Nanosecond}, {"106751d", 106751 * 24 * time.Hour},
	} {
		got, err := parseUsageSince(tc.input, now)
		if err != nil || !got.Equal(now.Add(-tc.duration)) {
			t.Errorf("%q: %v, %v", tc.input, got, err)
		}
	}
	for _, input := range []string{"", "0", "0s", "0d", "-1h", "-1d", "1.5d", "+1d", "d", "junk", "106752d", "999999999999999999999999999d", "2562048h"} {
		if _, err := parseUsageSince(input, now); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
}

func TestUsageCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPANION_HOME", home)
	t.Setenv("COMPANION_CONFIG", filepath.Join(home, "bad-config"))
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(home, "bad-config"), []byte("[broken"), 0600); err != nil {
		t.Fatal(err)
	}
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	store := usage.NewStore(home)
	first := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	last := first.Add(time.Hour)
	for _, event := range []usage.Event{
		{Time: first, Client: "opencode", Tool: "docs_get", Failed: true},
		{Time: last, Client: "opencode", Tool: "docs_get"},
		{Time: last, Client: "claude-code", Tool: "docs_list"},
	} {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, string, error) {
		cmd := NewRootCommand()
		var out, stderr bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), stderr.String(), err
	}
	out, _, err := run("usage", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Local MCP tool executions", "Period: all history", "First recorded: 2020-01-02T03:04:05Z", "Clients", "Tools by client", "opencode", "claude-code", "docs_get", "docs_list", "2020-01-02T04:04:05Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	lines := "\n" + strings.Join(strings.Fields(out), " ") + "\n"
	for _, want := range []string{"opencode 2 1 2020-01-02T04:04:05Z", "opencode docs_get 2 1 2020-01-02T04:04:05Z", "claude-code docs_list 1 0 2020-01-02T04:04:05Z"} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing row %q in:\n%s", want, out)
		}
	}
	out, _, err = run("usage", "--since", "1ns", "--json=false")
	if err != nil || !strings.Contains(out, "No tool executions in this period.") || !strings.Contains(out, "First recorded: 2020-01-02T03:04:05Z") {
		t.Fatalf("empty window: %v\n%s", err, out)
	}
	for _, args := range [][]string{{"usage", "--since="}, {"usage", "extra"}} {
		if _, _, err := run(args...); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	f, err := os.OpenFile(filepath.Join(home, "usage.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"incomplete\""); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := run("usage")
	if err != nil || !strings.Contains(stderr, "incomplete trailing record") {
		t.Fatalf("tail warning: %v %q", err, stderr)
	}
	cmd := (&App{}).usageCmd()
	cmd.InitDefaultHelpFlag()
	var names []string
	cmd.LocalFlags().VisitAll(func(flag *pflag.Flag) { names = append(names, flag.Name) })
	if strings.Join(names, ",") != "help,since" {
		t.Fatalf("local flags: %v", names)
	}
}

func TestUsageMissingHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("COMPANION_HOME", "")
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--home", home, "usage"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No recorded tool executions.") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("home created or inaccessible: %v", err)
	}
}
