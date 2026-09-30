package nginxconf

import (
	"net"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Summary is what an instance serves, read from its expanded tree. Every
// node keeps its file:line.
type Summary struct {
	Servers   []Server   `json:"servers"`
	Upstreams []Upstream `json:"upstreams"`
	Resolver  *Resolver  `json:"resolver,omitempty"`
	// Stream is set when a stream {} block exists. TCP/UDP proxying is out of
	// scope (RP-14, CONF-12): it is reported, never changed.
	Stream *Pos  `json:"stream,omitempty"`
	HTTP   *Pos  `json:"http,omitempty"`
	Hook   *Hook `json:"hook,omitempty"`
}

// Listen is one `listen` of a server.
type Listen struct {
	Addr     string `json:"addr,omitempty"` // "" = every address
	Port     int    `json:"port,omitempty"`
	Unix     string `json:"unix,omitempty"`
	SSL      bool   `json:"ssl,omitempty"`
	HTTP2    bool   `json:"http2,omitempty"`
	Default  bool   `json:"defaultServer,omitempty"`
	IPv6     bool   `json:"ipv6,omitempty"`
	Implicit bool   `json:"implicit,omitempty"` // no listen directive: nginx uses *:80
	Pos      Pos    `json:"pos"`
}

// Key is the socket this listen binds: address and port.
func (l Listen) Key() string {
	if l.Unix != "" {
		return "unix:" + l.Unix
	}
	a := l.Addr
	switch {
	case (a == "" || a == "*") && l.IPv6:
		a = "::"
	case a == "":
		a = "*"
	}
	return net.JoinHostPort(a, strconv.Itoa(l.Port))
}

func (l Listen) String() string {
	s := l.Key()
	if l.Addr == "" && l.Unix == "" && !l.IPv6 {
		s = strconv.Itoa(l.Port)
	}
	for _, f := range []struct {
		on   bool
		name string
	}{{l.SSL, "ssl"}, {l.HTTP2, "http2"}, {l.Default, "default_server"}} {
		if f.on {
			s += " " + f.name
		}
	}
	return s
}

// Name kinds for server_name.
const (
	NameExact    = "exact"
	NameWildcard = "wildcard"
	NameRegex    = "regex"
)

// ServerName is one server_name value.
type ServerName struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Server is one http server {}.
type Server struct {
	Pos       Pos          `json:"pos"`
	Listens   []Listen     `json:"listens"`
	Names     []ServerName `json:"names"`
	SSLCert   string       `json:"sslCertificate,omitempty"`
	SSLKey    string       `json:"sslCertificateKey,omitempty"`
	Proxies   bool         `json:"proxies,omitempty"`
	Redirects bool         `json:"redirects,omitempty"`
	Returns   bool         `json:"returns,omitempty"` // answers itself (return, ssl_reject_handshake)
	Static    bool         `json:"static,omitempty"`  // serves files (root, alias)
	Return    string       `json:"return,omitempty"`  // server-level return
	Locations []Location   `json:"locations,omitempty"`
}

// NameList is the server names joined by spaces.
func (s Server) NameList() string {
	names := make([]string, len(s.Names))
	for i, n := range s.Names {
		names[i] = n.Name
	}
	return strings.Join(names, " ")
}

// Location is one location {} (nested ones under Locations).
type Location struct {
	Pos       Pos        `json:"pos"`
	Modifier  string     `json:"modifier,omitempty"` // = ~ ~* ^~ or "" (also @ for named)
	Path      string     `json:"path"`
	Target    *Target    `json:"target,omitempty"`
	Root      string     `json:"root,omitempty"`
	Alias     string     `json:"alias,omitempty"`
	Return    string     `json:"return,omitempty"`
	Locations []Location `json:"locations,omitempty"`
}

// Target is where a location sends requests: proxy_pass, grpc_pass,
// fastcgi_pass or uwsgi_pass.
type Target struct {
	Directive string `json:"directive"` // proxy_pass, grpc_pass, …
	Raw       string `json:"raw"`       // as written
	Variable  bool   `json:"variable,omitempty"`
	// Resolved is Raw with variables replaced by the value a simple `set`
	// in scope gives them; Candidates are the values of a simple map (CONF-05).
	Resolved   string   `json:"resolved,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Scheme     string   `json:"scheme,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	Unix       string   `json:"unix,omitempty"`
	Upstream   string   `json:"upstream,omitempty"` // name of the upstream {} it names
	Pos        Pos      `json:"pos"`
}

// Effective is the target as it will be used: resolved when known.
func (t Target) Effective() string {
	if t.Resolved != "" {
		return t.Resolved
	}
	return t.Raw
}

// Upstream is an upstream {} block.
type Upstream struct {
	Name      string           `json:"name"`
	Method    string           `json:"method"` // round_robin, least_conn, ip_hash, hash …, random …, least_time …
	Servers   []UpstreamServer `json:"servers"`
	Zone      string           `json:"zone,omitempty"`
	Keepalive int              `json:"keepalive,omitempty"`
	Pos       Pos              `json:"pos"`
}

// UpstreamServer is one `server` in an upstream.
type UpstreamServer struct {
	Addr    string   `json:"addr"`
	Host    string   `json:"host,omitempty"`
	Port    int      `json:"port,omitempty"`
	Unix    string   `json:"unix,omitempty"`
	Params  []string `json:"params,omitempty"`
	Weight  int      `json:"weight,omitempty"`
	Backup  bool     `json:"backup,omitempty"`
	Down    bool     `json:"down,omitempty"`
	Resolve bool     `json:"resolve,omitempty"`
	Pos     Pos      `json:"pos"`
}

// Resolver is the http-level resolver.
type Resolver struct {
	Addrs []string `json:"addrs"`
	Valid string   `json:"valid,omitempty"`
	IPv6  string   `json:"ipv6,omitempty"`
	Pos   Pos      `json:"pos"`
}

// Hook is where NgiTool would add its one include (prompt 3). Nothing is
// written here; it is recorded so the plan can show it.
type Hook struct {
	// Existing is an include inside http {} of conf.d/*.conf or
	// sites-enabled/*, which NgiTool can drop files into.
	Existing string `json:"existing,omitempty"`
	Dir      string `json:"dir,omitempty"` // the directory that include reads
	// NeedsLine means no such include exists: one include line has to be
	// added inside http {} at Pos.
	NeedsLine bool `json:"needsLine,omitempty"`
	Pos       Pos  `json:"pos"`
}

func (h Hook) String() string {
	if h.NeedsLine {
		return "needs one include line in " + h.Pos.String()
	}
	return "include " + h.Existing + " (" + h.Pos.String() + ")"
}

// Summarize reads servers, upstreams, the resolver and the hook point.
func Summarize(tree []Directive) Summary {
	var s Summary
	maps := map[string][]string{}
	for _, d := range tree {
		switch d.Name {
		case "http":
			p := d.Pos()
			s.HTTP = &p
			s.Hook = hook(d)
			collectMaps(d.Block, maps)
		case "stream":
			if s.Stream == nil {
				p := d.Pos()
				s.Stream = &p
			}
		}
	}
	for _, d := range tree {
		if d.Name != "http" {
			continue
		}
		httpCert, httpKey := "", ""
		for _, h := range d.Block {
			switch h.Name {
			case "ssl_certificate":
				httpCert = h.Arg(0)
			case "ssl_certificate_key":
				httpKey = h.Arg(0)
			case "resolver":
				s.Resolver = resolver(h)
			case "upstream":
				s.Upstreams = append(s.Upstreams, upstream(h))
			}
		}
		for _, h := range d.Block {
			if h.Name == "server" && h.HasBlock {
				srv := server(h, maps)
				if srv.SSLCert == "" {
					srv.SSLCert, srv.SSLKey = httpCert, httpKey
				}
				s.Servers = append(s.Servers, srv)
			}
		}
	}
	names := map[string]bool{}
	for _, u := range s.Upstreams {
		names[u.Name] = true
	}
	for i := range s.Servers {
		linkUpstreams(s.Servers[i].Locations, names)
	}
	return s
}

func linkUpstreams(ls []Location, names map[string]bool) {
	Walk(ls, func(l *Location) {
		if t := l.Target; t != nil && names[t.Host] && !t.explicitPort() {
			t.Upstream = t.Host
		}
	})
}

// explicitPort reports whether the effective target spelled a port.
func (t Target) explicitPort() bool {
	h := strings.TrimPrefix(t.Effective(), t.Scheme+"://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	_, _, err := net.SplitHostPort(h)
	return err == nil
}

func hook(http Directive) *Hook {
	var confd, sites *Directive
	for i := range http.Block {
		d := &http.Block[i]
		if d.Name != "include" || len(d.Args) != 1 {
			continue
		}
		a := d.Args[0]
		switch {
		case confd == nil && strings.HasSuffix(a, "conf.d/*.conf"):
			confd = d
		case sites == nil && strings.Contains(a, "sites-enabled/") && strings.ContainsAny(a, "*?["):
			sites = d
		}
	}
	for _, d := range []*Directive{confd, sites} {
		if d != nil {
			return &Hook{Existing: d.Args[0], Dir: path.Dir(d.Args[0]), Pos: d.Pos()}
		}
	}
	return &Hook{NeedsLine: true, Pos: http.Pos()}
}

func collectMaps(ds []Directive, maps map[string][]string) {
	for _, d := range ds {
		if d.Name != "map" || len(d.Args) != 2 || !d.HasBlock {
			continue
		}
		var vals []string
		seen := map[string]bool{}
		for _, e := range d.Block {
			if len(e.Args) == 1 && e.Name != "hostnames" && e.Name != "volatile" && !seen[e.Args[0]] {
				seen[e.Args[0]] = true
				vals = append(vals, e.Args[0])
			}
		}
		maps[strings.TrimPrefix(d.Args[1], "$")] = vals
	}
}

func resolver(d Directive) *Resolver {
	r := &Resolver{Pos: d.Pos()}
	for _, a := range d.Args {
		switch {
		case strings.HasPrefix(a, "valid="):
			r.Valid = strings.TrimPrefix(a, "valid=")
		case strings.HasPrefix(a, "ipv6="):
			r.IPv6 = strings.TrimPrefix(a, "ipv6=")
		case strings.Contains(a, "="):
		default:
			r.Addrs = append(r.Addrs, a)
		}
	}
	return r
}

func upstream(d Directive) Upstream {
	u := Upstream{Name: d.Arg(0), Method: "round_robin", Pos: d.Pos()}
	for _, e := range d.Block {
		switch e.Name {
		case "server":
			u.Servers = append(u.Servers, upstreamServer(e))
		case "least_conn", "ip_hash", "ntlm", "least_time":
			u.Method = strings.TrimSpace(e.Name + " " + strings.Join(e.Args, " "))
		case "hash", "random", "sticky":
			u.Method = strings.TrimSpace(e.Name + " " + strings.Join(e.Args, " "))
		case "zone":
			u.Zone = strings.Join(e.Args, " ")
		case "keepalive":
			u.Keepalive, _ = strconv.Atoi(e.Arg(0))
		}
	}
	return u
}

func upstreamServer(d Directive) UpstreamServer {
	s := UpstreamServer{Addr: d.Arg(0), Pos: d.Pos()}
	if len(d.Args) > 1 {
		s.Params = append([]string{}, d.Args[1:]...)
	}
	if strings.HasPrefix(s.Addr, "unix:") {
		s.Unix = strings.TrimPrefix(s.Addr, "unix:")
	} else {
		s.Host, s.Port = splitHostPort(s.Addr, 80)
	}
	for _, p := range s.Params {
		switch {
		case p == "backup":
			s.Backup = true
		case p == "down":
			s.Down = true
		case p == "resolve":
			s.Resolve = true
		case strings.HasPrefix(p, "weight="):
			s.Weight, _ = strconv.Atoi(strings.TrimPrefix(p, "weight="))
		}
	}
	return s
}

// splitHostPort splits "host:port", "[v6]:port", "host" (port def).
func splitHostPort(a string, def int) (string, int) {
	if h, p, err := net.SplitHostPort(a); err == nil {
		n, _ := strconv.Atoi(p)
		return h, n
	}
	return strings.Trim(a, "[]"), def
}

// scope is the `set` values visible at a point: location over server.
type scope map[string]string

func (s scope) with(ds []Directive) scope {
	out := scope{}
	for k, v := range s {
		out[k] = v
	}
	for _, d := range ds {
		if d.Name == "set" && len(d.Args) == 2 && strings.HasPrefix(d.Args[0], "$") {
			out[strings.TrimPrefix(d.Args[0], "$")] = d.Args[1]
		}
	}
	return out
}

func server(d Directive, maps map[string][]string) Server {
	s := Server{Pos: d.Pos()}
	http2 := false
	sc := scope{}.with(d.Block)
	for _, e := range d.Block {
		switch e.Name {
		case "listen":
			s.Listens = append(s.Listens, listen(e))
		case "server_name":
			for _, n := range e.Args {
				s.Names = append(s.Names, serverName(n))
			}
		case "ssl_certificate":
			s.SSLCert = e.Arg(0)
		case "ssl_certificate_key":
			s.SSLKey = e.Arg(0)
		case "http2":
			http2 = e.Arg(0) == "on"
		case "ssl_reject_handshake":
			if e.Arg(0) == "on" {
				s.Returns = true
			}
		case "return":
			s.Return = strings.Join(e.Args, " ")
			if isRedirect(e) {
				s.Redirects = true
			} else {
				s.Returns = true
			}
		case "rewrite":
			if isRedirect(e) {
				s.Redirects = true
			}
		case "root", "alias":
			s.Static = true
		case "location":
			s.Locations = append(s.Locations, location(e, sc, maps, &s))
		}
	}
	if len(s.Listens) == 0 {
		s.Listens = []Listen{{Port: 80, Implicit: true, Pos: s.Pos}}
	}
	if http2 {
		for i := range s.Listens {
			s.Listens[i].HTTP2 = true
		}
	}
	return s
}

func isRedirect(d Directive) bool {
	switch d.Name {
	case "return":
		code, err := strconv.Atoi(d.Arg(0))
		if err != nil {
			return strings.Contains(d.Arg(0), "://") // `return URL` is a 302
		}
		return code >= 301 && code <= 308
	case "rewrite":
		last := d.Arg(len(d.Args) - 1)
		return last == "redirect" || last == "permanent" ||
			strings.HasPrefix(d.Arg(1), "http://") || strings.HasPrefix(d.Arg(1), "https://")
	}
	return false
}

func listen(d Directive) Listen {
	l := Listen{Pos: d.Pos()}
	a := d.Arg(0)
	switch {
	case strings.HasPrefix(a, "unix:"):
		l.Unix = strings.TrimPrefix(a, "unix:")
	case isDigits(a):
		l.Port, _ = strconv.Atoi(a)
	default:
		h, p := splitHostPort(a, 80)
		l.Addr, l.Port = h, p
		if h == "*" {
			l.Addr = ""
		}
		l.IPv6 = strings.HasPrefix(a, "[")
		if l.IPv6 && h == "::" {
			l.Addr = "::"
		}
	}
	for _, p := range d.Args[1:] {
		switch p {
		case "ssl":
			l.SSL = true
		case "http2":
			l.HTTP2 = true
		case "default_server", "default":
			l.Default = true
		}
	}
	return l
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func serverName(n string) ServerName {
	switch {
	case strings.HasPrefix(n, "~"):
		return ServerName{Name: n, Kind: NameRegex}
	case strings.HasPrefix(n, "*.") || strings.HasSuffix(n, ".*") || strings.HasPrefix(n, "."):
		return ServerName{Name: n, Kind: NameWildcard}
	}
	return ServerName{Name: n, Kind: NameExact}
}

var locModifiers = map[string]bool{"=": true, "~": true, "~*": true, "^~": true}

func location(d Directive, parent scope, maps map[string][]string, srv *Server) Location {
	l := Location{Pos: d.Pos()}
	switch {
	case len(d.Args) >= 2 && locModifiers[d.Args[0]]:
		l.Modifier, l.Path = d.Args[0], d.Args[1]
	case strings.HasPrefix(d.Arg(0), "@"):
		l.Modifier, l.Path = "@", strings.TrimPrefix(d.Arg(0), "@")
	case len(d.Arg(0)) > 1 && (strings.HasPrefix(d.Arg(0), "=") || strings.HasPrefix(d.Arg(0), "~")):
		// "=/exact" and "~regex" without a space are valid nginx too.
		m := d.Arg(0)[:1]
		if strings.HasPrefix(d.Arg(0), "~*") {
			m = "~*"
		}
		l.Modifier, l.Path = m, strings.TrimPrefix(d.Arg(0), m)
	default:
		l.Path = d.Arg(0)
	}
	sc := parent.with(d.Block)
	for _, e := range d.Block {
		switch e.Name {
		case "proxy_pass", "grpc_pass", "fastcgi_pass", "uwsgi_pass", "scgi_pass":
			l.Target = target(e, sc, maps)
			srv.Proxies = true
		case "root":
			l.Root = e.Arg(0)
			srv.Static = true
		case "alias":
			l.Alias = e.Arg(0)
			srv.Static = true
		case "return":
			l.Return = strings.Join(e.Args, " ")
			if isRedirect(e) {
				srv.Redirects = true
			} else {
				srv.Returns = true
			}
		case "rewrite":
			if isRedirect(e) {
				srv.Redirects = true
			}
		case "location":
			l.Locations = append(l.Locations, location(e, sc, maps, srv))
		}
	}
	return l
}

var varRE = regexp.MustCompile(`\$\{?([A-Za-z0-9_]+)\}?`)

func target(d Directive, sc scope, maps map[string][]string) *Target {
	t := &Target{Directive: d.Name, Raw: d.Arg(0), Pos: d.Pos()}
	if strings.Contains(t.Raw, "$") {
		t.Variable = true
		resolved, complete := t.Raw, true
		var mapVals []string
		resolved = varRE.ReplaceAllStringFunc(resolved, func(m string) string {
			name := varRE.FindStringSubmatch(m)[1]
			if v, ok := sc[name]; ok && !strings.Contains(v, "$") {
				return v
			}
			if vals, ok := maps[name]; ok && mapVals == nil {
				mapVals = vals
			}
			complete = false
			return m
		})
		if complete {
			t.Resolved = resolved
		} else if len(mapVals) > 0 && strings.Count(t.Raw, "$") == 1 {
			for _, v := range mapVals {
				if v != "" && !strings.Contains(v, "$") {
					t.Candidates = append(t.Candidates, varRE.ReplaceAllString(t.Raw, v))
				}
			}
			sort.Strings(t.Candidates)
		}
	}
	eff := t.Effective()
	if strings.Contains(eff, "$") {
		return t
	}
	rest := eff
	if i := strings.Index(rest, "://"); i >= 0 {
		t.Scheme, rest = rest[:i], rest[i+3:]
	}
	if strings.HasPrefix(rest, "unix:") {
		u := strings.TrimPrefix(rest, "unix:")
		if i := strings.IndexByte(u, ':'); i >= 0 {
			u = u[:i]
		}
		t.Unix = u
		return t
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	def := 80
	switch t.Scheme {
	case "https", "grpcs":
		def = 443
	}
	if t.Scheme == "" && (d.Name == "fastcgi_pass" || d.Name == "uwsgi_pass" || d.Name == "scgi_pass") {
		def = 0
	}
	t.Host, t.Port = splitHostPort(rest, def)
	return t
}

// Walk calls fn for every location, nested ones included.
func Walk(ls []Location, fn func(*Location)) {
	for i := range ls {
		fn(&ls[i])
		Walk(ls[i].Locations, fn)
	}
}
