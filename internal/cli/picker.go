package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// pickRow is one target in the member checklist.
type pickRow struct {
	Value    string // container:NAME, service:PROJECT/SERVICE, port:N
	Label    string
	Hint     string
	Badge    string
	Disabled string
	Ports    []int // ports the target exposes; asked when more than one
}

// memberSource is one group of the checklist. Prompt 4 appends "linked
// apps" here; the picker does not care where rows come from.
type memberSource struct {
	Title string
	Rows  func(st *model.State, rep *discover.Report, in *discover.Instance) []pickRow
}

var memberSources = []memberSource{
	{Title: "Linked apps", Rows: appRows},
	{Title: "Compose services", Rows: serviceRows},
	{Title: "Containers", Rows: containerRows},
	{Title: "Host ports", Rows: hostPortRows},
	{Title: "Static files", Rows: staticRows},
}

// staticNew is the checklist row "a new folder named after the host".
const staticNew = "static:\x00new"

// staticRows are the edge stack's www/ folders (EDGE-04); host nginx takes
// an absolute directory through the manual row (static:/srv/site).
func staticRows(st *model.State, _ *discover.Report, in *discover.Instance) []pickRow {
	ed := edgeStackFor(st, in)
	if ed == nil {
		return nil
	}
	var rows []pickRow
	for _, w := range (&env{}).wwwRows(st, ed.Dir) {
		hint := "not served yet"
		if len(w.Routes) > 0 {
			hint = "served at " + strings.Join(w.Routes, ", ")
		}
		rows = append(rows, pickRow{Value: "static:" + w.Dir, Label: "www/" + w.Dir, Hint: hint})
	}
	return append(rows, pickRow{Value: staticNew, Label: "＋ New folder", Hint: "www/<hostname> with a placeholder page — replace it with your build"})
}

const manualRow = "\x00manual"

func sharesNet(in *discover.Instance, c discover.Container) bool {
	return strings.HasPrefix(netChip(in, c), "on ")
}

// netChip says whether a container shares a network with the instance.
func netChip(in *discover.Instance, c discover.Container) string {
	for _, a := range in.Networks {
		for _, b := range c.Networks {
			if a == b && a != "host" && a != "none" {
				return "on " + a + " " + ui.SymOK
			}
		}
	}
	if len(c.Networks) == 0 {
		return "no network"
	}
	return strings.Join(c.Networks, ", ") + " (not shared)"
}

func portsText(ps []int) string {
	if len(ps) == 0 {
		return "no ports declared"
	}
	var s []string
	for _, p := range ps {
		s = append(s, strconv.Itoa(p))
	}
	return "port " + strings.Join(s, ", ")
}

func hostNetBlocked(in *discover.Instance) string {
	if model.HostNetwork(in) {
		return "host nginx cannot resolve container names — publish the port on 127.0.0.1 and pick it under Host ports (RP-05)"
	}
	return ""
}

func serviceRows(st *model.State, rep *discover.Report, in *discover.Instance) []pickRow {
	type svc struct {
		c     discover.Container
		n     int
		ports map[int]bool
	}
	by := map[string]*svc{}
	var keys []string
	for _, c := range rep.Containers {
		if !c.Running || c.Project == "" || c.Name == in.Container || st.AppOfProject(c.Project) != nil {
			continue // a linked app's services are listed under Linked apps
		}
		k := c.Project + "/" + c.Service
		if by[k] == nil {
			by[k] = &svc{c: c, ports: map[int]bool{}}
			keys = append(keys, k)
		}
		by[k].n++
		for _, p := range c.Exposed {
			by[k].ports[p] = true
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := sharesNet(in, by[keys[i]].c), sharesNet(in, by[keys[j]].c)
		if a != b {
			return a
		}
		return keys[i] < keys[j]
	})
	var rows []pickRow
	for _, k := range keys {
		s := by[k]
		var ps []int
		for p := range s.ports {
			ps = append(ps, p)
		}
		sort.Ints(ps)
		hint := s.c.Image + " · " + portsText(ps) + " · " + netChip(in, s.c)
		r := pickRow{Value: "service:" + k, Label: k, Hint: hint, Ports: ps, Disabled: hostNetBlocked(in)}
		if s.n > 1 {
			r.Badge = strconv.Itoa(s.n) + " replicas"
		}
		if s.c.Instance != "" {
			r.Badge = "nginx"
		}
		rows = append(rows, r)
	}
	return rows
}

func containerRows(_ *model.State, rep *discover.Report, in *discover.Instance) []pickRow {
	var rows []pickRow
	for _, c := range rep.Containers {
		if !c.Running || c.Project != "" || c.Name == in.Container {
			continue
		}
		r := pickRow{Value: "container:" + c.Name, Label: c.Name, Hint: c.Image + " · " + portsText(c.Exposed) + " · " + netChip(in, c), Ports: c.Exposed, Disabled: hostNetBlocked(in)}
		if c.Instance != "" {
			r.Badge = "nginx"
		}
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := strings.Contains(rows[i].Hint, ui.SymOK), strings.Contains(rows[j].Hint, ui.SymOK)
		if a != b {
			return a // on a shared network first
		}
		return rows[i].Label < rows[j].Label
	})
	return rows
}

func hostPortRows(_ *model.State, rep *discover.Report, in *discover.Instance) []pickRow {
	type port struct {
		addrs []string
		proc  string
	}
	by := map[int]*port{}
	var ports []int
	for _, l := range rep.Listeners {
		if l.Instance == in.ID || l.Port == 22 {
			continue
		}
		p := by[l.Port]
		if p == nil {
			p = &port{}
			by[l.Port] = p
			ports = append(ports, l.Port)
		}
		p.addrs = appendOnce(p.addrs, l.Addr)
		switch {
		case l.Container != "":
			p.proc = "docker-proxy → " + l.Container
		case p.proc == "" && l.Process != "":
			p.proc = l.Process
		}
	}
	sort.Ints(ports)
	var rows []pickRow
	for _, n := range ports {
		p := by[n]
		reach := ""
		if !model.HostNetwork(in) && allLoopback(p.addrs) {
			reach = " · 127.0.0.1 only: not reachable from a container (RP-06)"
		}
		rows = append(rows, pickRow{Value: "port:" + strconv.Itoa(n), Label: strconv.Itoa(n), Hint: firstNonEmpty(p.proc, "unknown process") + " · " + strings.Join(p.addrs, ", ") + reach, Ports: []int{n}})
	}
	return rows
}

func allLoopback(addrs []string) bool {
	for _, a := range addrs {
		if a != "127.0.0.1" && a != "::1" && !strings.HasPrefix(a, "127.") {
			return false
		}
	}
	return len(addrs) > 0
}

func appendOnce(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// pickMembers is wizard step c+d: a checklist of every target, then the
// port of each pick that exposes more than one.
func pickMembers(st *model.State, rep *discover.Report, in *discover.Instance, preselected []string) ([]string, error) {
	var opts []ui.Option
	rows := map[string]pickRow{}
	for _, src := range memberSources {
		rs := src.Rows(st, rep, in)
		if len(rs) == 0 {
			continue
		}
		opts = append(opts, ui.Sep(src.Title))
		for _, r := range rs {
			rows[r.Value] = r
			opts = append(opts, ui.Option{Value: r.Value, Label: r.Label, Hint: r.Hint, Badge: r.Badge, Disabled: r.Disabled})
		}
	}
	manualHint := "HOST:PORT, https://HOST, unix:/path"
	if in.Kind == discover.KindHost {
		manualHint += ", static:/dir (files)"
	}
	opts = append(opts, ui.Option{Value: manualRow, Label: ui.SymPen + " Enter an address manually…", Hint: manualHint})
	var pre []string
	for _, p := range preselected {
		if head, _, _ := strings.Cut(p, ","); rows[stripPort(head)].Value != "" {
			pre = append(pre, stripPort(head))
		}
	}
	for {
		picked, err := ui.MultiSelect(ui.MultiOpts{
			Title:    "Where should traffic go?",
			Note:     "one pick is a simple proxy, two or more make a load-balanced pool",
			Options:  opts,
			Selected: pre,
		})
		if err != nil {
			return nil, err
		}
		var specs []string
		for _, v := range picked {
			if v == manualRow {
				for {
					s, err := ui.Input(ui.InputOpts{Title: "Address", Placeholder: "10.0.0.5:8080 · https://api.example.net · unix:/run/app.sock",
						Validate: func(v string) error { _, _, err := model.ParseMember(v); return err }})
					if err != nil {
						return nil, err
					}
					specs = append(specs, s)
					more, err := ui.Confirm("Add another address?", "", false)
					if err != nil || !more {
						break
					}
				}
				continue
			}
			r := rows[v]
			if strings.HasPrefix(v, "port:") || strings.HasPrefix(v, "static:") {
				specs = append(specs, v)
				continue
			}
			port, err := pickPort(r)
			if err != nil {
				return nil, err
			}
			specs = append(specs, v+":"+strconv.Itoa(port))
		}
		if len(specs) == 0 {
			ui.Warning("pick at least one target")
			continue
		}
		return specs, nil
	}
}

func stripPort(spec string) string {
	kind, rest, _ := strings.Cut(spec, ":")
	if kind == "port" {
		return spec
	}
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		if _, err := strconv.Atoi(rest[i+1:]); err == nil {
			return kind + ":" + rest[:i]
		}
	}
	return spec
}

// pickPort ports cli/src/flows.mjs pickPort: one exposed port is used, more
// are a select with "Another port…", none is a typed port.
func pickPort(r pickRow) (int, error) {
	ask := func() (int, error) {
		s, err := ui.Input(ui.InputOpts{Title: "Port " + r.Label + " listens on", Placeholder: "3000", Validate: func(v string) error { _, err := model.ParsePort(v); return err }})
		if err != nil {
			return 0, err
		}
		return model.ParsePort(s)
	}
	switch len(r.Ports) {
	case 0:
		return ask()
	case 1:
		ui.Hint(r.Label + " listens on " + strconv.Itoa(r.Ports[0]))
		return r.Ports[0], nil
	}
	var opts []ui.Option
	for _, p := range r.Ports {
		opts = append(opts, ui.Option{Value: strconv.Itoa(p), Label: strconv.Itoa(p)})
	}
	opts = append(opts, ui.Option{Value: "other", Label: "Another port…"})
	v, err := ui.Select(ui.SelectOpts{Title: "Which port of " + r.Label + "?", Options: opts})
	if err != nil {
		return 0, err
	}
	if v == "other" {
		return ask()
	}
	return strconv.Atoi(v)
}

// methodOptions explain each balancing method and when to use it.
var methodOptions = []ui.Option{
	{Value: model.LeastConn, Label: "Least connections", Hint: "long or uneven requests — the default"},
	{Value: model.RoundRobin, Label: "Round robin", Hint: "even spread, stateless apps"},
	{Value: model.IPHash, Label: "IP hash", Hint: "simple stickiness, not behind Cloudflare"},
	{Value: model.Hash, Label: "Consistent hash on a key", Hint: "caches, sticky sessions"},
	{Value: model.RandomTwo, Label: "Random, two, least connections", Hint: "many members or several proxies"},
}

var stickyOptions = []ui.Option{
	{Value: "none", Label: "No", Hint: "any member may answer any request"},
	{Value: "ip", Label: "Client IP (direct)", Hint: "ip_hash — clients connect straight to this server"},
	{Value: "cloudflare", Label: "Client IP behind Cloudflare", Hint: "hash $http_cf_connecting_ip consistent (LB-07)"},
	{Value: "cookie", Label: "Cookie", Hint: "hash $cookie_<name> consistent — the app sets the cookie"},
}

// askMethod is wizard step e.
func askMethod(p *model.Pool) error {
	m, err := ui.Select(ui.SelectOpts{Title: "How should requests be spread?", Options: methodOptions, Default: firstNonEmpty(p.Method, model.DefaultMethod)})
	if err != nil {
		return err
	}
	p.Method = m
	if m == model.Hash {
		key, err := ui.Input(ui.InputOpts{Title: "Hash key", Default: firstNonEmpty(p.HashKey, "$request_uri"), Placeholder: "$request_uri, $cookie_session, $arg_user"})
		if err != nil {
			return err
		}
		p.HashKey, p.Consistent = key, true
		return nil
	}
	s, err := ui.Select(ui.SelectOpts{Title: "Sticky sessions?", Options: stickyOptions, Default: "none"})
	if err != nil {
		return err
	}
	if s == "cookie" {
		name, err := ui.Input(ui.InputOpts{Title: "Cookie name", Default: "session", Validate: func(v string) error { return model.ApplySticky(&model.Pool{}, "cookie:"+v) }})
		if err != nil {
			return err
		}
		s = "cookie:" + name
	}
	return model.ApplySticky(p, s)
}

// membersFromSpecs parses specs into members for an instance, filling
// names (never IPs) and checking one scheme per pool (LB-16). Services of
// linked apps are recorded with their app and, once attached, the alias
// the app's override gives them (DOCK-16).
func membersFromSpecs(st *model.State, rep *discover.Report, in *discover.Instance, specs []string, p *model.Pool) error {
	for _, s := range specs {
		m, scheme, err := model.ParseMember(s)
		if err != nil {
			return codeErr(err)
		}
		if err := appMember(st, &m); err != nil {
			return err
		}
		if scheme != "" {
			if len(p.Members) > 0 && p.Scheme != scheme {
				return problemErr(model.Problem{Code: "LB-16", Msg: fmt.Sprintf("%s is %s but pool %s is %s", m.Label(), scheme, p.Name, p.Scheme), Fix: "one scheme per pool: make two pools"})
			}
			p.Scheme = scheme
		}
		model.ResolveMember(rep, in, &m)
		if p.Member(m.Label()) >= 0 {
			return errors.New(m.Label() + " is already in pool " + p.Name)
		}
		p.Members = append(p.Members, m)
	}
	if p.Scheme == "" {
		p.Scheme = "http"
	}
	return nil
}
