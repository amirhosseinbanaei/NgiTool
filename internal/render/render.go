package render

import (
	"bytes"
	"embed"
	"fmt"
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

var tmpl = template.Must(template.ParseFS(tmplFS, "templates/*.tmpl"))

// HSTS lines per http mode, as cli/src/nginx.mjs writes them (RP-17).
const (
	HSTSOn = `add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;`
	// Served over plain HTTP too: HSTS would make browsers upgrade on their
	// own, and max-age=0 also clears what they cached from earlier visits.
	HSTSOff = `add_header Strict-Transport-Security "max-age=0" always;  # also served over plain HTTP`
)

// File is one rendered file, by the path nginx sees.
type File struct {
	Path string
	ID   string // route:<id>, pool:<name>, instance, snippet
	Body string
}

// Content is the file as written: marker, hash line, body.
func (f File) Content() string { return Stamp(f.ID, f.Body) }

// Output is everything one instance needs.
type Output struct {
	Files []File
	Dirs  []string // dirs that must exist even when empty (location include globs)
	Notes model.Problems
}

// Mutate, when set, changes a file's body after rendering. Only the
// integration test sets it, to break a file on purpose and watch the
// rollback.
var Mutate func(path, body string) string

// pool modes
const (
	modeResolve  = "resolve"  // upstream with zone + resolve (nginx ≥ 1.27.3)
	modeStatic   = "static"   // upstream with plain server lines
	modeVariable = "variable" // no upstream: set $ngt_upstream + proxy_pass (RP-07)
)

type renderer struct {
	f     Facts
	st    *model.State
	out   Output
	modes map[string]string
}

// Render produces the files of one adopted instance from the state.
func Render(st *model.State, f Facts) (Output, error) {
	r := &renderer{f: f, st: st, modes: map[string]string{}}
	id := f.Adopted.ID
	var routes []model.Route
	for _, rt := range st.RoutesOn(id) {
		if rt.Enabled {
			routes = append(routes, rt)
		}
	}
	used := map[string]bool{}
	for _, rt := range routes {
		used[rt.Pool] = true
	}
	l := f.Layout
	for _, p := range st.PoolsOn(id) {
		if !used[p.Name] || p.Static() {
			continue
		}
		if err := r.upstream(&p); err != nil {
			return r.out, err
		}
	}
	byHost := map[string][]model.Route{}
	var hosts []string
	for _, rt := range routes {
		if byHost[rt.Host] == nil {
			hosts = append(hosts, rt.Host)
		}
		byHost[rt.Host] = append(byHost[rt.Host], rt)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		if err := r.host(h, byHost[h]); err != nil {
			return r.out, err
		}
	}
	// The entry and the snippet always exist once adopted.
	entry := map[string]any{"Instance": f.Instance.Name}
	if f.OwnMap {
		entry["MapVar"] = f.UpgradeVar
	}
	var inc []string
	inc = append(inc, path.Join(l.Upstreams, "*.conf"))
	if l.Kind != model.LayoutEdge {
		inc = append(inc, path.Join(l.Servers, "*.conf"))
	}
	entry["Includes"] = inc
	if f.DefaultSSL && anyTLS(routes) {
		entry["DefaultSSL"] = true
		entry["SSLPort"] = f.HTTPSPort
		ls := []string{strconv.Itoa(f.HTTPSPort) + " ssl default_server"}
		if f.IPv6 {
			ls = append(ls, "[::]:"+strconv.Itoa(f.HTTPSPort)+" ssl default_server")
		}
		entry["SSLListens"] = ls
	}
	if err := r.add(l.Entry, "instance", "entry.conf.tmpl", entry); err != nil {
		return r.out, err
	}
	if err := r.add(l.Snippet, "snippet", "proxy.conf.tmpl", nil); err != nil {
		return r.out, err
	}
	sort.Slice(r.out.Files, func(i, j int) bool { return r.out.Files[i].Path < r.out.Files[j].Path })
	return r.out, nil
}

func anyTLS(rs []model.Route) bool {
	for _, r := range rs {
		if r.TLS != nil {
			return true
		}
	}
	return false
}

func (r *renderer) add(p, id, name string, data any) error {
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, name, data); err != nil {
		return fmt.Errorf("render %s: %w", p, err)
	}
	body := b.String()
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if Mutate != nil {
		body = Mutate(p, body)
	}
	r.out.Files = append(r.out.Files, File{Path: p, ID: id, Body: body})
	return nil
}

// addr is a member as nginx dials it, and whether it is a name to resolve.
func (r *renderer) addr(m model.Member) (string, bool, error) {
	port := strconv.Itoa(m.Port)
	switch m.Kind {
	case model.KindContainer, model.KindService:
		h := firstNonEmpty(m.Host, m.Ref)
		return net.JoinHostPort(h, port), net.ParseIP(h) == nil, nil
	case model.KindHostPort:
		if r.f.HostNet {
			return "127.0.0.1:" + port, false, nil
		}
		if r.f.Gateway == "" {
			return "", false, fmt.Errorf("%s: %s has no bridge gateway to reach the host through (RP-06)", m.Label(), r.f.Instance.Name)
		}
		return net.JoinHostPort(r.f.Gateway, port), false, nil
	case model.KindUnix:
		return "unix:" + m.Ref, false, nil
	}
	return net.JoinHostPort(m.Ref, port), net.ParseIP(m.Ref) == nil, nil
}

// ZoneSize grows with the member count (LB-14): 64k holds a few dozen
// resolved peers; each member adds room for its addresses.
func ZoneSize(members int) string {
	return strconv.Itoa(max(64, 32+8*members)) + "k"
}

func upstreamExtra(p *model.Pool) []string {
	var out []string
	for _, e := range p.Extra {
		if !strings.HasPrefix(e, "location:") {
			out = append(out, e)
		}
	}
	return out
}

func methodLine(p *model.Pool) string {
	switch p.Method {
	case model.LeastConn:
		return "least_conn"
	case model.IPHash:
		return "ip_hash"
	case model.Hash:
		s := "hash " + p.HashKey
		if p.Consistent {
			s += " consistent"
		}
		return s
	case model.Random:
		return "random"
	case model.RandomTwo:
		return "random two least_conn"
	}
	return ""
}

func (r *renderer) upstream(p *model.Pool) error {
	single := len(p.Members) == 1
	resolveNeeded := false
	var servers []string
	maxConns := false
	for _, m := range p.Members {
		a, name, err := r.addr(m)
		if err != nil {
			return err
		}
		if name {
			resolveNeeded = true
		}
		if m.MaxConns > 0 {
			maxConns = true
		}
		params := m.Params()
		if name && r.f.Resolve {
			params = append([]string{"resolve"}, params...)
		}
		servers = append(servers, strings.TrimSpace(a+" "+strings.Join(params, " ")))
	}
	mode := modeStatic
	switch {
	case r.f.Resolve:
		mode = modeResolve
	case single && resolveNeeded:
		mode = modeVariable
	}
	r.modes[p.Name] = mode
	ver := firstNonEmpty(r.f.Instance.Version, "this nginx")
	switch {
	case mode == modeVariable:
		r.out.Notes = append(r.out.Notes, model.Problem{Code: "LB-03", Level: model.LevelNote,
			Msg: "pool " + p.Name + ": " + ver + " is older than 1.27.3, so its one member is reached through a variable proxy_pass — nginx keeps starting while it is down (RP-07); keepalive and member parameters do not apply"})
		return nil
	case mode == modeStatic && resolveNeeded:
		r.out.Notes = append(r.out.Notes, model.Problem{Code: "LB-03", Level: model.LevelWarn,
			Msg: "nginx will refuse to start if a name does not resolve.",
			Fix: "pool " + p.Name + " lists names as static server lines: " + ver + " has no upstream resolve (1.27.3+)"})
	}
	data := map[string]any{
		"Pool": p.Name, "Name": p.Upstream(), "Desc": model.Describe(p),
		"Method": methodLine(p), "Servers": servers, "Keepalive": p.Keepalive, "Extra": upstreamExtra(p),
	}
	if mode == modeResolve {
		data["Zone"] = ZoneSize(len(p.Members))
		if resolveNeeded {
			data["Resolver"] = r.f.Resolver
		}
	} else if maxConns {
		data["Zone"] = ZoneSize(len(p.Members)) // max_conns needs a shared zone (LB-02)
	}
	return r.add(path.Join(r.f.Layout.Upstreams, p.Name+".conf"), "pool:"+p.Name, "upstream.conf.tmpl", data)
}

// host renders one server name: a server file for the whole host (with
// the whole-host route inline and an include of its path routes), plus
// one location file per path route. A path route on a host an existing
// edge site already serves only drops its location file there.
func (r *renderer) host(host string, routes []model.Route) error {
	var whole *model.Route
	var paths []model.Route
	for i := range routes {
		if routes[i].Path == "" {
			whole = &routes[i]
		} else {
			paths = append(paths, routes[i])
		}
	}
	l := r.f.Layout
	locDir := path.Join(l.Locations, host)
	for _, rt := range paths {
		data, err := r.location(rt)
		if err != nil {
			return err
		}
		if err := r.add(path.Join(locDir, Slug(rt.Path)+".conf"), "route:"+rt.ID, "location.conf.tmpl", data); err != nil {
			return err
		}
	}
	if whole == nil && r.attached(host) {
		return nil
	}
	r.out.Dirs = append(r.out.Dirs, locDir)
	lead := whole
	if lead == nil {
		lead = &paths[0]
	}
	var body string
	desc := ""
	needResolver := false
	if whole != nil {
		data, err := r.location(*whole)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, "location.conf.tmpl", data); err != nil {
			return err
		}
		body = indent(strings.TrimRight(b.String(), "\n"))
		desc = model.Describe(r.st.Pool(whole.Pool))
	} else {
		body = "    location / { return 404; }"
		desc = "paths only"
	}
	for _, rt := range routes {
		if r.modes[rt.Pool] == modeVariable {
			needResolver = true
		}
	}
	names := strings.Join(lead.Names(), " ")
	incl := append([]string{}, r.f.EdgeExtras...)
	incl = append(incl, path.Join(locDir, "*.conf"))
	port := func(p int, extra string) []string {
		ls := []string{strings.TrimSpace(strconv.Itoa(p) + " " + extra)}
		if r.f.IPv6 {
			ls = append(ls, strings.TrimSpace("[::]:"+strconv.Itoa(p)+" "+extra))
		}
		return ls
	}
	grpc := false
	if p := r.st.Pool(lead.Pool); p != nil && strings.HasPrefix(p.Scheme, "grpc") {
		grpc = true
	}
	resolver := ""
	if needResolver {
		resolver = r.f.Resolver
	}
	type srv struct {
		Note, Names, Cert, Key, AOP, AOPCert, HSTS, Resolver, Body string
		Listens, Includes, Extra                                   []string
		HTTP2                                                      bool
	}
	var servers []srv
	if t := lead.TLS; t != nil {
		// The edge stack answers ACME challenges over HTTPS too: Cloudflare's
		// "Always Use HTTPS" turns a renewal's http:// request into https://.
		s := srv{Names: names, Cert: t.Cert, Key: t.Key, Resolver: resolver, Body: body, Includes: withACME(incl, r.f.EdgeACME), Extra: extraOf(whole)}
		if t.AOP && r.f.EdgeAOP != "" {
			s.AOP, s.AOPCert = r.f.EdgeAOP, t.Name
		}
		if r.f.HTTP2Directive {
			s.Listens, s.HTTP2 = port(r.f.HTTPSPort, "ssl"), true
		} else {
			s.Listens = port(r.f.HTTPSPort, "ssl http2")
		}
		s.HSTS = HSTSOn
		if lead.HTTP == model.HTTPServe {
			s.HSTS = HSTSOff
		}
		servers = append(servers, s)
		if lead.HTTP == model.HTTPServe {
			h := srv{Note: "Plain HTTP as well (http: serve) — port " + strconv.Itoa(r.f.HTTPPort) + " is not redirected to HTTPS.",
				Names: names, Listens: port(r.f.HTTPPort, ""), Resolver: resolver, Body: body, Includes: withACME(incl, r.f.EdgeACME), Extra: extraOf(whole)}
			h.HTTP2 = grpc && r.f.HTTP2Directive
			servers = append(servers, h)
		} else {
			redirect := "    location / { return 301 https://$host$request_uri; }"
			var inc []string
			if r.f.EdgeACME != "" {
				inc = []string{r.f.EdgeACME}
			}
			servers = append(servers, srv{Note: "Port " + strconv.Itoa(r.f.HTTPPort) + " redirects to HTTPS (http: redirect).",
				Names: names, Listens: port(r.f.HTTPPort, ""), Body: redirect, Includes: inc})
		}
	} else {
		s := srv{Names: names, Listens: port(r.f.HTTPPort, ""), Resolver: resolver, Body: body, Includes: withACME(incl, r.f.EdgeACME), Extra: extraOf(whole)}
		s.HTTP2 = grpc && r.f.HTTP2Directive
		servers = append(servers, s)
	}
	data := map[string]any{"Host": host, "Desc": desc, "Servers": servers}
	return r.add(path.Join(l.Servers, host+".conf"), "route:"+host, "server.conf.tmpl", data)
}

func withACME(incl []string, acme string) []string {
	if acme == "" {
		return incl
	}
	return append(append([]string{}, incl...), acme)
}

func extraOf(rt *model.Route) []string {
	if rt == nil {
		return nil
	}
	var out []string
	for _, e := range rt.Extra {
		if strings.HasPrefix(e, "server:") {
			out = append(out, strings.TrimPrefix(e, "server:"))
		}
	}
	return out
}

// attached: an existing (non-NgiTool) edge site serves host and includes
// its locations dir, so path routes join that server.
func (r *renderer) attached(host string) bool {
	in := r.f.Instance
	if in.Summary == nil || in.Kind != discover.KindEdge {
		return false
	}
	own := map[string]bool{}
	for _, fi := range in.Files {
		if fi.Managed == "ngitool" {
			own[fi.Path] = true
		}
	}
	for _, srv := range in.Summary.Servers {
		if own[srv.Pos.File] {
			continue
		}
		for _, n := range srv.Names {
			if n.Name == host && model.Attaches(in, srv, host) != "" {
				return true
			}
		}
	}
	return false
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n")
}

// location is the proxy location of one route.
func (r *renderer) location(rt model.Route) (map[string]any, error) {
	p := r.st.Pool(rt.Pool)
	if p == nil {
		return nil, fmt.Errorf("route %s: pool %s does not exist (LB-12)", rt.ID, rt.Pool)
	}
	o := rt.Options
	grpc := strings.HasPrefix(p.Scheme, "grpc")
	pfx, hdr := "proxy", "proxy_set_header"
	if grpc {
		pfx, hdr = "grpc", "grpc_set_header"
	}
	var lines []string
	add := func(s ...string) { lines = append(lines, s...) }
	data := map[string]any{"Path": "/"}
	if rt.Path != "" {
		data["Path"], data["Bare"] = rt.Path+"/", rt.Path
	}
	if p.Static() {
		data["Lines"] = r.static(rt, p.Members[0])
		return data, nil
	}
	if rt.Path != "" {
		if o.StripPrefix {
			// The exact slash combination for "strip" (RP-23): /api/x → /x.
			add("rewrite ^" + regexp.QuoteMeta(rt.Path) + "/(.*)$ /$1 break;")
		}
	}
	target := p.Upstream()
	if r.modes[p.Name] == modeVariable {
		a, _, err := r.addr(p.Members[0])
		if err != nil {
			return nil, err
		}
		add("set $ngt_upstream " + a + ";")
		target = "$ngt_upstream"
	}
	add(pfx + "_pass " + p.Scheme + "://" + target + ";")
	ext := external(p)
	if grpc {
		add(hdr+" Host "+hostHeader(o, p, ext)+";",
			hdr+" X-Real-IP $remote_addr;",
			hdr+" X-Forwarded-For $proxy_add_x_forwarded_for;",
			hdr+" X-Forwarded-Proto $scheme;")
	} else {
		add("include "+r.f.Layout.Snippet+";", hdr+" Host "+hostHeader(o, p, ext)+";")
		if o.WebSocket {
			add(hdr+" Upgrade $http_upgrade;", hdr+" Connection $"+r.f.UpgradeVar+";")
		} else {
			add(hdr + ` Connection "";`)
		}
		if rt.Path != "" {
			add(hdr + " X-Forwarded-Prefix " + rt.Path + ";")
		}
	}
	for _, h := range o.Headers {
		add(hdr + " " + h.Name + " " + quote(h.Value) + ";")
	}
	add(pfx+"_connect_timeout "+firstNonEmpty(o.ConnectTimeout, "5s")+";",
		pfx+"_read_timeout "+firstNonEmpty(o.ReadTimeout, "120s")+";",
		pfx+"_send_timeout "+firstNonEmpty(o.SendTimeout, "120s")+";")
	if !o.Buffering && !grpc {
		add("proxy_buffering off;  # streaming / SSE (RP-08)")
	}
	if o.BodySize != "" {
		add("client_max_body_size " + o.BodySize + ";")
	}
	if len(p.Members) > 1 && r.modes[p.Name] != modeVariable {
		n := p.Next
		conds := n.Conditions
		if len(conds) == 0 {
			conds = []string{"error", "timeout"} // never POST unless asked (LB-10)
		}
		add(pfx + "_next_upstream " + strings.Join(conds, " ") + ";")
		tries := n.Tries
		if tries == 0 {
			tries = min(len(p.Members), 3)
		}
		add(pfx+"_next_upstream_tries "+strconv.Itoa(tries)+";", pfx+"_next_upstream_timeout "+firstNonEmpty(n.Timeout, "15s")+";")
	}
	if p.Scheme == "https" || p.Scheme == "grpcs" {
		// SNI and the name to verify (RP-11): $proxy_host would be ngt_<pool>.
		u := o.UpstreamTLS
		if u == nil {
			u = &model.UpstreamTLS{}
		}
		add(pfx+"_ssl_server_name on;", pfx+"_ssl_name "+sslName(u, p)+";")
		if u.Verify {
			add(pfx+"_ssl_verify on;", pfx+"_ssl_trusted_certificate "+firstNonEmpty(u.CA, "/etc/ssl/certs/ca-certificates.crt")+";",
				pfx+"_ssl_verify_depth 3;")
		} else {
			add(pfx + "_ssl_verify off;")
		}
	}
	if p.ErrorPage != "" {
		add("error_page 502 503 504 " + p.ErrorPage + ";  # every member down (LB-11)")
	}
	for _, e := range rt.Extra {
		if !strings.HasPrefix(e, "server:") {
			add(e)
		}
	}
	for _, e := range p.Extra {
		if strings.HasPrefix(e, "location:") {
			add(strings.TrimPrefix(e, "location:"))
		}
	}
	data["Lines"] = lines
	return data, nil
}

// static is the body of a static-file location (EDGE-04), as the legacy
// templates/body-static.tpl and path-static.conf.tpl wrote it.
func (r *renderer) static(rt model.Route, m model.Member) []string {
	dir := m.Ref
	if !strings.HasPrefix(dir, "/") {
		dir = path.Join(firstNonEmpty(r.f.WWW, "/var/www"), dir)
	}
	const spa = "# SPA fallback; for a multi-page site use: try_files $uri $uri/ =404;"
	if rt.Path == "" {
		return []string{"root " + dir + ";", spa, "try_files $uri $uri/ /index.html;"}
	}
	return []string{"alias " + dir + "/;", spa, "try_files $uri $uri/ " + rt.Path + "/index.html;"}
}

// external: every member is an address on an https pool — a service on
// the internet that serves by virtual host (RP-11, RP-24).
func external(p *model.Pool) string {
	if p.Scheme != "https" && p.Scheme != "grpcs" {
		return ""
	}
	for _, m := range p.Members {
		if m.Kind != model.KindAddress || net.ParseIP(m.Ref) != nil {
			return ""
		}
	}
	return p.Members[0].Ref
}

func hostHeader(o model.Options, p *model.Pool, ext string) string {
	switch o.HostHeader {
	case "", "$host":
		if ext != "" {
			return ext
		}
		return "$host"
	case "$proxy_host":
		if len(p.Members) > 0 && p.Members[0].Kind == model.KindAddress {
			return p.Members[0].Ref
		}
		return "$proxy_host"
	}
	return quote(o.HostHeader)
}

func sslName(u *model.UpstreamTLS, p *model.Pool) string {
	if u.ServerName != "" {
		return u.ServerName
	}
	for _, m := range p.Members {
		if m.Kind == model.KindAddress || m.Kind == model.KindContainer || m.Kind == model.KindService {
			return firstNonEmpty(m.Host, m.Ref)
		}
	}
	return "$host"
}

// quote wraps a value for nginx when it needs it.
func quote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t;{}\"'#\\") {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}
