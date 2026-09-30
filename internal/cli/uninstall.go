package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

type removal struct {
	path string
	what string
	dir  bool
}

func uninstallCmd(e *env) *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "remove the binary; --purge: all data too",
		Long: `Removes the ngitool binary, its ngt alias and the ngitool.prev kept by updates.

With --purge it also deletes /etc/ngitool, /var/lib/ngitool and /var/cache/ngitool,
after a typed confirmation (off a terminal: --yes --force). nginx, containers,
compose projects and the nginx files NgiTool wrote are left where they are.`,
		Example: `ngitool uninstall
ngitool uninstall --purge
ngitool uninstall --purge --yes --force   # scripts`,
		Annotations: map[string]string{annGroup: "tool", annSynopsis: "uninstall [--purge]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := uninstallPlan(e, purge)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				ui.Done("Nothing to remove")
				return nil
			}
			rows := make([][2]string, len(items))
			for i, it := range items {
				rows[i] = [2]string{it.what, it.path}
			}
			ui.Plan("Uninstall NgiTool", rows)
			if purge {
				ui.Explain(
					"deletes NgiTool's settings, what it manages, its backups and caches",
					"only the paths listed above; nginx, containers, compose projects and nginx files stay",
					"not possible — reinstall with install.sh and set things up again")
				fmt.Fprintln(ui.Out)
				if err := ui.SureDanger(e.yes, e.force, "Purge everything listed above?", "ngitool"); err != nil {
					return err
				}
			} else {
				ok, err := ui.Sure(e.yes, "Remove NgiTool?", "config and state stay; --purge removes them too")
				if err != nil {
					return err
				}
				if !ok {
					return errCancelled
				}
			}
			var failed []string
			for _, it := range items {
				var err error
				if it.dir {
					err = os.RemoveAll(it.path)
				} else {
					err = os.Remove(it.path)
				}
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					ui.Fail("could not remove " + it.path + ": " + err.Error())
					failed = append(failed, it.path)
					continue
				}
				ui.Done("Removed " + it.path)
			}
			if len(failed) > 0 {
				return &ExitError{Code: ExitFail, Msg: fmt.Sprintf("%d item(s) were not removed — run as root", len(failed))}
			}
			if !purge {
				ui.Hint("Config and state are still in " + e.paths.Etc + " and " + e.paths.Lib + " (ngitool uninstall --purge removes them).")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also remove config, state, backups and caches")
	return c
}

// uninstallPlan lists exactly what goes. The binary is $NGITOOL_PREFIX/bin/
// ngitool when that is set (tests), otherwise the running binary.
func uninstallPlan(e *env, purge bool) ([]removal, error) {
	var bin string
	if os.Getenv(paths.EnvPrefix) != "" {
		bin = filepath.Join(paths.BinDir(), "ngitool")
	} else {
		exe, err := paths.Executable()
		if err != nil {
			return nil, err
		}
		bin = exe
	}
	var items []removal
	if exists(bin) {
		items = append(items, removal{path: bin, what: "binary"})
	}
	if exists(bin + ".prev") {
		items = append(items, removal{path: bin + ".prev", what: "previous binary"})
	}
	// The alias is removed only when it really points at this binary.
	alias := filepath.Join(filepath.Dir(bin), "ngt")
	if target, err := filepath.EvalSymlinks(alias); err == nil {
		if resolved, _ := filepath.EvalSymlinks(bin); resolved != "" && target == resolved {
			items = append(items, removal{path: alias, what: "alias"})
		}
	} else if dest, err := os.Readlink(alias); err == nil && filepath.Base(dest) == "ngitool" {
		items = append(items, removal{path: alias, what: "alias (dangling)"})
	}
	if purge {
		for _, d := range e.paths.Dirs() {
			if !safeToPurge(d) {
				return nil, fmt.Errorf("refusing to purge %s: not an ngitool directory", d)
			}
			if exists(d) {
				items = append(items, removal{path: d, what: purgeLabel(e, d), dir: true})
			}
		}
	}
	return items, nil
}

func purgeLabel(e *env, d string) string {
	switch d {
	case e.paths.Etc:
		return "config"
	case e.paths.Lib:
		return "state + backups"
	}
	return "caches"
}

// safeToPurge guards RemoveAll: an absolute, non-root path that is either
// one of the fixed ngitool directories or sits under NGITOOL_ROOT.
func safeToPurge(d string) bool {
	if !filepath.IsAbs(d) || strings.Count(filepath.Clean(d), "/") < 2 {
		return false
	}
	if root := os.Getenv(paths.EnvRoot); root != "" {
		abs, _ := filepath.Abs(root)
		return strings.HasPrefix(d, abs+"/")
	}
	return filepath.Base(d) == "ngitool"
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
