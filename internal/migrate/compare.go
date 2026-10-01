package migrate

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// The effective config is what a request meets: for each port and host
// name, the server that answers (exact name, wildcard, regex, then the
// default server), its certificate, and what each of its locations does.
// Byte equality is not the goal: an upstream block with `resolve`
// replaces `set $upstream …; proxy_pass http://$upstream` and both
// normalise to the same target.

// Server is one server {} reduced to what is compared.
type Server struct {
	Pos       string
	Ports     []int
	Default   map[int]bool
	Names     []string
	Cert, Key string
	Verify    string // ssl_verify_client
	Return    string // server-level return
	Locations map[string]Location
}

// Location is one location's behaviour.
type Location struct {
	Pos  string
	What string // "proxy http://web:3000", "files root /var/www/x", "return 301 …"
}

// Config is the effective view of one nginx config.
type Config struct {
	Servers []*Server
	ports   map[int]bool
	names   map[string]bool
}

var setRE = regexp.MustCompile(`\$\{?([A-Za-z0-9_]+)\}?`)

// Effective reads a loaded config.
func Effective(cfg *nginxconf.Config) *Config {
	c := &Config{ports: map[int]bool{}, names: map[string]bool{}}
	for _, d := range cfg.Tree {
		if d.Name != "http" {
			continue
		}
		ups := map[string][]string{}
		for _, h := range d.Block {
			if h.Name == "upstream" && h.HasBlock {
				var addrs []string
				for _, s := range h.Block {
					if s.Name == "server" {
						addrs = append(addrs, s.Arg(0))
					}
				}
				sort.Strings(addrs)
				ups[h.Arg(0)] = addrs
			}
		}
		for _, h := range d.Block {
			if h.Name == "server" && h.HasBlock {
				c.Servers = append(c.Servers, c.server(h, ups))
			}
		}
	}
	return c
}

func (c *Config) server(d nginxconf.Directive, ups map[string][]string) *Server {
	s := &Server{Pos: d.Pos().String(), Default: map[int]bool{}, Locations: map[string]Location{}}
	vars := map[string]string{}
	for _, e := range d.Block {
		if e.Name == "set" && len(e.Args) == 2 {
			vars[strings.TrimPrefix(e.Args[0], "$")] = e.Args[1]
		}
	}
	for _, e := range d.Block {
		switch e.Name {
		case "listen":
			port := listenPort(e.Arg(0))
			if port == 0 {
				continue
			}
			s.Ports = append(s.Ports, port)
			c.ports[port] = true
			for _, a := range e.Args[1:] {
				if a == "default_server" || a == "default" {
					s.Default[port] = true
				}
			}
		case "server_name":
			for _, n := range e.Args {
				s.Names = append(s.Names, n)
				if !strings.HasPrefix(n, "~") && !strings.Contains(n, "*") && n != "_" && n != "" {
					c.names[n] = true
				}
			}
		case "ssl_certificate":
			s.Cert = e.Arg(0)
		case "ssl_certificate_key":
			s.Key = e.Arg(0)
		case "ssl_verify_client":
			s.Verify = e.Arg(0)
		case "return":
			s.Return = strings.Join(e.Args, " ")
		case "location":
			key, loc := location(e, vars, ups)
			s.Locations[key] = loc
		}
	}
	if len(s.Ports) == 0 {
		s.Ports = []int{80}
		c.ports[80] = true
	}
	return s
}

func listenPort(a string) int {
	if strings.HasPrefix(a, "unix:") {
		return 0
	}
	if i := strings.LastIndexByte(a, ':'); i >= 0 {
		a = a[i+1:]
	}
	p, _ := strconv.Atoi(a)
	return p
}

func location(d nginxconf.Directive, srvVars map[string]string, ups map[string][]string) (string, Location) {
	key := strings.Join(d.Args, " ")
	vars := map[string]string{}
	for k, v := range srvVars {
		vars[k] = v
	}
	for _, e := range d.Block {
		if e.Name == "set" && len(e.Args) == 2 {
			vars[strings.TrimPrefix(e.Args[0], "$")] = e.Args[1]
		}
	}
	var what []string
	strip := false
	for _, e := range d.Block {
		switch e.Name {
		case "proxy_pass", "grpc_pass", "fastcgi_pass", "uwsgi_pass":
			what = append(what, "proxy "+target(e.Arg(0), vars, ups))
		case "root":
			what = append(what, "files root "+strings.TrimSuffix(e.Arg(0), "/"))
		case "alias":
			what = append(what, "files alias "+strings.TrimSuffix(e.Arg(0), "/"))
		case "try_files":
			what = append(what, "try "+strings.Join(e.Args, " "))
		case "return":
			what = append(what, "return "+strings.Join(e.Args, " "))
		case "rewrite":
			if len(e.Args) >= 3 && e.Args[1] == "/$1" && e.Args[2] == "break" {
				strip = true
			} else {
				what = append(what, "rewrite "+strings.Join(e.Args, " "))
			}
		}
	}
	if strip {
		what = append(what, "strip prefix")
	}
	if len(what) == 0 {
		what = []string{"(nothing)"}
	}
	return key, Location{Pos: d.Pos().String(), What: strings.Join(what, " · ")}
}

// target resolves variables set in scope and upstream blocks into the
// addresses nginx dials.
func target(raw string, vars map[string]string, ups map[string][]string) string {
	for i := 0; i < 3 && strings.Contains(raw, "$"); i++ {
		raw = setRE.ReplaceAllStringFunc(raw, func(m string) string {
			if v, ok := vars[setRE.FindStringSubmatch(m)[1]]; ok {
				return v
			}
			return m
		})
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		scheme, rest = "", raw
	}
	host, uri := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, uri = rest[:i], rest[i:]
	}
	if addrs, ok := ups[host]; ok {
		host = strings.Join(addrs, ",")
	}
	if scheme != "" {
		return scheme + "://" + host + uri
	}
	return host + uri
}

// Lookup is the server that answers host on port, as nginx picks it:
// exact name, longest leading wildcard, regex, then the default server
// (or the first one on the port).
func (c *Config) Lookup(port int, host string) *Server {
	var on []*Server
	for _, s := range c.Servers {
		for _, p := range s.Ports {
			if p == port {
				on = append(on, s)
				break
			}
		}
	}
	for _, s := range on {
		for _, n := range s.Names {
			if n == host {
				return s
			}
		}
	}
	var best *Server
	bestLen := 0
	for _, s := range on {
		for _, n := range s.Names {
			if base, ok := strings.CutPrefix(n, "*."); ok && strings.HasSuffix(host, "."+base) && len(base) > bestLen {
				best, bestLen = s, len(base)
			}
			if base, ok := strings.CutPrefix(n, "."); ok && (host == base || strings.HasSuffix(host, "."+base)) && len(base) > bestLen {
				best, bestLen = s, len(base)
			}
		}
	}
	if best != nil {
		return best
	}
	for _, s := range on {
		for _, n := range s.Names {
			if re, ok := strings.CutPrefix(n, "~"); ok {
				if rx, err := regexp.Compile(re); err == nil && rx.MatchString(host) {
					return s
				}
			}
		}
	}
	for _, s := range on {
		if s.Default[port] {
			return s
		}
	}
	if len(on) > 0 {
		return on[0]
	}
	return nil
}

// Difference is one way the new config answers differently.
type Difference struct {
	Where  string `json:"where"` // "443 example.com location /"
	Was    string `json:"was"`
	Now    string `json:"now"`
	Reason string `json:"reason"`
}

// Compare lists every difference between the current config and the new
// one, per port and host name, with a reason.
func Compare(old, now *Config) []Difference {
	ports := map[int]bool{}
	for p := range old.ports {
		ports[p] = true
	}
	for p := range now.ports {
		ports[p] = true
	}
	names := map[string]bool{}
	for n := range old.names {
		names[n] = true
	}
	for n := range now.names {
		names[n] = true
	}
	var ps []int
	for p := range ports {
		ps = append(ps, p)
	}
	sort.Ints(ps)
	var ns []string
	for n := range names {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	var out []Difference
	for _, port := range ps {
		for _, host := range ns {
			out = append(out, compareHost(port, host, old.Lookup(port, host), now.Lookup(port, host))...)
		}
	}
	return out
}

func compareHost(port int, host string, a, b *Server) []Difference {
	where := strconv.Itoa(port) + " " + host
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		return []Difference{{Where: where, Was: "not served", Now: "served by " + b.Pos, Reason: "nothing listened on :" + strconv.Itoa(port) + " before"}}
	case b == nil:
		return []Difference{{Where: where, Was: "served by " + a.Pos, Now: "not served", Reason: "nothing listens on :" + strconv.Itoa(port) + " any more"}}
	}
	var out []Difference
	add := func(what, was, now, reason string) {
		out = append(out, Difference{Where: where + what, Was: was, Now: now, Reason: reason})
	}
	if a.Cert != b.Cert || a.Key != b.Key {
		add(" certificate", a.Cert+" "+a.Key, b.Cert+" "+b.Key, "a different certificate answers for "+host)
	}
	if a.Verify != b.Verify {
		add(" ssl_verify_client", orNone(a.Verify), orNone(b.Verify), "Authenticated Origin Pulls changed")
	}
	if a.Return != b.Return {
		add(" return", orNone(a.Return), orNone(b.Return), "the server answers by itself differently")
	}
	keys := map[string]bool{}
	for k := range a.Locations {
		keys[k] = true
	}
	for k := range b.Locations {
		keys[k] = true
	}
	var ks []string
	for k := range keys {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		la, oka := a.Locations[k]
		lb, okb := b.Locations[k]
		switch {
		case !oka:
			add(" location "+k, "none", lb.What, reasonFor(a, b, host, "only in the new config ("+lb.Pos+")"))
		case !okb:
			add(" location "+k, la.What, "none", reasonFor(a, b, host, "only in the current config ("+la.Pos+")"))
		case la.What != lb.What:
			add(" location "+k, la.What, lb.What, reasonFor(a, b, host, "the location does something else"))
		}
	}
	return out
}

// reasonFor explains a location difference by what answers the host.
func reasonFor(a, b *Server, host, fallback string) string {
	exact := func(s *Server) bool {
		for _, n := range s.Names {
			if n == host {
				return true
			}
		}
		return false
	}
	switch {
	case !exact(a) && exact(b):
		return "the default server answered " + host + " before (" + a.Pos + "); now a server of its own does — " + fallback
	case exact(a) && !exact(b):
		return host + " had a server of its own (" + a.Pos + "); now the default server answers — " + fallback
	}
	return fallback
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
