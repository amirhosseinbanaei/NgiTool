package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// withLock runs fn holding the exclusive lock that every mutating command
// takes. A second run waits up to 10 s with a spinner naming the holder,
// then fails. With required=false a lock directory we may not create (a
// non-root user updating their own copy) is not an error.
func (e *env) withLock(ctx context.Context, required bool, fn func() error) error {
	l, holder, err := state.TryLock(e.paths.Lock)
	if err != nil {
		if !required && errors.Is(err, os.ErrPermission) {
			return fn()
		}
		return fmt.Errorf("cannot take the lock %s: %w — run as root, or set NGITOOL_ROOT", e.paths.Lock, err)
	}
	if l == nil {
		err = ui.Task("Waiting for "+holder.String(), func(t *ui.TaskCtl) error {
			var werr error
			l, werr = state.Acquire(ctx, e.paths.Lock, state.LockWait, func(h state.Holder) {
				t.Update("Waiting for " + h.String() + " to finish")
			})
			return werr
		})
		var locked *state.LockedError
		if errors.As(err, &locked) {
			return fmt.Errorf("%s is still running after %s — try again when it finishes", locked.Holder, state.LockWait)
		}
		if err != nil {
			return err
		}
	}
	defer l.Release()
	return fn()
}
