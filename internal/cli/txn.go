package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// txFlags are the flags every command that changes nginx takes.
type txFlags struct {
	dryRun  bool
	onDrift string
}

func (f *txFlags) add(c *cobra.Command) {
	c.Flags().BoolVar(&f.dryRun, "dry-run", false, "show the diff and stop: nothing is written (APPLY-08)")
	c.Flags().StringVar(&f.onDrift, "on-drift", "", "when a managed file was edited by hand: overwrite, adopt or abort (APPLY-05)")
}

// newDeps builds the outside world of a transaction; tests replace it.
var newDeps = func(e *env, in *discover.Instance) apply.Deps {
	senv := newScanEnv(e)
	d := apply.Deps{
		Paths:  e.paths,
		Driver: discover.Driver(senv, in),
		Prober: apply.HTTPProber{},
		Ask:    cliAsker{},
		Retain: retain(e),
		Settle: 400 * time.Millisecond,
	}
	if in.Kind != discover.KindHost && in.Container != "" {
		name := in.Container
		d.Logs = func(ctx context.Context, since time.Time) string {
			res := senv.Run.Run(ctx, "docker", []string{"logs", "--since", since.UTC().Format(time.RFC3339Nano), name}, execx.Opts{Timeout: 10 * time.Second})
			return res.Stderr + "\n" + res.Stdout
		}
	}
	return d
}

func retain(e *env) int {
	if cfg, err := state.LoadConfig(e.paths); err == nil && cfg.Snapshots > 0 {
		return cfg.Snapshots
	}
	return apply.DefaultRetain
}

// cliAsker asks on the terminal and names the flag off one.
type cliAsker struct{}

func (cliAsker) Confirm(question, note string) (bool, error) {
	if err := ui.Need("confirmation", "--yes"); err != nil {
		return false, err
	}
	return ui.Confirm(question, note, true)
}

func (cliAsker) Drift(files []string, canAdopt bool) (string, error) {
	if err := ui.Need("a choice for the hand-edited files", "--on-drift overwrite|adopt|abort"); err != nil {
		return "", err
	}
	adopt := ui.Option{Value: apply.DriftAdopt, Label: "Keep the edit", Hint: "adopt the added lines into state as extra directives"}
	if !canAdopt {
		adopt.Disabled = "the entry file and the shared snippet hold no route or pool to keep it in"
	}
	return ui.Select(ui.SelectOpts{
		Title: fmt.Sprintf("%d managed %s edited by hand. What now?", len(files), either(len(files), "file was", "files were")),
		Note:  "APPLY-05",
		Options: []ui.Option{
			{Value: apply.DriftOverwrite, Label: "Overwrite", Hint: "NgiTool's version comes back; the edit is lost (it is in the snapshot)"},
			adopt,
			{Value: apply.DriftAbort, Label: "Abort", Hint: "change nothing"},
		},
	})
}

func either(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// freshReport scans now: a change is planned against what runs now.
func (e *env) freshReport(ctx context.Context, quiet bool) *discover.Report {
	return e.report(ctx, true, !quiet && ui.IsTTY())
}

// change is the one path from a command to nginx: under the lock it loads
// state, applies mut to a copy, validates, and runs the transaction.
func (e *env) change(ctx context.Context, rep *discover.Report, in *discover.Instance, f txFlags, summary string, probe []string,
	mut func(next *model.State) error, opts ...func(*apply.Change)) (*apply.Result, error) {
	var res *apply.Result
	err := e.withLock(ctx, true, func() error {
		before, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		next := before.Clone()
		if err := mut(next); err != nil {
			return err
		}
		c := apply.Change{Report: rep, Instance: in, Before: before, Next: next, Summary: summary, Probe: probe}
		for _, o := range opts {
			o(&c)
		}
		res, err = apply.Run(ctx, newDeps(e, in), c, apply.Opts{Yes: e.yes, DryRun: f.dryRun, OnDrift: f.onDrift})
		if errors.Is(err, apply.ErrAborted) {
			return errCancelled
		}
		return err
	})
	if err == nil && res != nil && !f.dryRun && !res.NoChange {
		msg := "Applied to " + in.Name + ui.Muted(" · "+res.Summary())
		if res.Snapshot != nil {
			msg += ui.Muted(" · snapshot " + res.Snapshot.Name())
		}
		ui.Done(msg)
	}
	return res, err
}

// validate checks the routes and pools a change touched. Warnings are
// printed; the first error is returned, naming its edge-case ID.
func validate(next *model.State, rep *discover.Report, routes, pools []string) error {
	var ps model.Problems
	for _, name := range pools {
		if p := next.Pool(name); p != nil {
			ps = append(ps, model.CheckPool(next, rep, p)...)
		}
	}
	for _, id := range routes {
		if r := next.Route(id); r != nil {
			ps = append(ps, model.CheckRoute(next, rep, r)...)
		}
	}
	showProblems(ps.Others())
	if errs := ps.Errors(); len(errs) > 0 {
		return problemErr(errs[0])
	}
	return nil
}

// problemErr is a refused change: the message, the edge-case ID muted, and
// what to do instead.
func problemErr(p model.Problem) error {
	msg := strings.TrimSpace(strings.TrimSuffix(p.Msg, "("+p.Code+")"))
	if p.Code != "" {
		msg += " " + ui.Muted("("+p.Code+")")
	}
	if p.Fix != "" {
		msg += "\n" + ui.SymArrow + " " + p.Fix
	}
	return errors.New(msg)
}

// codeErr turns a model error (a Problem or plain) into a CLI error.
func codeErr(err error) error {
	var p model.Problem
	if errors.As(err, &p) {
		return problemErr(p)
	}
	return err
}

func showProblems(ps model.Problems) {
	seen := map[string]bool{}
	for _, p := range ps {
		if seen[p.Code+p.Msg] {
			continue
		}
		seen[p.Code+p.Msg] = true
		msg := strings.TrimSpace(strings.TrimSuffix(p.Msg, "("+p.Code+")"))
		if p.Code != "" {
			msg += " " + ui.Muted("("+p.Code+")")
		}
		if p.Level == model.LevelNote {
			ui.Info(msg)
		} else {
			ui.Warning(msg)
		}
		if p.Fix != "" {
			ui.Hint(ui.SymArrow + " " + p.Fix)
		}
	}
}

// instanceFor picks the instance a command acts on: --instance, else the
// configured front door, else the scan's front door, else the only
// adopted one; asks when that is not enough.
func (e *env) instanceFor(rep *discover.Report, st *model.State, flag string, adoptedOnly bool) (*discover.Instance, error) {
	if flag != "" {
		in := lookupInstance(rep, flag)
		if in == nil {
			return nil, &UsageError{Msg: "no instance " + flag + " — see: ngitool instances"}
		}
		if adoptedOnly && st.Adopted(in.ID) == nil {
			return nil, fmt.Errorf("%s is not adopted yet\n%s run: ngitool instance adopt %s", in.Name, ui.SymArrow, in.ID)
		}
		return in, nil
	}
	var cands []*discover.Instance
	for i := range rep.Instances {
		in := &rep.Instances[i]
		if !adoptedOnly || st.Adopted(in.ID) != nil {
			cands = append(cands, in)
		}
	}
	front := rep.FrontDoor
	if cfg, err := state.LoadConfig(e.paths); err == nil && cfg.FrontDoor != "" {
		front = cfg.FrontDoor
	}
	for _, in := range cands {
		if in.ID == front {
			return in, nil
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	if len(cands) == 0 {
		if adoptedOnly {
			return nil, fmt.Errorf("no instance is adopted yet\n%s adopt one first: ngitool instance adopt <id> (see ngitool instances)", ui.SymArrow)
		}
		return nil, errors.New("no nginx found — run: ngitool scan")
	}
	if err := ui.Need("instance", "--instance ID"); err != nil {
		return nil, err
	}
	var opts []ui.Option
	for _, in := range cands {
		opts = append(opts, instanceOption(in))
	}
	id, err := ui.Select(ui.SelectOpts{Title: "Which nginx?", Options: opts})
	if err != nil {
		return nil, err
	}
	return rep.Find(id), nil
}

// lookupInstance matches an id, a name, a container name or a unique id
// prefix; nil when nothing (or more than one) matches.
func lookupInstance(rep *discover.Report, s string) *discover.Instance {
	in, err := findInstance(rep, s)
	if err != nil {
		return nil
	}
	return in
}

func instanceOption(in *discover.Instance) ui.Option {
	o := ui.Option{Value: in.ID, Label: kindIcon(in.Kind) + " " + in.Name, Hint: strings.TrimSpace(in.ID + " · " + dashPlain(in.Version))}
	if in.FrontDoor {
		o.Badge = "front door"
	}
	return o
}

func dashPlain(s string) string {
	if s == "" {
		return "–"
	}
	return s
}

// routeIDs are every route id, sorted (completion and selects).
func routeIDs(st *model.State) []string {
	var ids []string
	for _, r := range st.Routes {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}
