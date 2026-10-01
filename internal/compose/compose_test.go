package compose

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

func TestClassify(t *testing.T) {
	cases := map[string][3]string{
		"compose.yaml":                 {RoleBase, "", "ok"},
		"compose.yml":                  {RoleBase, "", "ok"},
		"docker-compose.yaml":          {RoleBase, "", "ok"},
		"docker-compose.yml":           {RoleBase, "", "ok"},
		"compose.override.yaml":        {RoleOverride, "override", "ok"},
		"docker-compose.override.yml":  {RoleOverride, "override", "ok"},
		"compose.dev.yml":              {RoleEnv, "dev", "ok"},
		"compose.prod.yaml":            {RoleEnv, "prod", "ok"},
		"docker-compose.staging.yml":   {RoleEnv, "staging", "ok"},
		"docker-compose.local.yaml":    {RoleEnv, "local", "ok"},
		"prod.compose.yaml":            {RoleEnv, "prod", "ok"},
		"monitoring.compose.yml":       {RoleEnv, "monitoring", "ok"},
		"dev.override.compose.yaml":    {RoleOverride, "dev.override", "ok"},
		"Compose.yaml":                 {"", "", ""}, // case-sensitive, as Docker is
		"compose.json":                 {"", "", ""},
		"compose.yaml.bak":             {"", "", ""},
		"compose..yaml":                {"", "", ""},
		"my-compose.yaml":              {"", "", ""},
		".compose.yaml":                {"", "", ""},
		"docker-compose.yml.orig":      {"", "", ""},
		"composer.yaml":                {"", "", ""},
		"docker-compose-prod.yml":      {"", "", ""},
		"compose.prod.override.yml":    {RoleOverride, "prod.override", "ok"},
		"docker-compose.ci.test.yml":   {RoleEnv, "ci.test", "ok"},
		"infra.compose.yml":            {RoleEnv, "infra", "ok"},
		"docker-compose.override.yaml": {RoleOverride, "override", "ok"},
	}
	for name, want := range cases {
		role, word, ok := Classify(name)
		got := [3]string{role, word, ""}
		if ok {
			got[2] = "ok"
		}
		if got != want {
			t.Errorf("Classify(%q) = %v, want %v", name, got, want)
		}
	}
}

// fixtureTree builds every file-name pattern, nested docker/ folders at
// and past the depth limit, a symlink loop, a broken symlink, a legacy
// apps/ symlink to a real project, and files owned by another uid.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range []string{
		"alice/shop/compose.yaml",
		"alice/shop/compose.override.yaml",
		"alice/shop/compose.prod.yaml",
		"alice/shop/docker-compose.yml", // a second base: compose.yaml wins the default
		"alice/blog/docker-compose.yml",
		"alice/blog/docker-compose.staging.yml",
		"alice/blog/prod.compose.yaml",
		"alice/api/backend/docker/compose.dev.yml", // two folders deep
		"alice/a/b/c/d/docker/compose.yaml",        // docker/ one past the depth limit
		"alice/a/b/c/d/e/compose.yaml",             // too deep
		"alice/a/b/c/d/docker/x/compose.yaml",      // below a special folder past the limit: not searched
		"alice/infra-app/.docker/compose.yml",      // hidden, but special
		"alice/web/node_modules/x/compose.yaml",    // skipped
		"alice/web/.next/compose.yaml",             // skipped
		"alice/locked/sub/compose.yaml",            // unreadable dir
		"bob/api/deploy/compose.yml",
	} {
		touch(t, filepath.Join(root, p))
	}
	// A symlink loop and a link to a sibling: directories are not followed.
	must(t, os.Symlink(filepath.Join(root, "alice"), filepath.Join(root, "alice/loop")))
	must(t, os.Symlink(filepath.Join(root, "bob/api"), filepath.Join(root, "alice/link")))
	// A broken compose symlink.
	touch(t, filepath.Join(root, "alice/gone/README"))
	must(t, os.Symlink(filepath.Join(root, "nowhere/compose.yaml"), filepath.Join(root, "alice/gone/compose.yaml")))
	// The legacy apps/<app>/compose.yaml → the real project's file.
	must(t, os.MkdirAll(filepath.Join(root, "alice/edge/apps/blog"), 0o700))
	must(t, os.Symlink(filepath.Join(root, "alice/blog/docker-compose.yml"), filepath.Join(root, "alice/edge/apps/blog/docker-compose.yml")))
	if os.Geteuid() == 0 {
		must(t, os.Chown(filepath.Join(root, "bob/api/deploy/compose.yml"), 1000, 1000))
	}
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestScanFixtureTree(t *testing.T) {
	root := fixtureTree(t)
	prevRead, prevUser := readDir, UserName
	locked := filepath.Join(root, "alice/locked")
	readDir = func(p string) ([]os.DirEntry, error) {
		if p == locked {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return os.ReadDir(p)
	}
	UserName = func(uid int) string { return map[int]string{0: "root", 1000: "bob"}[uid] }
	t.Cleanup(func() { readDir, UserName = prevRead, prevUser })

	res := Scan([]string{filepath.Join(root, "*")}, DefaultDepth)
	got := map[string][]string{}
	broken := map[string]string{}
	for _, d := range res.Dirs {
		rel, _ := filepath.Rel(root, d.Path)
		if d.Broken != "" {
			broken[rel] = d.Broken
			continue
		}
		for _, f := range d.Files {
			got[rel] = append(got[rel], filepath.Base(f.Path)+":"+f.Role+map[bool]string{true: ":broken", false: ""}[f.Broken != ""])
		}
	}
	want := map[string][]string{
		"alice/shop":               {"compose.yaml:base", "docker-compose.yml:base", "compose.override.yaml:override", "compose.prod.yaml:env"},
		"alice/blog":               {"docker-compose.yml:base", "prod.compose.yaml:env", "docker-compose.staging.yml:env"},
		"alice/api/backend/docker": {"compose.dev.yml:env"},
		"alice/a/b/c/d/docker":     {"compose.yaml:base"},
		"alice/infra-app/.docker":  {"compose.yml:base"},
		"alice/gone":               {"compose.yaml:base:broken"},
		"bob/api/deploy":           {"compose.yml:base"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan:\n got  %v\n want %v", got, want)
	}
	if !strings.Contains(broken["alice/locked"], "permission denied") {
		t.Errorf("unreadable dir: %v", broken)
	}
	for _, d := range res.Dirs {
		if strings.HasSuffix(d.Path, "bob/api/deploy") && os.Geteuid() == 0 && d.Files[0].Owner != "bob" {
			t.Errorf("owner of bob's file: %q", d.Files[0].Owner)
		}
		if strings.Contains(d.Path, "loop") || strings.Contains(d.Path, "alice/link") || strings.Contains(d.Path, "edge/apps") {
			t.Errorf("followed a symlinked directory or a legacy apps/ link: %s", d.Path)
		}
	}
	// Find keeps one file per project for discovery.
	files, _ := Find([]string{filepath.Join(root, "*")}, DefaultDepth)
	if len(files) != 6 { // shop, blog, api, d/docker, infra-app, deploy (gone is broken)
		t.Errorf("Find: %v", files)
	}
}

func TestDefaultsAndMergeWithLabels(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"shop/compose.yaml", "shop/compose.override.yaml", "shop/compose.prod.yaml", "solo/docker/compose.dev.yml", "idle/compose.yaml"} {
		touch(t, filepath.Join(root, p))
	}
	must(t, os.WriteFile(filepath.Join(root, "idle/compose.yaml"), []byte("name: Idle_App\nservices:\n  web:\n    image: nginx:1.27\n    ports: [\"8080:80\"]\n"), 0o600))
	legacy := filepath.Join(root, "outside/apps/shop/edge.override.yaml")
	touch(t, legacy)
	res := Scan([]string{root}, DefaultDepth)
	ctrs := []Ctr{
		{Name: "shop-web-1", Image: "shop-web", State: "running", Running: true, Project: "shop", Service: "web", WorkDir: filepath.Join(root, "shop"),
			ConfigFiles: []string{filepath.Join(root, "shop/compose.yaml"), filepath.Join(root, "shop/compose.prod.yaml"), legacy}, Networks: []string{"shop_default", "edge"},
			Ports: []Port{{HostIP: "0.0.0.0", HostPort: 8443, Target: 443, Proto: "tcp"}}},
		{Name: "shop-db-1", State: "exited", Project: "shop", Service: "db", WorkDir: filepath.Join(root, "shop"), ConfigFiles: []string{filepath.Join(root, "shop/compose.yaml")}},
		// DOCK-03: the project name differs from the folder (-p).
		{Name: "dev-api-1", State: "running", Running: true, Project: "ketab-dev", Service: "api", WorkDir: filepath.Join(root, "solo/docker"),
			ConfigFiles: []string{filepath.Join(root, "solo/docker/compose.dev.yml")}},
		// Outside every root; its file is gone (DOCK-10).
		{Name: "old-web-1", State: "exited", Project: "old", Service: "web", WorkDir: "/nonexistent/old", ConfigFiles: []string{"/nonexistent/old/compose.yaml"}},
	}
	cands := Merge(res.Dirs, ctrs)
	by := map[string]Candidate{}
	for _, c := range cands {
		by[c.Name] = c
	}
	shop := by["shop"]
	if shop.State != StateRunning || shop.Up != 1 || shop.Total != 2 {
		t.Errorf("shop state: %+v", shop)
	}
	wantDefault := []string{filepath.Join(root, "shop/compose.yaml"), filepath.Join(root, "shop/compose.prod.yaml"), legacy}
	if !reflect.DeepEqual(shop.Default(), wantDefault) {
		t.Errorf("shop default -f (DOCK-02): %v", shop.Default())
	}
	foundLegacy := false
	for _, f := range shop.Files {
		foundLegacy = foundLegacy || (f.Path == legacy && f.Role == RoleExtra && f.Broken == "")
	}
	if !foundLegacy {
		t.Errorf("legacy override from the label not listed: %+v", shop.Files)
	}
	if len(shop.Public()) != 1 {
		t.Errorf("0.0.0.0 publish not flagged (DOCK-11): %+v", shop.Ports)
	}
	if c := by["ketab-dev"]; c.Dir != filepath.Join(root, "solo/docker") || c.State != StateRunning {
		t.Errorf("label project name (DOCK-03): %+v", c)
	}
	idle := by["idle_app"]
	if idle.State != StateNever || !reflect.DeepEqual(idle.Services, []string{"web"}) || idle.Nginx != "compose:idle_app/web" || len(idle.Public()) != 1 {
		t.Errorf("never started, from raw YAML: %+v", idle)
	}
	if !reflect.DeepEqual(idle.Default(), []string{filepath.Join(root, "idle/compose.yaml")}) {
		t.Errorf("idle default: %v", idle.Default())
	}
	old := by["old"]
	if old.Broken == "" || !strings.Contains(old.Files[0].Broken, "last known path") {
		t.Errorf("moved project (DOCK-10): %+v", old)
	}
	// Without a label: base plus override, never a second base.
	stopped := Candidate{Files: []File{{Path: "/p/compose.yaml", Role: RoleBase}, {Path: "/p/docker-compose.yml", Role: RoleBase}, {Path: "/p/compose.override.yaml", Role: RoleOverride}, {Path: "/p/compose.dev.yaml", Role: RoleEnv}}}
	if got := stopped.Default(); !reflect.DeepEqual(got, []string{"/p/compose.yaml", "/p/compose.override.yaml"}) {
		t.Errorf("base + override default: %v", got)
	}
}

// configJSON is `docker compose config --format json` for a project with
// a secret in its environment: it must never come out.
const configJSON = `{
  "name": "shop",
  "networks": {"default": {"name": "shop_default"}, "back": {"name": "shop_back"}, "edge": {"name": "edge", "external": true}},
  "services": {
    "web": {"image": "shop-web", "build": {"context": "."}, "expose": ["3000"], "ports": [{"mode": "ingress", "target": 8080, "published": "18080", "protocol": "tcp", "host_ip": "127.0.0.1"}],
            "environment": {"SECRET_KEY": "hunter2"}, "networks": {"default": null, "edge": {"aliases": ["shop-web"]}}, "deploy": {"replicas": 3}},
    "db": {"image": "postgres:17", "environment": {"POSTGRES_PASSWORD": "hunter2"}, "networks": {"back": null}, "restart": "always"},
    "migrate": {"image": "shop-web", "restart": "no", "networks": {"default": null}, "profiles": ["tools"]},
    "agent": {"image": "agent", "network_mode": "host"},
    "side": {"image": "busybox", "network_mode": "service:web"}
  }
}`

func TestSummarizeServices(t *testing.T) {
	cfg, err := Read(context.Background(), &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
		return execx.Result{Stdout: configJSON}, true
	}}, V2, Project{Name: "shop", Dir: "/srv/shop", Files: []string{"/srv/shop/compose.yaml"}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.Services {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"agent", "db", "side", "web", "migrate"}) {
		t.Errorf("order (one-off last): %v", names)
	}
	web := cfg.Service("web")
	if !web.Build || !reflect.DeepEqual(web.Ports, []int{3000, 8080}) || web.Replicas != 3 ||
		!reflect.DeepEqual(web.Networks, []string{"shop_default", "edge"}) || !reflect.DeepEqual(web.Aliases["edge"], []string{"shop-web"}) ||
		len(web.Published) != 1 || web.Published[0].Public() {
		t.Errorf("web: %+v", web)
	}
	if db := cfg.Service("db"); !reflect.DeepEqual(db.NetworkKeys, []string{"back"}) || db.Networks[0] != "shop_back" {
		t.Errorf("db networks: %+v", db)
	}
	if m := cfg.Service("migrate"); !m.OneOff() || m.Profiles[0] != "tools" {
		t.Errorf("migrate: %+v", m)
	}
	if !strings.Contains(cfg.Service("agent").Detached(), "host") || !strings.Contains(cfg.Service("side").Detached(), "service:web") {
		t.Error("network_mode host / service:x must explain why it cannot join (DOCK-06)")
	}
	if cfg.Service("web").Detached() != "" {
		t.Error("web can join")
	}
	if b, _ := jsonOf(cfg); strings.Contains(b, "hunter2") || strings.Contains(b, "SECRET") {
		t.Errorf("resolved environment leaked into the summary (DOCK-04): %s", b)
	}
}

func TestReadFailureAndRawFallback(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "compose.yaml")
	must(t, os.WriteFile(f, []byte("include: [other.yaml]\nservices:\n  web:\n    image: nginx\n    environment: {TOKEN: abc}\n    ports: [\"80:80\"]\n"), 0o600))
	fake := &execx.Fake{Match: func(string, []string) (execx.Result, bool) {
		return execx.Result{Code: 15, Stderr: "line 1\nline 2\nline 3\nenv file /x/.env not found"}, true
	}}
	p := Project{Name: "x", Dir: dir, Files: []string{f}}
	_, err := Read(context.Background(), fake, V2, p)
	var re *ReadError
	if !errors.As(err, &re) || re.Tail != "line 2\nline 3\nenv file /x/.env not found" {
		t.Fatalf("last 3 lines of compose's error: %v", err)
	}
	cfg, err := ReadRaw(p)
	if err != nil || !cfg.Unresolved || cfg.Service("web") == nil || !cfg.Service("web").Published[0].Public() {
		t.Fatalf("raw fallback: %+v %v", cfg, err)
	}
	if b, _ := jsonOf(cfg); strings.Contains(b, "abc") {
		t.Error("raw fallback kept environment")
	}
}

func TestEnvScrubAndExplicitArgs(t *testing.T) {
	rec := &recorder{}
	p := Project{Name: "shop", Dir: "/srv/shop", Files: []string{"/srv/shop/compose.yaml", "/srv/shop/compose.prod.yaml"}, Override: "/var/lib/ngitool/overrides/shop.yaml", Profiles: []string{"tools"}}
	_, _ = Read(context.Background(), rec, V2, p)
	if rec.opts.Dir != "/srv/shop" {
		t.Errorf("cwd: %q", rec.opts.Dir)
	}
	env := execx.Environ([]string{"COMPOSE_PROJECT_NAME=edge", "COMPOSE_FILE=/x.yaml", "PATH=/bin"}, rec.opts.Env, rec.opts.Scrub)
	if !reflect.DeepEqual(env, []string{"PATH=/bin"}) {
		t.Errorf("DOCK-08: COMPOSE_PROJECT_NAME and COMPOSE_FILE must not reach compose: %v", env)
	}
	want := []string{"compose", "-p", "shop", "--project-directory", "/srv/shop", "-f", "/srv/shop/compose.yaml", "-f", "/srv/shop/compose.prod.yaml",
		"-f", "/var/lib/ngitool/overrides/shop.yaml", "--profile", "tools", "config", "--format", "json"}
	if !reflect.DeepEqual(rec.args, want) {
		t.Errorf("argv:\n got  %v\n want %v", rec.args, want)
	}
	// v1: same arguments, YAML output (DOCK-05).
	rec.out = "name: shop\nservices:\n  web:\n    image: x\n    ports:\n      - target: 80\n        published: \"8080\"\n"
	cfg, err := Read(context.Background(), rec, Bin{Name: "docker-compose", V1: true}, p)
	if err != nil || rec.name != "docker-compose" || rec.args[0] != "-p" || cfg.Service("web").Published[0].HostPort != 8080 {
		t.Errorf("v1: %v %s %v %+v", err, rec.name, rec.args, cfg)
	}
}

type recorder struct {
	name string
	args []string
	opts execx.Opts
	out  string
}

func (r *recorder) Run(_ context.Context, name string, args []string, o execx.Opts) execx.Result {
	r.name, r.args, r.opts = name, args, o
	if r.out != "" {
		return execx.Result{Stdout: r.out}
	}
	return execx.Result{Stdout: `{"name":"shop","services":{}}`}
}

func TestOverrideRenderAndRoundTrip(t *testing.T) {
	a := App{Name: "shop", Project: "shop", Attached: map[string]Attach{
		"web": {Network: "edge", Alias: "shop-web", Keys: []string{"default", "back"}},
		"cms": {Network: "edge", Alias: "shop-cms", Keys: []string{"default"}, Manual: true},
	}, Mounts: map[string][]Mount{"proxy": {{Source: "/var/lib/ngitool/externalized/shop", Target: "/etc/nginx", ReadOnly: true}}}}
	got := RenderOverride(a)
	want := `# Managed by NgiTool — the override of app "shop".
# Passed as the last -f by every ` + "`ngitool app`" + ` command; the project's own compose files are never edited.
# A plain ` + "`docker compose up`" + ` in the project leaves it out: run ` + "`ngitool app up shop`" + ` instead (DOCK-07).
# ngitool: {"app":"shop","services":{"web":{"network":"edge","alias":"shop-web","keys":["default","back"]}},"mounts":{"proxy":[{"source":"/var/lib/ngitool/externalized/shop","target":"/etc/nginx","readOnly":true}]}}
services:
  "proxy":
    volumes:
      - type: bind
        source: "/var/lib/ngitool/externalized/shop"
        target: "/etc/nginx"
        read_only: true
  "web":
    networks:
      "default": {}
      "back": {}
      "ngt_edge":
        aliases: ["shop-web"]
networks:
  "ngt_edge":
    name: "edge"
    external: true
`
	if got != want {
		t.Errorf("override:\n%s\nwant:\n%s", got, want)
	}
	m, err := ParseMeta(got)
	if err != nil || m.App != "shop" || m.Services["web"].Alias != "shop-web" || m.Mounts["proxy"][0].Target != "/etc/nginx" {
		t.Errorf("round trip: %+v %v", m, err)
	}
	if _, ok := m.Services["cms"]; ok {
		t.Error("a manual attachment is not in the override")
	}
	if _, err := ParseMeta("services: {}\n"); err == nil {
		t.Error("a file NgiTool did not write must not parse")
	}
	// Names only, never an IP (DOCK-16).
	for _, l := range strings.Split(got, "\n") {
		if strings.Contains(l, "172.") || strings.Contains(l, "ipv4_address") {
			t.Errorf("an address in the override: %s", l)
		}
	}
	if !a.NeedsOverride() || (App{Attached: map[string]Attach{"x": {Manual: true}}}).NeedsOverride() {
		t.Error("NeedsOverride")
	}
	snip := ManualSnippet("cms", a.Attached["cms"])
	if !strings.Contains(snip, "ngt_edge:\n        aliases: [shop-cms]") || !strings.Contains(snip, "name: edge\n    external: true") {
		t.Errorf("snippet:\n%s", snip)
	}
}

func TestStepsGolden(t *testing.T) {
	svc := []string{"web"}
	cases := []struct {
		action string
		o      Opts
		svc    []string
		want   [][]string
	}{
		{Up, Opts{}, nil, [][]string{{"up", "-d"}}},
		{Up, Opts{Build: true, Pull: true, NoDeps: true, RemoveOrphans: true}, svc, [][]string{{"up", "-d", "--build", "--pull", "always", "--no-deps", "--remove-orphans", "web"}}},
		{Up, Opts{Build: true}, svc, [][]string{{"up", "-d", "--build", "web"}}},
		{Restart, Opts{}, svc, [][]string{{"restart", "web"}}},
		{Recreate, Opts{}, nil, [][]string{{"up", "-d", "--force-recreate"}}},
		{Recreate, Opts{NoDeps: true}, svc, [][]string{{"up", "-d", "--force-recreate", "--no-deps", "web"}}},
		{Rebuild, Opts{}, svc, [][]string{{"build", "web"}, {"up", "-d", "web"}}},
		{Rebuild, Opts{NoCache: true, PullBase: true, NoDeps: true}, svc, [][]string{{"build", "--no-cache", "--pull", "web"}, {"up", "-d", "--no-deps", "web"}}},
		{Pull, Opts{}, []string{"db", "cache"}, [][]string{{"pull", "db", "cache"}, {"up", "-d", "db", "cache"}}},
		{Stop, Opts{}, nil, [][]string{{"stop"}}},
		{Stop, Opts{}, svc, [][]string{{"stop", "web"}}},
		{Down, Opts{}, nil, [][]string{{"down"}}},
		{Down, Opts{Volumes: true, RemoveOrphans: true}, nil, [][]string{{"down", "--volumes", "--remove-orphans"}}},
		{Logs, Opts{}, nil, [][]string{{"logs", "--follow", "--tail", "100"}}},
		{Logs, Opts{NoFollow: true, Tail: 20, Since: "10m", Timestamps: true}, svc, [][]string{{"logs", "--tail", "20", "--since", "10m", "--timestamps", "web"}}},
		{PS, Opts{}, nil, [][]string{{"ps", "-a", "--format", "json"}}},
	}
	for _, c := range cases {
		if got := Steps(c.action, c.o, c.svc); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %+v %v:\n got  %v\n want %v", c.action, c.o, c.svc, got, c.want)
		}
	}
	// Every option of every action maps to a flag and changes the argv.
	for _, a := range Actions {
		base := Steps(a.ID, Opts{}, nil)
		for _, k := range a.Options {
			if _, ok := Options[k]; !ok {
				t.Errorf("%s: option %s has no explanation", a.ID, k)
			}
			var o Opts
			o.Set(k)
			if reflect.DeepEqual(Steps(a.ID, o, nil), base) {
				t.Errorf("%s: option %s changes nothing", a.ID, k)
			}
		}
		if a.What == "" || a.Affects == "" || a.Undo == "" {
			t.Errorf("%s: explain panel incomplete", a.ID)
		}
	}
	p := Project{Name: "shop", Dir: "/srv/my shop", Files: []string{"/srv/my shop/compose.yaml"}, Override: "/var/lib/ngitool/overrides/shop.yaml"}
	line := CommandLine(V2, p, Steps(Rebuild, Opts{NoCache: true}, svc)[0])
	if line != "docker compose -p shop --project-directory '/srv/my shop' -f '/srv/my shop/compose.yaml' -f /var/lib/ngitool/overrides/shop.yaml build --no-cache web" {
		t.Errorf("Will run: %s", line)
	}
	if got := Argv(Bin{Name: "docker-compose", V1: true}, p, []string{"stop"}); got[0] != "-p" {
		t.Errorf("v1 argv: %v", got)
	}
}

func TestDriftFromLabels(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "compose.yaml")
	touch(t, base)
	ov := filepath.Join(dir, "shop.yaml")
	a := App{Name: "shop", Project: "shop", WorkingDir: dir, Files: []string{base}, Override: ov,
		Attached: map[string]Attach{"web": {Network: "edge", Alias: "shop-web", Keys: []string{"default"}}}}
	good := []Ctr{{Name: "shop-web-1", Running: true, Project: "shop", Service: "web", ConfigFiles: []string{base, ov}, Networks: []string{"shop_default", "edge"}},
		{Name: "shop-db-1", Running: true, Project: "shop", Service: "db", ConfigFiles: []string{base, ov}}}
	if d := Check(a, good); d.Status != NetOK || d.Up != 2 {
		t.Errorf("ok: %+v", d)
	}
	plain := []Ctr{{Name: "shop-web-1", Running: true, Project: "shop", Service: "web", ConfigFiles: []string{base}, Networks: []string{"shop_default"}}}
	d := Check(a, plain)
	if d.Status != NetDetached || !reflect.DeepEqual(d.Services, []string{"web"}) || !strings.Contains(d.Why, "502") {
		t.Errorf("plain docker compose up (DOCK-07): %+v", d)
	}
	if d := Check(a, []Ctr{{Name: "shop-web-1", Project: "shop", Service: "web", ConfigFiles: []string{base}}}); d.Status != NetStopped {
		t.Errorf("stopped: %+v", d)
	}
	if d := Check(App{Name: "x", Project: "x", WorkingDir: dir, Files: []string{base}}, nil); d.Status != NetNone {
		t.Errorf("nothing attached: %+v", d)
	}
	// A copy under another project name is not this app's.
	other := []Ctr{{Name: "copy-web-1", Running: true, Project: "copy", Service: "web", ConfigFiles: []string{base}}}
	if d := Check(a, other); d.Status != NetStopped {
		t.Errorf("another project: %+v", d)
	}
	gone := a
	gone.Files = []string{filepath.Join(dir, "moved.yaml")}
	if d := Check(gone, good); d.Status != NetMissing || !strings.Contains(d.Why, "DOCK-10") {
		t.Errorf("missing files: %+v", d)
	}
}

func TestParseLineAndPS(t *testing.T) {
	cases := map[string]Line{
		"web-1  | listening on :3000":               {Service: "web", Text: "listening on :3000", Keep: true},
		"#7 [web 2/4] RUN npm ci":                   {Service: "web", Text: "RUN npm ci", Keep: true},
		"#7 DONE 3.2s":                              {Text: "#7 DONE 3.2s"},
		"#4 sha256:abc 1.2MB / 3MB":                 {Text: "#4 sha256:abc 1.2MB / 3MB"},
		" Container shop-web-1  Recreated":          {Service: "web", Text: "container Recreated", Keep: true},
		" ✔ Container shop-db-1  Started":           {Service: "db", Text: "container Started", Keep: true},
		" Network shop_default  Created":            {Text: "network Created", Keep: true},
		"failed to solve: process did not complete": {Text: "failed to solve: process did not complete", Keep: true},
		"#5 ERROR: failed to compute cache key":     {Text: "#5 ERROR: failed to compute cache key", Keep: true},
		"some chatter":                              {Text: "some chatter"},
	}
	for in, want := range cases {
		if got := ParseLine("shop", in); got != want {
			t.Errorf("ParseLine(%q) = %+v, want %+v", in, got, want)
		}
	}
	if ServiceOf("shop", "shop_api_2") != "api" || ServiceOf("shop", "custom-name") != "custom-name" {
		t.Error("ServiceOf")
	}
	ps := "web\tshop-web\trunning\tUp 2 hours (healthy)\tedge,shop_default\t0.0.0.0:8080->80/tcp, [::]:8080->80/tcp, 127.0.0.1:9000->9000/tcp\tshop\tweb\t/srv/shop\t/srv/shop/compose.yaml,/v/shop.yaml\n" +
		"plain\tbusybox\texited\tExited (0)\tbridge\t\t\t\t\t\n"
	cs := ParsePS(ps)
	if len(cs) != 1 || cs[0].Health != "healthy" || len(cs[0].Ports) != 2 || !cs[0].Ports[0].Public() || cs[0].Ports[1].Public() ||
		!reflect.DeepEqual(cs[0].ConfigFiles, []string{"/srv/shop/compose.yaml", "/v/shop.yaml"}) || !reflect.DeepEqual(cs[0].Networks, []string{"edge", "shop_default"}) {
		t.Errorf("ParsePS: %+v", cs)
	}
	js := `{"Name":"shop-web-1","Service":"web","Project":"shop","State":"running","Health":"healthy","Networks":"edge,shop_default","Labels":"SECRET=x","Publishers":[{"URL":"0.0.0.0","TargetPort":80,"PublishedPort":8080,"Protocol":"tcp"}]}
{"Name":"shop-db-1","Service":"db","Project":"shop","State":"exited","Networks":"shop_default"}`
	got := ParsePSJSON(js)
	if len(got) != 2 || got[1].Name != "shop-web-1" || !got[1].Running || got[1].Ports[0].HostPort != 8080 || got[0].Running {
		t.Errorf("ParsePSJSON: %+v", got)
	}
	if arr := ParsePSJSON("[" + strings.Replace(js, "\n", ",", 1) + "]"); len(arr) != 2 {
		t.Errorf("array form: %+v", arr)
	}
}

func TestNamesAndOrder(t *testing.T) {
	if ProjectName("My_App.v2") != "my_appv2" || ProjectName("-x") != "x" {
		t.Error("ProjectName")
	}
	taken := map[string]bool{"shop": true, "shop-2": true}
	if n := UniqueName("Shop", func(s string) bool { return taken[s] }); n != "shop-3" {
		t.Errorf("UniqueName: %s", n)
	}
	if ValidName("Shop") == nil || ValidName("shop.v2-x") != nil {
		t.Error("ValidName")
	}
	if !NginxImage("nginx:1.27-alpine") || !NginxImage("ghcr.io/x/nginx-unprivileged@sha256:ab") || !NginxImage("openresty/openresty") || NginxImage("node:22") || NginxImage("") {
		t.Error("NginxImage")
	}
	if Alias("shop", "web") != "shop-web" || NetKey("my.net") != "ngt_my_net" {
		t.Error("Alias / NetKey")
	}
}

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func TestParseOneTargetBuild(t *testing.T) {
	cases := map[string]Line{
		"#6 [2/2] RUN echo ok":                {Text: "2/2 RUN echo ok", Keep: true},
		"#2 [internal] load build definition": {Text: "load build definition"},
		" Image shop-web:it  Building":        {Service: "web", Text: "image Building", Keep: true},
		" Image postgres:17  Pulling":         {Text: "image Pulling", Keep: true},
	}
	for in, want := range cases {
		if got := ParseLine("shop", in); got != want {
			t.Errorf("ParseLine(%q) = %+v, want %+v", in, got, want)
		}
	}
}
