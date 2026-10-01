package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// adoptedInstance resolves [instance] for the apply-group commands.
func (e *env) adoptedInstance(cmd *cobra.Command, args []string) (*discover.Report, *model.State, *discover.Instance, error) {
	st, err := model.Load(e.paths)
	if err != nil {
		return nil, nil, nil, err
	}
	rep := e.freshReport(cmd.Context(), true)
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	in, err := e.instanceFor(rep, st, id, true)
	return rep, st, in, err
}

func diffCmd(e *env) *cobra.Command {
	var onDrift string
	c := &cobra.Command{
		Use: "diff [instance]", Short: "what apply would change, and any hand edits", Args: maxArgs(1),
		Long:        "Renders the instance from state and prints a coloured diff against its files, with hand-edited managed\nfiles shown separately (APPLY-05). Changes nothing (APPLY-08).",
		Annotations: map[string]string{annGroup: "apply", annSynopsis: "diff [instance]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, st, in, err := e.adoptedInstance(cmd, args)
			if err != nil {
				return err
			}
			_, err = apply.Run(cmd.Context(), newDeps(e, in), apply.Change{Report: rep, Instance: in, Before: st, Next: st, Summary: "diff"},
				apply.Opts{DryRun: true, OnDrift: onDrift})
			return err
		},
	}
	c.Flags().StringVar(&onDrift, "on-drift", "", "unused by diff; accepted for symmetry")
	_ = c.Flags().MarkHidden("on-drift")
	return c
}

func applyCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use: "apply [instance]", Short: "re-render an instance from state and apply it", Args: maxArgs(1),
		Long:        "Writes what state says, through the usual transaction: diff, confirm, snapshot, nginx -t, reload, probe.\nUse it after a failed save, or to put back files someone changed.",
		Annotations: map[string]string{annGroup: "apply", annSynopsis: "apply [instance] [--dry-run]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, st, in, err := e.adoptedInstance(cmd, args)
			if err != nil {
				return err
			}
			var probe []string
			for _, r := range st.RoutesOn(in.ID) {
				probe = append(probe, r.ID)
			}
			_, err = e.change(cmd.Context(), rep, in, f, "apply", probe, func(*model.State) error { return nil })
			return err
		},
	}
	f.add(c)
	return c
}

func rollbackCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:   "rollback [instance] [snapshot]",
		Short: "restore an earlier snapshot of an instance's files and state",
		Long: "Every apply snapshots the files it is about to touch (APPLY-06). Rollback puts one back through the same\n" +
			"transaction — itself snapshotted first, so a rollback can be rolled back.",
		Annotations: map[string]string{annGroup: "apply", annSynopsis: "rollback [instance] [snapshot]"},
		Args:        maxArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, st, in, err := e.adoptedInstance(cmd, args)
			if err != nil {
				return err
			}
			snaps, err := apply.Snapshots(e.paths.Backups, in.ID)
			if err != nil {
				return err
			}
			if len(snaps) == 0 {
				return errors.New("no snapshots of " + in.Name + " yet — one is taken before every apply")
			}
			var m *apply.Manifest
			if len(args) == 2 {
				for _, s := range snaps {
					if s.Name() == args[1] || strings.HasPrefix(s.Name(), args[1]) {
						m = s
						break
					}
				}
				if m == nil {
					return &UsageError{Msg: "no snapshot " + args[1] + " of " + in.Name}
				}
			} else {
				if err := ui.Need("snapshot", "<snapshot> (see the list with --dry-run off a terminal: ngitool rollback "+in.ID+" <name>)"); err != nil {
					return err
				}
				var opts []ui.Option
				for i, s := range snaps {
					label := s.At.Local().Format("Jan 2 15:04:05")
					if i == 0 {
						label += "  " + ui.Muted("(before the last change)")
					}
					opts = append(opts, ui.Option{Value: s.Name(), Label: label, Hint: "before: " + s.Summary + " · " + s.Changes})
				}
				v, err := ui.Select(ui.SelectOpts{Title: "Restore " + in.Name + " to how it was…", Options: opts, Filter: len(opts) > 8})
				if err != nil {
					return err
				}
				for _, s := range snaps {
					if s.Name() == v {
						m = s
					}
				}
			}
			old, err := apply.SnapshotState(m)
			if err != nil {
				return err
			}
			ui.Plan("Roll back "+in.Name, [][2]string{
				{"Snapshot", m.Name() + ui.Muted("  "+m.At.Local().Format("2006-01-02 15:04:05"))},
				{"Taken before", m.Summary + ui.Muted("  ("+m.Changes+")")},
				{"Routes then", strconv.Itoa(len(old.RoutesOn(in.ID))) + ui.Muted("  now "+strconv.Itoa(len(st.RoutesOn(in.ID))))},
			})
			var probe []string
			for _, r := range old.RoutesOn(in.ID) {
				probe = append(probe, r.ID)
			}
			_, err = e.change(cmd.Context(), rep, in, f, "rollback to "+m.Name(), probe, func(next *model.State) error {
				*next = *keepOthers(next, old, in.ID)
				return nil
			}, func(c *apply.Change) { c.Snapshot = m })
			return err
		},
	}
	f.add(c)
	return c
}

// keepOthers is the snapshot's state for one instance and today's state
// for every other: a rollback of one instance must not undo the rest.
func keepOthers(now, then *model.State, id string) *model.State {
	out := now.Clone()
	for _, r := range out.RoutesOn(id) {
		out.RemoveRoute(r.ID)
	}
	for _, p := range out.PoolsOn(id) {
		out.RemovePool(p.Name)
	}
	out.Routes = append(out.Routes, then.RoutesOn(id)...)
	out.Pools = append(out.Pools, then.PoolsOn(id)...)
	insts := out.Instances[:0]
	for _, a := range out.Instances {
		if a.ID != id {
			insts = append(insts, a)
		}
	}
	if a := then.Adopted(id); a != nil {
		insts = append(insts, *a)
	} else if a := now.Adopted(id); a != nil {
		insts = append(insts, *a) // the snapshot predates adopt: keep the record so its files can be found
	}
	out.Instances = insts
	return out
}

func testCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use: "test [instance]", Short: "run nginx -t on an instance", Args: maxArgs(1),
		Annotations: map[string]string{annGroup: "apply", annSynopsis: "test [instance]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			rep := e.freshReport(cmd.Context(), true)
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			in, err := e.instanceFor(rep, st, id, false)
			if err != nil {
				return err
			}
			res, err := discover.Test(cmd.Context(), newScanEnv(e), in)
			if err != nil {
				return err
			}
			if res.OK {
				ui.Done("nginx -t: the config of " + in.Name + " is valid")
				return nil
			}
			ui.Fail("nginx -t: the config of " + in.Name + " is invalid")
			for _, l := range strings.Split(res.Output, "\n") {
				ui.Plain("    " + ui.Muted(l))
			}
			if res.File != "" {
				if hp, err := render.HostPath(in, res.File, ""); err == nil {
					showLine(hp, res.File, res.Line)
				}
			}
			return &ExitError{Code: ExitFail}
		},
	}
	return c
}

// showLine prints a file:line with two lines around it, the line marked.
func showLine(hostPath, name string, line int) {
	content := mustRead(hostPath)
	if content == "" {
		return
	}
	who := "hand-written"
	if render.Parse(content).Managed {
		who = "written by NgiTool"
	}
	ui.Plain("")
	ui.Plain("  " + ui.Bold(name+":"+strconv.Itoa(line)) + "  " + ui.Muted("("+who+")"))
	lines := strings.Split(content, "\n")
	for i := max(1, line-2); i <= min(len(lines), line+2); i++ {
		num := fmt.Sprintf("%5d ", i)
		if i == line {
			ui.Plain("  " + ui.Err(ui.SymPointer+num+lines[i-1]))
		} else {
			ui.Plain("   " + ui.Muted(num) + lines[i-1])
		}
	}
}

func reloadCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use: "reload [instance]", Short: "nginx -t, then reload an instance", Args: maxArgs(1),
		Annotations: map[string]string{annGroup: "apply", annSynopsis: "reload [instance]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			rep := e.freshReport(cmd.Context(), true)
			id := ""
			if len(args) > 0 {
				id = args[0]
			}
			in, err := e.instanceFor(rep, st, id, false)
			if err != nil {
				return err
			}
			if !in.Caps.Reload.OK {
				return errors.New("cannot reload " + in.Name + ": " + in.Caps.Reload.Reason)
			}
			drv := discover.Driver(newScanEnv(e), in)
			return e.withLock(cmd.Context(), true, func() error {
				res, err := drv.Test(cmd.Context())
				if err != nil {
					return err
				}
				if !res.OK {
					return errors.New("nginx -t fails, so nothing was reloaded (CONF-03)\n" + res.Output)
				}
				return ui.Task("Reloading "+in.Name, func(*ui.TaskCtl) error { return drv.Reload(cmd.Context()) })
			})
		},
	}
	return c
}
