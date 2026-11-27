// Package usage stores local, best-effort counts of completed tool calls.
package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofrs/flock"
)

type Event struct {
	Time   time.Time `json:"time"`
	Client string    `json:"client"`
	Tool   string    `json:"tool"`
	Failed bool      `json:"failed"`
}

type Count struct {
	Client   string
	Tool     string
	Calls    int
	Errors   int
	LastUsed time.Time
}

type Report struct {
	StartedAt      time.Time
	Clients        []Count
	Tools          []Count
	IncompleteTail bool
}

type Store struct{ home, path string }

func NewStore(home string) *Store {
	return &Store{home: home, path: filepath.Join(home, "usage.jsonl")}
}

// clean bounds names by bytes without splitting UTF-8 and strips terminal controls.
func clean(s string) string {
	var b strings.Builder
	for s = strings.TrimSpace(s); s != ""; {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if (r == utf8.RuneError && size == 1) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > 128 {
			break
		}
		b.WriteRune(r)
	}
	s = strings.TrimSpace(b.String())
	if s == "" {
		return "unknown"
	}
	return s
}

// openRegular rejects symlinks and special files, and makes existing files private.
func openRegular(path string, flag int, create bool) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("usage: non-regular file %s", path)
	}
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, os.ErrNotExist
		}
		flag |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flag, 0o600)
	if errors.Is(err, os.ErrExist) && flag&os.O_EXCL != 0 {
		return openRegular(path, flag&^(os.O_CREATE|os.O_EXCL), false)
	}
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err == nil && (!actual.Mode().IsRegular() || (info != nil && !os.SameFile(info, actual))) {
		err = fmt.Errorf("usage: file changed while opening %s", path)
	}
	if err == nil && !(runtime.GOOS == "windows" && flag == os.O_RDONLY) && (actual.Mode().Perm() != 0o600 || actual.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0) {
		err = f.Chmod(0o600)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) lock() (func() error, error) {
	f, err := openRegular(s.path+".lock", os.O_RDWR, true)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	l := flock.New(s.path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !ok {
		return nil, errors.Join(errors.New("usage: timed out waiting for lock"), f.Close())
	}
	current, err := os.Lstat(s.path + ".lock")
	if err == nil && (!current.Mode().IsRegular() || !os.SameFile(info, current)) {
		err = errors.New("usage: lock file changed")
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return nil, errors.Join(err, l.Unlock())
	}
	return l.Unlock, nil
}

// committedSize finds the last newline; bytes after it are an uncommitted tail.
func committedSize(f *os.File, size int64) (int64, error) {
	buf := make([]byte, 4096)
	for size > 0 {
		n := int64(len(buf))
		if n > size {
			n = size
		}
		size -= n
		if _, err := f.ReadAt(buf[:n], size); err != nil {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return size + int64(i) + 1, nil
		}
	}
	return 0, nil
}

func (s *Store) Append(e Event) (err error) {
	if e.Time.IsZero() {
		return errors.New("usage: event time is required")
	}
	e.Time, e.Client, e.Tool = e.Time.UTC(), clean(e.Client), clean(e.Tool)
	b, err := json.Marshal(struct {
		Version int `json:"version"`
		Event
	}{1, e})
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	f, err := openRegular(s.path, os.O_RDWR, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	end, err := committedSize(f, info.Size())
	if err != nil {
		return err
	}
	if end != info.Size() {
		if err := f.Truncate(end); err != nil {
			return err
		}
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		return err
	}
	n, err := f.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func (s *Store) Report(since time.Time) (report Report, err error) {
	lockInfo, lockErr := os.Lstat(s.path + ".lock")
	if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		return report, lockErr
	}
	if lockErr == nil && !lockInfo.Mode().IsRegular() {
		return report, errors.New("usage: non-regular lock file")
	}
	// Avoid creating a home or lock for missing or empty history.
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	if !info.Mode().IsRegular() {
		return report, fmt.Errorf("usage: non-regular data file")
	}
	if info.Size() == 0 && info.Mode().Perm() == 0o600 {
		return report, nil
	}
	unlock, err := s.lock()
	if err != nil {
		return report, err
	}
	f, err := openRegular(s.path, os.O_RDONLY, false)
	if err != nil {
		return report, errors.Join(err, unlock())
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err = f.Stat()
	var end int64
	if err == nil {
		end, err = committedSize(f, info.Size())
	}
	unlockErr := unlock()
	if err = errors.Join(err, unlockErr); err != nil {
		return report, err
	}
	report.IncompleteTail = end != info.Size()
	clients := map[string]*Count{}
	type key struct{ client, tool string }
	tools := map[key]*Count{}
	// shortcut: reports scan a growing log, add an index or retention when history becomes slow.
	scanner := bufio.NewScanner(io.NewSectionReader(f, 0, end))
	scanner.Buffer(make([]byte, 4096), 8192)
	for scanner.Scan() {
		var r struct {
			Version *int       `json:"version"`
			Time    *time.Time `json:"time"`
			Client  *string    `json:"client"`
			Tool    *string    `json:"tool"`
			Failed  *bool      `json:"failed"`
		}
		dec := json.NewDecoder(bytes.NewReader(scanner.Bytes()))
		dec.DisallowUnknownFields()
		if e := dec.Decode(&r); e != nil {
			return Report{}, fmt.Errorf("usage: invalid record: %w", e)
		}
		var extra any
		if e := dec.Decode(&extra); e != io.EOF || r.Version == nil || *r.Version != 1 || r.Time == nil || r.Time.IsZero() || r.Client == nil || r.Tool == nil || r.Failed == nil {
			return Report{}, errors.New("usage: invalid or unsupported record")
		}
		t := r.Time.UTC()
		if report.StartedAt.IsZero() || t.Before(report.StartedAt) {
			report.StartedAt = t
		}
		if t.Before(since) {
			continue
		}
		client, tool := clean(*r.Client), clean(*r.Tool)
		if clients[client] == nil {
			clients[client] = &Count{Client: client}
		}
		k := key{client, tool}
		if tools[k] == nil {
			tools[k] = &Count{Client: client, Tool: tool}
		}
		for _, c := range []*Count{clients[client], tools[k]} {
			c.Calls++
			if *r.Failed {
				c.Errors++
			}
			if t.After(c.LastUsed) {
				c.LastUsed = t
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("usage: reading records: %w", err)
	}
	for _, c := range clients {
		report.Clients = append(report.Clients, *c)
	}
	for _, c := range tools {
		report.Tools = append(report.Tools, *c)
	}
	sortCounts := func(cs []Count) {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Calls != cs[j].Calls {
				return cs[i].Calls > cs[j].Calls
			}
			if cs[i].Client != cs[j].Client {
				return cs[i].Client < cs[j].Client
			}
			return cs[i].Tool < cs[j].Tool
		})
	}
	sortCounts(report.Clients)
	sortCounts(report.Tools)
	return report, nil
}
