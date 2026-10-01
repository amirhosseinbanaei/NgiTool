package cli

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// CacheFor is how long `instances`, `inspect` and `doctor` reuse a scan.
// `scan` itself always scans (decision recorded in AGENTS.md).
const CacheFor = 60 * time.Second

// newScanEnv is the machine to scan; tests replace it.
var newScanEnv = func(e *env) discover.Env {
	roots := discover.DefaultRoots
	if cfg, err := state.LoadConfig(e.paths); err == nil && len(cfg.ScanRoots) > 0 {
		roots = cfg.ScanRoots
	}
	return discover.System(roots)
}

// scanMemo shares one scan between the checks of a doctor run.
type scanMemo struct {
	mu  sync.Mutex
	rep *discover.Report
}

// report returns a scan no older than CacheFor, or scans now. With show
// the steps are drawn (not for --json).
func (e *env) report(ctx context.Context, fresh, show bool) *discover.Report {
	if e.fixed != nil {
		return e.fixed
	}
	e.memo.mu.Lock()
	defer e.memo.mu.Unlock()
	if e.memo.rep != nil && !fresh {
		return e.memo.rep
	}
	senv := newScanEnv(e)
	if !fresh {
		if r, ok := discover.LoadCache(e.paths.ScanCache, CacheFor, senv.Now()); ok {
			e.memo.rep = r
			return r
		}
	}
	s := discover.New(senv)
	if show {
		steps := []ui.StepFunc{}
		for _, d := range s.Steps() {
			steps = append(steps, ui.StepFunc{Label: d.Name, Run: func(t *ui.TaskCtl) error {
				t.Update(s.RunStep(ctx, d).Label())
				return nil
			}})
		}
		_ = ui.Steps("Scanning this server", steps)
	} else {
		for _, d := range s.Steps() {
			s.RunStep(ctx, d)
		}
	}
	r := s.Report()
	// A cache we may not write (not root, no NGITOOL_ROOT) is not an error.
	_ = discover.SaveCache(e.paths.ScanCache, r)
	e.memo.rep = r
	return r
}

func scanCmd(e *env) *cobra.Command {
	var asJSON, fresh bool
	c := &cobra.Command{
		Use:         "scan",
		Short:       "find every nginx: host, containers, compose, edge",
		Long:        "Scans host processes, Docker containers, compose files and port owners, parses every config,\nand shows each instance with what NgiTool may do to it, then cross-instance findings.\nRead-only. Always scans afresh and refreshes the cache other commands use for 60 s.",
		Annotations: map[string]string{annGroup: "instances", annSynopsis: "scan [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := e.report(cmd.Context(), true, !asJSON)
			if asJSON {
				return printJSON(r)
			}
			printScan(r)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&fresh, "fresh", false, "ignore the cache (scan always does)")
	return c
}

func instancesCmd(e *env) *cobra.Command {
	var asJSON, fresh bool
	c := &cobra.Command{
		Use:         "instances",
		Short:       "one line per nginx instance",
		Long:        "A compact table of every instance: id, kind, state, version, owner, front door and whether NgiTool may write it.\nUses a scan up to 60 s old unless --fresh.",
		Annotations: map[string]string{annGroup: "instances", annSynopsis: "instances [--json] [--fresh]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := e.report(cmd.Context(), fresh, !asJSON && ui.IsTTY())
			if asJSON {
				return printJSON(compactList(r))
			}
			if len(r.Instances) == 0 {
				ui.Info("no nginx found on this machine")
				ui.Hint(r.Host)
				return nil
			}
			rows := [][]string{}
			for _, in := range r.Instances {
				front, write := "", ui.Muted("no")
				if in.FrontDoor {
					front = ui.OK("yes")
				}
				if in.Caps.Write.OK {
					write = ui.OK("yes")
				}
				if m, ok := strings.CutPrefix(in.ManagedBy, "other:"); ok {
					// Shown, not hidden: hiding them would hide real port conflicts.
					write = ui.Warn("read-only") + ui.Muted(" ("+m+")")
				}
				rows = append(rows, []string{in.ID, in.Kind, stateText(in.State), dash(in.Version), dash(in.Owner), dash(front), write})
			}
			ui.Plain("")
			ui.PrintTable(rows, ui.TableOpts{Indent: 2, Header: []string{"ID", "KIND", "STATE", "VERSION", "OWNER", "FRONT DOOR", "WRITE"}})
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&fresh, "fresh", false, "scan now instead of using a scan up to 60 s old")
	return c
}

type compactInstance struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	State     string   `json:"state"`
	Version   string   `json:"version,omitempty"`
	Owner     string   `json:"owner,omitempty"`
	FrontDoor bool     `json:"frontDoor"`
	ManagedBy string   `json:"managedBy"`
	Write     bool     `json:"write"`
	WhyNot    string   `json:"writeReason,omitempty"`
	Servers   int      `json:"servers"`
	Upstreams int      `json:"upstreams"`
	Networks  []string `json:"networks,omitempty"`
}

func compactList(r *discover.Report) []compactInstance {
	out := []compactInstance{}
	for _, in := range r.Instances {
		s, u := in.Counts()
		out = append(out, compactInstance{
			ID: in.ID, Kind: in.Kind, Name: in.Name, State: in.State, Version: in.Version, Owner: in.Owner,
			FrontDoor: in.FrontDoor, ManagedBy: in.ManagedBy, Write: in.Caps.Write.OK, WhyNot: in.Caps.Write.Reason,
			Servers: s, Upstreams: u, Networks: in.Networks,
		})
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return ui.Muted("–")
	}
	return s
}

// kindIcon is the symbol in front of an instance.
func kindIcon(kind string) string {
	switch kind {
	case discover.KindEdge:
		return "◆"
	case discover.KindHost:
		return "⌂"
	case discover.KindCompose:
		return "◫"
	}
	return "▣"
}

func stateText(s string) string {
	switch s {
	case discover.StateRunning:
		return ui.OK(ui.SymDot) + " running"
	case discover.StateStopped:
		return ui.Warn(ui.SymDot) + " stopped"
	}
	return ui.Muted(ui.SymRing + " defined")
}

func printScan(r *discover.Report) {
	note := fmt.Sprintf("%d found", len(r.Instances))
	if r.FrontDoor != "" {
		if in := r.Find(r.FrontDoor); in != nil {
			note += " · front door " + in.Name
		}
	} else {
		note += " · no front door"
	}
	ui.Heading("Instances", note)
	for _, in := range r.Instances {
		for _, l := range card(in) {
			ui.Plain(l)
		}
	}
	ui.Plain("")
	for _, l := range statusLines(r) {
		ui.Plain(l)
	}
	printFindings(r.Findings)
}

// statusLines say what could not be scanned and why.
func statusLines(r *discover.Report) []string {
	var out []string
	add := func(ok bool, label, text string) {
		mark := ui.Muted(ui.SymInfo)
		if !ok {
			mark = ui.Warn(ui.SymWarn)
		}
		out = append(out, "  "+mark+" "+label+"  "+ui.Muted(text))
	}
	add(true, "host", r.Host)
	d := r.Docker
	switch {
	case d.Usable():
		add(true, "docker", strings.TrimSpace(d.State+" "+d.Version+" · "+fmt.Sprintf("%d compose file%s scanned", r.ComposeFiles, plural(r.ComposeFiles))))
	default:
		add(d.State == "missing", "docker", d.Detail+" — "+d.Hint)
	}
	if r.PortsStatus != "" {
		add(false, "ports", r.PortsStatus)
	}
	if n := len(r.ReadFailures); n > 0 {
		add(false, "reads", fmt.Sprintf("%d failed: %s", n, strings.Join(r.ReadFailures, "; ")))
	}
	return out
}

// card is one instance in the scan output.
func card(in discover.Instance) []string {
	head := "  " + ui.Accent(kindIcon(in.Kind)) + " " + ui.Bold(in.Name) + "  " + stateText(in.State)
	if in.Version != "" {
		head += "  " + in.Version
	} else if in.Image != "" {
		head += "  " + ui.Muted(in.Image)
	}
	if in.FrontDoor {
		head += "  " + ui.OK("[front door]")
	}
	if strings.HasPrefix(in.ManagedBy, "other:") {
		head += "  " + ui.Warn("["+strings.TrimPrefix(in.ManagedBy, "other:")+"]")
	}
	s, u := in.Counts()
	sub := []string{in.ID}
	if in.Owner != "" {
		sub = append(sub, "owner "+in.Owner)
	}
	sub = append(sub, fmt.Sprintf("%d server%s · %d upstream%s", s, plural(s), u, plural(u)))
	lines := []string{"", head, "    " + ui.Muted(strings.Join(sub, " · "))}
	caps := []struct {
		name string
		c    discover.Capability
	}{{"read", in.Caps.Read}, {"test", in.Caps.Test}, {"reload", in.Caps.Reload}, {"write", in.Caps.Write}}
	var chips []string
	var why []string
	for _, c := range caps {
		if c.c.OK {
			chips = append(chips, ui.OK(ui.SymOK+" "+c.name))
		} else {
			chips = append(chips, ui.Muted(ui.SymRing+" "+c.name))
			why = append(why, "      "+ui.Muted(c.name+": "+c.c.Reason))
		}
	}
	lines = append(lines, "    "+strings.Join(chips, "  "))
	lines = append(lines, why...)
	if in.Caps.Write.OK && in.Caps.Write.Note != "" {
		lines = append(lines, "      "+ui.Muted("write: "+in.Caps.Write.Note))
	}
	return lines
}

var sevOrder = []struct{ sev, title, one string }{
	{discover.SevError, "Errors", "error"}, {discover.SevWarn, "Warnings", "warning"}, {discover.SevInfo, "Notes", "note"},
}

func printFindings(fs []discover.Finding) {
	count := map[string]int{}
	for _, f := range fs {
		count[f.Severity]++
	}
	if len(fs) == 0 {
		ui.Heading("Findings", "nothing to report")
		return
	}
	var parts []string
	for _, s := range sevOrder {
		if n := count[s.sev]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s%s", n, s.one, plural(n)))
		}
	}
	ui.Heading("Findings", strings.Join(parts, " · "))
	for _, s := range sevOrder {
		if count[s.sev] == 0 {
			continue
		}
		ui.Plain("  " + ui.Muted(s.title))
		for _, f := range fs {
			if f.Severity != s.sev {
				continue
			}
			for _, l := range findingLines(f) {
				ui.Plain(l)
			}
		}
	}
}

func findingLines(f discover.Finding) []string {
	msg := f.Message + "  " + ui.Muted(f.Code)
	switch f.Severity {
	case discover.SevError:
		return ui.CheckLine(ui.StatusFail, msg, "", f.Fix)
	case discover.SevWarn:
		return ui.CheckLine(ui.StatusWarn, msg, "", f.Fix)
	}
	out := []string{"  " + ui.Accent(ui.SymInfo) + " " + msg}
	if f.Fix != "" {
		out = append(out, "    "+ui.Muted(ui.SymArrow+" "+f.Fix))
	}
	return out
}

func inspectCmd(e *env) *cobra.Command {
	var asJSON, raw, fresh bool
	c := &cobra.Command{
		Use:         "inspect [id]",
		Short:       "one instance: servers, locations, targets",
		Long:        "Shows one instance as a tree: listen → server_name → location → target (upstream members\nexpanded), each with its file:line. Targets are coloured by reachability.\n--raw prints the whole config with a header per file. Without an id on a terminal, pick from a list.",
		Annotations: map[string]string{annGroup: "instances", annSynopsis: "inspect [id] [--raw] [--json]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else if err := ui.Need("instance", "an id (ngitool instances lists them)"); err != nil {
				return err
			}
			r := e.report(cmd.Context(), fresh, !asJSON && ui.IsTTY() && !raw)
			in, err := pickInstance(r, id)
			if err != nil {
				return err
			}
			switch {
			case raw:
				out, err := discover.Raw(cmd.Context(), newScanEnv(e), in)
				if err != nil {
					return err
				}
				fmt.Fprint(ui.Out, out)
				return nil
			case asJSON:
				return printJSON(in)
			}
			printInspect(in)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&raw, "raw", false, "print the dumped config, one header per file")
	c.Flags().BoolVar(&fresh, "fresh", false, "scan now instead of using a scan up to 60 s old")
	return c
}

// pickInstance finds id (an id, a container or instance name, or a unique
// prefix of an id), or asks on a terminal.
func pickInstance(r *discover.Report, id string) (*discover.Instance, error) {
	if len(r.Instances) == 0 {
		return nil, &ExitError{Code: ExitFail, Msg: "no nginx found on this machine — " + r.Host}
	}
	if id == "" {
		opts := make([]ui.Option, 0, len(r.Instances))
		for _, in := range r.Instances {
			hint := in.Kind + " · " + in.State + " · " + in.ID
			badge := ""
			if in.FrontDoor {
				badge = "front door"
			}
			opts = append(opts, ui.Option{Value: in.ID, Label: in.Name, Hint: hint, Badge: badge})
		}
		choice, err := ui.Select(ui.SelectOpts{
			Title: "Which instance?", Options: opts, Filter: len(opts) > 6,
			Manual: &ui.Manual{Title: "Instance id or name", Placeholder: "ctr:web-1", Validate: func(v string) error {
				_, err := findInstance(r, v)
				return err
			}},
		})
		if err != nil {
			return nil, err
		}
		id = choice
	}
	return findInstance(r, id)
}

func findInstance(r *discover.Report, id string) (*discover.Instance, error) {
	if in := r.Find(id); in != nil {
		return in, nil
	}
	var hits []*discover.Instance
	for i := range r.Instances {
		in := &r.Instances[i]
		if in.Name == id || in.Container == id {
			return in, nil
		}
		if strings.HasPrefix(in.ID, id) || strings.HasPrefix(strings.SplitN(in.ID, ":", 2)[1], id) {
			hits = append(hits, in)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	ids := make([]string, len(r.Instances))
	for i, in := range r.Instances {
		ids[i] = in.ID
	}
	if len(hits) > 1 {
		return nil, &UsageError{Msg: fmt.Sprintf("%q matches %d instances — use the full id", id, len(hits)), Cmd: "ngitool inspect"}
	}
	return nil, &UsageError{Msg: fmt.Sprintf("no instance %q (known: %s)", id, strings.Join(ids, ", ")), Cmd: "ngitool inspect"}
}

func capLine(name string, c discover.Capability) string {
	if c.OK {
		s := ui.OK(ui.SymOK) + " " + name
		if c.Note != "" {
			s += "  " + ui.Muted(c.Note)
		}
		return s
	}
	return ui.Muted(ui.SymRing) + " " + name + "  " + ui.Muted(c.Reason)
}

func printInspect(in *discover.Instance) {
	pairs := [][2]string{{"id", in.ID}, {"kind", in.Kind}, {"state", stateText(in.State)}}
	add := func(k, v string) {
		if v != "" {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	add("version", in.Version)
	add("image", in.Image)
	add("binary", in.Exe)
	add("unit", in.Unit)
	add("package", in.Package)
	add("project", strings.Trim(in.Project+"/"+in.Service, "/"))
	add("compose", strings.Join(in.ComposeFile, ", "))
	add("owner", in.Owner)
	if in.FrontDoor {
		add("front door", ui.OK("yes")+" "+ui.Muted("owns :443 or :80"))
	}
	add("managed by", in.ManagedBy)
	add("networks", strings.Join(in.Networks, ", "))
	var ports []string
	for _, p := range in.Ports {
		ports = append(ports, fmt.Sprintf("%s→%d", net.JoinHostPort(firstNonEmpty(p.HostIP, "0.0.0.0"), strconv.Itoa(p.HostPort)), p.ContainerPort))
	}
	add("ports", strings.Join(ports, ", "))
	src := in.Source
	if src == "dump" {
		src = "nginx -T"
	}
	add("config", strings.TrimSpace(in.Conf+"  "+ui.Muted(src)))
	for i, m := range in.Mounts {
		k := ""
		if i == 0 {
			k = "mounts"
		}
		mode := "rw"
		if !m.RW {
			mode = "ro"
		}
		what := "dir"
		if m.File {
			what = "file"
		}
		pairs = append(pairs, [2]string{k, m.Dest + " ← " + firstNonEmpty(m.Source, m.Name) + " " + ui.Muted(m.Type+" "+what+" "+mode)})
	}
	if in.Summary != nil && in.Summary.Hook != nil {
		add("hook point", in.Summary.Hook.String())
	}
	add("test", in.Methods.Test)
	add("reload", in.Methods.Reload)
	ui.Plan(in.Name, pairs)
	for _, l := range []string{
		capLine("read", in.Caps.Read), capLine("test", in.Caps.Test),
		capLine("reload", in.Caps.Reload), capLine("write", in.Caps.Write),
	} {
		ui.Plain("    " + l)
	}
	for _, n := range in.Notes {
		ui.Plain("    " + ui.Accent(ui.SymInfo) + " " + n)
	}
	if in.Valid != nil && !*in.Valid {
		ui.Plain("")
		ui.Callout(ui.KindDanger, "nginx -t fails", in.TestOutput)
	}
	if len(in.ConfigErrors) > 0 {
		ui.Plain("")
		ui.Callout(ui.KindWarn, "Config problems", strings.Join(in.ConfigErrors, "\n"))
	}
	if in.Summary == nil {
		return
	}
	ui.Plain("")
	for _, l := range ui.Tree(configTree(in)) {
		ui.Plain(l)
	}
	ui.Plain("")
}

// configTree is listen → server_name → location → target → upstream
// members, each with file:line.
func configTree(in *discover.Instance) ui.Node {
	s := in.Summary
	root := ui.Node{Label: ui.Bold(in.Name), Hint: fmt.Sprintf("%d server%s · %d upstream%s", len(s.Servers), plural(len(s.Servers)), len(s.Upstreams), plural(len(s.Upstreams)))}
	ups := map[string]nginxconf.Upstream{}
	for _, u := range s.Upstreams {
		ups[u.Name] = u
	}
	byListen := map[string][]nginxconf.Server{}
	var order []string
	for _, srv := range s.Servers {
		for _, l := range srv.Listens {
			k := l.String()
			if _, ok := byListen[k]; !ok {
				order = append(order, k)
			}
			byListen[k] = append(byListen[k], srv)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return listenPort(order[i]) < listenPort(order[j]) })
	for _, k := range order {
		ln := ui.Node{Label: ui.Accent("listen " + k)}
		for _, srv := range byListen[k] {
			ln.Children = append(ln.Children, serverNode(in, srv, ups))
		}
		root.Children = append(root.Children, ln)
	}
	if s.Stream != nil {
		root.Children = append(root.Children, ui.Node{Label: ui.Muted("stream {}  read-only, out of scope"), Hint: s.Stream.String()})
	}
	return root
}

func listenPort(k string) int {
	var p int
	if i := strings.LastIndex(strings.Fields(k)[0], ":"); i >= 0 {
		fmt.Sscan(strings.Fields(k)[0][i+1:], &p)
	} else {
		fmt.Sscan(strings.Fields(k)[0], &p)
	}
	return p
}

func serverNode(in *discover.Instance, srv nginxconf.Server, ups map[string]nginxconf.Upstream) ui.Node {
	name := srv.NameList()
	if name == "" {
		name = ui.Muted("(no server_name)")
	}
	n := ui.Node{Label: name, Hint: srv.Pos.String()}
	var tags []string
	if srv.SSLCert != "" {
		tags = append(tags, "tls "+srv.SSLCert)
	}
	if srv.Return != "" {
		tags = append(tags, "return "+oneLine(srv.Return, 60))
	}
	for _, t := range tags {
		n.Children = append(n.Children, ui.Node{Label: ui.Muted(t)})
	}
	for _, l := range srv.Locations {
		n.Children = append(n.Children, locationNode(in, l, ups))
	}
	return n
}

func locationNode(in *discover.Instance, l nginxconf.Location, ups map[string]nginxconf.Upstream) ui.Node {
	label := "location " + strings.TrimSpace(l.Modifier+" "+l.Path)
	switch {
	case l.Return != "":
		label += ui.Muted("  return " + oneLine(l.Return, 60))
	case l.Alias != "":
		label += ui.Muted("  alias " + l.Alias)
	case l.Root != "":
		label += ui.Muted("  root " + l.Root)
	}
	n := ui.Node{Label: label, Hint: l.Pos.String()}
	if t := l.Target; t != nil {
		n.Children = append(n.Children, targetNode(in, t, ups))
	}
	for _, c := range l.Locations {
		n.Children = append(n.Children, locationNode(in, c, ups))
	}
	return n
}

// oneLine keeps a config value on one tree line: control characters
// escaped, long values cut.
func oneLine(s string, n int) string {
	s = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func reachPaint(r discover.Reach, s string) string {
	switch r.Status {
	case discover.ReachOK:
		return ui.OK(s)
	case discover.ReachErr:
		return ui.Err(s)
	case discover.ReachWarn:
		return ui.Warn(s)
	}
	return s
}

func targetNode(in *discover.Instance, t *nginxconf.Target, ups map[string]nginxconf.Upstream) ui.Node {
	r := in.Reach[t.Pos.String()]
	text := ui.SymArrow + " " + t.Directive + " " + t.Raw
	if t.Resolved != "" && t.Resolved != t.Raw {
		text += " = " + t.Resolved
	}
	if len(t.Candidates) > 0 {
		text += " ∈ {" + strings.Join(t.Candidates, ", ") + "}"
	}
	n := ui.Node{Label: reachPaint(r, text), Hint: strings.TrimSpace(r.Why + "  " + t.Pos.String())}
	if u, ok := ups[t.Upstream]; ok {
		n.Hint = strings.TrimSpace("upstream " + u.Name + " · " + u.Method + "  " + u.Pos.String())
		for _, m := range u.Servers {
			mr := in.Reach[m.Pos.String()]
			label := reachPaint(mr, m.Addr)
			if len(m.Params) > 0 {
				label += " " + ui.Muted(strings.Join(m.Params, " "))
			}
			n.Children = append(n.Children, ui.Node{Label: label, Hint: strings.TrimSpace(mr.Why + "  " + m.Pos.String())})
		}
	}
	return n
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}
