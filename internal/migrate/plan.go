package migrate

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
)

// Levels of a migration finding.
const (
	Blocker = "blocker"
	Warn    = "warn"
	Note    = "note"
)

// Finding is something the plan or the dry run noticed.
type Finding struct {
	Level string `json:"level"`
	Code  string `json:"code,omitempty"`
	Msg   string `json:"msg"`
	Fix   string `json:"fix,omitempty"`
}

// Row is one line of the mapping table: what it was, what it becomes.
type Row struct {
	Kind    string `json:"kind"`
	Legacy  string `json:"legacy"`
	NgiTool string `json:"ngitool"`
	Note    string `json:"note,omitempty"`
}

// Options are what the mapping needs from the machine.
type Options struct {
	Instance    string // the edge instance id, edge:<dir>
	Project     string // the stack's compose project
	OverrideDir string // /var/lib/ngitool/overrides
	// ProjectOf is the compose project an app's containers run as (their
	// label); "" when none run. The name: in the file or the folder is the
	// fallback.
	ProjectOf func(workingDir string) string
	Now       time.Time
}

// Plan is the migration of one directory.
type Plan struct {
	Dir      string    `json:"dir"`
	Instance string    `json:"instance"`
	Rows     []Row     `json:"rows"`
	Findings []Finding `json:"findings"`
	// Legacy are the legacy-managed files (rel paths) NgiTool's files
	// replace or remove; Unmanaged are left in place.
	Legacy    []string `json:"legacyFiles"`
	Unmanaged []string `json:"unmanaged"`
	Drift     []string `json:"drift"` // MIG-02
	// Next is the state with everything imported; Routes the ids added.
	Next   *model.State `json:"-"`
	Routes []string     `json:"routes"`
	Apps   []string     `json:"apps"` // newly linked apps
}

// Blockers are the findings that stop a migration.
func (p *Plan) Blockers() []Finding {
	var out []Finding
	for _, f := range p.Findings {
		if f.Level == Blocker {
			out = append(out, f)
		}
	}
	return out
}

func (p *Plan) add(level, code, msg, fix string) {
	p.Findings = append(p.Findings, Finding{Level: level, Code: code, Msg: msg, Fix: fix})
}

var nameLineRE = regexp.MustCompile(`(?m)^name:\s*["']?([A-Za-z0-9][A-Za-z0-9_.-]*)["']?\s*$`)

// projectName is how compose names a project run with only -f and
// --project-directory (the legacy CLI never passed -p): `name:` in the
// file, else the folder.
func projectName(target, dir string) string {
	if b, err := os.ReadFile(target); err == nil {
		if m := nameLineRE.FindSubmatch(b); m != nil {
			return compose.ProjectName(string(m[1]))
		}
	}
	return compose.ProjectName(filepath.Base(dir))
}

// Map turns a legacy directory into state: before plus the stack, its
// certificates, domains, apps, routes and pools.
func Map(l *Legacy, before *model.State, o Options) *Plan {
	p := &Plan{Dir: l.Dir, Instance: o.Instance, Unmanaged: append([]string{}, l.Unmanaged...)}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	now := model.Now(o.Now)
	next := before.Clone()
	in := o.Instance

	// The stack.
	if next.EdgeAt(l.Dir) == nil {
		next.Edges = append(next.Edges, model.Edge{Dir: l.Dir, Project: o.Project, Instance: in, Added: now})
	}
	p.Rows = append(p.Rows, Row{Kind: "stack", Legacy: "the directory", NgiTool: "edge stack, project " + o.Project, Note: "stays where it is: " + l.Dir})
	lay := edge.Layout{Dir: l.Dir}

	// Certificates: registered with their files, never reissued.
	names := sortedKeys(l.State.Certs)
	for _, n := range names {
		c := l.State.Certs[n]
		crt, key := LegacyCertPaths(n, c)
		host := filepath.Join(lay.Certs(), n)
		mc := model.Cert{Name: n, Instance: in, Kind: c.Type, Names: c.Names, AOP: c.AOP, Cert: crt, Key: key, Added: now}
		if c.Type == certs.LetsEncrypt {
			host = filepath.Join(lay.LE(), "live", n)
			mc.Challenge, mc.Certbot = certs.ChallengeOf(c.Type, c.Challenge), "stack"
		}
		mc.HostCert, mc.HostKey = filepath.Join(host, "fullchain.pem"), filepath.Join(host, "privkey.pem")
		note := ""
		if info, err := certs.InspectFile(mc.HostCert); err != nil {
			p.add(Warn, "MIG-01", "certificate "+n+": "+err.Error(), "routes using it fail nginx -t until its files exist")
			note = err.Error()
		} else {
			note = info.NotAfter.Format("2006-01-02")
		}
		if next.Cert(in, n) == nil {
			next.Certs = append(next.Certs, mc)
		}
		p.Rows = append(p.Rows, Row{Kind: "cert", Legacy: n, NgiTool: certs.Label(c.Type, c.Challenge) + ", files in place", Note: "until " + note})
	}

	// Domains keep their zone ids.
	for _, d := range sortedKeys(l.State.Domains) {
		x := l.State.Domains[d]
		md := model.Domain{Name: d, Instance: in, Cert: x.Cert, Added: firstNonEmpty(x.Added, now)}
		if x.Zone != nil {
			md.Zone = &model.Zone{ID: x.Zone.ID, Name: x.Zone.Name}
		}
		if next.Domain(d) == nil {
			next.Domains = append(next.Domains, md)
		}
		zone := "no Cloudflare zone"
		if x.Zone != nil {
			zone = "zone " + x.Zone.Name
		}
		p.Rows = append(p.Rows, Row{Kind: "domain", Legacy: d, NgiTool: "domain, cert " + firstNonEmpty(x.Cert, "none"), Note: zone})
	}

	// Apps: the same files, the override moved to NgiTool's directory.
	appName := map[string]string{} // legacy app → NgiTool app
	var recreate []string          // apps whose containers carry the legacy override
	for _, a := range l.Apps {
		if a.Broken != "" {
			p.add(Warn, "DOCK-10", "apps/"+a.Name+": "+a.Broken+" — not linked; its routes keep their alias",
				"re-link it once the project is back: ngitool app link <dir>")
			p.Rows = append(p.Rows, Row{Kind: "app", Legacy: "apps/" + a.Name, NgiTool: "–", Note: "broken: " + a.Broken})
			continue
		}
		proj := ""
		if o.ProjectOf != nil {
			proj = o.ProjectOf(a.ProjectDir)
		}
		if proj == "" {
			proj = projectName(a.Target, a.ProjectDir)
		}
		if ex := next.AppOfProject(proj); ex != nil {
			appName[a.Name] = ex.Name
			p.Rows = append(p.Rows, Row{Kind: "app", Legacy: "apps/" + a.Name, NgiTool: "app " + ex.Name, Note: "already linked · " + a.Target})
			continue
		}
		name := compose.UniqueName(a.Name, func(n string) bool { return next.App(n) != nil })
		ca := compose.App{Name: name, Project: proj, WorkingDir: a.ProjectDir, Files: []string{a.Target}, Owner: compose.OwnerOf(a.Target), LinkedAt: now}
		note := "no override"
		if a.Meta != nil && len(a.Meta.Services) > 0 {
			ca.Attached = map[string]compose.Attach{}
			var svcs []string
			for svc, at := range a.Meta.Services {
				ca.Attached[svc] = compose.Attach{Network: a.Meta.Network, Alias: at.Alias, Keys: append([]string{}, at.Keys...)}
				svcs = append(svcs, svc)
			}
			sort.Strings(svcs)
			ca.Override = compose.OverridePath(o.OverrideDir, name)
			recreate = append(recreate, name)
			note = "override: " + strings.Join(svcs, ", ") + " on " + a.Meta.Network + " → " + ca.Override
		}
		next.Apps = append(next.Apps, ca)
		appName[a.Name] = name
		p.Apps = append(p.Apps, name)
		p.Rows = append(p.Rows, Row{Kind: "app", Legacy: "apps/" + a.Name, NgiTool: "app " + name + " (project " + proj + ")", Note: note + " · " + a.Target})
	}
	if len(recreate) > 0 {
		p.add(Note, "MIG-04", "apps started with the legacy override must be recreated once through ngitool app up "+strings.Join(recreate, " ")+
			": their config_files label still names apps/<app>/edge.override.yaml, so the drift check calls them detached until then (DOCK-07)", "")
	}

	// Routes: sites, then paths.
	member := func(src LSource) (model.Member, string) {
		switch src.Type {
		case "app":
			m := model.Member{Kind: model.KindService, Host: src.Upstream, Port: src.Port}
			if a := appName[src.App]; a != "" {
				m.App = a
				m.Ref = next.App(a).Project + "/" + src.Service
			} else {
				m.Ref = src.App + "/" + src.Service
				p.add(Warn, "MIG-01", "route to app "+src.App+", which is not linked (missing or broken in apps/): kept as compose service "+m.Ref+" through "+src.Upstream, "")
			}
			return m, ""
		case "container":
			return model.Member{Kind: model.KindContainer, Ref: src.Name, Host: src.Name, Port: src.Port}, ""
		case "port":
			return model.Member{Kind: model.KindHostPort, Port: src.Port}, "the host is reached through the network's gateway (legacy: " + src.IP + ")"
		case "static":
			return model.Member{Kind: model.KindStatic, Ref: src.Dir}, ""
		}
		return model.Member{}, "unknown source type " + src.Type
	}
	addRoute := func(r model.Route, m model.Member, legacy, note string) {
		pool := model.Pool{Name: next.PoolName(r.Host, r.Path), Instance: in, Method: model.RoundRobin, Scheme: "http", Members: []model.Member{m}}
		r.Pool, r.Instance, r.Options.WebSocket, r.Options.Buffering = pool.Name, in, true, true
		if next.Route(r.ID) != nil {
			p.add(Blocker, "RP-03", r.ID+" is already a route in NgiTool's state", "remove it first: ngitool rm "+r.ID)
			return
		}
		next.Pools = append(next.Pools, pool)
		next.Routes = append(next.Routes, r)
		p.Routes = append(p.Routes, r.ID)
		ng := "→ " + m.Label()
		if !r.Enabled {
			ng += " (disabled)"
		}
		p.Rows = append(p.Rows, Row{Kind: "route", Legacy: legacy, NgiTool: ng, Note: note})
	}
	tlsOf := func(cert string) *model.TLS {
		if c := next.Cert(in, cert); c != nil {
			return c.TLS()
		}
		return nil
	}
	for _, host := range sortedKeys(l.State.Sites) {
		s := l.State.Sites[host]
		m, why := member(s.Source)
		if m.Kind == "" {
			p.add(Blocker, "MIG-01", host+": "+why, "")
			continue
		}
		r := model.Route{ID: host, Host: host, Enabled: s.On(), HTTP: model.HTTPRedirect, DNS: s.DNS, Created: firstNonEmpty(s.Added, now)}
		if !r.Enabled {
			r.Disabled = now
		}
		if s.HTTP == "serve" {
			r.HTTP = model.HTTPServe
		}
		r.TLS = tlsOf(s.Cert)
		if r.TLS == nil {
			p.add(Blocker, "MIG-01", host+" uses certificate "+s.Cert+", which is not in edge.json", "")
			continue
		}
		if _, ok := l.State.Domains[host]; ok && certs.Covers(r.TLS.Names, "www."+host) {
			r.WWW = true
		}
		var notes []string
		if why != "" {
			notes = append(notes, why)
		}
		if s.HTTP == "serve" {
			notes = append(notes, "plain HTTP too")
		}
		if s.DNS != "" {
			notes = append(notes, "dns "+s.DNS)
		}
		addRoute(r, m, host, strings.Join(notes, " · "))
	}
	for _, key := range sortedKeys(l.State.Paths) {
		x := l.State.Paths[key]
		m, why := member(x.Source)
		if m.Kind == "" {
			p.add(Blocker, "MIG-01", key+": "+why, "")
			continue
		}
		r := model.Route{ID: model.RouteID(x.Host, x.Path), Host: x.Host, Path: x.Path, Enabled: x.On(), HTTP: model.HTTPRedirect, Created: firstNonEmpty(x.Added, now)}
		if !r.Enabled {
			r.Disabled = now
		}
		r.Options.StripPrefix = x.Strip && x.Source.Type != "static"
		var notes []string
		if why != "" {
			notes = append(notes, why)
		}
		if site, ok := l.State.Sites[x.Host]; ok {
			r.TLS = tlsOf(site.Cert)
			if site.HTTP == "serve" {
				r.HTTP = model.HTTPServe
			}
		} else {
			r.TLS, r.HTTP = nil, model.HTTPServe
			notes = append(notes, "no site for "+x.Host)
		}
		if r.Options.StripPrefix {
			notes = append(notes, "prefix stripped")
		}
		addRoute(r, m, key, strings.Join(notes, " · "))
	}
	for _, f := range l.Unmanaged {
		p.Rows = append(p.Rows, Row{Kind: "file", Legacy: f, NgiTool: "left in place", Note: "hand-written: no \"" + LegacyMarker + "\" line"})
	}
	if l.TokenSet {
		p.Rows = append(p.Rows, Row{Kind: "token", Legacy: "secrets/cloudflare.ini", NgiTool: "used in place", Note: "never copied or printed"})
	}
	for rel := range l.Managed {
		p.Legacy = append(p.Legacy, rel)
	}
	sort.Strings(p.Legacy)
	drift, err := l.Drift()
	if err != nil {
		p.add(Blocker, "MIG-01", err.Error(), "")
	}
	p.Drift = drift
	for _, d := range drift {
		p.add(Blocker, "MIG-02", d+" was edited by hand (it is not what edge.json renders): the edit would be lost",
			"keep the edit in a hand-written file, or accept losing it: --on-drift overwrite")
	}
	p.Next = next
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
