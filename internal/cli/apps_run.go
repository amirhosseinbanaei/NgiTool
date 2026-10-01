package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// holdInterrupt makes Ctrl-C reach only the child (logs -f): the child
// stops and the menu comes back instead of NgiTool exiting.
var holdInterrupt atomic.Bool

// actFlags are every lifecycle option as a flag (off a terminal there is
// no checklist).
type actFlags struct {
	build, pull, noDeps, removeOrphans, noCache, volumes bool
	noFollow, timestamps, verbose                        bool
	tail                                                 int
	since                                                string
}

func (f *actFlags) opts(act compose.Action) compose.Opts {
	o := compose.Opts{Build: f.build, NoDeps: f.noDeps, RemoveOrphans: f.removeOrphans, NoCache: f.noCache, Volumes: f.volumes,
		NoFollow: f.noFollow, Timestamps: f.timestamps, Tail: f.tail, Since: f.since}
	if act.ID == compose.Rebuild {
		o.PullBase = f.pull
	} else {
		o.Pull = f.pull
	}
	return o
}

func optSelected(o compose.Opts, key string) bool {
	switch key {
	case "build":
		return o.Build
	case "pull":
		return o.Pull
	case "no-deps":
		return o.NoDeps
	case "remove-orphans":
		return o.RemoveOrphans
	case "no-cache":
		return o.NoCache
	case "pull-base":
		return o.PullBase
	case "volumes":
		return o.Volumes
	case "no-follow":
		return o.NoFollow
	case "timestamps":
		return o.Timestamps
	}
	return false
}

func appActionCmd(e *env, act compose.Action) *cobra.Command {
	f := &actFlags{}
	syn := act.ID + " [app] [service…]"
	if !act.Services {
		syn = act.ID + " [app]"
	}
	var flagNames []string
	for _, k := range act.Options {
		flagNames = append(flagNames, compose.Options[k].Flag)
	}
	if act.ID == compose.Logs {
		flagNames = append(flagNames, "--tail N", "--since T")
	}
	if len(flagNames) > 0 {
		syn += " [" + strings.Join(flagNames, "] [") + "]"
	}
	c := &cobra.Command{
		Use:   act.ID + " [app] [service…]",
		Short: act.Hint,
		Long: act.What + "\nAffects: " + act.Affects + "\nUndo: " + act.Undo + "\n\n" +
			"On a terminal: the explain panel, a service checklist (all by default), the options with one line\n" +
			"each, then \"Will run:\" with the exact command. Off a terminal every option is a flag and --yes\n" +
			"confirms; --volumes also needs --force.",
		Annotations: map[string]string{annSynopsis: syn},
		Args:        cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.runAction(cmd, act, args, f)
		},
	}
	fl := c.Flags()
	for _, k := range act.Options {
		o := compose.Options[k]
		name := strings.TrimPrefix(o.Flag, "--")
		switch k {
		case "build":
			fl.BoolVar(&f.build, name, false, o.Hint)
		case "pull", "pull-base":
			fl.BoolVar(&f.pull, name, false, o.Hint)
		case "no-deps":
			fl.BoolVar(&f.noDeps, name, false, o.Hint)
		case "remove-orphans":
			fl.BoolVar(&f.removeOrphans, name, false, o.Hint+" (DOCK-14)")
		case "no-cache":
			fl.BoolVar(&f.noCache, name, false, o.Hint)
		case "volumes":
			fl.BoolVarP(&f.volumes, name, "v", false, o.Hint+" — needs --yes --force off a terminal")
		case "no-follow":
			fl.BoolVar(&f.noFollow, name, false, o.Hint)
		case "timestamps":
			fl.BoolVar(&f.timestamps, name, false, o.Hint)
		}
	}
	if act.ID == compose.Logs {
		fl.IntVar(&f.tail, "tail", 100, "lines of history to show first")
		fl.StringVar(&f.since, "since", "", "only lines newer than this, e.g. 10m or 2026-10-01T12:00:00")
	}
	if act.ID != compose.PS && act.ID != compose.Logs {
		fl.BoolVar(&f.verbose, "verbose", false, "raw compose output instead of the condensed per-service view")
	}
	return c
}

// runAction is every lifecycle command: explain, services, options, the
// exact command, confirm, stream, summary.
func (e *env) runAction(cmd *cobra.Command, act compose.Action, args []string, f *actFlags) error {
	ctx := cmd.Context()
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	a, err := pickApp(st, args, act.Label+" — which app?")
	if err != nil {
		return err
	}
	var services []string
	if len(args) > 1 {
		if !act.Services {
			return &UsageError{Msg: act.ID + " acts on the whole app, not on services", Cmd: "ngitool app " + act.ID}
		}
		services = args[1:]
	}
	if miss := a.Missing(); len(miss) > 0 {
		return problemErr(model.Problem{Code: "DOCK-10", Msg: a.Name + "'s files are gone: " + strings.Join(miss, ", "),
			Fix: "re-link it: ngitool app unlink " + a.Name + " && ngitool app link <new path>"})
	}
	b, err := e.composeBin(ctx)
	if err != nil {
		return err
	}
	p := a.Compose()
	cfg, err := e.readProject(ctx, b, p, false)
	if err != nil {
		return err
	}
	for _, s := range services {
		if cfg.Service(s) == nil {
			var names []string
			for _, x := range cfg.Services {
				names = append(names, x.Name)
			}
			return &UsageError{Msg: a.Name + " has no service " + s + " — it has " + strings.Join(names, ", ")}
		}
	}
	tty := ui.CanPrompt()
	if act.ID == compose.PS {
		return e.appSummary(ctx, b, *a, nil, true)
	}
	ui.Heading(act.Label+" · "+a.Name, a.WorkingDir)
	ui.Explain(act.What, act.Affects, act.Undo)

	// Services: all by default.
	if act.Services && len(services) == 0 && tty && len(cfg.Services) > 1 {
		var opts []ui.Option
		var all []string
		for _, s := range cfg.Services {
			opts = append(opts, ui.Option{Value: s.Name, Label: s.Name, Hint: serviceHint(s, *a)})
			all = append(all, s.Name)
		}
		for {
			picked, err := ui.MultiSelect(ui.MultiOpts{Title: "Which services?", Note: "all by default", Options: opts, Selected: all})
			if err != nil {
				return err
			}
			if len(picked) == 0 {
				ui.Warning("pick at least one service")
				continue
			}
			if len(picked) < len(all) {
				services = picked
			}
			break
		}
	}

	// Options: a checklist with one line each, ticked from the flags.
	o := f.opts(act)
	if tty && len(act.Options) > 0 {
		var opts []ui.Option
		var pre []string
		for _, k := range act.Options {
			op := compose.Options[k]
			row := ui.Option{Value: k, Label: op.Label, Hint: op.Flag + " — " + op.Hint}
			if op.Danger != "" {
				row.Badge = "danger"
				row.Hint = op.Flag + " — " + op.Danger
			}
			opts = append(opts, row)
			if optSelected(o, k) {
				pre = append(pre, k)
			}
		}
		picked, err := ui.MultiSelect(ui.MultiOpts{Title: "Options", Note: "none by default", Options: opts, Selected: pre})
		if err != nil {
			return err
		}
		o = compose.Opts{Tail: o.Tail, Since: o.Since}
		for _, k := range picked {
			o.Set(k)
		}
	}

	// pull skips services that build; rebuild only builds those (DOCK-13).
	switch act.ID {
	case compose.Pull, compose.Rebuild:
		want := act.ID == compose.Rebuild
		var keep, skip []string
		for _, s := range cfg.Services {
			if len(services) > 0 && !contains(services, s.Name) {
				continue
			}
			if s.Build == want {
				keep = append(keep, s.Name)
			} else {
				skip = append(skip, s.Name)
			}
		}
		if len(keep) == 0 {
			if want {
				return errors.New("no chosen service has a build: section — use: ngitool app pull " + a.Name)
			}
			return errors.New("every chosen service has a build: section — use: ngitool app rebuild " + a.Name)
		}
		if len(skip) > 0 {
			why := "they have a build: section — rebuild builds them"
			if want {
				why = "image-only — pull updates them"
			}
			ui.Info("skipped: " + strings.Join(skip, ", ") + ui.Muted(" ("+why+")"))
			services = keep
		}
	}
	steps := compose.Steps(act.ID, o, services)
	printWillRun(b, p, steps)

	switch {
	case act.ID == compose.Logs:
	case o.Volumes:
		vols := projectVolumes(ctx, a.Project)
		body := "no named volumes found for " + a.Project
		if len(vols) > 0 {
			body = strings.Join(vols, "\n")
		}
		ui.Callout(ui.KindDanger, "down --volumes deletes these volumes and the data in them", body)
		if err := ui.SureDanger(e.yes, e.force, "Delete the volumes of "+a.Project+"?", a.Project); err != nil {
			return err
		}
	case o.RemoveOrphans && tty && !(e.yes && e.force):
		ui.Callout(ui.KindDanger, "--remove-orphans", compose.Options["remove-orphans"].Danger+" (DOCK-14)")
		if err := ui.ConfirmTyped("Remove orphan containers of "+a.Project+"?", a.Project); err != nil {
			return err
		}
	default:
		if ok, err := ui.Sure(e.yes, "Run it?", ""); err != nil || !ok {
			if err == nil {
				err = errCancelled
			}
			return err
		}
	}
	if act.ID == compose.Logs {
		holdInterrupt.Store(true)
		defer holdInterrupt.Store(false)
		res := appRunner.Run(ctx, b.Name, compose.Argv(b, p, steps[0]), execx.Opts{Dir: p.Dir, Scrub: execx.ComposeScrub, Out: newPrefixWriter(a.Project, true)})
		if res.Code != 0 && res.Code != 130 && res.Code != 2 && ctx.Err() == nil {
			return errors.New("docker compose logs failed\n" + execx.Tail(res, 3))
		}
		return nil
	}
	return e.runSteps(ctx, b, *a, act.ID, steps, services, f.verbose)
}

// serviceHint is a service's checklist hint: image or build, ports, network.
func serviceHint(s compose.Service, a compose.App) string {
	parts := []string{s.Image}
	if s.Build {
		parts[0] = "build"
	}
	if len(s.Ports) > 0 {
		var ps []string
		for _, p := range s.Ports {
			ps = append(ps, strconv.Itoa(p))
		}
		parts = append(parts, "port "+strings.Join(ps, ","))
	}
	if at, ok := a.Attached[s.Name]; ok {
		parts = append(parts, "on "+at.Network+" as "+at.Alias)
	}
	if s.Replicas > 1 {
		parts = append(parts, strconv.Itoa(s.Replicas)+" replicas")
	}
	if s.OneOff() {
		parts = append(parts, "one-off")
	}
	return strings.Join(parts, " · ")
}

// printWillRun is the "Will run:" block: every command exactly as run.
func printWillRun(b compose.Bin, p compose.Project, steps [][]string) {
	ui.Plain(ui.Bold("Will run:"))
	for _, s := range steps {
		ui.Plain("  " + ui.Key(compose.CommandLine(b, p, s)))
	}
	if p.Override != "" {
		ui.Hint("the last -f is NgiTool's override: it keeps the attached services on their nginx network (DOCK-07)")
	}
}

func projectVolumes(ctx context.Context, project string) []string {
	res := appRunner.Run(ctx, "docker", []string{"volume", "ls", "--filter", "label=" + compose.LabelProject + "=" + project, "--format", "{{.Name}}"}, execx.Opts{Timeout: 10 * time.Second})
	var out []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// runSteps streams each step's output (condensed per service unless
// verbose), records the app's last action and prints the summary table.
func (e *env) runSteps(ctx context.Context, b compose.Bin, a compose.App, action string, steps [][]string, services []string, verbose bool) error {
	p := a.Compose()
	var failed error
	for _, s := range steps {
		ui.Plain(ui.Accent(ui.SymPointer) + " " + ui.Muted(strings.Join(s, " ")))
		w := newPrefixWriter(a.Project, verbose)
		res := appRunner.Run(ctx, b.Name, compose.Argv(b, p, s), execx.Opts{Dir: p.Dir, Scrub: execx.ComposeScrub, Out: w})
		w.Flush()
		if res.Code != 0 {
			failed = fmt.Errorf("docker compose %s failed (exit %d)\n%s", s[0], res.Code, execx.Tail(res, 6))
			break
		}
	}
	_, _ = e.updateApp(ctx, a.Name, func(x *compose.App) error {
		x.Last = &compose.LastAction{Action: action, At: stamp(), OK: failed == nil, Services: services}
		return nil
	})
	if failed != nil {
		return failed
	}
	ui.Done(a.Name + ": " + action + " finished")
	return e.appSummary(ctx, b, a, services, false)
}

// appSummary is the table after an action (and app ps): container, state,
// health, on the proxy network, routes to it, and a probe of each route
// through its instance.
func (e *env) appSummary(ctx context.Context, b compose.Bin, a compose.App, services []string, ps bool) error {
	p := a.Compose()
	res := appRunner.Run(ctx, b.Name, compose.Argv(b, p, compose.Steps(compose.PS, compose.Opts{}, nil)[0]), p.Opts())
	if res.Code != 0 {
		return errors.New("docker compose ps failed\n" + execx.Tail(res, 3))
	}
	ctrs := compose.ParsePSJSON(res.Stdout)
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	if cur := st.App(a.Name); cur != nil {
		a = *cur
	}
	routes := st.RoutesToApp(a, "")
	probes := map[string]apply.Probe{}
	instNets := map[string][]string{}
	if len(routes) > 0 {
		rep := e.report(ctx, false, false)
		h := apply.LoadHealth(e.paths.Cache)
		for _, id := range routes {
			r := st.Route(id)
			in := lookupInstance(rep, r.Instance)
			if in == nil {
				continue
			}
			instNets[id] = in.Networks
			facts := render.GatherFacts(rep, in, st.Adopted(in.ID))
			ts, skipped := apply.ProbeTargets(st, in, []string{id}, facts.HTTPPort, facts.HTTPSPort)
			for _, sk := range skipped {
				probes[id] = sk
			}
			for _, t := range ts {
				pr := probeWith(ctx, t)
				probes[id] = pr
				h.Routes[id] = pr
			}
		}
		h.Save(e.paths.Cache)
	}
	rows := [][]string{}
	for _, c := range ctrs {
		if len(services) > 0 && !contains(services, c.Service) {
			continue
		}
		state := ui.OK(ui.SymDot) + " " + c.State
		if !c.Running {
			state = ui.Warn(ui.SymDot) + " " + c.State
		}
		health := dashPlain(c.Health)
		switch c.Health {
		case "healthy":
			health = ui.OK(c.Health)
		case "unhealthy":
			health = ui.Err(c.Health)
		}
		ids := st.RoutesToApp(a, c.Service)
		onNet := ui.Muted("–")
		if at, ok := a.Attached[c.Service]; ok {
			onNet = yesNo(contains(c.Networks, at.Network))
		} else if len(ids) > 0 {
			shared := false
			for _, id := range ids {
				for _, n := range instNets[id] {
					shared = shared || contains(c.Networks, n)
				}
			}
			onNet = yesNo(shared)
		}
		var probe []string
		for _, id := range ids {
			pr, ok := probes[id]
			switch {
			case !ok:
				probe = append(probe, ui.Muted(ui.SymRing+" "+id))
			case pr.Skipped != "":
				probe = append(probe, ui.Muted(ui.SymRing+" "+id+" skipped"))
			case pr.Reached:
				probe = append(probe, ui.OK(ui.SymDot+" "+strconv.Itoa(pr.Status))+" "+id)
			default:
				code := pr.Err
				if pr.Status > 0 {
					code = strconv.Itoa(pr.Status)
				}
				probe = append(probe, ui.Err(ui.SymErr+" "+code)+" "+id)
			}
		}
		rows = append(rows, []string{c.Name, state, health, onNet, strconv.Itoa(len(ids)), dashPlain(strings.Join(probe, "  "))})
	}
	if len(rows) == 0 {
		ui.Info("no containers" + ui.Muted(" — start it: ngitool app up "+a.Name))
		return nil
	}
	ui.PrintTable(rows, ui.TableOpts{Header: []string{"CONTAINER", "STATE", "HEALTH", "PROXY NET", "ROUTES", "PROBE"}})
	if !ps {
		ctrsAll, _ := appContainers(ctx)
		warnDetached([]appJSON{appView(st, a, ctrsAll)})
	}
	return nil
}

// probeWith requests one route; tests replace it.
var probeWith = func(ctx context.Context, t apply.Target) apply.Probe {
	return apply.HTTPProber{Timeout: 8 * time.Second}.Probe(ctx, t)
}

func yesNo(b bool) string {
	if b {
		return ui.OK("yes")
	}
	return ui.Err("no")
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// prefixWriter prints compose output a line at a time, each line behind
// its service's coloured name (DOCK-13: build output streams live).
// Condensed drops BuildKit's bookkeeping; verbose prints every line.
type prefixWriter struct {
	mu      sync.Mutex
	project string
	verbose bool
	buf     bytes.Buffer
	colours map[string]int
	width   int
	last    string
}

func newPrefixWriter(project string, verbose bool) *prefixWriter {
	return &prefixWriter{project: project, verbose: verbose, colours: map[string]int{}, width: 8}
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		i := bytes.IndexAny(data, "\n\r")
		if i < 0 {
			break
		}
		line := string(data[:i])
		w.buf.Next(i + 1)
		w.line(line)
	}
	return len(p), nil
}

// Flush prints what is left without a newline.
func (w *prefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.line(w.buf.String())
		w.buf.Reset()
	}
}

func (w *prefixWriter) line(raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	l := compose.ParseLine(w.project, raw)
	if !w.verbose && !l.Keep {
		return
	}
	text := l.Text
	if w.verbose && l.Service == "" {
		text = raw
	}
	out := ""
	if l.Service != "" {
		n, ok := w.colours[l.Service]
		if !ok {
			n = len(w.colours)
			w.colours[l.Service] = n
		}
		w.width = max(w.width, ui.Width(l.Service))
		out = ui.Series(n, ui.Pad(l.Service, w.width)) + " " + ui.Muted(ui.SymBar) + " " + text
	} else {
		out = strings.Repeat(" ", w.width) + " " + ui.Muted(ui.SymBar) + " " + ui.Muted(text)
	}
	if out == w.last {
		return
	}
	w.last = out
	ui.Plain(out)
}
