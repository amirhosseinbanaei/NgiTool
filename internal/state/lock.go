package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LockWait is how long a mutating command waits for another run to finish.
const LockWait = 10 * time.Second

// Holder describes the process holding the lock; it is written into the
// lock file so a waiting run can say who it is waiting for.
type Holder struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Since   time.Time `json:"since"`
}

func (h Holder) String() string {
	if h.PID == 0 {
		return "another ngitool run"
	}
	return fmt.Sprintf("pid %d (%s, since %s)", h.PID, h.Command, h.Since.Format("15:04:05"))
}

// LockedError is returned when the wait runs out.
type LockedError struct {
	Path   string
	Holder Holder
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("another ngitool run holds %s: %s", e.Path, e.Holder)
}

// Lock is an exclusive flock on the lock file.
type Lock struct{ f *os.File }

// TryLock takes the lock without waiting. When it is busy it returns
// (nil, holder, nil).
func TryLock(path string) (*Lock, *Holder, error) {
	if err := os.MkdirAll(filepath.Dir(path), DirMode); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, FileMode)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			h := readHolder(path)
			return nil, &h, nil
		}
		return nil, nil, fmt.Errorf("lock %s: %w", path, err)
	}
	h := Holder{PID: os.Getpid(), Command: command(), Since: time.Now()}
	b, _ := json.Marshal(h)
	_ = f.Truncate(0)
	_, _ = f.WriteAt(b, 0)
	return &Lock{f: f}, nil, nil
}

// Acquire waits up to `wait` for the lock. onWait is called whenever the
// holder changes while waiting (the CLI shows it in a spinner).
func Acquire(ctx context.Context, path string, wait time.Duration, onWait func(Holder)) (*Lock, error) {
	deadline := time.Now().Add(wait)
	var last Holder
	for {
		l, h, err := TryLock(path)
		if err != nil || l != nil {
			return l, err
		}
		if onWait != nil && *h != last {
			onWait(*h)
		}
		last = *h
		if time.Now().After(deadline) {
			return nil, &LockedError{Path: path, Holder: *h}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Release clears the holder and drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = l.f.Truncate(0)
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

func readHolder(path string) Holder {
	var h Holder
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &h)
	}
	return h
}

func command() string {
	args := make([]string, 0, len(os.Args))
	for i, a := range os.Args {
		if i == 0 {
			a = filepath.Base(a)
		}
		args = append(args, a)
	}
	s := strings.Join(args, " ")
	if len(s) > 80 {
		s = s[:79] + "…"
	}
	return s
}
