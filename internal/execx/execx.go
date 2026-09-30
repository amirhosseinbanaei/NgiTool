// Package execx runs child processes: docker, docker compose, nginx. Output
// is captured unless Inherit is set (logs, interactive tools), so spinners
// stay clean, and errors carry the tail of what the command printed.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// TailLines is how much of a failing command's output an error keeps.
const TailLines = 12

// ComposeScrub are the variables that must not leak into a child compose run:
// an inherited COMPOSE_PROJECT_NAME makes `docker compose` act on the wrong
// project (edge case DOCK-08).
var ComposeScrub = []string{"COMPOSE_PROJECT_NAME", "COMPOSE_FILE"}

// Opts configures one run.
type Opts struct {
	Dir     string
	Stdin   io.Reader
	Inherit bool          // stream to the terminal instead of capturing
	Env     []string      // KEY=VALUE additions, override the parent's
	Scrub   []string      // names removed from the child's environment
	Timeout time.Duration // 0 = only the context's deadline
}

// Result is what a finished command left behind. Code is 127 when the
// program could not be started and 124 when it timed out, as in the shell.
type Result struct {
	Code   int
	Stdout string
	Stderr string
}

// Error is a non-zero exit from Must.
type Error struct {
	Cmd string
	Res Result
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s failed (exit %d)", e.Cmd, e.Res.Code)
	if t := Tail(e.Res, TailLines); t != "" {
		msg += "\n" + t
	}
	return msg
}

// Tail returns the last n lines of stderr, or of stdout when stderr is empty.
func Tail(r Result, n int) string {
	out := strings.TrimSpace(r.Stderr)
	if out == "" {
		out = strings.TrimSpace(r.Stdout)
	}
	if out == "" {
		return ""
	}
	lines := strings.Split(out, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Run never returns an error: a failure to start is exit 127, a timeout 124.
func Run(ctx context.Context, name string, args []string, o Opts) Result {
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = o.Dir
	cmd.Env = Environ(os.Environ(), o.Env, o.Scrub)
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	if o.Inherit {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = o.Stdin, &stdout, &stderr
	}
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.Code = 124
		res.Stderr = strings.TrimRight(res.Stderr, "\n") + "\n" + fmt.Sprintf("%s: %v", name, timeoutText(ctx, o.Timeout))
	case errors.As(err, &exitErr):
		res.Code = exitErr.ExitCode()
		if res.Code < 0 {
			res.Code = 1
		}
	default:
		res.Code = 127
		res.Stderr = err.Error()
	}
	return res
}

// Must is Run, but a non-zero exit becomes an *Error.
func Must(ctx context.Context, name string, args []string, o Opts) (Result, error) {
	res := Run(ctx, name, args, o)
	if res.Code != 0 {
		label := name
		if len(args) > 0 {
			label += " " + args[0]
		}
		return res, &Error{Cmd: label, Res: res}
	}
	return res, nil
}

// Environ builds a child environment: base, minus scrubbed names, plus add.
func Environ(base, add, scrub []string) []string {
	drop := map[string]bool{}
	for _, k := range scrub {
		drop[k] = true
	}
	for _, kv := range add {
		drop[keyOf(kv)] = true
	}
	out := make([]string, 0, len(base)+len(add))
	for _, kv := range base {
		if !drop[keyOf(kv)] {
			out = append(out, kv)
		}
	}
	return append(out, add...)
}

// Has reports whether a program is on PATH.
func Has(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func keyOf(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

func timeoutText(ctx context.Context, d time.Duration) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && d > 0 {
		return fmt.Sprintf("timed out after %s", d)
	}
	return ctx.Err().Error()
}
