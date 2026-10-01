package model

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// Problem levels.
const (
	LevelError = "error"
	LevelWarn  = "warn"
	LevelNote  = "note"
)

// Problem is one rule a route or pool breaks, or a warning about it. Code
// is the edge-case ID in docs/edge-cases.md.
type Problem struct {
	Code  string
	Level string // "" is an error
	Msg   string
	Fix   string
	// Connect is set when a container is not on a network the instance
	// shares (RP-05): the CLI offers `docker network connect`.
	Connect *Connect
}

// Connect is a container to join to a network.
type Connect struct {
	Container string
	Network   string
}

func (p Problem) Error() string {
	if p.Code == "" || strings.Contains(p.Msg, "("+p.Code+")") {
		return p.Msg
	}
	return p.Msg + " (" + p.Code + ")"
}

// IsError is true for errors (the default level).
func (p Problem) IsError() bool { return p.Level == "" || p.Level == LevelError }

// Problems are a check's findings.
type Problems []Problem

// Errors are the problems that block the change.
func (ps Problems) Errors() Problems {
	var out Problems
	for _, p := range ps {
		if p.IsError() {
			out = append(out, p)
		}
	}
	return out
}

// Others are warnings and notes.
func (ps Problems) Others() Problems {
	var out Problems
	for _, p := range ps {
		if !p.IsError() {
			out = append(out, p)
		}
	}
	return out
}

// Err is the first error, or nil.
func (ps Problems) Err() error {
	if e := ps.Errors(); len(e) > 0 {
		return e[0]
	}
	return nil
}

// HostNetwork reports whether an instance resolves names like the host:
// a host nginx, or a container in host network mode (DISC-12).
func HostNetwork(in *discover.Instance) bool {
	return in.Kind == discover.KindHost || in.NetworkMode == "host"
}

// CheckPool checks a pool and its members against the instance it lives
// on (LB-01, LB-02, LB-15, LB-16, RP-05, RP-06, RP-16).
func CheckPool(st *State, rep *discover.Report, p *Pool) Problems {
	var ps Problems
	add := func(code, level, msg, fix string) {
		ps = append(ps, Problem{Code: code, Level: level, Msg: msg, Fix: fix})
	}
	if err := ValidPoolName(p.Name); err != nil {
		add("LB-13", "", err.Error(), "")
	}
	n := 0
	for _, q := range st.Pools {
		if q.Name == p.Name {
			n++
		}
	}
	if n > 1 {
		add("LB-13", "", "a pool named "+p.Name+" already exists", "pick another name, or edit it: ngitool pool show "+p.Name)
	}
	switch p.Method {
	case RoundRobin, LeastConn, IPHash, Random, RandomTwo:
	case Hash:
		if p.HashKey == "" {
			add("LB-01", "", "the hash method needs a key, e.g. $request_uri or $cookie_session", "pass --hash-key KEY")
		}
	default:
		if why, ok := PlusOnly[p.Method]; ok {
			add("LB-15", "", why, "")
		} else {
			add("LB-01", "", "unknown method "+p.Method, "one of round_robin, least_conn, ip_hash, hash, random_two, random")
		}
	}
	if len(p.Members) == 0 {
		add("LB-11", "", "pool "+p.Name+" has no members", "add one: ngitool pool member add "+p.Name+" container:NAME:PORT")
	}
	hasBackup := false
	seen := map[string]bool{}
	for _, m := range p.Members {
		if m.Backup {
			hasBackup = true
		}
		if seen[m.Label()] {
			add("LB-01", "", m.Label()+" is in pool "+p.Name+" twice", "remove one: ngitool pool member rm "+p.Name+" "+m.Label())
		}
		seen[m.Label()] = true
		if m.Kind != KindUnix && m.Port == 0 {
			add("RP-05", "", m.Label()+" has no port", "give it one, e.g. "+m.Label()+":3000")
		}
	}
	if hasBackup && (p.Method == Hash || p.Method == IPHash || p.Method == Random || p.Method == RandomTwo) {
		add("LB-02", "", "nginx rejects backup members with "+p.MethodText(),
			"use round robin or least connections for a pool with a backup, or drop backup from the member")
	}
	if len(p.Active()) == 0 && len(p.Members) > 0 {
		add("LB-11", LevelWarn, "every member of "+p.Name+" is down or backup: requests get 502"+errPageNote(p),
			"bring one back: ngitool pool undrain "+p.Name+" MEMBER")
	}
	switch p.Scheme {
	case "http", "https", "grpc", "grpcs":
	case "tcp", "udp":
		add("RP-14", "", "TCP/UDP stream proxying is out of scope: NgiTool reports stream {} blocks but never writes them", "proxy it with a hand-written stream {} block")
	default:
		add("LB-16", "", "unknown upstream scheme "+p.Scheme, "http, https, grpc or grpcs")
	}
	if p.Keepalive < 0 || p.Keepalive > 1024 {
		add("LB-06", "", "keepalive is the number of idle connections per worker, 0 to 1024", "")
	}
	switch {
	case p.ErrorPage == "", strings.HasPrefix(p.ErrorPage, "http://"), strings.HasPrefix(p.ErrorPage, "https://"):
	case strings.HasPrefix(p.ErrorPage, "/"):
		add("LB-11", LevelNote, "error page "+p.ErrorPage+" must be served by another route on the same host (a path route to a static or healthy pool) — otherwise nginx shows its own 502 page", "")
	default:
		add("LB-11", "", "the error page is a path on the same host (/maintenance.html) or a URL", "")
	}
	if err := ValidTime(p.Next.Timeout); err != nil {
		add("LB-10", "", "next_upstream timeout: "+err.Error(), "")
	}
	for _, c := range p.Next.Conditions {
		if !validNext[c] {
			add("LB-10", "", "unknown proxy_next_upstream condition "+c, "error, timeout, invalid_header, http_500, http_502, http_503, http_504, http_403, http_404, http_429, non_idempotent, off")
		}
	}
	in := rep.Find(p.Instance)
	if in == nil {
		add("", "", "instance "+p.Instance+" was not found by the last scan", "run: ngitool scan")
		return ps
	}
	for _, m := range p.Members {
		ps = append(ps, checkMember(rep, in, m)...)
	}
	return ps
}

var validNext = map[string]bool{"error": true, "timeout": true, "invalid_header": true, "http_500": true, "http_502": true,
	"http_503": true, "http_504": true, "http_403": true, "http_404": true, "http_429": true, "non_idempotent": true, "off": true}

func errPageNote(p *Pool) string {
	if p.ErrorPage != "" {
		return " (" + p.ErrorPage + " is shown)"
	}
	return ""
}

// checkMember is RP-05, RP-06 and RP-16 for one member on one instance.
func checkMember(rep *discover.Report, in *discover.Instance, m Member) Problems {
	var ps Problems
	hostNet := HostNetwork(in)
	switch m.Kind {
	case KindContainer, KindService:
		cs := memberContainers(rep, m)
		if hostNet {
			fix := "publish its port on 127.0.0.1 (ports: [\"127.0.0.1:3000:3000\"]) and add it as port:3000"
			for _, c := range cs {
				for _, pub := range c.Ports {
					if pub.ContainerPort == m.Port && pub.Proto == "tcp" {
						fix = "it already publishes " + pub.HostIP + ":" + strconv.Itoa(pub.HostPort) + " — add it as port:" + strconv.Itoa(pub.HostPort)
					}
				}
			}
			ps = append(ps, Problem{Code: "RP-05", Msg: in.Name + " runs on the host, where the container name " + m.Ref + " does not resolve", Fix: fix})
			return ps
		}
		if len(cs) == 0 {
			ps = append(ps, Problem{Code: "RP-07", Level: LevelWarn, Msg: m.Label() + " is not running right now — the route answers 502 until it is"})
			return ps
		}
		if m.Kind == KindService && len(cs) > 1 {
			ps = append(ps, Problem{Code: "LB-04", Level: LevelNote, Msg: m.Ref + " has " + strconv.Itoa(len(cs)) + " replicas behind one name: resolve keeps all of them in the pool"})
		}
		for _, c := range cs {
			if !shares(in, c) {
				net := joinNetwork(in)
				ps = append(ps, Problem{Code: "RP-05", Level: LevelWarn,
					Msg:     c.Name + " is not on a network " + in.Name + " shares (it is on " + orNone(c.Networks) + ")",
					Fix:     "connect it: docker network connect " + net + " " + c.Name + " — lasts until the container is recreated (prompt 4 makes it permanent)",
					Connect: &Connect{Container: c.Name, Network: net}})
			}
		}
	case KindHostPort:
		addr := "127.0.0.1"
		if !hostNet {
			addr = Gateway(rep, in)
			if addr == "" {
				ps = append(ps, Problem{Code: "RP-06", Msg: in.Name + " has no bridge gateway to reach the host through", Fix: "use host network mode for it, or an address member"})
				return ps
			}
		}
		ps = append(ps, listenCheck(rep, addr, m.Port, hostNet)...)
		if !hostNet {
			ps = append(ps, Problem{Code: "RP-06", Level: LevelNote, Msg: in.Name + " reaches the host at " + addr + ":" + strconv.Itoa(m.Port),
				Fix: "firewall: ufw allow from " + subnet16(addr) + " to any port " + strconv.Itoa(m.Port) + " proto tcp"})
		}
	case KindUnix:
		if hostNet && in.Kind == discover.KindHost {
			st, err := os.Stat(m.Ref)
			switch {
			case err != nil:
				ps = append(ps, Problem{Code: "RP-16", Level: LevelWarn, Msg: m.Ref + " does not exist yet — the route answers 502 until it does"})
			case st.Mode()&os.ModeSocket == 0:
				ps = append(ps, Problem{Code: "RP-16", Msg: m.Ref + " is not a unix socket"})
			case st.Mode().Perm()&0o006 != 0o006:
				ps = append(ps, Problem{Code: "RP-16", Level: LevelWarn, Msg: m.Ref + " is " + st.Mode().Perm().String() + ": nginx workers (" + firstNonEmpty(in.User, "the nginx user") + ") need read and write on it",
					Fix: "chmod 666 the socket, or put the worker user in its group (chmod 660)"})
			}
		} else {
			ps = append(ps, Problem{Code: "RP-16", Level: LevelWarn, Msg: m.Ref + " must be inside a volume mounted into " + in.Name + ", at the same path"})
		}
	case KindAddress:
		if c := rep.Container(m.Ref); c != nil {
			ps = append(ps, Problem{Code: "RP-05", Level: LevelWarn, Msg: m.Ref + " is a container name", Fix: "add it as container:" + m.Ref + ":" + strconv.Itoa(m.Port) + " so NgiTool checks its network"})
		}
	}
	return ps
}

// listenCheck ports cli/src/flows.mjs portSource: something must listen on
// port at an address the instance can reach.
func listenCheck(rep *discover.Report, addr string, port int, hostNet bool) Problems {
	var on []string
	reach := false
	for _, l := range rep.Listeners {
		if l.Port != port {
			continue
		}
		on = append(on, net.JoinHostPort(l.Addr, strconv.Itoa(port))+procNote(l))
		switch {
		case l.Addr == "" || l.Addr == "*" || l.Addr == "0.0.0.0" || l.Addr == "::" || l.Addr == addr:
			reach = true
		case hostNet && net.ParseIP(l.Addr) != nil && net.ParseIP(l.Addr).IsLoopback():
			reach = true
		}
	}
	switch {
	case len(on) == 0:
		return Problems{{Code: "RP-06", Level: LevelWarn, Msg: "nothing listens on port " + strconv.Itoa(port) + " yet — the route answers 502 until something does"}}
	case !reach:
		return Problems{{Code: "RP-06", Level: LevelWarn, Msg: "port " + strconv.Itoa(port) + " listens on " + strings.Join(on, ", ") + " — nginx needs " + addr + " or 0.0.0.0",
			Fix: "bind the process to 0.0.0.0 or " + addr}}
	}
	return nil
}

func procNote(l discover.PortOwner) string {
	if l.Process != "" {
		return " (" + l.Process + ")"
	}
	return ""
}

func subnet16(ip string) string {
	p := strings.Split(ip, ".")
	if len(p) == 4 {
		return p[0] + "." + p[1] + ".0.0/16"
	}
	return ip
}

func orNone(xs []string) string {
	if len(xs) == 0 {
		return "no network"
	}
	return strings.Join(xs, ", ")
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// memberContainers are the containers behind a container or service member.
func memberContainers(rep *discover.Report, m Member) []discover.Container {
	var out []discover.Container
	switch m.Kind {
	case KindContainer:
		if c := rep.Container(m.Ref); c != nil && c.Running {
			out = append(out, *c)
		}
	case KindService:
		proj, svc, _ := strings.Cut(m.Ref, "/")
		for _, c := range rep.Containers {
			if c.Project == proj && c.Service == svc && c.Running {
				out = append(out, c)
			}
		}
	}
	return out
}

func shares(in *discover.Instance, c discover.Container) bool {
	for _, a := range in.Networks {
		for _, b := range c.Networks {
			if a == b && a != "none" && a != "host" {
				return true
			}
		}
	}
	return false
}

// joinNetwork is the network a container should join to be reachable from
// the instance: a user network (Docker DNS only works there), not "bridge".
func joinNetwork(in *discover.Instance) string {
	for _, n := range in.Networks {
		if n != "bridge" && n != "host" && n != "none" {
			return n
		}
	}
	if len(in.Networks) > 0 {
		return in.Networks[0]
	}
	return "bridge"
}

// Gateway is the host as a containerised instance reaches it: the gateway
// of its first user network (RP-06). Not stored: networks get recreated.
func Gateway(rep *discover.Report, in *discover.Instance) string {
	c := rep.Container(in.Container)
	if c == nil {
		return ""
	}
	nets := append([]string{}, c.Networks...)
	sort.SliceStable(nets, func(i, j int) bool { return nets[i] != "bridge" && nets[j] == "bridge" })
	for _, n := range nets {
		if g := c.Gateways[n]; g != "" {
			return g
		}
	}
	return ""
}

// ResolveMember fills Host for a compose-service member: the service name
// when it resolves on a network the instance shares, else the container
// name. Names only, never IPs (DOCK-16).
func ResolveMember(rep *discover.Report, in *discover.Instance, m *Member) {
	switch m.Kind {
	case KindContainer:
		if m.Host == "" {
			m.Host = m.Ref
		}
	case KindService:
		cs := memberContainers(rep, *m)
		_, svc, _ := strings.Cut(m.Ref, "/")
		for _, c := range cs {
			for _, n := range in.Networks {
				for _, dn := range c.DNS[n] {
					if dn == svc {
						m.Host = svc
						return
					}
				}
			}
		}
		if len(cs) > 0 {
			m.Host = cs[0].Name
		} else if m.Host == "" {
			m.Host = svc
		}
	}
}

// CheckRoute checks a route against the state (which already holds it) and
// what the scan found on its instance and the others (RP-02, RP-03, RP-04,
// RP-17, RP-19, RP-20).
func CheckRoute(st *State, rep *discover.Report, r *Route) Problems {
	var ps Problems
	add := func(code, level, msg, fix string) {
		ps = append(ps, Problem{Code: code, Level: level, Msg: msg, Fix: fix})
	}
	in := rep.Find(r.Instance)
	if in == nil {
		add("", "", "instance "+r.Instance+" was not found by the last scan", "run: ngitool scan")
		return ps
	}
	if st.Adopted(r.Instance) == nil {
		add("", "", in.Name+" is not adopted yet", "run: ngitool instance adopt "+r.Instance)
	}
	for _, o := range st.Routes {
		if o.Host == r.Host && o.Instance != r.Instance {
			add("RP-04", "", r.Host+" is already routed on "+o.Instance,
				"one host lives on one instance; to chain, route it on the front door to the inner nginx as an address member")
		}
	}
	n := 0
	for _, o := range st.Routes {
		if o.ID == r.ID && o.Instance == r.Instance {
			n++
		}
	}
	if n > 1 {
		add("RP-03", "", r.ID+" is already routed on "+in.Name, "change it instead: ngitool route edit "+r.ID)
	}
	p := st.Pool(r.Pool)
	switch {
	case p == nil:
		add("LB-12", "", "pool "+r.Pool+" does not exist", "create it: ngitool pool add "+r.Pool)
	case p.Instance != r.Instance:
		add("LB-12", "", "pool "+r.Pool+" lives on "+p.Instance+", not "+r.Instance, "a pool serves routes of its own instance")
	}
	if r.HTTP != HTTPRedirect && r.HTTP != HTTPServe {
		add("RP-17", "", "http mode is redirect or serve, not "+r.HTTP, "")
	}
	if r.HTTP == HTTPServe && r.TLS != nil {
		add("RP-17", LevelNote, r.Host+" is also served over plain HTTP: HSTS is turned off (max-age=0), but browsers that saw it before may keep upgrading to HTTPS until it expires", "")
	}
	if r.TLS != nil && len(r.TLS.Names) > 0 {
		for _, name := range r.Names() {
			if !Covers(r.TLS.Names, name) {
				add("RP-18", "", "certificate "+r.TLS.Name+" does not cover "+name+" (it covers "+strings.Join(r.TLS.Names, ", ")+")", "pick another certificate, or serve it over HTTP only")
			}
		}
	}
	if r.Path != "" {
		add("RP-02", LevelWarn, "apps without a base path redirect-loop on a path mount (Next.js is the known case): "+r.Path+" works only if the app is built for it",
			"build the app with basePath "+r.Path+", or use a subdomain")
	} else if r.Options.StripPrefix {
		add("RP-23", "", "strip prefix needs a path route", "")
	}
	o := r.Options
	for _, err := range []error{ValidSize(o.BodySize), ValidTime(o.ConnectTimeout), ValidTime(o.ReadTimeout), ValidTime(o.SendTimeout)} {
		if err != nil {
			add("RP-10", "", err.Error(), "")
		}
	}
	if o.UpstreamTLS != nil && p != nil && p.Scheme != "https" && p.Scheme != "grpcs" {
		add("RP-11", "", "upstream TLS options need an https or grpcs pool", "")
	}
	if p != nil && (p.Scheme == "grpc" || p.Scheme == "grpcs") && r.TLS == nil {
		add("RP-14", LevelWarn, "gRPC without TLS needs clients that speak h2c with prior knowledge", "attach a certificate for normal gRPC clients")
	}
	ps = append(ps, existingServers(rep, in, r)...)
	return ps
}

// existingServers is RP-03 / RP-04 against what the configs already serve,
// and RP-20 when a path route attaches to an existing server.
func existingServers(rep *discover.Report, in *discover.Instance, r *Route) Problems {
	var ps Problems
	for _, other := range rep.Instances {
		if other.Summary == nil {
			continue
		}
		managed := managedFiles(&other)
		for _, srv := range other.Summary.Servers {
			if managed[srv.Pos.File] == "ngitool" || !serves(srv, r.Names()) {
				continue
			}
			if other.ID != in.ID {
				ps = append(ps, Problem{Code: "RP-04", Msg: r.Host + " is already served by " + other.Name + " (" + srv.Pos.String() + ")",
					Fix: "one host lives on one instance; to chain, route it on the front door to " + other.Name + " as an address member"})
				continue
			}
			if r.Path != "" && Attaches(&other, srv, r.Host) != "" {
				ps = append(ps, Shadowed(srv.Locations, r.Path)...)
				continue
			}
			who := "a hand-written server"
			if managed[srv.Pos.File] == "edge" {
				who = "a server the edge CLI manages"
			}
			ps = append(ps, Problem{Code: "RP-03", Msg: r.Host + " is already served on " + in.Name + " by " + who + " (" + srv.Pos.String() + ")",
				Fix: "nginx would keep the first and ignore the other; edit or remove that server first"})
		}
	}
	return ps
}

// Attaches is the include of locations/<host>/*.conf in an existing edge
// server, which a path route can drop its location file into instead of
// adding a second server; "" when there is none.
func Attaches(in *discover.Instance, srv nginxconf.Server, host string) string {
	if in.Kind != discover.KindEdge {
		return ""
	}
	for _, f := range in.Files {
		if strings.HasSuffix(path0(f.Path), "/locations/"+host) {
			return f.Path
		}
	}
	// The glob may match nothing yet: an edge site file always includes it.
	if managedFiles(in)[srv.Pos.File] == "edge" {
		return "/etc/nginx/edge/locations/" + host
	}
	return ""
}

func path0(p string) string {
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return p
}

func managedFiles(in *discover.Instance) map[string]string {
	out := map[string]string{}
	for _, f := range in.Files {
		if f.Managed != "" {
			out[f.Path] = f.Managed
		}
	}
	return out
}

func serves(srv nginxconf.Server, names []string) bool {
	for _, n := range srv.Names {
		for _, want := range names {
			if n.Kind == nginxconf.NameExact && strings.EqualFold(n.Name, want) {
				return true
			}
		}
	}
	return false
}

// Shadowed is RP-20: the existing locations that would win over a new
// prefix location for path, in nginx's order (exact, ^~, regex, longest
// prefix). A warning, not an error.
func Shadowed(locs []nginxconf.Location, p string) Problems {
	var ps Problems
	sample := []string{p + "/", p + "/index.html", p + "/a/b.js", p + "/api/x.json"}
	for _, l := range locs {
		switch l.Modifier {
		case "^~":
			if strings.HasPrefix(p+"/", l.Path) && len(l.Path) < len(p)+1 {
				ps = append(ps, Problem{Code: "RP-20", Level: LevelWarn,
					Msg: "location ^~ " + l.Path + " (" + l.Pos.String() + ") is a shorter non-regex prefix: it stops regex checks but " + p + "/ still wins as the longer prefix"})
			}
		case "~", "~*":
			expr := l.Path
			if l.Modifier == "~*" {
				expr = "(?i)" + expr
			}
			re, err := regexp.Compile(expr)
			if err != nil {
				continue
			}
			for _, s := range sample {
				if re.MatchString(s) {
					ps = append(ps, Problem{Code: "RP-20", Level: LevelWarn,
						Msg: "regex location " + l.Modifier + " " + l.Path + " (" + l.Pos.String() + ") wins over " + p + "/ for requests like " + s,
						Fix: "regex locations beat prefix ones; NgiTool's location is not ^~ so the app's own files may be served by that regex"})
					break
				}
			}
		}
	}
	return ps
}

// Covers reports whether certificate names cover host; a wildcard covers
// one level only (cli/src/targets.mjs nameCovers).
func Covers(names []string, host string) bool {
	for _, n := range names {
		n = strings.ToLower(n)
		if n == host {
			return true
		}
		if base, ok := strings.CutPrefix(n, "*."); ok && strings.HasSuffix(host, "."+base) && !strings.Contains(strings.TrimSuffix(host, "."+base), ".") {
			return true
		}
	}
	return false
}

// Describe is a pool in one line: "web:3000, api:3000 · least connections".
func Describe(p *Pool) string {
	if p == nil {
		return "?"
	}
	var ms []string
	for _, m := range p.Members {
		s := m.Label()
		switch {
		case m.Down:
			s += " (down)"
		case m.Backup:
			s += " (backup)"
		}
		ms = append(ms, s)
	}
	if len(p.Members) == 1 {
		return ms[0]
	}
	return fmt.Sprintf("%s · %s", strings.Join(ms, ", "), p.MethodText())
}
