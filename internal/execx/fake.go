package execx

import (
	"context"
	"strings"
	"sync"
)

// Fake is a Runner that answers from recorded output. Responses are keyed by
// the command line ("docker ps -a …"); Match, when set, is asked first and
// can answer by pattern. Anything unanswered exits 127, like a missing
// program. Every call is recorded in Calls.
type Fake struct {
	Responses map[string]Result
	Match     func(name string, args []string) (Result, bool)

	mu    sync.Mutex
	Calls []string
}

// Key is how Fake names a command line.
func Key(name string, args ...string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (f *Fake) Run(_ context.Context, name string, args []string, _ Opts) Result {
	k := Key(name, args...)
	f.mu.Lock()
	f.Calls = append(f.Calls, k)
	f.mu.Unlock()
	if f.Match != nil {
		if r, ok := f.Match(name, args); ok {
			return r
		}
	}
	if r, ok := f.Responses[k]; ok {
		return r
	}
	return Result{Code: 127, Stderr: "fake: no response for " + k}
}

// Called reports whether a command line starting with prefix was run.
func (f *Fake) Called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}
