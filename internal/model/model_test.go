package model

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
)

func scan() *discover.Report {
	return &discover.Report{
		Containers: []discover.Container{
			{Name: "front", Running: true, Networks: []string{"app"}, Gateways: map[string]string{"app": "172.20.0.1"}},
			{Name: "web", Running: true, Networks: []string{"app"}, DNS: map[string][]string{"app": {"web"}}, Exposed: []int{3000}},
			{Name: "lonely", Running: true, Networks: []string{"other"}, Exposed: []int{8080},
				Ports: []discover.Published{{HostIP: "127.0.0.1", HostPort: 18081, ContainerPort: 8080, Proto: "tcp"}}},
			{Name: "shop-web-1", Running: true, Project: "shop", Service: "web", Networks: []string{"app"}, DNS: map[string][]string{"app": {"shop-web-1", "web"}}},
			{Name: "shop-web-2", Running: true, Project: "shop", Service: "web", Networks: []string{"app"}, DNS: map[string][]string{"app": {"shop-web-2", "web"}}},
		},
		Listeners: []discover.PortOwner{{Addr: "0.0.0.0", Port: 9000, Process: "node"}, {Addr: "127.0.0.1", Port: 9001, Process: "python3"}},
		Instances: []discover.Instance{
			{ID: "ctr:front", Kind: discover.KindContainer, Name: "front", Container: "front", Networks: []string{"app"},
				Summary: &nginxconf.Summary{Servers: []nginxconf.Server{{
					Pos:   nginxconf.Pos{File: "/etc/nginx/conf.d/hand.conf", Line: 1},
					Names: []nginxconf.ServerName{{Name: "taken.example.com", Kind: nginxconf.NameExact}},
				}}}},
			{ID: "host:nginx.service", Kind: discover.KindHost, Name: "nginx.service",
				Summary: &nginxconf.Summary{Servers: []nginxconf.Server{{
					Pos:   nginxconf.Pos{File: "/etc/nginx/sites-enabled/inner", Line: 3},
					Names: []nginxconf.ServerName{{Name: "inner.example.com", Kind: nginxconf.NameExact}},
				}}}},
		},
	}
}

func base() *State {
	return &State{Instances: []Adopted{{ID: "ctr:front"}, {ID: "host:nginx.service"}}}
}

func codes(ps Problems) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Code+":"+p.Level)
	}
	return strings.Join(out, " ")
}

func has(ps Problems, code string, errorLevel bool) bool {
	for _, p := range ps {
		if p.Code == code && p.IsError() == errorLevel {
			return true
		}
	}
	return false
}

func newPool(inst, method string, specs ...string) *Pool {
	p := &Pool{Name: "p", Instance: inst, Method: method, Scheme: "http"}
	for _, s := range specs {
		m, _, err := ParseMember(s)
		if err != nil {
			panic(err)
		}
		p.Members = append(p.Members, m)
	}
	return p
}

func TestBackupWithHashMethodsIsRefused(t *testing.T) {
	for _, m := range []string{IPHash, Hash, Random, RandomTwo} {
		p := newPool("ctr:front", m, "container:web:3000", "container:web:3001,backup")
		p.HashKey = "$request_uri"
		st := base()
		st.Pools = []Pool{*p}
		if ps := CheckPool(st, scan(), p); !has(ps, "LB-02", true) {
			t.Errorf("%s with backup: %s", m, codes(ps))
		}
	}
	p := newPool("ctr:front", LeastConn, "container:web:3000", "container:web:3001,backup")
	if ps := CheckPool(&State{Pools: []Pool{*p}}, scan(), p); has(ps, "LB-02", true) {
		t.Errorf("least_conn takes a backup: %s", codes(ps))
	}
}

func TestMethodsAreExclusiveAndPlusOnlyIsRefused(t *testing.T) {
	for _, m := range []string{"sticky", "least_time", "ntlm"} {
		_, err := ParseMethod(m)
		var p Problem
		if !errors.As(err, &p) || p.Code != "LB-15" {
			t.Errorf("method %s: %v (LB-15)", m, err)
		}
	}
	for _, kv := range []string{"slow_start=30s", "drain", "route=a"} {
		var m Member
		var p Problem
		if err := SetParam(&m, kv); !errors.As(err, &p) || p.Code != "LB-15" {
			t.Errorf("param %s: %v", kv, err)
		}
	}
	if !strings.Contains(PlusOnly["health_check"], "LB-08") {
		t.Error("active health checks name LB-08")
	}
	p := newPool("ctr:front", Hash, "container:web:3000")
	if ps := CheckPool(&State{Pools: []Pool{*p}}, scan(), p); !has(ps, "LB-01", true) {
		t.Errorf("hash without a key: %s", codes(ps))
	}
	if m, err := ParseMethod("random two least_conn"); err != nil || m != RandomTwo {
		t.Errorf("random two: %v %v", m, err)
	}
}

func TestStickyPresets(t *testing.T) {
	for preset, want := range map[string]string{"ip": "ip_hash/", "cloudflare": "hash/$http_cf_connecting_ip", "cookie:sid": "hash/$cookie_sid"} {
		var p Pool
		if err := ApplySticky(&p, preset); err != nil {
			t.Fatal(err)
		}
		if got := p.Method + "/" + p.HashKey; got != want || (p.Method == Hash && !p.Consistent) {
			t.Errorf("%s: %s consistent=%v (LB-07)", preset, got, p.Consistent)
		}
	}
	if err := ApplySticky(&Pool{}, "cookie:bad name"); err == nil {
		t.Error("bad cookie name accepted")
	}
}

func TestHostnamesAndPaths(t *testing.T) {
	for in, want := range map[string]string{
		"API.Example.COM":   "api.example.com",
		"bücher.example":    "xn--bcher-kva.example",
		"*.Example.com":     "*.example.com",
		"münchen.de.":       "xn--mnchen-3ya.de",
		"~^(www\\.)?a\\.io": "~^(www\\.)?a\\.io",
	} {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v; want %q (RP-19)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "localhost", "-a.example.com", "a..example.com", "a_b.example.com", strings.Repeat("a", 64) + ".com", "~(unclosed"} {
		if _, err := NormalizeHost(bad); err == nil {
			t.Errorf("NormalizeHost(%q) accepted", bad)
		}
	}
	if h, p, err := ParseTarget("https://Example.com/Admin/"); err != nil || h != "example.com" || p != "/Admin" {
		t.Errorf("ParseTarget: %q %q %v", h, p, err)
	}
	if _, err := NormalizePath("/a b"); err == nil {
		t.Error("path with a space accepted")
	}
	if HostKind("*.example.com") != "wildcard" || HostKind("~x") != "regex" {
		t.Error("host kinds")
	}
}

func TestDuplicateRoutes(t *testing.T) {
	rep := scan()
	st := base()
	p := newPool("ctr:front", RoundRobin, "container:web:3000")
	st.Pools = []Pool{*p}
	r := Route{ID: "app.example.com", Instance: "ctr:front", Host: "app.example.com", Pool: "p", HTTP: HTTPServe, Enabled: true}
	st.Routes = []Route{r, r}
	if ps := CheckRoute(st, rep, &st.Routes[1]); !has(ps, "RP-03", true) {
		t.Errorf("same host twice on one instance: %s", codes(ps))
	}
	other := r
	other.Instance = "host:nginx.service"
	st.Routes = []Route{r, other}
	if ps := CheckRoute(st, rep, &st.Routes[1]); !has(ps, "RP-04", true) {
		t.Errorf("same host on another instance: %s", codes(ps))
	}
	hand := Route{ID: "taken.example.com", Instance: "ctr:front", Host: "taken.example.com", Pool: "p", HTTP: HTTPServe}
	st.Routes = []Route{hand}
	if ps := CheckRoute(st, rep, &st.Routes[0]); !has(ps, "RP-03", true) {
		t.Errorf("host a hand-written server serves: %s", codes(ps))
	}
	inner := Route{ID: "inner.example.com", Instance: "ctr:front", Host: "inner.example.com", Pool: "p", HTTP: HTTPServe}
	st.Routes = []Route{inner}
	if ps := CheckRoute(st, rep, &st.Routes[0]); !has(ps, "RP-04", true) {
		t.Errorf("host another instance serves: %s", codes(ps))
	}
}

func TestShadowingRegexIsAWarning(t *testing.T) {
	locs := []nginxconf.Location{
		{Modifier: "~*", Path: `\.(js|css)$`, Pos: nginxconf.Pos{File: "/etc/nginx/edge/sites/a.conf", Line: 12}},
		{Modifier: "~", Path: `^/admin`},
	}
	ps := Shadowed(locs, "/api")
	if len(ps) != 1 || ps[0].IsError() || ps[0].Code != "RP-20" || !strings.Contains(ps[0].Msg, "a.conf:12") {
		t.Errorf("RP-20: %+v", ps)
	}
}

func TestContainerMembers(t *testing.T) {
	rep := scan()
	front := rep.Find("ctr:front")
	ps := checkMember(rep, front, Member{Kind: KindContainer, Ref: "lonely", Host: "lonely", Port: 8080})
	if !has(ps, "RP-05", false) || ps[0].Connect == nil || ps[0].Connect.Network != "app" || ps[0].Connect.Container != "lonely" {
		t.Errorf("not on a shared network: offer to connect (RP-05): %+v", ps)
	}
	if !strings.Contains(ps[0].Fix, "recreated") {
		t.Errorf("the connect warning says it lasts until recreate: %q", ps[0].Fix)
	}
	host := rep.Find("host:nginx.service")
	ps = checkMember(rep, host, Member{Kind: KindContainer, Ref: "lonely", Port: 8080})
	if !has(ps, "RP-05", true) || !strings.Contains(ps[0].Fix, "port:18081") {
		t.Errorf("container on a host instance is refused, suggesting the 127.0.0.1 publish: %+v", ps)
	}
	if ps := checkMember(rep, front, Member{Kind: KindContainer, Ref: "gone", Port: 80}); !has(ps, "RP-07", false) {
		t.Errorf("missing container: %s", codes(ps))
	}
	svc := Member{Kind: KindService, Ref: "shop/web", Port: 3000}
	ResolveMember(rep, front, &svc)
	if svc.Host != "web" {
		t.Errorf("service resolves by its service name on a shared network, got %q", svc.Host)
	}
	if ps := checkMember(rep, front, svc); !has(ps, "LB-04", false) {
		t.Errorf("replicas note: %s", codes(ps))
	}
}

func TestHostPortMembers(t *testing.T) {
	rep := scan()
	front := rep.Find("ctr:front")
	ps := checkMember(rep, front, Member{Kind: KindHostPort, Port: 9000})
	if has(ps, "RP-06", true) || !strings.Contains(ps[len(ps)-1].Msg, "172.20.0.1:9000") || !strings.Contains(ps[len(ps)-1].Fix, "ufw allow") {
		t.Errorf("host port from a container uses the gateway and the firewall hint: %+v", ps)
	}
	ps = checkMember(rep, front, Member{Kind: KindHostPort, Port: 9001})
	if !strings.Contains(ps[0].Msg, "127.0.0.1:9001") || !strings.Contains(ps[0].Msg, "python3") {
		t.Errorf("loopback-only listener: %+v", ps)
	}
	ps = checkMember(rep, front, Member{Kind: KindHostPort, Port: 9999})
	if !strings.Contains(ps[0].Msg, "nothing listens") {
		t.Errorf("nothing listening: %+v", ps)
	}
	host := rep.Find("host:nginx.service")
	if ps := checkMember(rep, host, Member{Kind: KindHostPort, Port: 9001}); len(ps) != 0 {
		t.Errorf("host instance reaches 127.0.0.1: %+v", ps)
	}
}

func TestPathRouteWarnsAboutBasePath(t *testing.T) {
	st := base()
	st.Pools = []Pool{*newPool("ctr:front", RoundRobin, "container:web:3000")}
	st.Routes = []Route{{ID: "a.example.com/app", Instance: "ctr:front", Host: "a.example.com", Path: "/app", Pool: "p", HTTP: HTTPServe}}
	ps := CheckRoute(st, scan(), &st.Routes[0])
	if !has(ps, "RP-02", false) || !strings.Contains(ps.Others()[0].Msg, "Next.js") {
		t.Errorf("RP-02: %s", codes(ps))
	}
	if ps.Err() != nil {
		t.Errorf("a path route is allowed: %v", ps.Err())
	}
}

func TestMemberSpecs(t *testing.T) {
	for spec, want := range map[string]string{
		"container:web:3000":      "container web web:3000",
		"service:shop/web:3000":   "compose-service shop/web shop/web:3000",
		"port:8080":               "host-port  host:8080",
		"unix:/run/a.sock":        "unix /run/a.sock unix:/run/a.sock",
		"10.0.0.5:8080":           "address 10.0.0.5 10.0.0.5:8080",
		"https://api.example.net": "address api.example.net api.example.net:443",
		"addr:[2001:db8::1]:80":   "address 2001:db8::1 [2001:db8::1]:80",
		"container:web:3000,weight=2,backup,max_conns=5": "container web web:3000",
	} {
		m, _, err := ParseMember(spec)
		if err != nil {
			t.Errorf("%s: %v", spec, err)
			continue
		}
		if got := m.Kind + " " + m.Ref + " " + m.Label(); got != want {
			t.Errorf("%s → %q, want %q", spec, got, want)
		}
	}
	m, _, _ := ParseMember("container:web:3000,weight=2,backup,max_conns=5,max_fails=3,fail_timeout=10s")
	if got := strings.Join(m.Params(), " "); got != "weight=2 max_fails=3 fail_timeout=10s max_conns=5 backup" {
		t.Errorf("params: %s", got)
	}
	// A service of a linked app: the CLI completes Ref with the project.
	if m, _, err := ParseMember("app:shop/web:3000"); err != nil || m.Kind != KindService || m.App != "shop" || m.Ref != "/web" || m.Port != 3000 {
		t.Errorf("app member: %+v %v", m, err)
	}
	for _, bad := range []string{"app:shop", "app:/web", "app:shop/a/b"} {
		if _, _, err := ParseMember(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if _, s, _ := ParseMember("grpcs://g.example.net:50051"); s != "grpcs" {
		t.Errorf("scheme: %s", s)
	}
	for _, bad := range []string{"container:", "port:0", "unix:rel.sock", "nohost", "https://a.example/x"} {
		if _, _, err := ParseMember(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCertificateCoverage(t *testing.T) {
	if !Covers([]string{"*.example.com"}, "a.example.com") || Covers([]string{"*.example.com"}, "a.b.example.com") || Covers([]string{"*.example.com"}, "example.com") {
		t.Error("wildcards cover one level only")
	}
	st := base()
	st.Pools = []Pool{*newPool("ctr:front", RoundRobin, "container:web:3000")}
	st.Routes = []Route{{ID: "example.com", Instance: "ctr:front", Host: "example.com", WWW: true, Pool: "p", HTTP: HTTPRedirect,
		TLS: &TLS{Name: "c", Names: []string{"example.com"}}}}
	if ps := CheckRoute(st, scan(), &st.Routes[0]); !has(ps, "RP-18", true) {
		t.Errorf("www not covered: %s", codes(ps))
	}
}

func TestPoolName(t *testing.T) {
	st := &State{Pools: []Pool{{Name: "api_example_com"}}}
	if n := st.PoolName("api.example.com", ""); n != "api_example_com_2" {
		t.Errorf("pool name: %s", n)
	}
	if n := st.PoolName("*.Example.com", "/v1"); n != "example_com_v1" {
		t.Errorf("pool name: %s", n)
	}
	if (Pool{Name: "shop-web"}).Upstream() != "ngt_shop_web" {
		t.Error("upstreams are namespaced ngt_ (LB-13)")
	}
}

func TestStreamIsRefused(t *testing.T) {
	_, _, err := ParseMember("tcp://db.example.com:5432")
	var p Problem
	if !errors.As(err, &p) || p.Code != "RP-14" {
		t.Errorf("tcp member: %v", err)
	}
	pool := newPool("ctr:front", RoundRobin, "container:web:3000")
	pool.Scheme = "udp"
	if ps := CheckPool(&State{Pools: []Pool{*pool}}, scan(), pool); !has(ps, "RP-14", true) {
		t.Errorf("udp pool: %s", codes(ps))
	}
}

func TestUnixSocketMembers(t *testing.T) {
	rep := scan()
	host := rep.Find("host:nginx.service")
	dir := t.TempDir()
	sock := filepath.Join(dir, "app.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	defer l.Close()
	if err := os.Chmod(sock, 0o700); err != nil {
		t.Fatal(err)
	}
	ps := checkMember(rep, host, Member{Kind: KindUnix, Ref: sock})
	if !has(ps, "RP-16", false) || !strings.Contains(ps[0].Msg, "read and write") {
		t.Errorf("socket workers cannot open: %+v", ps)
	}
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}
	if ps := checkMember(rep, host, Member{Kind: KindUnix, Ref: sock}); len(ps) != 0 {
		t.Errorf("open socket: %+v", ps)
	}
	if ps := checkMember(rep, host, Member{Kind: KindUnix, Ref: filepath.Join(dir, "gone.sock")}); !has(ps, "RP-16", false) {
		t.Errorf("missing socket: %+v", ps)
	}
	plain := filepath.Join(dir, "plain")
	_ = os.WriteFile(plain, nil, 0o666)
	if ps := checkMember(rep, host, Member{Kind: KindUnix, Ref: plain}); !has(ps, "RP-16", true) {
		t.Errorf("not a socket: %+v", ps)
	}
}

func TestStateSchema2GetsAppsAndCerts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NGITOOL_ROOT", dir)
	p := paths.Get()
	if err := os.MkdirAll(p.Lib, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.State, []byte(`{"schema":2,"instances":[],"pools":[],"routes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Load(p)
	if err != nil || st.Schema != 4 || st.Apps == nil || st.Certs == nil || st.Domains == nil || st.Edges == nil {
		t.Fatalf("2 → 4: %+v %v", st, err)
	}
	st.Apps = append(st.Apps, compose.App{Name: "shop", Attached: map[string]compose.Attach{"web": {Network: "edge", Keys: []string{"default"}}}})
	c := st.Clone()
	c.Apps[0].Attached["web"] = compose.Attach{Network: "other"}
	if st.Apps[0].Attached["web"].Network != "edge" {
		t.Error("Clone shares the apps' maps")
	}
	m := Member{Kind: KindService, Ref: "shop/web"}
	if !m.OfApp(compose.App{Name: "x", Project: "shop"}, "web") || m.OfApp(compose.App{Name: "x", Project: "shop"}, "db") {
		t.Error("OfApp")
	}
}

// legacy: "parseTarget normalises scheme, case and slashes" and
// "validatePath refuses the root and odd characters". The root "/" is the
// whole host in NgiTool (a path route needs a path), not an error.
func TestLegacyTargets(t *testing.T) {
	for in, want := range map[string][2]string{
		"https://Example.com/admin/": {"example.com", "/admin"},
		"api.example.com":            {"api.example.com", ""},
		"example.com/":               {"example.com", ""},
		"example.com/api/v1":         {"example.com", "/api/v1"},
	} {
		h, p, err := ParseTarget(in)
		if err != nil || h != want[0] || p != want[1] {
			t.Errorf("ParseTarget(%q) = %q %q %v", in, h, p, err)
		}
	}
	if _, _, err := ParseTarget("bad host"); err == nil {
		t.Error("invalid hostname accepted")
	}
	if _, _, err := ParseTarget("example.com/a b"); err == nil || !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("invalid path: %v", err)
	}
}

// legacy: "sourceFromFlags builds each source type" — every legacy source
// is a member spec: app, container, port, static. Two sources at once were
// an error there; here two --to make a pool.
func TestLegacySourcesAreMembers(t *testing.T) {
	for spec, want := range map[string]Member{
		"app:shop/web:3000":              {Kind: KindService, App: "shop", Ref: "/web", Port: 3000},
		"container:my-app:3000":          {Kind: KindContainer, Ref: "my-app", Host: "my-app", Port: 3000},
		"port:3001":                      {Kind: KindHostPort, Port: 3001},
		"static:www/blog/":               {Kind: KindStatic, Ref: "blog"},
		"static:/srv/site/":              {Kind: KindStatic, Ref: "/srv/site"},
		"service:shop/web:3000":          {Kind: KindService, Ref: "shop/web", Port: 3000},
		"container:my-app:3000,weight=2": {Kind: KindContainer, Ref: "my-app", Host: "my-app", Port: 3000, Weight: 2},
	} {
		m, _, err := ParseMember(spec)
		if err != nil || !reflect.DeepEqual(m, want) {
			t.Errorf("ParseMember(%q) = %+v %v, want %+v", spec, m, err, want)
		}
	}
	for _, bad := range []string{"static:../etc", "static:", "app:shop", "port:abc"} {
		if _, _, err := ParseMember(bad); err == nil {
			t.Errorf("ParseMember(%q) accepted", bad)
		}
	}
	if (Member{Kind: KindStatic, Ref: "docs"}).Label() != "static:docs" {
		t.Error("static label")
	}
}

// EDGE-04: a static route serves one folder, on the edge stack or host
// nginx only.
func TestStaticMembers(t *testing.T) {
	st := &State{}
	p := Pool{Name: "s", Instance: "edge:/srv/edge", Method: RoundRobin, Scheme: "http",
		Members: []Member{{Kind: KindStatic, Ref: "site"}, {Kind: KindContainer, Ref: "web", Port: 80}}}
	st.Pools = []Pool{p}
	rep := &discover.Report{Instances: []discover.Instance{{ID: "edge:/srv/edge", Kind: discover.KindEdge,
		Mounts: []discover.Mount{{Type: "bind", Source: t.TempDir(), Dest: "/var/www"}}},
		{ID: "ctr:front", Kind: discover.KindContainer, Name: "front"}}}
	if ps := CheckPool(st, rep, &st.Pools[0]); !has(ps, "EDGE-04", true) {
		t.Errorf("static with other members: %+v", ps)
	}
	st.Pools[0].Members = st.Pools[0].Members[:1]
	if !st.Pools[0].Static() {
		t.Error("one static member is a static pool")
	}
	if ps := CheckPool(st, rep, &st.Pools[0]); !has(ps, "EDGE-04", false) || has(ps, "EDGE-04", true) {
		t.Errorf("missing folder is a warning: %+v", ps)
	}
	st.Pools[0].Instance = "ctr:front"
	if ps := CheckPool(st, rep, &st.Pools[0]); !has(ps, "EDGE-04", true) {
		t.Errorf("a plain container cannot serve static routes: %+v", ps)
	}
}
