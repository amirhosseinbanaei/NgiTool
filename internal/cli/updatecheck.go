package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/update"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

// The background update check: at most once per 24 h, only on a terminal,
// never blocking or failing a command. The check itself runs in a detached
// child (`ngitool __update-check`, 2 s timeout) that writes the cache; the
// notice is printed from the cache after a later command finishes.
const (
	checkEvery   = 24 * time.Hour
	checkTimeout = 2 * time.Second
	hiddenCheck  = "__update-check"
	EnvNoCheck   = "NGITOOL_NO_UPDATE_CHECK"
)

type checkCache struct {
	CheckedAt time.Time `json:"checkedAt"`
	Latest    string    `json:"latest,omitempty"`
	Error     string    `json:"error,omitempty"`
}

func updateCheckCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:    hiddenCheck,
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			refreshCheck(cmd.Context(), e)
			return nil
		},
	}
}

// skipNotice lists commands after which no notice is printed.
var skipNotice = map[string]bool{"update": true, "uninstall": true, "completion": true, hiddenCheck: true, "__complete": true, "help": true}

// afterCommand prints the notice from the cache and starts a refresh when
// the cache is stale.
func afterCommand(e *env, cmd *cobra.Command) {
	if cmd == nil || skipNotice[cmd.Name()] || !checksEnabled(e) {
		return
	}
	if f := cmd.Flags().Lookup("json"); f != nil && f.Changed {
		return
	}
	cache := readCheck(e.paths)
	if cache.Latest != "" && version.Compare(cache.Latest, version.Version) > 0 {
		fmt.Fprintln(ui.Errw, ui.Muted(fmt.Sprintf("NgiTool %s is available (you have %s) — ngitool update", cache.Latest, version.Version)))
	}
	if time.Since(cache.CheckedAt) >= checkEvery {
		// Stamp first so parallel runs don't all spawn a check.
		cache.CheckedAt = time.Now()
		if writeCheck(e.paths, cache) == nil {
			spawnCheck()
		}
	}
}

func checksEnabled(e *env) bool {
	if os.Getenv(EnvNoCheck) == "1" || os.Getenv("CI") != "" || version.IsDev() {
		return false
	}
	if !ui.IsTTY() || !term.IsTerminal(int(os.Stderr.Fd())) {
		return false
	}
	cfg, err := state.LoadConfig(e.paths)
	return err == nil && cfg.UpdateCheck && cfg.Pinned == ""
}

func spawnCheck() {
	exe, err := paths.Executable()
	if err != nil {
		return
	}
	c := exec.Command(exe, hiddenCheck)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Env = append(os.Environ(), EnvNoCheck+"=1")
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}

func refreshCheck(ctx context.Context, e *env) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cfg, _ := state.LoadConfig(e.paths)
	cache := checkCache{CheckedAt: time.Now()}
	rs, err := update.NewClient(checkTimeout).Releases(ctx)
	if err != nil {
		cache.Error = err.Error()
	} else if r, ok := update.Latest(rs, cfg.Channel == "prerelease"); ok {
		cache.Latest = r.Tag
	}
	_ = writeCheck(e.paths, cache)
}

func readCheck(p paths.Paths) checkCache {
	var c checkCache
	if b, err := os.ReadFile(p.UpdateCheck); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func writeCheck(p paths.Paths, c checkCache) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return state.WriteFile(p.UpdateCheck, b, state.FileMode)
}
