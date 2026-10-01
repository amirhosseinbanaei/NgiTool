package execx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMustKeepsTailOfStderr(t *testing.T) {
	script := `for i in $(seq 1 30); do echo "line $i" >&2; done; exit 3`
	_, err := Must(context.Background(), "sh", []string{"-c", script}, Opts{})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %v", err)
	}
	msg := e.Error()
	if !strings.HasPrefix(msg, "sh -c failed (exit 3)") {
		t.Errorf("header: %q", msg)
	}
	if strings.Contains(msg, "line 18\n") || !strings.Contains(msg, "line 19\n") || !strings.HasSuffix(msg, "line 30") {
		t.Errorf("want exactly the last %d lines:\n%s", TailLines, msg)
	}
}

func TestTailFallsBackToStdout(t *testing.T) {
	if got := Tail(Result{Stdout: "a\nb\n"}, 1); got != "b" {
		t.Fatalf("got %q", got)
	}
}

func TestRunCodes(t *testing.T) {
	ctx := context.Background()
	if r := Run(ctx, "sh", []string{"-c", "echo out; echo err >&2"}, Opts{}); r.Code != 0 || r.Stdout != "out\n" || r.Stderr != "err\n" {
		t.Errorf("capture: %+v", r)
	}
	if r := Run(ctx, "definitely-not-a-program-ngt", nil, Opts{}); r.Code != 127 {
		t.Errorf("missing program: %+v", r)
	}
	if r := Run(ctx, "sleep", []string{"5"}, Opts{Timeout: 100 * time.Millisecond}); r.Code != 124 || !strings.Contains(r.Stderr, "timed out") {
		t.Errorf("timeout: %+v", r)
	}
	if r := Run(ctx, "cat", nil, Opts{Stdin: strings.NewReader("piped")}); r.Stdout != "piped" {
		t.Errorf("stdin: %+v", r)
	}
}

func TestEnvScrub(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "leaked")
	t.Setenv("COMPOSE_FILE", "leaked.yaml")
	t.Setenv("NGT_KEEP", "kept")
	r := Run(context.Background(), "sh", []string{"-c", `echo "[$COMPOSE_PROJECT_NAME][$COMPOSE_FILE][$NGT_KEEP][$NGT_ADD]"`},
		Opts{Scrub: ComposeScrub, Env: []string{"NGT_ADD=added"}})
	if got := strings.TrimSpace(r.Stdout); got != "[][][kept][added]" {
		t.Fatalf("got %s", got)
	}
}

func TestEnvironOverrides(t *testing.T) {
	got := Environ([]string{"A=1", "B=2"}, []string{"A=9"}, []string{"B"})
	if strings.Join(got, ",") != "A=9" {
		t.Fatalf("got %v", got)
	}
}
