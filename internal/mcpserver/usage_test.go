package mcpserver

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/repocompanion/companion/internal/usage"
)

type usageInput struct {
	Mode string `json:"mode"`
}

func usageClient(t *testing.T, server *mcp.Server, name string) *mcp.ClientSession {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: name, Title: "ignored title", Version: "test"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestUsageInstrumentedCalls(t *testing.T) {
	store := usage.NewStore(t.TempDir())
	p := &provider{opts: Options{Usage: store}}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	entered := 0
	addTool(server, p, &mcp.Tool{Name: "fixture"}, func(_ context.Context, _ *mcp.CallToolRequest, in usageInput) (*mcp.CallToolResult, any, error) {
		entered++
		switch in.Mode {
		case "tool-error":
			return errorResult(errors.New("fixture tool error"))
		case "go-error":
			return nil, nil, errors.New("fixture go error")
		default:
			return textResult("fixture success")
		}
	})
	a := usageClient(t, server, "alpha")
	b := usageClient(t, server, "beta")
	if _, err := a.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	report, err := store.Report(time.Time{})
	if err != nil || len(report.Clients) != 0 || len(report.Tools) != 0 {
		t.Fatalf("discovery recorded usage: %+v, %v", report, err)
	}
	for _, tc := range []struct {
		client *mcp.ClientSession
		mode   string
		text   string
		failed bool
	}{
		{a, "success", "fixture success", false},
		{a, "tool-error", "fixture tool error", true},
		{b, "go-error", "fixture go error", true},
	} {
		res, err := tc.client.CallTool(context.Background(), &mcp.CallToolParams{Name: "fixture", Arguments: map[string]any{"mode": tc.mode}})
		if err != nil || res == nil || res.IsError != tc.failed || len(res.Content) == 0 {
			t.Fatalf("%s: result=%+v err=%v", tc.mode, res, err)
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok || !strings.Contains(text.Text, tc.text) {
			t.Fatalf("%s: content=%+v", tc.mode, res.Content)
		}
	}
	_, _ = a.CallTool(context.Background(), &mcp.CallToolParams{Name: "missing", Arguments: map[string]any{}})
	_, _ = a.CallTool(context.Background(), &mcp.CallToolParams{Name: "fixture", Arguments: map[string]any{"mode": 42}})
	if entered != 3 {
		t.Fatalf("handler entered %d times, want 3", entered)
	}
	report, err = store.Report(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	clients := map[string]usage.Count{}
	for _, c := range report.Clients {
		clients[c.Client] = c
	}
	if len(clients) != 2 || clients["alpha"].Calls != 2 || clients["alpha"].Errors != 1 || clients["beta"].Calls != 1 || clients["beta"].Errors != 1 {
		t.Fatalf("clients: %+v", report.Clients)
	}
	tools := map[string]usage.Count{}
	for _, c := range report.Tools {
		tools[c.Client] = c
	}
	if len(report.Tools) != 2 || tools["alpha"].Tool != "fixture" || tools["alpha"].Calls != 2 || tools["alpha"].Errors != 1 || tools["alpha"].LastUsed.IsZero() || tools["beta"].Tool != "fixture" || tools["beta"].Calls != 1 || tools["beta"].Errors != 1 || tools["beta"].LastUsed.IsZero() {
		t.Fatalf("tools: %+v", report.Tools)
	}
}

func TestUsageDirectReturnAndPanic(t *testing.T) {
	store := usage.NewStore(t.TempDir())
	p := &provider{opts: Options{Usage: store}}
	wantResult := &mcp.CallToolResult{IsError: true}
	wantErr := errors.New("cancelled fixture")
	called := 0
	handler := instrumentTool(p, "direct", func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, string, error) {
		called++
		if ctx.Err() == nil {
			t.Fatal("expected cancelled context")
		}
		return wantResult, "unchanged", wantErr
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, out, err := handler(ctx, nil, struct{}{})
	if result != wantResult || out != "unchanged" || err != wantErr || called != 1 {
		t.Fatalf("result=%+v out=%q err=%v called=%d", result, out, err, called)
	}
	_, _, _ = handler(ctx, &mcp.CallToolRequest{}, struct{}{})
	panicHandler := instrumentTool(p, "panic", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, string, error) {
		panic("fixture panic")
	})
	func() {
		defer func() {
			if got := recover(); got != "fixture panic" {
				t.Fatalf("panic = %v", got)
			}
		}()
		_, _, _ = panicHandler(context.Background(), &mcp.CallToolRequest{}, struct{}{})
	}()
	report, err := store.Report(time.Time{})
	if err != nil || len(report.Clients) != 1 || report.Clients[0].Client != "unknown" || report.Clients[0].Calls != 2 || report.Clients[0].Errors != 2 || len(report.Tools) != 1 || report.Tools[0].Tool != "direct" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func captureUsageOutput(t *testing.T, f func()) (string, string) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr; _ = stdout.Close(); _ = stderr.Close() }()
	f()
	if _, err := stdout.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := stderr.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	warning, err := io.ReadAll(stderr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(warning)
}

func TestUsageFailureIsOpenAndWarnsOnce(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(home, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &provider{opts: Options{Usage: usage.NewStore(home)}}
	want := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result"}}}
	handler := instrumentTool(p, "fixture", func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, string, error) {
		return want, "output", nil
	})
	stdout, stderr := captureUsageOutput(t, func() {
		for i := 0; i < 2; i++ {
			res, out, err := handler(context.Background(), nil, struct{}{})
			if res != want || out != "output" || err != nil {
				t.Fatalf("result=%+v output=%q err=%v", res, out, err)
			}
		}
	})
	if stdout != "" || stderr != "warning: could not record companion usage\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestUsageDisabledForNewServer(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("COMPANION_HOME", home)
	t.Setenv("COMPANION_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	server, err := NewServer(Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	client := usageClient(t, server, "test")
	res, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs_pull", Arguments: map[string]any{}})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if _, err := os.Stat(filepath.Join(home, "usage.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("usage file stat error = %v", err)
	}
}

func TestUsageEnabledForNewServer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("COMPANION_HOME", home)
	t.Setenv("COMPANION_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	store := usage.NewStore(home)
	server, err := NewServer(Options{Cwd: t.TempDir(), Usage: store})
	if err != nil {
		t.Fatal(err)
	}
	client := usageClient(t, server, "actual-client")
	res, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs_pull", Arguments: map[string]any{}})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	report, err := store.Report(time.Time{})
	if err != nil || len(report.Clients) != 1 || report.Clients[0].Client != "actual-client" || report.Clients[0].Calls != 1 || report.Clients[0].Errors != 1 || len(report.Tools) != 1 || report.Tools[0].Tool != "docs_pull" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestRunUsageHomeFailureIsOpen(t *testing.T) {
	t.Setenv("COMPANION_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	serverIn, clientOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	clientIn, serverOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = serverIn, serverOut, stderr
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	cwd := t.TempDir()
	go func() { done <- Run(ctx, Options{Cwd: cwd}) }()
	stopped := false
	t.Cleanup(func() {
		cancel()
		_ = clientOut.Close()
		_ = clientIn.Close()
		_ = serverIn.Close()
		_ = serverOut.Close()
		if !stopped {
			<-done
		}
		os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr
		_ = stderr.Close()
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || tools == nil || len(tools.Tools) != 27 {
		t.Fatalf("list tools: %+v, %v", tools, err)
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "docs_pull", Arguments: map[string]any{}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("call tool: %+v, %v", result, err)
	}
	_ = cs.Close()
	cancel()
	err = <-done
	stopped = true
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if _, err := stderr.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	warning, err := io.ReadAll(stderr)
	if err != nil || string(warning) != "warning: could not initialize companion usage\n" {
		t.Fatalf("stderr=%q, err=%v", warning, err)
	}
}
