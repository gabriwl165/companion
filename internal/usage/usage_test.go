package usage

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestReportAndCutoff(t *testing.T) {
	home := t.TempDir()
	s := NewStore(home)
	t0 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.FixedZone("offset", 3600))
	for _, e := range []Event{
		{Time: t0, Client: " z\x1b\u202eé ", Tool: "read"},
		{Time: t0.Add(time.Second), Client: "a", Tool: "read", Failed: true},
		{Time: t0.Add(2 * time.Second), Client: "a", Tool: "write"},
		{Time: t0.Add(3 * time.Second), Client: "a", Tool: "read"},
	} {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewStore(home).Report(t0.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !r.StartedAt.Equal(t0) || r.StartedAt.Location() != time.UTC || len(r.Clients) != 1 || r.Clients[0].Calls != 3 || r.Clients[0].Errors != 1 || len(r.Tools) != 2 || r.Tools[0].Tool != "read" || r.Tools[0].Calls != 2 || r.Tools[0].Errors != 1 || !r.Tools[0].LastUsed.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("unexpected filtered report: %+v", r)
	}
	r, err = s.Report(time.Time{})
	if err != nil || len(r.Clients) != 2 || r.Clients[1].Client != "zé" || len(r.Tools) != 3 {
		t.Fatalf("unexpected full report: %+v, %v", r, err)
	}
	r, err = s.Report(t0.Add(4 * time.Second))
	if err != nil || len(r.Clients) != 0 || !r.StartedAt.Equal(t0) {
		t.Fatalf("unexpected future report: %+v, %v", r, err)
	}
	b, err := os.ReadFile(filepath.Join(home, "usage.jsonl"))
	if err != nil || strings.Contains(string(b), "\u202e") || strings.Contains(string(b), "\\u001b") || !strings.Contains(string(b), `"version":1`) {
		t.Fatalf("unexpected data %q: %v", b, err)
	}
}

func TestMissingAndEmptyDoNotCreateFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "not-created")
	s := NewStore(home)
	if _, err := s.Report(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("home created: %v", err)
	}
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(time.Time{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil || len(entries) != 1 {
		t.Fatalf("empty report wrote files: %v, %v", entries, err)
	}
}

func TestTornTailAndMalformedCommitted(t *testing.T) {
	s := NewStore(t.TempDir())
	e := Event{Time: time.Now(), Client: "a", Tool: "read"}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":1`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Report(time.Time{})
	if err != nil || !r.IncompleteTail || r.Clients[0].Calls != 1 {
		t.Fatalf("tail report: %+v %v", r, err)
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	r, err = s.Report(time.Time{})
	if err != nil || r.IncompleteTail || r.Clients[0].Calls != 2 {
		t.Fatalf("repaired: %+v %v", r, err)
	}
	for _, bad := range []string{"broken\n", `{"version":2,"time":"2026-01-01T00:00:00Z","client":"a","tool":"b","failed":false}` + "\n", `{"version":1}` + "\n"} {
		if err := os.WriteFile(s.path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Report(time.Time{}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(s.path)
		if err != nil || !strings.HasPrefix(string(b), bad) {
			t.Fatalf("overwrote committed record: %q, %v", b, err)
		}
		if _, err := s.Report(time.Time{}); err == nil {
			t.Fatal("append hid invalid record")
		}
	}
}

func TestConcurrentAppends(t *testing.T) {
	if home := os.Getenv("COMPANION_USAGE_TEST_CHILD"); home != "" {
		s := NewStore(home)
		for i := 0; i < 20; i++ {
			if err := s.Append(Event{Time: time.Now(), Client: "child", Tool: "read"}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	home := t.TempDir()
	s := NewStore(home)
	if err := s.Append(Event{Time: time.Now(), Client: "goroutine", Tool: "read"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 9)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := s.Append(Event{Time: time.Now(), Client: "goroutine", Tool: "read"}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentAppends$")
			cmd.Env = append(os.Environ(), "COMPANION_USAGE_TEST_CHILD="+home)
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("child: %w: %s", err, out)
			}
		}()
	}
	// Each report must observe complete records while writers are active.
	for i := 0; i < 30; i++ {
		if _, err := s.Report(time.Time{}); err != nil {
			t.Fatalf("concurrent snapshot: %v", err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	r, err := s.Report(time.Time{})
	if err != nil || len(r.Clients) != 2 || r.Clients[0].Calls != 121 || r.Clients[1].Calls != 60 || r.IncompleteTail {
		t.Fatalf("concurrent result: %+v %v", r, err)
	}
}

func TestLockTimeout(t *testing.T) {
	s := NewStore(t.TempDir())
	if err := s.Append(Event{Time: time.Now(), Tool: "read"}); err != nil {
		t.Fatal(err)
	}
	l := flock.New(s.path + ".lock")
	if err := l.Lock(); err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	// Count only descriptors for this lock, not unrelated runtime descriptors.
	countFDs := func() int {
		dir := "/dev/fd"
		if runtime.GOOS == "linux" {
			dir = "/proc/self/fd"
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return -1
		}
		lockInfo, err := os.Stat(s.path + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, entry := range entries {
			info, err := os.Stat(filepath.Join(dir, entry.Name()))
			if err == nil && os.SameFile(info, lockInfo) {
				n++
			}
		}
		return n
	}
	before := countFDs()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.Append(Event{Time: time.Now(), Tool: "read"}); err == nil || time.Since(start) > 6*time.Second {
			t.Fatalf("expected bounded lock failure: %v, elapsed %v", err, time.Since(start))
		}
	}
	if before > 0 {
		if after := countFDs(); after != before {
			t.Fatalf("lock descriptor leak: before %d, after %d", before, after)
		}
	} else {
		t.Log("lock descriptor accounting unavailable")
	}
}

func TestPermissionsAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions and symlinks")
	}
	home := filepath.Join(t.TempDir(), "home")
	s := NewStore(home)
	e := Event{Time: time.Now(), Tool: "read"}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, s.path, s.path + ".lock"} {
		info, err := os.Stat(path)
		want := os.FileMode(0o600)
		if path == home {
			want = 0o700
		}
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", path, info, err)
		}
	}
	for _, path := range []string{s.path, s.path + ".lock"} {
		if err := os.Chmod(path, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Report(time.Time{}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.path, s.path + ".lock"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("report permissions %s: %v %v", path, info, err)
		}
	}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.path, s.path + ".lock"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions %s: %v %v", path, info, err)
		}
	}
	for _, name := range []string{"usage.jsonl", "usage.jsonl.lock"} {
		t.Run(name, func(t *testing.T) {
			h := t.TempDir()
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(h, name)); err != nil {
				t.Fatal(err)
			}
			if err := NewStore(h).Append(e); err == nil {
				t.Fatal("accepted symlink")
			}
			if _, err := NewStore(h).Report(time.Time{}); err == nil {
				t.Fatal("report accepted symlink")
			}
			b, err := os.ReadFile(target)
			if err != nil || !reflect.DeepEqual(b, []byte("safe")) {
				t.Fatalf("changed target: %q %v", b, err)
			}
		})
	}
}

func TestWindowsReadonlyReport(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows read-only attribute")
	}
	s := NewStore(t.TempDir())
	if err := s.Append(Event{Time: time.Now(), Client: "client", Tool: "read"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(s.path, 0o400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(s.path, 0o600)
	r, err := s.Report(time.Time{})
	if err != nil || len(r.Clients) != 1 || r.Clients[0].Calls != 1 {
		t.Fatalf("read-only report: %+v, %v", r, err)
	}
}

func TestCleanBounds(t *testing.T) {
	if got := clean(" \x1b\u202e "); got != "unknown" {
		t.Fatal(got)
	}
	got := clean(strings.Repeat("é", 100))
	if len(got) != 128 || got != strings.Repeat("é", 64) {
		t.Fatalf("bad bounded Unicode: %q", got)
	}
}

func TestSanitizeOnRead(t *testing.T) {
	s := NewStore(t.TempDir())
	line := fmt.Sprintf("{\"version\":1,\"time\":\"2026-01-01T00:00:00Z\",\"client\":\"\\u001b%s\",\"tool\":\"\\u202e\\u0007\",\"failed\":false}\n", strings.Repeat("é", 100))
	if err := os.WriteFile(s.path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Report(time.Time{})
	if err != nil || len(r.Tools) != 1 || r.Tools[0].Client != strings.Repeat("é", 64) || r.Tools[0].Tool != "unknown" {
		t.Fatalf("unsafe report: %+v %v", r, err)
	}
}
