package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Removal kinds (cli/src/remove.mjs).
const (
	RmDomain = "domain"
	RmHost   = "host"
	RmPath   = "path"
	RmApp    = "app"
	RmCert   = "cert"
	RmWWW    = "www"
	RmAll    = "all"
)

// RmKinds name each kind for people.
var RmKinds = map[string]string{
	RmDomain: "domain", RmHost: "host", RmPath: "path", RmApp: "app",
	RmCert: "certificate", RmWWW: "static folder", RmAll: "everything",
}

// Target is what to remove. MoveTo, for a certificate, moves its routes
// to that certificate instead of removing them. Instance narrows a
// certificate name to one instance.
type Target struct {
	Kind, Key, MoveTo, Instance string
}

// Label is "host api.example.com", "everything" …
func (t Target) Label() string {
	if t.Kind == RmAll {
		return "everything"
	}
	return RmKinds[t.Kind] + " " + t.Key
}

// CertRef is one certificate of one instance.
type CertRef struct{ Instance, Name string }

// Extras are what a removal leaves unused: deleted only with --purge or
// when picked (cli/src/remove.mjs). Things that were unused already are
// never offered.
type Extras struct {
	Certs []CertRef `json:"certs"`
	WWW   []string  `json:"www"` // static folders
	DNS   []string  `json:"dns"` // hostnames whose A/AAAA records go
	Down  []string  `json:"down"`
}

// Removal is the plan: what goes, what moves, and the state afterwards.
type Removal struct {
	Target  Target
	Routes  []string            // route ids, in state order
	Pools   []string            // pools left without routes
	Members map[string][]string // pool → member labels taken out (an app in a shared pool)
	Domains []string
	Certs   []CertRef
	Apps    []string
	WWW     []string
	Moved   map[string]string // route id → certificate it moves to
	// DomainCert: domain → its new default certificate ("" none). A
	// domain's certificate is only its default: the domain is never
	// dropped with it.
	DomainCert map[string]string
	Next       *State
	Extras     Extras
}

// ErrNothing is a target that does not exist.
var ErrNothing = errors.New("nothing to remove")

// staticDir is the static folder a pool serves, "" for a proxy pool.
func staticDir(p *Pool) string {
	if p != nil && p.Static() {
		return p.Members[0].Ref
	}
	return ""
}

// wwwUsed are the static folders the routes of st serve.
func wwwUsed(st *State) map[string]bool {
	out := map[string]bool{}
	for _, r := range st.Routes {
		if d := staticDir(st.Pool(r.Pool)); d != "" {
			out[d] = true
		}
	}
	return out
}

// certsUsed are the certificates routes and domains use.
func certsUsed(st *State) map[CertRef]bool {
	out := map[CertRef]bool{}
	for _, r := range st.Routes {
		if r.TLS != nil {
			out[CertRef{r.Instance, r.TLS.Name}] = true
		}
	}
	for _, d := range st.Domains {
		if d.Cert != "" {
			for _, c := range st.Certs {
				if c.Name == d.Cert && (d.Instance == "" || c.Instance == d.Instance) {
					out[CertRef{c.Instance, c.Name}] = true
				}
			}
		}
	}
	return out
}

// PlanRemoval works out what removing t means. linked are the linked app
// names; wwwExists says whether www/<dir> exists on disk. It is pure: the
// CLI shows it, asks, and applies it.
func PlanRemoval(st *State, t Target, wwwExists func(string) bool) (*Removal, error) {
	rm := &Removal{Target: t, Members: map[string][]string{}, Moved: map[string]string{}, DomainCert: map[string]string{}}
	drop := map[string]bool{}
	dropRoute := func(id string) { drop[id] = true }
	dropHost := func(host string) {
		for _, r := range st.Routes {
			if r.Host == host {
				dropRoute(r.ID)
			}
		}
	}
	// dropOne drops a whole-host route with its paths, a path route alone.
	dropOne := func(r Route) {
		if r.Path == "" {
			dropHost(r.Host)
		} else {
			dropRoute(r.ID)
		}
	}
	memberDrop := map[string]map[string]bool{} // pool → labels
	switch t.Kind {
	case RmPath:
		r := st.Route(t.Key)
		if r == nil || r.Path == "" {
			return nil, fmt.Errorf("nothing is served at %s", t.Key)
		}
		dropRoute(r.ID)
	case RmHost:
		found := false
		for _, r := range st.Routes {
			found = found || r.Host == t.Key
		}
		if !found {
			return nil, fmt.Errorf("%s is not served", t.Key)
		}
		dropHost(t.Key)
	case RmDomain:
		if st.Domain(t.Key) == nil {
			return nil, fmt.Errorf("%s is not set up as a domain", t.Key)
		}
		rm.Domains = append(rm.Domains, t.Key)
		names := st.DomainNames()
		// Hosts of a more specific domain (shop.example.com inside
		// example.com) stay with it.
		for _, r := range st.Routes {
			if domainOf(r.Host, names) == t.Key {
				dropRoute(r.ID)
			}
		}
	case RmCert:
		c := st.Cert(t.Instance, t.Key)
		if c == nil {
			return nil, fmt.Errorf("no certificate named %s", t.Key)
		}
		rm.Certs = append(rm.Certs, CertRef{c.Instance, c.Name})
		var to *Cert
		if t.MoveTo != "" {
			if t.MoveTo == t.Key {
				return nil, errors.New("pick a different certificate to move to")
			}
			if to = st.Cert(c.Instance, t.MoveTo); to == nil {
				return nil, fmt.Errorf("no certificate named %s", t.MoveTo)
			}
		}
		for _, r := range st.Routes {
			if r.Instance != c.Instance || r.TLS == nil || r.TLS.Name != c.Name {
				continue
			}
			if to == nil {
				dropOne(r)
				continue
			}
			for _, n := range r.Names() {
				if !Covers(to.Names, n) {
					return nil, fmt.Errorf("%s does not cover %s", to.Name, n)
				}
			}
			rm.Moved[r.ID] = to.Name
		}
		for _, d := range st.Domains {
			if d.Cert == c.Name {
				rm.DomainCert[d.Name] = t.MoveTo
			}
		}
	case RmApp:
		a := st.App(t.Key)
		used := false
		for _, p := range st.Pools {
			for _, m := range p.Members {
				if m.App == t.Key || (a != nil && m.OfApp(*a, "")) {
					used = true
				}
			}
		}
		if a == nil && !used {
			return nil, fmt.Errorf("no app named %s — see: ngitool app ls", t.Key)
		}
		if a != nil {
			rm.Apps = append(rm.Apps, a.Name)
		}
		for _, p := range st.Pools {
			mine := map[string]bool{}
			for _, m := range p.Members {
				if m.App == t.Key || (a != nil && m.OfApp(*a, "")) {
					mine[m.Label()] = true
				}
			}
			switch {
			case len(mine) == 0:
			case len(mine) == len(p.Members):
				for _, r := range st.RoutesOf(p.Name) {
					dropRoute(r.ID)
				}
			default:
				memberDrop[p.Name] = mine
			}
		}
	case RmWWW:
		if !validWWW(t.Key) {
			return nil, fmt.Errorf("invalid folder: %s", t.Key)
		}
		if !wwwUsed(st)[t.Key] && (wwwExists == nil || !wwwExists(t.Key)) {
			return nil, fmt.Errorf("www/%s does not exist", t.Key)
		}
		rm.WWW = append(rm.WWW, t.Key)
		for _, r := range st.Routes {
			if t.Instance != "" && r.Instance != t.Instance {
				continue // another stack's www/
			}
			if staticDir(st.Pool(r.Pool)) == t.Key {
				dropOne(r)
			}
		}
	case RmAll:
		for _, r := range st.Routes {
			dropRoute(r.ID)
		}
		for _, d := range st.Domains {
			rm.Domains = append(rm.Domains, d.Name)
		}
		for _, c := range st.Certs {
			rm.Certs = append(rm.Certs, CertRef{c.Instance, c.Name})
		}
		for _, a := range st.Apps {
			rm.Apps = append(rm.Apps, a.Name)
		}
	default:
		return nil, fmt.Errorf("cannot remove a %s", t.Kind)
	}
	// Removing a whole host takes its paths too (any kind that dropped the
	// host's own route).
	for _, r := range st.Routes {
		if drop[r.ID] && r.Path == "" {
			dropHost(r.Host)
		}
	}
	for _, r := range st.Routes {
		if drop[r.ID] {
			rm.Routes = append(rm.Routes, r.ID)
		}
	}

	// The state afterwards.
	next := st.Clone()
	for id := range drop {
		next.RemoveRoute(id)
	}
	for id, to := range rm.Moved {
		r := next.Route(id)
		if c := next.Cert(r.Instance, to); c != nil {
			r.TLS = c.TLS()
		}
	}
	for pool, labels := range memberDrop {
		p := next.Pool(pool)
		var keep []Member
		for _, m := range p.Members {
			if !labels[m.Label()] {
				keep = append(keep, m)
			}
		}
		p.Members = keep
		var ls []string
		for l := range labels {
			ls = append(ls, l)
		}
		sort.Strings(ls)
		rm.Members[pool] = ls
	}
	for _, p := range st.Pools {
		if len(next.RoutesOf(p.Name)) == 0 && len(st.RoutesOf(p.Name)) > 0 {
			rm.Pools = append(rm.Pools, p.Name)
			next.RemovePool(p.Name)
		}
	}
	for _, d := range rm.Domains {
		next.RemoveDomain(d)
	}
	for d, to := range rm.DomainCert {
		if x := next.Domain(d); x != nil {
			x.Cert = to
		}
	}
	for _, c := range rm.Certs {
		next.RemoveCert(c.Instance, c.Name)
	}
	for _, a := range rm.Apps {
		next.RemoveApp(a)
	}
	rm.Next = next

	// Left unused by this removal.
	usedAfter := certsUsed(next)
	before := map[CertRef]bool{}
	var order []CertRef
	note := func(c CertRef) {
		if !before[c] {
			before[c] = true
			order = append(order, c)
		}
	}
	for _, r := range st.Routes {
		if drop[r.ID] && r.TLS != nil {
			note(CertRef{r.Instance, r.TLS.Name})
		}
	}
	for _, d := range rm.Domains {
		if x := st.Domain(d); x != nil && x.Cert != "" {
			for _, c := range st.Certs {
				if c.Name == x.Cert {
					note(CertRef{c.Instance, c.Name})
				}
			}
		}
	}
	for _, c := range order {
		if next.Cert(c.Instance, c.Name) != nil && !usedAfter[c] {
			rm.Extras.Certs = append(rm.Extras.Certs, c)
		}
	}
	sort.Slice(rm.Extras.Certs, func(i, j int) bool { return rm.Extras.Certs[i].Name < rm.Extras.Certs[j].Name })

	wwwAfter := wwwUsed(next)
	gone := map[string]bool{}
	for _, w := range rm.WWW {
		gone[w] = true
	}
	seen := map[string]bool{}
	for _, r := range st.Routes {
		d := staticDir(st.Pool(r.Pool))
		if (drop[r.ID] || t.Kind == RmAll) && d != "" && !gone[d] && !wwwAfter[d] && !seen[d] {
			seen[d] = true
			rm.Extras.WWW = append(rm.Extras.WWW, d)
		}
	}
	sort.Strings(rm.Extras.WWW)

	for _, r := range st.Routes {
		if !drop[r.ID] || r.Path != "" || (r.DNS != DNSProxied && r.DNS != DNSOnly) {
			continue
		}
		rm.Extras.DNS = append(rm.Extras.DNS, r.Host)
		if st.Domain(r.Host) != nil || r.WWW {
			rm.Extras.DNS = append(rm.Extras.DNS, "www."+r.Host)
		}
	}
	rm.Extras.Down = append([]string(nil), rm.Apps...)
	sort.Strings(rm.Extras.Down)
	return rm, nil
}

// Cascades reports whether anything besides the target itself goes.
func (rm *Removal) Cascades() bool {
	t := rm.Target
	if t.Kind == RmAll {
		return false
	}
	total := len(rm.Domains) + len(rm.Certs) + len(rm.Apps) + len(rm.WWW) + len(rm.Members)
	own := 0
	switch t.Kind {
	case RmPath:
		total += len(rm.Routes)
		own = 1
	case RmHost:
		// the host's own routes are the target; its path routes cascade
		for _, id := range rm.Routes {
			if id == t.Key {
				own = 1
			}
		}
		total += len(rm.Routes)
	default:
		total += len(rm.Routes)
		own = 1
	}
	return total > own
}

// Instances are the instances whose nginx files change, sorted.
func (rm *Removal) Instances(st *State) []string {
	set := map[string]bool{}
	for _, id := range rm.Routes {
		if r := st.Route(id); r != nil {
			set[r.Instance] = true
		}
	}
	for id := range rm.Moved {
		if r := st.Route(id); r != nil {
			set[r.Instance] = true
		}
	}
	for p := range rm.Members {
		if x := st.Pool(p); x != nil {
			set[x.Instance] = true
		}
	}
	var out []string
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ApplyOn makes the nginx part of the removal on one instance: its routes,
// pools, members and certificate moves.
func (rm *Removal) ApplyOn(next *State, instance string) {
	for _, id := range rm.Routes {
		if r := next.Route(id); r != nil && r.Instance == instance {
			next.RemoveRoute(id)
		}
	}
	for id, to := range rm.Moved {
		if r := next.Route(id); r != nil && r.Instance == instance {
			if c := next.Cert(instance, to); c != nil {
				r.TLS = c.TLS()
			}
		}
	}
	for pool, labels := range rm.Members {
		p := next.Pool(pool)
		if p == nil || p.Instance != instance {
			continue
		}
		var keep []Member
		for _, m := range p.Members {
			if !contains(labels, m.Label()) {
				keep = append(keep, m)
			}
		}
		p.Members = keep
	}
	for _, name := range rm.Pools {
		if p := next.Pool(name); p != nil && p.Instance == instance && len(next.RoutesOf(name)) == 0 {
			next.RemovePool(name)
		}
	}
}

// ApplyState makes the state-only part: domains, certificates, apps.
func (rm *Removal) ApplyState(next *State, extraCerts []CertRef) {
	for _, d := range rm.Domains {
		next.RemoveDomain(d)
	}
	for d, to := range rm.DomainCert {
		if x := next.Domain(d); x != nil {
			x.Cert = to
		}
	}
	for _, c := range append(append([]CertRef{}, rm.Certs...), extraCerts...) {
		next.RemoveCert(c.Instance, c.Name)
	}
	for _, a := range rm.Apps {
		next.RemoveApp(a)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func domainOf(host string, domains []string) string {
	best := ""
	for _, d := range domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

func validWWW(dir string) bool {
	if dir == "" || strings.HasPrefix(dir, "/") {
		return false
	}
	for _, part := range strings.Split(dir, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}
