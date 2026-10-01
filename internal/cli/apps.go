package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// appRunner runs docker and docker compose for app commands; tests
// replace it with recorded output.
var appRunner execx.Runner = execx.System

func appCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "app",
		Short: "link compose projects, then start, rebuild and fix them",
		Long: "link       pick compose projects from a checklist of every one on this server, then serve one\n" +
			"scan       the same checklist, linking several at once (--all: every root-owned one)\n" +
			"ls, show   linked apps with their state and whether they are on their nginx network\n" +
			"attach     put a service on an nginx network through NgiTool's override\n" +
			"up, restart, recreate, rebuild, pull, stop, down, logs, ps\n" +
			"           each explained first, then the exact docker compose command, then its output\n" +
			"fix        recreate a detached app with its override (DOCK-07)\n" +
			"unlink     forget an app, with the routes that point at it\n\n" +
			"Every compose run passes -p, --project-directory, every -f and NgiTool's override last, with\n" +
			"COMPOSE_PROJECT_NAME and COMPOSE_FILE scrubbed. A project's own files are never edited.",
		Annotations: map[string]string{annGroup: "apps", annSynopsis: "app link|scan|ls|show|up|rebuild|…"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(appLinkCmd(e, false), appLinkCmd(e, true), appLsCmd(e), appShowCmd(e), appAttachCmd(e))
	for _, a := range compose.Actions {
		c.AddCommand(appActionCmd(e, a))
	}
	c.AddCommand(appFixCmd(e), appUnlinkCmd(e), appMenuCmd(e))
	return c
}

// composeBin finds compose once per run and warns once when only v1
// exists (DOCK-05).
func (e *env) composeBin(ctx context.Context) (compose.Bin, error) {
	if e.bin != nil {
		return *e.bin, nil
	}
	b, err := compose.Detect(ctx, appRunner)
	if err != nil {
		return b, err
	}
	if b.V1 {
		ui.Warning(compose.V1Warning)
	}
	e.bin = &b
	return b, nil
}

// pickApp is the app named by args[0], or a select of the linked apps.
func pickApp(st *model.State, args []string, title string) (*compose.App, error) {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		if len(st.Apps) == 0 {
			return nil, errors.New("no app is linked yet\n" + ui.SymArrow + " link one: ngitool app link")
		}
		if err := ui.Need("app", "<app>"); err != nil {
			return nil, err
		}
		var opts []ui.Option
		for _, a := range st.Apps {
			opts = append(opts, ui.Option{Value: a.Name, Label: a.Name, Hint: a.WorkingDir})
		}
		var err error
		if name, err = ui.Select(ui.SelectOpts{Title: title, Options: opts}); err != nil {
			return nil, err
		}
	}
	a := st.App(name)
	if a == nil {
		return nil, &UsageError{Msg: "no app " + name + " — see: ngitool app ls"}
	}
	return a, nil
}

// appMember fills a compose-service member that belongs to a linked app:
// its app, its project/service ref and, once attached, the alias the
// override gives it (DOCK-16).
func appMember(st *model.State, m *model.Member) error {
	if m.Kind != model.KindService {
		return nil
	}
	if m.App != "" {
		a := st.App(m.App)
		if a == nil {
			return &UsageError{Msg: "no linked app " + m.App + " — see: ngitool app ls"}
		}
		m.Ref = a.Project + m.Ref // "/web" → "shop/web"
	} else if proj, _, _ := strings.Cut(m.Ref, "/"); st.AppOfProject(proj) != nil {
		m.App = st.AppOfProject(proj).Name
	}
	if m.App == "" {
		return nil
	}
	_, svc, _ := strings.Cut(m.Ref, "/")
	if at, ok := st.App(m.App).Attached[svc]; ok {
		m.Host = at.Alias
	}
	return nil
}

// writeOverride renders the app's override, or removes it when nothing
// needs one any more. Only NgiTool's own file is ever written.
func (e *env) writeOverride(a *compose.App) error {
	if !a.NeedsOverride() {
		if a.Override != "" {
			if err := os.Remove(a.Override); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		a.Override = ""
		return nil
	}
	a.Override = compose.OverridePath(e.paths.Overrides, a.Name)
	return state.WriteFile(a.Override, []byte(compose.RenderOverride(*a)), state.FileMode)
}

// updateApp changes one app under the lock and rewrites its override.
func (e *env) updateApp(ctx context.Context, name string, fn func(a *compose.App) error) (*compose.App, error) {
	var out compose.App
	err := e.withLock(ctx, true, func() error {
		st, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		a := st.App(name)
		if a == nil {
			return errors.New("app " + name + " is no longer linked")
		}
		if err := fn(a); err != nil {
			return err
		}
		if err := e.writeOverride(a); err != nil {
			return err
		}
		out = *a
		return model.Save(e.paths, st)
	})
	return &out, err
}

// readProject runs `compose config`; when that fails it shows compose's
// last lines and falls back to the raw YAML (asked on a terminal), marked
// unresolved (DOCK-04).
func (e *env) readProject(ctx context.Context, b compose.Bin, p compose.Project, ask bool) (*compose.Config, error) {
	cfg, err := compose.Read(ctx, appRunner, b, p)
	if err == nil {
		return cfg, nil
	}
	var re *compose.ReadError
	if !errors.As(err, &re) {
		return nil, err
	}
	ui.Warning("docker compose could not read " + p.Name + ":")
	for _, l := range strings.Split(re.Tail, "\n") {
		ui.Hint(l)
	}
	if ask && ui.CanPrompt() {
		ok, aerr := ui.Confirm("Read the files as plain YAML instead?", "unresolved: include/extends/env not applied", true)
		if aerr != nil {
			return nil, aerr
		}
		if !ok {
			return nil, errCancelled
		}
	}
	raw, rerr := compose.ReadRaw(p)
	if rerr != nil {
		return nil, fmt.Errorf("%s: %v", re.Error(), rerr)
	}
	ui.Info("read as plain YAML " + ui.Muted("(unresolved: include/extends/env not applied — DOCK-04)"))
	return raw, nil
}

// appContainers lists compose containers (docker ps); nil when Docker
// cannot be asked, with the reason.
func appContainers(ctx context.Context) ([]compose.Ctr, error) {
	if !execx.Has("docker") && appRunner == execx.System {
		return nil, errors.New("docker is not installed")
	}
	return compose.Containers(ctx, appRunner)
}

// ---- ls / show -------------------------------------------------------------

type appJSON struct {
	compose.App
	Drift    compose.Drift `json:"drift"`
	Services []string      `json:"services"`
	Routes   []string      `json:"routes"`
}

func appView(st *model.State, a compose.App, ctrs []compose.Ctr) appJSON {
	j := appJSON{App: a, Drift: compose.Check(a, ctrs), Routes: st.RoutesToApp(a, "")}
	seen := map[string]bool{}
	for _, c := range ctrs {
		if c.Project == a.Project && !seen[c.Service] {
			seen[c.Service] = true
			j.Services = append(j.Services, c.Service)
		}
	}
	for _, s := range compose.RawSummary(a.Files).Services {
		if !seen[s] {
			seen[s] = true
			j.Services = append(j.Services, s)
		}
	}
	sort.Strings(j.Services)
	if j.Routes == nil {
		j.Routes = []string{}
	}
	return j
}

// netChipOf is the network status as one coloured word.
func netChipOf(d compose.Drift) string {
	switch d.Status {
	case compose.NetOK:
		return ui.OK(ui.SymOK + " ok")
	case compose.NetDetached:
		return ui.Err(ui.SymErr + " detached")
	case compose.NetMissing:
		return ui.Err(ui.SymErr + " missing")
	case compose.NetStopped:
		return ui.Warn(ui.SymRing + " stopped")
	}
	return ui.Muted(ui.SymRing + " not attached")
}

func appStateText(up, total int) string {
	switch {
	case total == 0:
		return ui.Muted(ui.SymRing + " never started")
	case up == 0:
		return ui.Warn(ui.SymDot) + " stopped"
	}
	return ui.OK(ui.SymDot) + " " + strconv.Itoa(up) + "/" + strconv.Itoa(total)
}

// warnDetached prints DOCK-07 for every detached app (app ls, route ls,
// doctor's fix line).
func warnDetached(views []appJSON) {
	for _, v := range views {
		switch v.Drift.Status {
		case compose.NetDetached:
			ui.Plain(ui.Err(ui.SymErr) + " " + v.Name + " " + ui.Err("detached") + ": " + compose.DetachedWhy + " " + ui.Muted("(DOCK-07)"))
			ui.Hint(ui.SymArrow + " fix: ngitool app fix " + v.Name)
		case compose.NetMissing:
			ui.Warning(v.Name + ": " + v.Drift.Why)
		}
	}
}

func appLsCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "ls",
		Short:       "linked apps: state, services, network, routes, owner, path",
		Long:        "One line per linked app. NETWORK is ok, detached (started without NgiTool's override: its routes\nreturn 502, DOCK-07), not attached, stopped, or missing (its files are gone, DOCK-10).",
		Annotations: map[string]string{annSynopsis: "ls [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			ctrs, cerr := appContainers(cmd.Context())
			views := []appJSON{}
			for _, a := range st.Apps {
				views = append(views, appView(st, a, ctrs))
			}
			if asJSON {
				return printJSON(views)
			}
			if len(views) == 0 {
				ui.Info("no app is linked yet")
				ui.Hint("link one: ngitool app link")
				return nil
			}
			if cerr != nil {
				ui.Warning("container state unknown: " + firstLine(cerr.Error()))
			}
			rows := [][]string{}
			for _, v := range views {
				rows = append(rows, []string{v.Name, appStateText(v.Drift.Up, v.Drift.Total), strconv.Itoa(len(v.Services)), netChipOf(v.Drift),
					strconv.Itoa(len(v.Routes)), dashPlain(v.Owner), ui.Muted(v.WorkingDir)})
			}
			ui.PrintTable(rows, ui.TableOpts{Header: []string{"APP", "STATE", "SERVICES", "NETWORK", "ROUTES", "OWNER", "PATH"}})
			warnDetached(views)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func appShowCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "show [app]",
		Short:       "files in order, services with ports and networks, routes, last action",
		Annotations: map[string]string{annSynopsis: "show <app> [--json]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			a, err := pickApp(st, args, "Show which app?")
			if err != nil {
				return err
			}
			ctrs, _ := appContainers(ctx)
			v := appView(st, *a, ctrs)
			var cfg *compose.Config
			if b, err := e.composeBin(ctx); err == nil && len(a.Missing()) == 0 {
				if asJSON {
					cfg, _ = compose.Read(ctx, appRunner, b, a.Compose())
				} else {
					cfg, _ = e.readProject(ctx, b, a.Compose(), false)
				}
			}
			if asJSON {
				return printJSON(struct {
					appJSON
					Config *compose.Config `json:"config,omitempty"`
				}{v, cfg})
			}
			printAppShow(st, v, cfg, ctrs)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func printAppShow(st *model.State, v appJSON, cfg *compose.Config, ctrs []compose.Ctr) {
	a := v.App
	pairs := [][2]string{{"Project", a.Project}, {"Directory", a.WorkingDir}}
	for i, f := range a.Files {
		k := ""
		if i == 0 {
			k = "Files"
		}
		pairs = append(pairs, [2]string{k, strconv.Itoa(i+1) + ". " + f})
	}
	if a.NeedsOverride() {
		pairs = append(pairs, [2]string{"", strconv.Itoa(len(a.Files)+1) + ". " + a.Override + ui.Muted("  NgiTool's override, always last")})
	}
	if len(a.Profiles) > 0 {
		pairs = append(pairs, [2]string{"Profiles", strings.Join(a.Profiles, ", ")})
	}
	if len(a.EnvFiles) > 0 {
		pairs = append(pairs, [2]string{"Env files", strings.Join(a.EnvFiles, ", ")})
	}
	pairs = append(pairs, [2]string{"State", appStateText(v.Drift.Up, v.Drift.Total)}, [2]string{"Network", netChipOf(v.Drift)},
		[2]string{"Owner", dashPlain(a.Owner)}, [2]string{"Linked", a.LinkedAt})
	if l := a.Last; l != nil {
		res := ui.OK("ok")
		if !l.OK {
			res = ui.Err("failed")
		}
		pairs = append(pairs, [2]string{"Last action", l.Action + " " + strings.Join(l.Services, " ") + ui.Muted(" · "+l.At+" · ") + res})
	}
	ui.Plan(a.Name, pairs)
	if cfg != nil {
		rows := [][]string{}
		var public []string
		for _, s := range cfg.Services {
			src := s.Image
			if s.Build {
				src = "build" + ui.Muted(" "+s.Image)
			}
			ports := []string{}
			for _, p := range s.Ports {
				ports = append(ports, strconv.Itoa(p))
			}
			for _, p := range s.Published {
				if p.Public() {
					public = append(public, s.Name+" "+p.String())
				}
			}
			net := strings.Join(s.Networks, ", ")
			if s.NetworkMode != "" {
				net = "network_mode: " + s.NetworkMode
			}
			if at, ok := a.Attached[s.Name]; ok {
				net += ui.Muted(" + ") + ui.OK(at.Network+" as "+at.Alias)
			}
			name := s.Name
			if s.Replicas > 1 {
				name += ui.Muted(" ×" + strconv.Itoa(s.Replicas))
			}
			if s.OneOff() {
				name += ui.Muted(" (one-off)")
			}
			rows = append(rows, []string{name, src, dashPlain(strings.Join(ports, ",")), net, strconv.Itoa(len(st.RoutesToApp(a, s.Name)))})
		}
		ui.Heading("Services", "")
		if cfg.Unresolved {
			ui.Hint("unresolved: include/extends/env not applied (DOCK-04)")
		}
		ui.PrintTable(rows, ui.TableOpts{Header: []string{"SERVICE", "IMAGE", "PORTS", "NETWORKS", "ROUTES"}})
		if len(public) > 0 {
			ui.Warning("published on every address, so reachable without the proxy: " + strings.Join(public, ", ") + " " + ui.Muted("(DOCK-11)"))
			ui.Hint(ui.SymArrow + " bind it to 127.0.0.1 (\"127.0.0.1:8080:80\"), or use expose: and route through nginx")
			if ufwActive() {
				ui.Hint("ufw is active, but Docker's own iptables rules open published ports before ufw sees them (DOCK-12)")
			}
		}
	}
	if len(v.Routes) > 0 {
		ui.Heading("Routes", "")
		for _, id := range v.Routes {
			r := st.Route(id)
			ui.Plain("    " + id + ui.Muted("  on "+r.Instance+" → "+model.Describe(st.Pool(r.Pool))))
		}
	}
	warnDetached([]appJSON{v})
}

// ufwActive reports whether ufw is enabled (DOCK-12); quiet when absent.
func ufwActive() bool {
	b, err := os.ReadFile("/etc/ufw/ufw.conf")
	return err == nil && strings.Contains(string(b), "ENABLED=yes")
}

// ---- attach ----------------------------------------------------------------

// attachService puts service of app on network through NgiTool's override
// (or prints the snippet to paste, how = manual), then offers to recreate
// only that service (cli/src/flows.mjs attachService). now: 1 yes, 0 no,
// -1 ask (--yes answers it). It reports whether the service was recreated.
func (e *env) attachService(ctx context.Context, appName, service, network, how string, now int, verbose bool) (compose.Attach, bool, error) {
	var at compose.Attach
	st, err := model.Load(e.paths)
	if err != nil {
		return at, false, err
	}
	a := st.App(appName)
	if a == nil {
		return at, false, errors.New("no app " + appName)
	}
	b, err := e.composeBin(ctx)
	if err != nil {
		return at, false, err
	}
	cfg, err := e.readProject(ctx, b, a.Compose(), false)
	if err != nil {
		return at, false, err
	}
	s := cfg.Service(service)
	if s == nil {
		return at, false, &UsageError{Msg: "app " + a.Name + " has no service " + service}
	}
	if why := s.Detached(); why != "" {
		fix := "add it as a host port member (port:PORT) if it listens on the host"
		for _, p := range s.Published {
			fix = "it publishes " + p.String() + " — route to it as port:" + strconv.Itoa(p.HostPort)
		}
		return at, false, problemErr(model.Problem{Code: "DOCK-06", Msg: service + " cannot join " + network + ": " + strings.TrimSuffix(why, " (DOCK-06)"), Fix: fix})
	}
	at = compose.Attach{Network: network, Alias: compose.Alias(a.Name, service), Keys: s.NetworkKeys}
	if how == "" {
		how = "override"
		if ui.CanPrompt() {
			if how, err = ui.Select(ui.SelectOpts{
				Title: service + " is not on the " + network + " network. How should it join?",
				Options: []ui.Option{
					{Value: "override", Label: "NgiTool's override", Hint: "recommended — " + compose.OverridePath(e.paths.Overrides, a.Name) + "; the project's files stay untouched"},
					{Value: "manual", Label: "I'll edit the compose file", Hint: "shows the lines to paste into the project"},
				},
			}); err != nil {
				return at, false, err
			}
		}
	}
	at.Manual = how == "manual"
	updated, err := e.updateApp(ctx, a.Name, func(x *compose.App) error {
		if x.Attached == nil {
			x.Attached = map[string]compose.Attach{}
		}
		x.Attached[service] = at
		return nil
	})
	if err != nil {
		return at, false, err
	}
	if at.Manual {
		ui.Heading("Add this to "+a.Files[len(a.Files)-1], "then recreate "+service)
		for _, l := range strings.Split(compose.ManualSnippet(service, at), "\n") {
			ui.Plain("    " + ui.Accent(l))
		}
		ui.Hint("then: ngitool app recreate " + a.Name + " " + service + " — until then its routes answer 502")
		return at, false, nil
	}
	ui.Done("Wrote " + updated.Override + ui.Muted("  ("+service+" joins "+network+" as "+at.Alias+")"))
	steps := compose.Steps(compose.Up, compose.Opts{NoDeps: true}, []string{service})
	if now == 0 {
		ui.Hint("start it later with: ngitool app up " + a.Name + " " + service + " — until then its routes answer 502")
		return at, false, nil
	}
	ui.Explain("Recreates "+service+" with the override so it joins "+network+"; nothing else is touched (--no-deps).",
		service+": a few seconds of downtime",
		"ngitool app unlink "+a.Name+" (or remove the route), then ngitool app recreate "+a.Name+" "+service)
	printWillRun(b, updated.Compose(), steps)
	run := now == 1 || e.yes
	if !run && ui.CanPrompt() {
		if run, err = ui.Confirm("Recreate "+service+" now?", "", true); err != nil {
			return at, false, err
		}
	}
	if !run {
		ui.Hint("start it later with: ngitool app up " + a.Name + " " + service + " — until then its routes answer 502")
		return at, false, nil
	}
	return at, true, e.runSteps(ctx, b, *updated, compose.Up, steps, []string{service}, verbose)
}

func (e *env) loadApp(name string) (*compose.App, error) {
	st, err := model.Load(e.paths)
	if err != nil {
		return nil, err
	}
	if a := st.App(name); a != nil {
		return a, nil
	}
	return nil, errors.New("no app " + name)
}

func appAttachCmd(e *env) *cobra.Command {
	var network, instance, how string
	var later, verbose bool
	c := &cobra.Command{
		Use:   "attach <app> <service>",
		Short: "put a service on an nginx network through NgiTool's override",
		Long: "Writes /var/lib/ngitool/overrides/<app>.yaml: the service keeps its own networks and joins the\n" +
			"instance's network with the alias <app>-<service>, which routes use (never an IP, DOCK-16).\n" +
			"Then recreates only that service (up -d --no-deps). route add does this on its own when a\n" +
			"target is not on the instance's network.",
		Annotations: map[string]string{annSynopsis: "attach <app> <service> [--instance ID | --network NAME]"},
		Args:        maxArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			a, err := pickApp(st, args, "Attach a service of which app?")
			if err != nil {
				return err
			}
			service := ""
			if len(args) == 2 {
				service = args[1]
			} else {
				if err := ui.Need("service", "<service>"); err != nil {
					return err
				}
				var opts []ui.Option
				for _, s := range compose.RawSummary(a.Files).Services {
					opts = append(opts, ui.Option{Value: s, Label: s})
				}
				if service, err = ui.Select(ui.SelectOpts{Title: "Which service?", Options: opts}); err != nil {
					return err
				}
			}
			if network == "" {
				rep := e.report(ctx, false, ui.IsTTY())
				in, err := e.instanceFor(rep, st, instance, false)
				if err != nil {
					return err
				}
				network = instanceNetwork(in)
				if network == "" {
					return problemErr(model.Problem{Code: "RP-05", Msg: in.Name + " is not on a Docker network", Fix: "host nginx reaches services through a port published on 127.0.0.1"})
				}
			}
			now := -1
			if later {
				now = 0
			}
			_, _, err = e.attachService(ctx, a.Name, service, network, how, now, verbose)
			return err
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "the nginx whose network to join (default: the front door)")
	c.Flags().StringVar(&network, "network", "", "join this network instead of an instance's")
	c.Flags().StringVar(&how, "how", "", "override (default) or manual (print the snippet to paste)")
	c.Flags().BoolVar(&later, "later", false, "do not recreate the service now")
	c.Flags().BoolVar(&verbose, "verbose", false, "raw compose output")
	return c
}

// instanceNetwork is the user network an instance's upstreams join
// (model.joinNetwork's choice), "" for host nginx.
func instanceNetwork(in *discover.Instance) string {
	if model.HostNetwork(in) {
		return ""
	}
	for _, n := range in.Networks {
		if n != "bridge" && n != "host" && n != "none" {
			return n
		}
	}
	if len(in.Networks) > 0 {
		return in.Networks[0]
	}
	return ""
}

// ---- unlink ----------------------------------------------------------------

func appUnlinkCmd(e *env) *cobra.Command {
	var down, keep bool
	var f txFlags
	c := &cobra.Command{
		Use:   "unlink [app]",
		Short: "forget an app, with the routes and pool members that point at it",
		Long: "Shows the cascade first (cli/src/remove.mjs): routes whose pools hold only this app's services go\n" +
			"with their pools; in shared pools only the app's members go. Then the app and its override are\n" +
			"removed from NgiTool. The project itself is untouched unless --down (asked separately).",
		Annotations: map[string]string{annSynopsis: "unlink <app> [--down] [--keep-running]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			a, err := pickApp(st, args, "Unlink which app?")
			if err != nil {
				return err
			}
			app := *a
			// The cascade, per instance.
			byInst := map[string]*cascade{}
			members := map[string][]string{} // pool → member labels that go
			var insts []string
			for _, p := range st.Pools {
				var mine []string
				for _, m := range p.Members {
					if m.OfApp(app, "") {
						mine = append(mine, m.Label())
					}
				}
				if len(mine) == 0 {
					continue
				}
				if byInst[p.Instance] == nil {
					byInst[p.Instance] = &cascade{}
					insts = append(insts, p.Instance)
				}
				if len(mine) == len(p.Members) {
					for _, r := range st.RoutesOf(p.Name) {
						byInst[p.Instance].routes = append(byInst[p.Instance].routes, r.ID)
					}
					byInst[p.Instance].pools = append(byInst[p.Instance].pools, p.Name)
				} else {
					members[p.Name] = mine
				}
			}
			sort.Strings(insts)
			pairs := [][2]string{{"App", app.Name + ui.Muted("  "+app.WorkingDir)}}
			if app.NeedsOverride() {
				pairs = append(pairs, [2]string{"Override", app.Override + ui.Muted("  deleted")})
			}
			for _, id := range insts {
				for _, r := range byInst[id].routes {
					pairs = append(pairs, [2]string{"Route", r + ui.Muted("  on "+id)})
				}
				for _, p := range byInst[id].pools {
					pairs = append(pairs, [2]string{"Pool", p + ui.Muted("  only this app's members")})
				}
			}
			for p, ms := range members {
				pairs = append(pairs, [2]string{"Members", strings.Join(ms, ", ") + ui.Muted("  out of pool "+p)})
			}
			ui.Plan("Unlink "+app.Name, pairs)
			ui.Explain("NgiTool forgets "+app.Name+" and stops routing to it; the project's files and containers stay as they are",
				fmt.Sprintf("%d routes, %d pools, %d shared pools", countRoutes(byInst), countPools(byInst), len(members)),
				"ngitool app link "+app.WorkingDir+", then add the routes again (or ngitool rollback <instance>)")
			if ok, err := ui.Sure(e.yes, "Unlink "+app.Name+"?", ""); err != nil || !ok {
				if err == nil {
					err = errCancelled
				}
				return err
			}
			if len(insts) > 0 {
				rep := e.freshReport(ctx, true)
				for _, id := range insts {
					in := lookupInstance(rep, id)
					if in == nil {
						return fmt.Errorf("instance %s was not found by the scan — remove its routes first", id)
					}
					plan := *byInst[id]
					if _, err := e.change(ctx, rep, in, f, "app unlink "+app.Name, nil, func(next *model.State) error {
						plan.apply(next)
						for i := range next.Pools {
							if next.Pools[i].Instance != id {
								continue
							}
							keepM := next.Pools[i].Members[:0]
							for _, m := range next.Pools[i].Members {
								if !m.OfApp(app, "") {
									keepM = append(keepM, m)
								}
							}
							next.Pools[i].Members = keepM
						}
						return nil
					}); err != nil {
						return err
					}
				}
			}
			if err := e.withLock(ctx, true, func() error {
				st, err := model.Load(e.paths)
				if err != nil {
					return err
				}
				st.RemoveApp(app.Name)
				if app.Override != "" {
					if err := os.Remove(app.Override); err != nil && !os.IsNotExist(err) {
						return err
					}
				}
				return model.Save(e.paths, st)
			}); err != nil {
				return err
			}
			ui.Done("Unlinked " + app.Name)
			run := down
			if !down && !keep && ui.CanPrompt() && !e.yes {
				if run, err = ui.Confirm("Also stop and remove its containers (docker compose down)?", "volumes are kept", false); err != nil {
					return err
				}
			}
			if !run {
				return nil
			}
			b, err := e.composeBin(ctx)
			if err != nil {
				return err
			}
			p := app.Compose()
			steps := compose.Steps(compose.Down, compose.Opts{}, nil)
			printWillRun(b, p, steps)
			return e.runSteps(ctx, b, app, compose.Down, steps, nil, false)
		},
	}
	c.Flags().BoolVar(&down, "down", false, "also run docker compose down (volumes are kept)")
	c.Flags().BoolVar(&keep, "keep-running", false, "do not ask about down")
	f.add(c)
	return c
}

func countRoutes(m map[string]*cascade) int {
	n := 0
	for _, c := range m {
		n += len(c.routes)
	}
	return n
}

func countPools(m map[string]*cascade) int {
	n := 0
	for _, c := range m {
		n += len(c.pools)
	}
	return n
}

// ---- fix -------------------------------------------------------------------

func appFixCmd(e *env) *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use:   "fix [app]",
		Short: "recreate a detached app's services with its override (DOCK-07)",
		Long: "An app is detached when its containers run without NgiTool's override — someone ran a plain\n" +
			"`docker compose up` in the project — so they left the nginx network and their routes answer 502.\n" +
			"fix recreates exactly those services with every -f and the override (up -d --no-deps --force-recreate).",
		Annotations: map[string]string{annSynopsis: "fix [app]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			ctrs, err := appContainers(ctx)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				var bad []string
				for _, a := range st.Apps {
					if compose.Check(a, ctrs).Status == compose.NetDetached {
						bad = append(bad, a.Name)
					}
				}
				switch {
				case len(bad) == 0:
					ui.Done("no app is detached")
					return nil
				case len(bad) == 1:
					args = bad
				default:
					if err := ui.Need("app", "<app>"); err != nil {
						return err
					}
					var opts []ui.Option
					for _, n := range bad {
						opts = append(opts, ui.Option{Value: n, Label: n, Hint: compose.DetachedWhy})
					}
					n, err := ui.Select(ui.SelectOpts{Title: "Fix which app?", Options: opts})
					if err != nil {
						return err
					}
					args = []string{n}
				}
			}
			a, err := pickApp(st, args, "")
			if err != nil {
				return err
			}
			d := compose.Check(*a, ctrs)
			if d.Status != compose.NetDetached {
				ui.Done(a.Name + " is not detached " + ui.Muted("("+d.Status+")"))
				return nil
			}
			b, err := e.composeBin(ctx)
			if err != nil {
				return err
			}
			var nets []string
			for _, s := range d.Services {
				nets = appendOnce(nets, a.Attached[s].Network)
			}
			steps := compose.Steps(compose.Recreate, compose.Opts{NoDeps: true}, d.Services)
			ui.Explain("Recreates "+strings.Join(d.Services, ", ")+" with every -f and NgiTool's override, so "+either(len(d.Services), "it rejoins ", "they rejoin ")+strings.Join(nets, ", "),
				strings.Join(d.Services, ", ")+": a few seconds of downtime; their routes answer again afterwards",
				"nothing to undo: this is how NgiTool always starts them")
			printWillRun(b, a.Compose(), steps)
			if ok, err := ui.Sure(e.yes, "Fix "+a.Name+" now?", ""); err != nil || !ok {
				if err == nil {
					err = errCancelled
				}
				return err
			}
			return e.runSteps(ctx, b, *a, "fix", steps, d.Services, verbose)
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "raw compose output")
	return c
}

// ---- the app menu ----------------------------------------------------------

func appMenuCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:    "menu <app>",
		Short:  "an app's action menu",
		Hidden: true,
		Args:   maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			a, err := pickApp(st, args, "Which app?")
			if err != nil {
				return err
			}
			opts := []ui.Option{}
			for _, act := range compose.Actions {
				opts = append(opts, ui.Option{Value: act.ID, Label: act.Label, Hint: act.Hint})
			}
			opts = append(opts,
				ui.Option{Value: "show", Label: "Details", Hint: "files, services, routes"},
				ui.Option{Value: "fix", Label: "Fix detached", Hint: "recreate with the override (DOCK-07)"},
				ui.Option{Value: "unlink", Label: "Unlink", Hint: "forget it, with its routes — the project is untouched"})
			for {
				choice, err := ui.Select(ui.SelectOpts{Title: a.Name, Note: a.WorkingDir + " · esc goes back", Options: opts})
				if errors.Is(err, ui.ErrBack) {
					return nil
				}
				if err != nil {
					return err
				}
				err = runArgs(e.root, []string{"app", choice, a.Name})
				switch {
				case errors.Is(err, ui.ErrInterrupted):
					return err
				case err != nil && !errors.Is(err, ui.ErrBack):
					report(err, nil)
				}
				if choice == "unlink" && err == nil {
					return nil
				}
				fmt.Fprintln(ui.Out)
			}
		},
	}
}

// appMenuItems are the Apps group's entries: one per linked app.
func appMenuItems(e *env) []MenuItem {
	st, err := model.Load(e.paths)
	if err != nil {
		return nil
	}
	var out []MenuItem
	for _, a := range st.Apps {
		out = append(out, MenuItem{Group: "apps", Label: a.Name, Hint: a.WorkingDir, Args: []string{"app", "menu", a.Name}})
	}
	return out
}

// stamp is now in state's format.
func stamp() string { return model.Now(time.Now()) }
