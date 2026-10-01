package model

import (
	"reflect"
	"sort"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
)

// remState is cli/test/run.mjs's REM fixture in NgiTool's shape.
func remState() *State {
	const in = "edge:/srv/edge"
	cert := func(name, kind, ch string, names ...string) Cert {
		return Cert{Name: name, Instance: in, Kind: kind, Challenge: ch, Names: names,
			Cert: "/etc/edge-certs/" + name + "/fullchain.pem", Key: "/etc/edge-certs/" + name + "/privkey.pem"}
	}
	st := &State{
		Certs: []Cert{
			cert("example.com", "letsencrypt", "http", "example.com", "www.example.com"),
			cert("wild.example.com", "letsencrypt", "", "example.com", "*.example.com"),
			cert("api.example.com", "self-signed", "", "api.example.com"),
			cert("shop.example.com", "origin", "", "shop.example.com", "*.shop.example.com"),
			cert("spare", "custom", "", "spare.org"),
		},
		Domains: []Domain{{Name: "example.com", Cert: "example.com"}, {Name: "shop.example.com", Cert: "shop.example.com"}},
		Apps:    []compose.App{{Name: "shop", Project: "shop"}, {Name: "other", Project: "other"}},
	}
	tlsOf := func(name string) *TLS { return st.Cert(in, name).TLS() }
	add := func(id, host, path, certName, dns string, m Member) {
		pool := st.PoolName(host, path)
		st.Pools = append(st.Pools, Pool{Name: pool, Instance: in, Method: RoundRobin, Scheme: "http", Members: []Member{m}})
		r := Route{ID: RouteID(host, path), Instance: in, Host: host, Path: path, Pool: pool, Enabled: true, HTTP: HTTPRedirect, DNS: dns}
		if certName != "" {
			r.TLS = tlsOf(certName)
		}
		st.Routes = append(st.Routes, r)
		_ = id
	}
	static := func(dir string) Member { return Member{Kind: KindStatic, Ref: dir} }
	app := func(svc string, port int) Member {
		return Member{Kind: KindService, App: "shop", Ref: "shop/" + svc, Host: "shop-" + svc, Port: port}
	}
	add("1", "example.com", "", "example.com", DNSProxied, static("example.com"))
	add("2", "api.example.com", "", "api.example.com", DNSOnly, app("api", 8000))
	add("3", "blog.example.com", "", "wild.example.com", DNSSkip, static("shared"))
	add("4", "shop.example.com", "", "shop.example.com", DNSProxied, app("web", 3000))
	add("5", "example.com", "/docs", "example.com", "", static("shared"))
	add("6", "example.com", "/api", "example.com", "", app("api", 8000))
	return st
}

func sorted(xs []string) []string {
	out := append([]string{}, xs...)
	sort.Strings(out)
	return out
}

func hosts(st *State, ids []string) []string {
	var out []string
	for _, id := range ids {
		if r := st.Route(id); r != nil && r.Path == "" {
			out = append(out, id)
		}
	}
	return sorted(out)
}

func pathRoutes(st *State, ids []string) []string {
	var out []string
	for _, id := range ids {
		if r := st.Route(id); r != nil && r.Path != "" {
			out = append(out, id)
		}
	}
	return sorted(out)
}

func certNames(cs []CertRef) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// legacy: "removing a host takes its paths; unused cert, folder and DNS become extras"
func TestRemovingAHostTakesItsPaths(t *testing.T) {
	st := remState()
	p, err := PlanRemoval(st, Target{Kind: RmHost, Key: "example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "hosts", hosts(st, p.Routes), []string{"example.com"})
	eq(t, "paths", pathRoutes(st, p.Routes), []string{"example.com/api", "example.com/docs"})
	if p.Next.Domain("example.com") == nil {
		t.Error("the domain stays")
	}
	eq(t, "extra certs (the domain still uses its cert)", len(p.Extras.Certs), 0)
	eq(t, "extra www (shared is still served by blog)", p.Extras.WWW, []string{"example.com"})
	eq(t, "extra dns", p.Extras.DNS, []string{"example.com", "www.example.com"})
	if !p.Cascades() {
		t.Error("cascades")
	}
}

// legacy: "removing a domain keeps hosts of a more specific domain"
func TestRemovingADomainKeepsHostsOfAMoreSpecificDomain(t *testing.T) {
	st := remState()
	p, err := PlanRemoval(st, Target{Kind: RmDomain, Key: "example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "hosts", hosts(st, p.Routes), []string{"api.example.com", "blog.example.com", "example.com"})
	if p.Next.Route("shop.example.com") == nil || p.Next.Domain("shop.example.com") == nil {
		t.Error("shop.example.com and its domain stay")
	}
	eq(t, "extra certs", certNames(p.Extras.Certs), []string{"api.example.com", "example.com", "wild.example.com"})
	eq(t, "extra www", p.Extras.WWW, []string{"example.com", "shared"})
	eq(t, "extra dns", p.Extras.DNS, []string{"example.com", "www.example.com", "api.example.com"})
	for _, c := range p.Extras.Certs {
		if c.Name == "spare" {
			t.Error("certs that were unused already are not offered")
		}
	}
}

// legacy: "removing an app drops every route that points at it"
func TestRemovingAnAppDropsEveryRouteToIt(t *testing.T) {
	st := remState()
	p, err := PlanRemoval(st, Target{Kind: RmApp, Key: "shop"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "apps", p.Apps, []string{"shop"})
	eq(t, "hosts", hosts(st, p.Routes), []string{"api.example.com", "shop.example.com"})
	eq(t, "paths", pathRoutes(st, p.Routes), []string{"example.com/api"})
	eq(t, "down", p.Extras.Down, []string{"shop"})
	if _, err := PlanRemoval(st, Target{Kind: RmApp, Key: "nope"}, nil); err == nil || err.Error() != "no app named nope — see: ngitool app ls" {
		t.Errorf("unknown app: %v", err)
	}
}

// legacy: "removing a cert: move its hosts to a covering cert, or drop them"
func TestRemovingACertMovesOrDropsItsHosts(t *testing.T) {
	st := remState()
	moved, err := PlanRemoval(st, Target{Kind: RmCert, Key: "example.com", MoveTo: "wild.example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "moved host cert", moved.Next.Route("example.com").TLS.Name, "wild.example.com")
	eq(t, "moved domain cert", moved.Next.Domain("example.com").Cert, "wild.example.com")
	eq(t, "nothing dropped", len(moved.Routes), 0)
	if _, err := PlanRemoval(st, Target{Kind: RmCert, Key: "example.com", MoveTo: "spare"}, nil); err == nil || err.Error() != "spare does not cover example.com" {
		t.Errorf("non-covering move: %v", err)
	}
	dropped, err := PlanRemoval(st, Target{Kind: RmCert, Key: "example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "dropped hosts", hosts(st, dropped.Routes), []string{"example.com"})
	d := dropped.Next.Domain("example.com")
	if d == nil {
		t.Fatal("a domain is never dropped with its cert")
	}
	eq(t, "domain cert", d.Cert, "")
	spare, _ := PlanRemoval(st, Target{Kind: RmCert, Key: "spare"}, nil)
	if spare.Cascades() {
		t.Error("an unused cert does not cascade")
	}
}

// legacy: "removing a static folder drops the routes serving it; reset drops everything"
func TestRemovingAStaticFolderAndReset(t *testing.T) {
	st := remState()
	p, err := PlanRemoval(st, Target{Kind: RmWWW, Key: "shared"}, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "hosts", hosts(st, p.Routes), []string{"blog.example.com"})
	eq(t, "paths", pathRoutes(st, p.Routes), []string{"example.com/docs"})
	if _, err := PlanRemoval(st, Target{Kind: RmWWW, Key: "../etc"}, nil); err == nil || err.Error() != "invalid folder: ../etc" {
		t.Errorf("escape: %v", err)
	}
	all, err := PlanRemoval(st, Target{Kind: RmAll}, nil)
	if err != nil {
		t.Fatal(err)
	}
	n := all.Next
	if len(n.Routes)+len(n.Pools)+len(n.Certs)+len(n.Domains)+len(n.Apps) != 0 {
		t.Errorf("reset leaves %+v", n)
	}
	eq(t, "apps", sorted(all.Apps), []string{"other", "shop"})
}

func TestRemovalAppliesPerInstance(t *testing.T) {
	st := remState()
	p, _ := PlanRemoval(st, Target{Kind: RmHost, Key: "example.com"}, nil)
	next := st.Clone()
	for _, id := range p.Instances(st) {
		p.ApplyOn(next, id)
	}
	p.ApplyState(next, nil)
	if !reflect.DeepEqual(next.Routes, p.Next.Routes) || len(next.Pools) != len(p.Next.Pools) {
		t.Errorf("ApplyOn differs from the plan:\n%+v\n%+v", next.Routes, p.Next.Routes)
	}
}
