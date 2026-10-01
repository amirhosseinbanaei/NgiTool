package migrate

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
)

func on(b bool) *bool { return &b }

// legacyState is cli/test/run.mjs's STATE fixture.
func legacyState() EdgeJSON {
	return EdgeJSON{
		Version: 1,
		Certs: map[string]LCert{
			"example.com":   {Type: "letsencrypt", Names: []string{"example.com", "*.example.com"}},
			"api.other.org": {Type: "custom", Names: []string{"api.other.org"}, AOP: true},
		},
		Domains: map[string]LDomain{"example.com": {Cert: "example.com"}},
		Sites: map[string]LSite{
			"example.com":     {Cert: "example.com", Source: LSource{Type: "static", Dir: "example.com"}, DNS: "proxied"},
			"api.example.com": {Cert: "example.com", Source: LSource{Type: "app", App: "shop", Service: "web", Port: 3000, Upstream: "shop-web"}},
			"api.other.org":   {Cert: "api.other.org", Source: LSource{Type: "port", Port: 3001, IP: "172.30.0.1"}},
			"off.example.com": {Cert: "example.com", Enabled: on(false), Source: LSource{Type: "container", Name: "x", Port: 1}},
		},
		Paths: map[string]LPath{
			"example.com/admin":  {Host: "example.com", Path: "/admin", Source: LSource{Type: "container", Name: "admin", Port: 3000}},
			"example.com/api/v1": {Host: "example.com", Path: "/api/v1", Source: LSource{Type: "container", Name: "api", Port: 8000}, Strip: true},
			"example.com/docs":   {Host: "example.com", Path: "/docs", Source: LSource{Type: "static", Dir: "docs"}},
		},
	}
}

// legacy: "buildFiles writes one file per cert, enabled site and path"
func TestLegacyBuildFiles(t *testing.T) {
	files, err := BuildLegacy(legacyState())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k, v := range files {
		keys = append(keys, k)
		if !strings.HasPrefix(v, LegacyMarker) {
			t.Errorf("%s lacks the marker", k)
		}
	}
	sort.Strings(keys)
	want := []string{
		"conf/locations/example.com/admin.conf", "conf/locations/example.com/api-v1.conf", "conf/locations/example.com/docs.conf",
		"conf/sites/api.example.com.conf", "conf/sites/api.other.org.conf", "conf/sites/example.com.conf",
		"conf/snippets/ssl/api.other.org.conf", "conf/snippets/ssl/example.com.conf",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("files:\n%v\n%v", keys, want)
	}
	match := func(file, re string, want bool) {
		t.Helper()
		if regexp.MustCompile(re).MatchString(files[file]) != want {
			t.Errorf("%s: %s should match=%v:\n%s", file, re, want, files[file])
		}
	}
	// legacy: "apex gets www when its cert covers it; upstreams and certs are wired"
	match("conf/sites/example.com.conf", `server_name example\.com www\.example\.com;`, true)
	match("conf/sites/example.com.conf", `root /var/www/example\.com;`, true)
	match("conf/sites/api.example.com.conf", `set \$upstream shop-web:3000;`, true)
	match("conf/sites/api.example.com.conf", `snippets/ssl/example\.com\.conf`, true)
	match("conf/sites/api.other.org.conf", `set \$upstream 172\.30\.0\.1:3001;`, true)
	match("conf/snippets/ssl/example.com.conf", `/etc/letsencrypt/live/example\.com/fullchain\.pem`, true)
	match("conf/snippets/ssl/api.other.org.conf", `/etc/edge-certs/api\.other\.org/privkey\.pem`, true)
	match("conf/snippets/ssl/api.other.org.conf", `include .*cloudflare-aop\.conf;`, true)
	match("conf/snippets/ssl/example.com.conf", `include`, false)
	// legacy: "path routes: strip adds a rewrite, keep does not"
	match("conf/locations/example.com/api-v1.conf", `rewrite \^/api/v1/\(\.\*\)\$ /\$1 break;`, true)
	match("conf/locations/example.com/admin.conf", `rewrite`, false)
	match("conf/locations/example.com/admin.conf", `\{\{`, false)
	match("conf/locations/example.com/docs.conf", `alias /var/www/docs/;`, true)
	// legacy: "generated sites answer ACME challenges"
	match("conf/sites/example.com.conf", `include /etc/nginx/edge/snippets/acme-challenge\.conf;`, true)

	// legacy: "buildFiles refuses a site whose cert is unknown"
	bad := legacyState()
	bad.Sites = map[string]LSite{"x.io": {Cert: "nope", Source: LSource{Type: "static", Dir: "x"}}}
	if _, err := BuildLegacy(bad); err == nil || !strings.Contains(err.Error(), "not in edge.json") {
		t.Errorf("unknown cert: %v", err)
	}

	// legacy: "plain HTTP: serve adds a :80 server and turns HSTS off for that host only"
	st := legacyState()
	s := st.Sites["api.example.com"]
	s.HTTP = "serve"
	st.Sites["api.example.com"] = s
	files, _ = BuildLegacy(st)
	api := files["conf/sites/api.example.com.conf"]
	if n := len(regexp.MustCompile(`(?m)^server \{`).FindAllString(api, -1)); n != 2 {
		t.Errorf("servers: %d", n)
	}
	match = func(file, re string, want bool) {
		t.Helper()
		if regexp.MustCompile(re).MatchString(files[file]) != want {
			t.Errorf("%s: %s should match=%v", file, re, want)
		}
	}
	match("conf/sites/api.example.com.conf", `listen 80;\n    server_name api\.example\.com;`, true)
	match("conf/sites/api.example.com.conf", `(?s)listen 80;.*include /etc/nginx/edge/locations/api\.example\.com/\*\.conf;.*set \$upstream shop-web:3000;`, true)
	match("conf/sites/api.example.com.conf", `Strict-Transport-Security "max-age=0"`, true)
	match("conf/sites/api.example.com.conf", `max-age=31536000`, false)
	match("conf/sites/example.com.conf", `listen 80;`, false)
	match("conf/sites/example.com.conf", `Strict-Transport-Security "max-age=31536000; includeSubDomains" always;`, true)
}

// fixtureToken must never reach NgiTool's state or output.
const fixtureToken = "fixture-token-not-real-0123456789"

// fixture is a synthetic nginx-edge directory: every source type,
// disabled routes, a plain-HTTP host, strip paths, AOP, a broken app
// symlink and a hand-written site. example.com names only.
func fixture(t *testing.T) (dir string, projects string) {
	t.Helper()
	root := t.TempDir()
	dir, projects = filepath.Join(root, "nginx-edge"), filepath.Join(root, "projects")
	if _, err := edge.Write(dir); err != nil {
		t.Fatal(err)
	}
	if err := edge.WriteEnv(dir, [][2]string{{"ACME_EMAIL", "it@example.com"}, {"SERVER_IP", "203.0.113.10"}}); err != nil {
		t.Fatal(err)
	}
	if err := edge.WriteToken(dir, fixtureToken); err != nil {
		t.Fatal(err)
	}
	st := EdgeJSON{
		Version: 1,
		Certs: map[string]LCert{
			"example.com":      {Type: "letsencrypt", Challenge: "http", Names: []string{"example.com", "www.example.com"}},
			"wild.example.com": {Type: "letsencrypt", Names: []string{"example.com", "*.example.com"}},
			"api.example.org":  {Type: "custom", Names: []string{"api.example.org"}, AOP: true},
			"shop.example.com": {Type: "origin", Names: []string{"shop.example.com"}},
		},
		Domains: map[string]LDomain{"example.com": {Cert: "example.com", Zone: &LZone{ID: "zone-0001", Name: "example.com"}, Added: "2026-01-01"}},
		Sites: map[string]LSite{
			"example.com":        {Cert: "example.com", Source: LSource{Type: "static", Dir: "example.com"}, DNS: "proxied"},
			"api.example.org":    {Cert: "api.example.org", Source: LSource{Type: "port", Port: 3001, IP: "172.30.0.1"}, DNS: "dns-only"},
			"shop.example.com":   {Cert: "shop.example.com", Source: LSource{Type: "app", App: "shop", Service: "web", Port: 3000, Upstream: "shop-web"}, DNS: "proxied", HTTP: "serve"},
			"off.example.com":    {Cert: "wild.example.com", Enabled: on(false), Source: LSource{Type: "container", Name: "off", Port: 80}},
			"legacy.example.com": {Cert: "wild.example.com", Source: LSource{Type: "app", App: "gone", Service: "web", Port: 80, Upstream: "gone-web"}},
			"box.example.com":    {Cert: "wild.example.com", Source: LSource{Type: "container", Name: "box", Port: 8080}, DNS: "skip"},
		},
		Paths: map[string]LPath{
			"example.com/admin":  {Host: "example.com", Path: "/admin", Source: LSource{Type: "container", Name: "admin", Port: 3000}},
			"example.com/api/v1": {Host: "example.com", Path: "/api/v1", Source: LSource{Type: "container", Name: "api", Port: 8000}, Strip: true},
			"example.com/docs":   {Host: "example.com", Path: "/docs", Source: LSource{Type: "static", Dir: "docs"}},
			"example.com/old":    {Host: "example.com", Path: "/old", Source: LSource{Type: "container", Name: "old", Port: 80}, Enabled: on(false)},
		},
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("edge.json", string(b))
	files, err := BuildLegacy(st)
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		write(rel, body)
	}
	for host := range st.Sites {
		_ = os.MkdirAll(filepath.Join(dir, "conf", "locations", host), 0o755)
	}
	write("conf/sites/custom.example.org.conf", "# hand-written\nserver {\n    listen 80;\n    server_name custom.example.org;\n    location / { return 200 \"hi\\n\"; }\n}\n")
	// AOP needs Cloudflare's origin-pull CA (edge cf-sync); any CA PEM does for nginx -t.
	ca, _, _ := certs.SelfSignedPair([]string{"origin-pull.example.com"}, 90, time.Now())
	write("conf/certs/cloudflare-origin-pull-ca.pem", string(ca))
	write("www/example.com/index.html", "home\n")
	write("www/docs/index.html", "docs\n")
	for name, c := range st.Certs {
		chain, key, _ := certs.SelfSignedPair(c.Names, 90, time.Now())
		home := filepath.Join(dir, "data", "certs", name)
		if c.Type == "letsencrypt" {
			home = filepath.Join(dir, "data", "letsencrypt", "live", name)
		}
		if err := certs.Store(home, chain, key); err != nil {
			t.Fatal(err)
		}
	}
	// apps/shop → a project with an override; apps/gone → a broken link.
	if err := os.MkdirAll(filepath.Join(projects, "shop"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(projects, "shop", "compose.yaml"), []byte("name: shop\nservices:\n  web:\n    image: nginx:alpine\n"), 0o644)
	for _, a := range []string{"shop", "gone"} {
		_ = os.MkdirAll(filepath.Join(dir, "apps", a), 0o755)
	}
	_ = os.Symlink(filepath.Join(projects, "shop", "compose.yaml"), filepath.Join(dir, "apps", "shop", "compose.yaml"))
	_ = os.Symlink(filepath.Join(projects, "gone", "compose.yaml"), filepath.Join(dir, "apps", "gone", "compose.yaml"))
	write("apps/shop/edge.override.yaml", "# Managed by edge — attaches services of this app to the nginx network \"edge\".\n"+
		"# Applied by `edge app up|restart`; the project's own compose file is never touched.\n"+
		`# edge: {"network":"edge","services":{"web":{"alias":"shop-web","keys":["default"]}}}`+"\nservices: {}\n")
	return dir, projects
}

// fixtureEnv is a scan in which the stack's nginx runs on network edge.
func fixtureEnv(dir string) Env {
	in := &discover.Instance{ID: model.EdgeInstanceID(dir), Kind: discover.KindEdge, Name: "edge-nginx-1", Container: "edge-nginx-1",
		Version: "nginx/1.28.0", State: discover.StateRunning, Networks: []string{"edge"}, WorkingDir: dir, Project: "edge"}
	rep := &discover.Report{Containers: []discover.Container{{Name: "edge-nginx-1", Running: true, Networks: []string{"edge"}, Gateways: map[string]string{"edge": "172.30.0.1"}}}}
	return Env{Report: rep, Instance: in, Image: edge.Image(dir), SkipTest: true}
}

func TestMappingOfEverySourceType(t *testing.T) {
	dir, projects := fixture(t)
	l, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !l.TokenSet || len(l.Managed) == 0 {
		t.Fatalf("read: token %v, %d managed", l.TokenSet, len(l.Managed))
	}
	if !reflect.DeepEqual(l.Unmanaged, []string{"conf/sites/00-default.conf", "conf/sites/custom.example.org.conf"}) {
		t.Errorf("unmanaged: %v", l.Unmanaged)
	}
	id := model.EdgeInstanceID(dir)
	p := Map(l, &model.State{}, Options{Instance: id, Project: "edge", OverrideDir: "/var/lib/ngitool/overrides", Now: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
	n := p.Next
	if len(p.Drift) != 0 || len(p.Blockers()) != 0 {
		t.Fatalf("drift %v blockers %+v", p.Drift, p.Blockers())
	}
	if n.EdgeAt(dir) == nil || len(n.Certs) != 4 || len(n.Domains) != 1 || n.Domains[0].Zone.ID != "zone-0001" {
		t.Fatalf("edge/certs/domains: %+v %+v %+v", n.Edges, n.Certs, n.Domains)
	}
	le := n.Cert(id, "example.com")
	if le.Kind != "letsencrypt" || le.Challenge != "http" || le.Cert != "/etc/letsencrypt/live/example.com/fullchain.pem" ||
		le.HostCert != filepath.Join(dir, "data", "letsencrypt", "live", "example.com", "fullchain.pem") || le.Certbot != "stack" {
		t.Errorf("letsencrypt cert: %+v", le)
	}
	if c := n.Cert(id, "wild.example.com"); c.Challenge != "dns" {
		t.Errorf("a letsencrypt cert without a challenge is DNS-01: %+v", c)
	}
	if c := n.Cert(id, "api.example.org"); !c.AOP || c.Cert != "/etc/edge-certs/api.example.org/fullchain.pem" {
		t.Errorf("custom cert: %+v", c)
	}
	route := func(rid string) (*model.Route, model.Member) {
		t.Helper()
		r := n.Route(rid)
		if r == nil {
			t.Fatalf("no route %s", rid)
		}
		return r, n.Pool(r.Pool).Members[0]
	}
	r, m := route("example.com")
	if !r.WWW || m.Kind != model.KindStatic || m.Ref != "example.com" || r.DNS != "proxied" || r.HTTP != model.HTTPRedirect || r.TLS.Name != "example.com" {
		t.Errorf("static apex: %+v %+v", r, m)
	}
	r, m = route("api.example.org")
	if m.Kind != model.KindHostPort || m.Port != 3001 || !r.TLS.AOP || r.DNS != "dns-only" {
		t.Errorf("host port + AOP: %+v %+v", r, m)
	}
	r, m = route("shop.example.com")
	if m.Kind != model.KindService || m.App != "shop" || m.Ref != "shop/web" || m.Host != "shop-web" || r.HTTP != model.HTTPServe {
		t.Errorf("app + plain HTTP: %+v %+v", r, m)
	}
	r, _ = route("off.example.com")
	if r.Enabled {
		t.Error("a disabled site stays disabled")
	}
	r, m = route("legacy.example.com")
	if m.App != "" || m.Ref != "gone/web" || m.Host != "gone-web" || !r.Enabled {
		t.Errorf("route to a broken app: %+v %+v", r, m)
	}
	_, m = route("box.example.com")
	if m.Kind != model.KindContainer || m.Ref != "box" || m.Port != 8080 {
		t.Errorf("container: %+v", m)
	}
	r, _ = route("example.com/api/v1")
	if !r.Options.StripPrefix || r.TLS.Name != "example.com" {
		t.Errorf("strip path: %+v", r)
	}
	_, m = route("example.com/docs")
	if m.Kind != model.KindStatic || m.Ref != "docs" {
		t.Errorf("static path: %+v", m)
	}
	if r, _ := route("example.com/old"); r.Enabled {
		t.Error("a disabled path stays disabled")
	}
	a := n.App("shop")
	if a == nil || a.Project != "shop" || a.WorkingDir != filepath.Join(projects, "shop") || a.Files[0] != filepath.Join(projects, "shop", "compose.yaml") ||
		a.Override != "/var/lib/ngitool/overrides/shop.yaml" || a.Attached["web"].Alias != "shop-web" || a.Attached["web"].Network != "edge" {
		t.Errorf("app: %+v", a)
	}
	if n.App("gone") != nil {
		t.Error("a broken app is not linked")
	}
	var warnedBroken, noted bool
	for _, f := range p.Findings {
		warnedBroken = warnedBroken || (f.Code == "DOCK-10" && strings.Contains(f.Msg, "apps/gone"))
		noted = noted || (f.Code == "MIG-04" && strings.Contains(f.Msg, "ngitool app up shop"))
	}
	if !warnedBroken || !noted {
		t.Errorf("findings: %+v", p.Findings)
	}
	b, _ := json.Marshal(n)
	if strings.Contains(string(b), fixtureToken) {
		t.Fatal("the Cloudflare token reached state")
	}
}

func TestDryRunVerdict(t *testing.T) {
	dir, _ := fixture(t)
	l, _ := Read(dir)
	p := Map(l, &model.State{}, Options{Instance: model.EdgeInstanceID(dir), Project: "edge", OverrideDir: "/var/lib/ngitool/overrides"})
	r, err := DryRun(context.Background(), fixtureEnv(dir), p)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Safe() || len(r.Diffs) != 0 {
		t.Fatalf("expected safe, got blockers:\n%+v\ndiffs:\n%+v", r.Blockers, r.Diffs)
	}
	if len(r.Removed) != len(l.Managed) || len(r.Files) == 0 {
		t.Errorf("files: %d written, %d removed of %d", len(r.Files), len(r.Removed), len(l.Managed))
	}
	// dir itself is never written by a dry run.
	if _, err := os.Stat(filepath.Join(dir, "conf", "conf.d", "ngitool.conf")); err == nil {
		t.Error("the dry run wrote into the directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "edge.json")); err != nil {
		t.Error("edge.json is still there")
	}

	// A target that changes is a difference, with where and why.
	changed := Map(l, &model.State{}, Options{Instance: model.EdgeInstanceID(dir), Project: "edge"})
	changed.Next.Pool(changed.Next.Route("box.example.com").Pool).Members[0].Port = 9999
	r, _ = DryRun(context.Background(), fixtureEnv(dir), changed)
	if r.Safe() || len(r.Diffs) != 1 || r.Diffs[0].Where != "443 box.example.com location /" ||
		r.Diffs[0].Was != "proxy http://box:8080" || r.Diffs[0].Now != "proxy http://box:9999" || r.Diffs[0].Reason == "" {
		t.Errorf("a changed target: %+v", r.Diffs)
	}

	// MIG-02: a hand edit in a legacy-managed file is a blocker.
	site := filepath.Join(dir, "conf", "sites", "box.example.com.conf")
	b, _ := os.ReadFile(site)
	_ = os.WriteFile(site, []byte(strings.Replace(string(b), "location / {", "location / {\n        client_max_body_size 1g;", 1)), 0o644)
	l, _ = Read(dir)
	p = Map(l, &model.State{}, Options{Instance: model.EdgeInstanceID(dir), Project: "edge"})
	if !reflect.DeepEqual(p.Drift, []string{"conf/sites/box.example.com.conf"}) {
		t.Errorf("drift: %v", p.Drift)
	}
	r, _ = DryRun(context.Background(), fixtureEnv(dir), p)
	if r.Safe() {
		t.Error("drift must block")
	}

	// A real difference: a hand-written file that pointed a host elsewhere
	// and now loses to NgiTool's server is reported with its reason.
	_ = os.WriteFile(site, b, 0o644)
	_ = os.WriteFile(filepath.Join(dir, "conf", "locations", "box.example.com", "extra.conf"), []byte("location /x { return 418; }\n"), 0o644)
	l, _ = Read(dir)
	p = Map(l, &model.State{}, Options{Instance: model.EdgeInstanceID(dir), Project: "edge"})
	r, _ = DryRun(context.Background(), fixtureEnv(dir), p)
	if !r.Safe() {
		t.Errorf("a hand-written location file is kept and still included: %+v %+v", r.Blockers, r.Diffs)
	}
}

// The dry run's nginx -t with the stack's real image (NGITOOL_IT=1).
func TestIntegrationDryRunNginxT(t *testing.T) {
	if os.Getenv("NGITOOL_IT") != "1" {
		t.Skip("set NGITOOL_IT=1 to run against the real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	dir, _ := fixture(t)
	l, _ := Read(dir)
	p := Map(l, &model.State{}, Options{Instance: model.EdgeInstanceID(dir), Project: "edge"})
	env := fixtureEnv(dir)
	env.SkipTest = false
	r, err := DryRun(context.Background(), env, p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Test != "ok" || !r.Safe() {
		t.Fatalf("nginx -t %s:\n%s\nblockers %+v", r.Test, r.TestOut, r.Blockers)
	}
}
