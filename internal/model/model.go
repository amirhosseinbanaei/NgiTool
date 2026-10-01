// Package model is what NgiTool manages: routes, pools and their members,
// and the instances it was allowed to write to. It is stored in
// /var/lib/ngitool/state.json. Everything is checked here (Check) before
// anything renders, and every rule names its edge-case ID.
//
// Container IPs are never stored (DOCK-16): members name containers, compose
// services and aliases, and nginx resolves them.
package model

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
)

// Member kinds.
const (
	KindContainer = "container"
	KindService   = "compose-service"
	KindHostPort  = "host-port"
	KindAddress   = "address"
	KindUnix      = "unix"
	// KindStatic serves files instead of proxying: Ref is a folder under
	// the edge stack's www/, or an absolute directory on a host instance
	// (EDGE-04).
	KindStatic = "static"
)

// Balancing methods (LB-01). One per pool.
const (
	RoundRobin = "round_robin"
	LeastConn  = "least_conn"
	IPHash     = "ip_hash"
	Hash       = "hash"
	Random     = "random"
	RandomTwo  = "random_two" // random two least_conn
)

// Methods is every method, in the order the wizard shows them.
var Methods = []string{RoundRobin, LeastConn, IPHash, Hash, RandomTwo, Random}

// DefaultMethod for a new pool of two or more members (open decision,
// recorded in AGENTS.md): least_conn degrades better than round robin when
// request times are uneven.
const DefaultMethod = LeastConn

// HTTP modes for port 80 (RP-17).
const (
	HTTPRedirect = "redirect"
	HTTPServe    = "serve"
)

// Instance layouts (render): where NgiTool's files go on each kind.
const (
	LayoutHost  = "host"  // /etc/nginx/ngitool/{upstreams,servers,locations}/ + conf.d/ngitool.conf
	LayoutEdge  = "edge"  // the edge stack's own conf/sites and conf/locations
	LayoutMount = "mount" // a ngitool/ subdir inside a container's bind-mounted conf dir
)

// Member is one server of a pool.
type Member struct {
	Kind string `json:"kind"`
	// Ref is what the user picked: a container name, "project/service", a
	// host or IP (address), a socket path (unix); empty for host-port.
	Ref string `json:"ref,omitempty"`
	// Host is the DNS name nginx uses for containers and services: a
	// container name or alias that resolves on a shared network. Never an IP.
	Host string `json:"host,omitempty"`
	// App is the linked app a compose-service member belongs to; Ref is
	// then "<project>/<service>" and Host the alias the override gives it.
	App         string `json:"app,omitempty"`
	Port        int    `json:"port,omitempty"`
	Weight      int    `json:"weight,omitempty"`
	Backup      bool   `json:"backup,omitempty"`
	Down        bool   `json:"down,omitempty"`
	MaxFails    *int   `json:"maxFails,omitempty"`
	FailTimeout string `json:"failTimeout,omitempty"`
	MaxConns    int    `json:"maxConns,omitempty"`
}

// Label is how a member is shown and referenced: web:3000,
// shop/web:3000, host:3000, 10.0.0.5:8080, unix:/run/app.sock.
func (m Member) Label() string {
	switch m.Kind {
	case KindUnix:
		return "unix:" + m.Ref
	case KindHostPort:
		return "host:" + strconv.Itoa(m.Port)
	case KindStatic:
		return "static:" + m.Ref
	}
	return hostPort(m.Ref, m.Port)
}

// Matches reports whether s names this member: its label, its ref, or
// ref:port.
func (m Member) Matches(s string) bool {
	return s == m.Label() || s == m.Ref || s == m.Host || s == hostPort(m.Host, m.Port) ||
		(m.Kind == KindContainer && s == "container:"+m.Label())
}

// Params are the nginx server parameters (no resolve; render adds it).
func (m Member) Params() []string {
	var p []string
	if m.Weight > 1 {
		p = append(p, "weight="+strconv.Itoa(m.Weight))
	}
	if m.MaxFails != nil {
		p = append(p, "max_fails="+strconv.Itoa(*m.MaxFails))
	}
	if m.FailTimeout != "" {
		p = append(p, "fail_timeout="+m.FailTimeout)
	}
	if m.MaxConns > 0 {
		p = append(p, "max_conns="+strconv.Itoa(m.MaxConns))
	}
	if m.Backup {
		p = append(p, "backup")
	}
	if m.Down {
		p = append(p, "down")
	}
	return p
}

func hostPort(h string, port int) string {
	if port == 0 {
		return h
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
}

// NextUpstream is when a request moves on to the next member (LB-10).
// Non-idempotent requests (POST …) are never retried unless "non_idempotent"
// is one of the conditions.
type NextUpstream struct {
	Conditions []string `json:"conditions,omitempty"`
	Tries      int      `json:"tries,omitempty"`
	Timeout    string   `json:"timeout,omitempty"`
}

// Pool is a group of members behind one upstream, ngt_<name> (LB-13).
// A route to a single target is a pool of one member.
type Pool struct {
	Name       string `json:"name"`
	Instance   string `json:"instance"`
	Method     string `json:"method"`
	HashKey    string `json:"hashKey,omitempty"`
	Consistent bool   `json:"consistent,omitempty"`
	// Sticky is the preset the method came from (LB-07): ip, cloudflare,
	// cookie:<name>. Shown only; Method and HashKey carry the behaviour.
	Sticky    string       `json:"sticky,omitempty"`
	Scheme    string       `json:"scheme"` // http, https, grpc, grpcs — one per pool (LB-16)
	Members   []Member     `json:"members"`
	Keepalive int          `json:"keepalive,omitempty"`
	Next      NextUpstream `json:"nextUpstream,omitempty"`
	ErrorPage string       `json:"errorPage,omitempty"` // shown when every member is down (LB-11)
	// Previous is the member set before `pool switch`, kept as backup
	// members until the switch is confirmed (LB-09).
	Previous []Member `json:"previous,omitempty"`
	// Extra are directives adopted from a hand edit (APPLY-05).
	Extra []string `json:"extra,omitempty"`
}

// Upstream is the nginx name of the pool (LB-13).
func (p Pool) Upstream() string { return "ngt_" + strings.ReplaceAll(p.Name, "-", "_") }

// Static reports whether the pool serves files: one static member.
func (p Pool) Static() bool { return len(p.Members) == 1 && p.Members[0].Kind == KindStatic }

// Active are the members that take traffic now (not down, not backup).
func (p Pool) Active() []Member {
	var out []Member
	for _, m := range p.Members {
		if !m.Down && !m.Backup {
			out = append(out, m)
		}
	}
	return out
}

// MethodText is the method as a person reads it.
func (p Pool) MethodText() string {
	switch p.Method {
	case "", RoundRobin:
		return "round robin"
	case LeastConn:
		return "least connections"
	case IPHash:
		return "IP hash"
	case Hash:
		s := "hash " + p.HashKey
		if p.Consistent {
			s += " consistent"
		}
		return s
	case RandomTwo:
		return "random two least_conn"
	}
	return p.Method
}

// Member returns the index of the member s names, or -1.
func (p Pool) Member(s string) int {
	for i, m := range p.Members {
		if m.Matches(s) {
			return i
		}
	}
	return -1
}

// TLS is the certificate of a route, by the paths nginx sees: one that was
// found on the instance, or one NgiTool issued (State.Certs).
type TLS struct {
	Name string `json:"name"`
	Cert string `json:"cert"`
	Key  string `json:"key"`
	// Names are the DNS names the certificate covers, when known.
	Names []string `json:"names,omitempty"`
	// AOP: Authenticated Origin Pulls — only Cloudflare may connect (the
	// edge stack's snippets/cloudflare-aop.conf). Set per certificate.
	AOP bool `json:"aop,omitempty"`
}

// Header is one extra request header sent upstream.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// UpstreamTLS is how nginx talks to an https/grpcs pool (RP-11).
type UpstreamTLS struct {
	Verify     bool   `json:"verify,omitempty"`
	ServerName string `json:"serverName,omitempty"`
	CA         string `json:"ca,omitempty"` // trusted CA file when verifying
}

// Options are per-route proxy settings.
type Options struct {
	StripPrefix    bool         `json:"stripPrefix,omitempty"`
	WebSocket      bool         `json:"websocket"` // on by default
	Buffering      bool         `json:"buffering"` // on by default; off for SSE and streaming (RP-08)
	BodySize       string       `json:"bodySize,omitempty"`
	ConnectTimeout string       `json:"connectTimeout,omitempty"`
	ReadTimeout    string       `json:"readTimeout,omitempty"`
	SendTimeout    string       `json:"sendTimeout,omitempty"`
	Headers        []Header     `json:"headers,omitempty"`
	UpstreamTLS    *UpstreamTLS `json:"upstreamTls,omitempty"`
	// HostHeader is "" ($host), "$proxy_host", or a literal name (RP-11, RP-24).
	HostHeader string `json:"hostHeader,omitempty"`
}

// DefaultOptions are what a new route gets.
func DefaultOptions() Options { return Options{WebSocket: true, Buffering: true} }

// Route sends a host, or a path of it, to a pool.
type Route struct {
	ID       string `json:"id"` // host, or host/path
	Instance string `json:"instance"`
	Host     string `json:"host"`
	Path     string `json:"path,omitempty"` // "" is the whole host
	WWW      bool   `json:"www,omitempty"`  // also serve www.<host> (RP-01)
	Pool     string `json:"pool"`
	Enabled  bool   `json:"enabled"`
	HTTP     string `json:"http"` // redirect or serve (RP-17)
	// DNS is how the host's Cloudflare record is kept: proxied, dns-only,
	// or skip / "" (NgiTool leaves DNS alone).
	DNS      string   `json:"dns,omitempty"`
	TLS      *TLS     `json:"tls,omitempty"` // nil: plain HTTP only
	Options  Options  `json:"options"`
	Extra    []string `json:"extra,omitempty"` // adopted hand edits (APPLY-05)
	Created  string   `json:"created,omitempty"`
	Disabled string   `json:"disabledAt,omitempty"`
}

// RouteID is host or host/path.
func RouteID(host, path string) string { return host + path }

// Names are the server_name values of the route's server.
func (r Route) Names() []string {
	if r.WWW {
		return []string{r.Host, "www." + r.Host}
	}
	return []string{r.Host}
}

// Include is the one edit NgiTool ever makes to a hand-written file: an
// include line inside http {} (adopt). It is backed up and undone by
// `instance release`.
type Include struct {
	File   string `json:"file"`   // host path of the hand-written file
	Line   int    `json:"line"`   // 1-based line the include was inserted at
	Text   string `json:"text"`   // the exact line
	Backup string `json:"backup"` // host path of the copy taken before
}

// Adopted is an instance NgiTool may write to.
type Adopted struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Layout string `json:"layout"`
	// Root is NgiTool's directory as nginx sees it; HostRoot is the same
	// directory on this machine (through a bind mount for containers).
	Root     string   `json:"root"`
	HostRoot string   `json:"hostRoot"`
	Include  *Include `json:"include,omitempty"`
	At       string   `json:"adoptedAt"`
}

// DNS modes of a route's Cloudflare record.
const (
	DNSProxied = "proxied"
	DNSOnly    = "dns-only"
	DNSSkip    = "skip"
)

// Cert is a certificate NgiTool issued or imported (prompt 5), on one
// instance. Kind and Challenge are internal/certs' constants.
type Cert struct {
	Name      string   `json:"name"`
	Instance  string   `json:"instance"`
	Kind      string   `json:"kind"`
	Challenge string   `json:"challenge,omitempty"` // letsencrypt: http or dns
	Names     []string `json:"names"`
	AOP       bool     `json:"aop,omitempty"`
	// Cert and Key are the paths nginx sees; HostCert and HostKey the same
	// files on this machine.
	Cert     string `json:"cert"`
	Key      string `json:"key"`
	HostCert string `json:"hostCert"`
	HostKey  string `json:"hostKey"`
	// Certbot is where a Let's Encrypt certificate is renewed: "stack" (the
	// edge stack's certbot container) or "host" (certbot on this machine,
	// CERT-05) with Authenticator webroot or nginx.
	Certbot       string `json:"certbot,omitempty"`
	Authenticator string `json:"authenticator,omitempty"`
	Webroot       string `json:"webroot,omitempty"`
	Added         string `json:"added,omitempty"`
}

// TLS is the cert as a route attaches it.
func (c Cert) TLS() *TLS {
	return &TLS{Name: c.Name, Cert: c.Cert, Key: c.Key, Names: append([]string(nil), c.Names...), AOP: c.AOP}
}

// Zone is a Cloudflare zone a domain lives in.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Domain is a zone NgiTool keeps DNS records in, with its default
// certificate (the legacy edge.json "domains").
type Domain struct {
	Name     string `json:"name"`
	Instance string `json:"instance,omitempty"`
	Zone     *Zone  `json:"zone,omitempty"`
	Cert     string `json:"cert,omitempty"`
	Added    string `json:"added,omitempty"`
}

// Edge is an edge stack NgiTool runs: its directory and compose project.
// Its nginx is the instance "edge:<dir>", adopted like any other.
type Edge struct {
	Dir      string `json:"dir"`
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Added    string `json:"added,omitempty"`
}

// EdgeInstanceID is the instance id discovery gives an edge stack's nginx.
func EdgeInstanceID(dir string) string { return "edge:" + dir }

// State is state.json.
type State struct {
	Schema    int           `json:"schema"`
	Instances []Adopted     `json:"instances"`
	Pools     []Pool        `json:"pools"`
	Routes    []Route       `json:"routes"`
	Apps      []compose.App `json:"apps"`
	Certs     []Cert        `json:"certs"`
	Domains   []Domain      `json:"domains"`
	Edges     []Edge        `json:"edges"`
}

// Cert returns the certificate named name on instance, or nil.
func (s *State) Cert(instance, name string) *Cert {
	for i := range s.Certs {
		if s.Certs[i].Name == name && (instance == "" || s.Certs[i].Instance == instance) {
			return &s.Certs[i]
		}
	}
	return nil
}

// RemoveCert drops a certificate by instance and name.
func (s *State) RemoveCert(instance, name string) {
	out := s.Certs[:0]
	for _, c := range s.Certs {
		if !(c.Name == name && c.Instance == instance) {
			out = append(out, c)
		}
	}
	s.Certs = out
}

// CertUsers are the ids of the routes on instance that use certificate
// name, sorted.
func (s *State) CertUsers(instance, name string) []string {
	var ids []string
	for _, r := range s.Routes {
		if r.Instance == instance && r.TLS != nil && r.TLS.Name == name {
			ids = append(ids, r.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Domain returns the domain named name, or nil.
func (s *State) Domain(name string) *Domain {
	for i := range s.Domains {
		if s.Domains[i].Name == name {
			return &s.Domains[i]
		}
	}
	return nil
}

// RemoveDomain drops a domain by name.
func (s *State) RemoveDomain(name string) {
	out := s.Domains[:0]
	for _, d := range s.Domains {
		if d.Name != name {
			out = append(out, d)
		}
	}
	s.Domains = out
}

// DomainNames are every domain's name.
func (s *State) DomainNames() []string {
	var out []string
	for _, d := range s.Domains {
		out = append(out, d.Name)
	}
	return out
}

// EdgeOf returns the edge stack whose nginx is instance, or nil.
func (s *State) EdgeOf(instance string) *Edge {
	for i := range s.Edges {
		if s.Edges[i].Instance == instance {
			return &s.Edges[i]
		}
	}
	return nil
}

// EdgeAt returns the edge stack in dir, or nil.
func (s *State) EdgeAt(dir string) *Edge {
	for i := range s.Edges {
		if s.Edges[i].Dir == dir {
			return &s.Edges[i]
		}
	}
	return nil
}

// App returns the linked app named name, or nil.
func (s *State) App(name string) *compose.App {
	for i := range s.Apps {
		if s.Apps[i].Name == name {
			return &s.Apps[i]
		}
	}
	return nil
}

// AppOfProject returns the app linked to a compose project, or nil.
func (s *State) AppOfProject(project string) *compose.App {
	for i := range s.Apps {
		if s.Apps[i].Project == project {
			return &s.Apps[i]
		}
	}
	return nil
}

// RemoveApp drops an app by name.
func (s *State) RemoveApp(name string) {
	out := s.Apps[:0]
	for _, a := range s.Apps {
		if a.Name != name {
			out = append(out, a)
		}
	}
	s.Apps = out
}

// RoutesToApp are the ids of routes whose pool has a member of app
// (service "" = any service), sorted.
func (s *State) RoutesToApp(a compose.App, service string) []string {
	var ids []string
	for _, r := range s.Routes {
		p := s.Pool(r.Pool)
		if p == nil {
			continue
		}
		for _, m := range p.Members {
			if m.OfApp(a, service) {
				ids = append(ids, r.ID)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
}

// OfApp reports whether the member is a service of app (any service when
// service is "").
func (m Member) OfApp(a compose.App, service string) bool {
	if m.Kind != KindService {
		return false
	}
	proj, svc, _ := strings.Cut(m.Ref, "/")
	if m.App != a.Name && proj != a.Project {
		return false
	}
	return service == "" || svc == service
}

// Adopted returns the adopted instance id, or nil.
func (s *State) Adopted(id string) *Adopted {
	for i := range s.Instances {
		if s.Instances[i].ID == id {
			return &s.Instances[i]
		}
	}
	return nil
}

// Pool returns the pool named name, or nil.
func (s *State) Pool(name string) *Pool {
	for i := range s.Pools {
		if s.Pools[i].Name == name {
			return &s.Pools[i]
		}
	}
	return nil
}

// Route returns the route with id, or nil.
func (s *State) Route(id string) *Route {
	for i := range s.Routes {
		if s.Routes[i].ID == id {
			return &s.Routes[i]
		}
	}
	return nil
}

// RoutesOf are the routes using pool name.
func (s *State) RoutesOf(pool string) []Route {
	var out []Route
	for _, r := range s.Routes {
		if r.Pool == pool {
			out = append(out, r)
		}
	}
	return out
}

// RoutesOn are the routes of one instance, sorted by host then path.
func (s *State) RoutesOn(instance string) []Route {
	var out []Route
	for _, r := range s.Routes {
		if r.Instance == instance {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PoolsOn are the pools of one instance, by name.
func (s *State) PoolsOn(instance string) []Pool {
	var out []Pool
	for _, p := range s.Pools {
		if p.Instance == instance {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Clone is a deep copy, so a change can be planned without touching the
// state that is still in force.
func (s *State) Clone() *State {
	c := &State{Schema: s.Schema}
	c.Instances = append([]Adopted{}, s.Instances...)
	c.Edges = append([]Edge(nil), s.Edges...)
	for _, ct := range s.Certs {
		ct.Names = append([]string(nil), ct.Names...)
		c.Certs = append(c.Certs, ct)
	}
	for _, d := range s.Domains {
		if d.Zone != nil {
			z := *d.Zone
			d.Zone = &z
		}
		c.Domains = append(c.Domains, d)
	}
	for _, a := range s.Apps {
		a.Files = append([]string(nil), a.Files...)
		a.Profiles = append([]string(nil), a.Profiles...)
		a.EnvFiles = append([]string(nil), a.EnvFiles...)
		if a.Attached != nil {
			m := map[string]compose.Attach{}
			for k, v := range a.Attached {
				v.Keys = append([]string(nil), v.Keys...)
				m[k] = v
			}
			a.Attached = m
		}
		if a.Mounts != nil {
			m := map[string][]compose.Mount{}
			for k, v := range a.Mounts {
				m[k] = append([]compose.Mount(nil), v...)
			}
			a.Mounts = m
		}
		if a.Last != nil {
			l := *a.Last
			a.Last = &l
		}
		c.Apps = append(c.Apps, a)
	}
	for i := range c.Instances {
		if inc := c.Instances[i].Include; inc != nil {
			cp := *inc
			c.Instances[i].Include = &cp
		}
	}
	for _, p := range s.Pools {
		p.Members = append([]Member{}, p.Members...)
		p.Previous = append([]Member(nil), p.Previous...)
		p.Extra = append([]string(nil), p.Extra...)
		p.Next.Conditions = append([]string(nil), p.Next.Conditions...)
		c.Pools = append(c.Pools, p)
	}
	for _, r := range s.Routes {
		if r.TLS != nil {
			t := *r.TLS
			t.Names = append([]string(nil), t.Names...)
			r.TLS = &t
		}
		if u := r.Options.UpstreamTLS; u != nil {
			cp := *u
			r.Options.UpstreamTLS = &cp
		}
		r.Options.Headers = append([]Header(nil), r.Options.Headers...)
		r.Extra = append([]string(nil), r.Extra...)
		c.Routes = append(c.Routes, r)
	}
	return c
}

// RemovePool drops a pool by name.
func (s *State) RemovePool(name string) {
	out := s.Pools[:0]
	for _, p := range s.Pools {
		if p.Name != name {
			out = append(out, p)
		}
	}
	s.Pools = out
}

// RemoveRoute drops a route by id.
func (s *State) RemoveRoute(id string) {
	out := s.Routes[:0]
	for _, r := range s.Routes {
		if r.ID != id {
			out = append(out, r)
		}
	}
	s.Routes = out
}

// PoolName is the pool name a new single-target route gets:
// api.example.com/v1 → api_example_com_v1, made unique in s.
func (s *State) PoolName(host, path string) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, strings.ToLower(strings.TrimPrefix(host, "*.")+path))
	base = strings.Trim(base, "_")
	if base == "" {
		base = "pool"
	}
	name := base
	for n := 2; s.Pool(name) != nil; n++ {
		name = fmt.Sprintf("%s_%d", base, n)
	}
	return name
}

// Now is the timestamp format stored in state.
func Now(t time.Time) string { return t.UTC().Format(time.RFC3339) }
