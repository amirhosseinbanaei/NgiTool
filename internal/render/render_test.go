package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	newNginx = "nginx/1.30.5"
	oldNginx = "nginx/1.25.3"
)

// world is a scan with one nginx of each layout and a few app containers.
func world(t *testing.T, kind, version string) (*discover.Report, *discover.Instance) {
	t.Helper()
	rep := &discover.Report{Resolvers: []string{"127.0.0.53"}}
	rep.Containers = []discover.Container{
		{Name: "front", Running: true, Networks: []string{"app"}, Gateways: map[string]string{"app": "172.20.0.1"}},
		{Name: "web-1", Running: true, Networks: []string{"app"}, Exposed: []int{3000}},
		{Name: "web-2", Running: true, Networks: []string{"app"}, Exposed: []int{3000}},
		{Name: "web-3", Running: true, Networks: []string{"app"}, Exposed: []int{3000}},
	}
	var in discover.Instance
	switch kind {
	case discover.KindHost:
		in = discover.Instance{ID: "host:nginx.service", Kind: kind, Name: "nginx.service", Version: version, Conf: "/etc/nginx/nginx.conf",
			Summary: &nginxconf.Summary{Hook: &nginxconf.Hook{Existing: "/etc/nginx/conf.d/*.conf", Dir: "/etc/nginx/conf.d"}}}
	case discover.KindEdge:
		src := t.TempDir()
		for _, f := range []string{"snippets/security-headers.conf", "snippets/acme-challenge.conf"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(src, f)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, f), []byte("# test\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		in = discover.Instance{ID: "edge:/srv/edge", Kind: kind, Name: "edge-nginx-1", Container: "front", Version: version,
			Conf: "/etc/nginx/edge/nginx.conf", Networks: []string{"app"},
			Mounts: []discover.Mount{{Type: "bind", Source: src, Dest: "/etc/nginx/edge"}},
			Files:  []nginxconf.FileInfo{{Path: "/etc/nginx/edge/conf.d/websocket.conf"}},
			Summary: &nginxconf.Summary{
				Hook: &nginxconf.Hook{Existing: "/etc/nginx/edge/conf.d/*.conf", Dir: "/etc/nginx/edge/conf.d"},
				Maps: []nginxconf.Map{{Source: "$http_upgrade", Var: "connection_upgrade", Values: map[string]string{"default": "upgrade", "": ""},
					Pos: nginxconf.Pos{File: "/etc/nginx/edge/conf.d/websocket.conf", Line: 2}}},
				Servers: []nginxconf.Server{{Listens: []nginxconf.Listen{{Port: 443, SSL: true, Default: true}}}},
			}}
	default:
		in = discover.Instance{ID: "ctr:front", Kind: discover.KindContainer, Name: "front", Container: "front", Version: version,
			Image: "nginx:stable-alpine", Conf: "/etc/nginx/nginx.conf", Networks: []string{"app"},
			Mounts:  []discover.Mount{{Type: "bind", Source: "/srv/front/conf.d", Dest: "/etc/nginx/conf.d"}},
			Summary: &nginxconf.Summary{Hook: &nginxconf.Hook{Existing: "/etc/nginx/conf.d/*.conf", Dir: "/etc/nginx/conf.d"}}}
	}
	rep.Instances = []discover.Instance{in}
	return rep, &rep.Instances[0]
}

func member(spec string) model.Member {
	m, _, err := model.ParseMember(spec)
	if err != nil {
		panic(err)
	}
	return m
}

type tcase struct {
	name    string
	kind    string
	version string
	pools   []model.Pool
	routes  []model.Route
	tweak   func(*Facts)
}

func pool(name, method string, ms ...string) model.Pool {
	p := model.Pool{Name: name, Method: method, Scheme: "http"}
	for _, s := range ms {
		p.Members = append(p.Members, member(s))
	}
	return p
}

func route(host, path, pool string) model.Route {
	return model.Route{ID: host + path, Host: host, Path: path, Pool: pool, Enabled: true, HTTP: model.HTTPServe, Options: model.DefaultOptions()}
}

func tlsRoute(host, path, pool, mode string) model.Route {
	r := route(host, path, pool)
	r.HTTP = mode
	r.TLS = &model.TLS{Name: "example.com", Cert: "/etc/ssl/example.com/fullchain.pem", Key: "/etc/ssl/example.com/privkey.pem", Names: []string{"example.com", "*.example.com"}}
	return r
}

func with(p model.Pool, f func(*model.Pool)) model.Pool { f(&p); return p }

func withR(r model.Route, f func(*model.Route)) model.Route { f(&r); return r }

func sticky(preset string) func(*model.Pool) {
	return func(p *model.Pool) {
		if err := model.ApplySticky(p, preset); err != nil {
			panic(err)
		}
	}
}

func TestGolden(t *testing.T) {
	web := "container:web-1:3000"
	three := []string{"container:web-1:3000", "container:web-2:3000", "container:web-3:3000"}
	cases := []tcase{
		{name: "simple-host", pools: []model.Pool{pool("app", model.RoundRobin, web)}, routes: []model.Route{route("app.example.com", "", "app")}},
		{name: "apex-www", pools: []model.Pool{pool("site", model.RoundRobin, web)},
			routes: []model.Route{withR(tlsRoute("example.com", "", "site", model.HTTPRedirect), func(r *model.Route) { r.WWW = true })}},
		{name: "path-strip-and-keep", pools: []model.Pool{pool("api", model.RoundRobin, web), pool("admin", model.RoundRobin, "container:web-2:8080")},
			routes: []model.Route{
				withR(route("app.example.com", "/api", "api"), func(r *model.Route) { r.Options.StripPrefix = true }),
				route("app.example.com", "/admin", "admin"),
			}},
		{name: "method-round-robin", pools: []model.Pool{pool("p", model.RoundRobin, three...)}, routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "method-least-conn", pools: []model.Pool{pool("p", model.LeastConn, three...)}, routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "method-ip-hash", pools: []model.Pool{pool("p", model.IPHash, three...)}, routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "method-hash", pools: []model.Pool{with(pool("p", model.Hash, three...), func(p *model.Pool) { p.HashKey, p.Consistent = "$request_uri", true })},
			routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "method-random", pools: []model.Pool{pool("p", model.Random, three...)}, routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "method-random-two", pools: []model.Pool{pool("p", model.RandomTwo, three...)}, routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "weights-backup-maxconns", pools: []model.Pool{pool("p", model.LeastConn,
			"container:web-1:3000,weight=3,max_fails=2,fail_timeout=10s", "container:web-2:3000,max_conns=50", "container:web-3:3000,backup")},
			routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "fallback-old-single", version: oldNginx, pools: []model.Pool{pool("app", model.RoundRobin, web)}, routes: []model.Route{route("app.example.com", "", "app")}},
		{name: "fallback-old-multi", version: oldNginx, pools: []model.Pool{pool("p", model.LeastConn, "container:web-1:3000", "container:web-2:3000,max_conns=10")},
			routes: []model.Route{route("lb.example.com", "", "p")}},
		{name: "keepalive-websocket-own-map", pools: []model.Pool{with(pool("p", model.LeastConn, three...), func(p *model.Pool) { p.Keepalive = 32 })},
			routes: []model.Route{route("ws.example.com", "", "p")}},
		{name: "keepalive-existing-close-map", pools: []model.Pool{with(pool("p", model.LeastConn, web), func(p *model.Pool) { p.Keepalive = 16 })},
			routes: []model.Route{withR(route("ws.example.com", "", "p"), func(r *model.Route) { r.Options.Buffering = false })},
			tweak:  func(f *Facts) { f.UpgradeVar, f.OwnMap = "ngt_connection_upgrade", true }},
		{name: "https-upstream", pools: []model.Pool{with(pool("ext", model.RoundRobin, "https://api.upstream.example.net"), func(p *model.Pool) { p.Scheme = "https" })},
			routes: []model.Route{withR(route("proxy.example.com", "", "ext"), func(r *model.Route) {
				r.Options.UpstreamTLS = &model.UpstreamTLS{Verify: true}
				r.Options.Headers = []model.Header{{Name: "X-Env", Value: "prod one"}}
				r.Options.BodySize = "100m"
			})}},
		{name: "grpc", pools: []model.Pool{with(pool("g", model.RoundRobin, "container:web-1:50051", "container:web-2:50051"), func(p *model.Pool) { p.Scheme = "grpc" })},
			routes: []model.Route{tlsRoute("grpc.example.com", "", "g", model.HTTPRedirect)}},
		{name: "sticky-ip", pools: []model.Pool{with(pool("p", model.RoundRobin, three...), sticky("ip"))}, routes: []model.Route{route("s.example.com", "", "p")}},
		{name: "sticky-cloudflare", pools: []model.Pool{with(pool("p", model.RoundRobin, three...), sticky("cloudflare"))}, routes: []model.Route{route("s.example.com", "", "p")}},
		{name: "sticky-cookie", pools: []model.Pool{with(pool("p", model.RoundRobin, three...), sticky("cookie:session"))}, routes: []model.Route{route("s.example.com", "", "p")}},
		{name: "tls-redirect-hsts", pools: []model.Pool{pool("app", model.RoundRobin, web)}, routes: []model.Route{tlsRoute("app.example.com", "", "app", model.HTTPRedirect)}},
		{name: "tls-serve-no-hsts", pools: []model.Pool{pool("app", model.RoundRobin, web)}, routes: []model.Route{tlsRoute("app.example.com", "", "app", model.HTTPServe)}},
		{name: "layout-host", kind: discover.KindHost, pools: []model.Pool{pool("app", model.RoundRobin, "port:3000"), pool("sock", model.RoundRobin, "unix:/run/app.sock")},
			routes: []model.Route{tlsRoute("app.example.com", "", "app", model.HTTPRedirect), route("app.example.com", "/sock", "sock")}},
		{name: "layout-edge", kind: discover.KindEdge, pools: []model.Pool{pool("app", model.LeastConn, three...), pool("hostapp", model.RoundRobin, "port:8081")},
			routes: []model.Route{tlsRoute("app.example.com", "", "app", model.HTTPRedirect), route("tools.example.com", "", "hostapp")}},
		{name: "layout-mount", pools: []model.Pool{pool("app", model.RoundRobin, web), pool("other", model.RoundRobin, "addr:[2001:db8::10]:8080")},
			routes: []model.Route{route("app.example.com", "", "app"), route("v6.example.com", "", "other")}},
		{name: "mixed-members-error-page", pools: []model.Pool{with(pool("mix", model.LeastConn, "container:web-1:3000", "port:9000,weight=2", "addr:10.0.0.7:8080,backup"),
			func(p *model.Pool) { p.ErrorPage = "/maintenance.html" })},
			routes: []model.Route{route("mix.example.com", "", "mix")}},
		{name: "disabled-route", pools: []model.Pool{pool("app", model.RoundRobin, web)},
			routes: []model.Route{withR(route("app.example.com", "", "app"), func(r *model.Route) { r.Enabled = false })}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ver := c.version
			if ver == "" {
				ver = newNginx
			}
			rep, in := world(t, c.kind, ver)
			a, err := Plan(in)
			if err != nil {
				t.Fatal(err)
			}
			st := &model.State{Instances: []model.Adopted{a}}
			for _, p := range c.pools {
				p.Instance = in.ID
				st.Pools = append(st.Pools, p)
			}
			for _, r := range c.routes {
				r.Instance = in.ID
				st.Routes = append(st.Routes, r)
			}
			for i := range st.Pools {
				for j := range st.Pools[i].Members {
					model.ResolveMember(rep, in, &st.Pools[i].Members[j])
				}
			}
			f := GatherFacts(rep, in, &st.Instances[0])
			if c.tweak != nil {
				c.tweak(&f)
			}
			out, err := Render(st, f)
			if err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, fl := range out.Files {
				b.WriteString("=== " + fl.Path + "\n" + fl.Content() + "\n")
			}
			for _, n := range out.Notes {
				b.WriteString("--- " + n.Code + " " + n.Level + ": " + n.Msg + " " + n.Fix + "\n")
			}
			golden := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test ./internal/render -update)", err)
			}
			if b.String() != string(want) {
				t.Errorf("%s differs from %s:\n%s", c.name, golden, b.String())
			}
			for _, fl := range out.Files {
				if p := Parse(fl.Content()); !p.Managed || p.Edited {
					t.Errorf("%s: header does not round-trip: %+v", fl.Path, p)
				}
			}
		})
	}
}

func TestParseDetectsHandEdits(t *testing.T) {
	c := Stamp("route:a.example.com", "server {\n}\n")
	if p := Parse(c); !p.Managed || p.Edited || p.ID != "route:a.example.com" {
		t.Fatalf("clean file: %+v", p)
	}
	if p := Parse(strings.Replace(c, "server {", "server { # mine", 1)); !p.Edited {
		t.Fatal("an edited body is drift (APPLY-05)")
	}
	if p := Parse("server {}\n"); p.Managed {
		t.Fatal("a file without the marker is hand-written")
	}
}

func TestVersionGates(t *testing.T) {
	for v, want := range map[string]bool{"nginx/1.27.3": true, "nginx/1.30.5": true, "nginx/1.27.2": false, "openresty/1.25.3.1": false, "Angie/1.7.0": true, "": false} {
		if got := SupportsResolve(v); got != want {
			t.Errorf("SupportsResolve(%q) = %v, want %v (LB-03)", v, got, want)
		}
	}
	if ZoneSize(1) != "64k" || ZoneSize(20) != "192k" {
		t.Errorf("zone sizes: %s %s (LB-14)", ZoneSize(1), ZoneSize(20))
	}
}
