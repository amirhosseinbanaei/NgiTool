package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// dockerFake answers docker and docker compose from what a test set:
// the containers `docker ps` lists and the project's resolved config.
type dockerFake struct {
	mu     sync.Mutex
	ps     string // compose.Containers' tab format
	psJSON string
	config string
	calls  []string
}

func (f *dockerFake) Run(_ context.Context, name string, args []string, o execx.Opts) execx.Result {
	j := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+j)
	f.mu.Unlock()
	switch {
	case name != "docker":
		return execx.Result{Code: 127}
	case j == "compose version":
		return execx.Result{Stdout: "Docker Compose version v2.40.0"}
	case args[0] == "ps":
		return execx.Result{Stdout: f.ps}
	case args[0] == "volume":
		return execx.Result{Stdout: "shop_data\n"}
	case strings.HasSuffix(j, "config --format json"):
		return execx.Result{Stdout: f.config}
	case strings.HasSuffix(j, "config --profiles"):
		return execx.Result{Stdout: "tools\n"}
	case strings.HasSuffix(j, "ps -a --format json"):
		return execx.Result{Stdout: f.psJSON}
	}
	if o.Out != nil {
		_, _ = o.Out.Write([]byte(" Container shop-web-1  Recreated\n#3 DONE 0.1s\n"))
	}
	return execx.Result{}
}

func (f *dockerFake) ran(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

const shopConfig = `{"name":"shop","networks":{"default":{"name":"shop_default"}},"services":{
 "web":{"image":"shop-web","build":{"context":"."},"expose":["3000"],"networks":{"default":null},"environment":{"KEY":"secret"}},
 "db":{"image":"postgres:17","networks":{"default":null}},
 "tool":{"image":"x","network_mode":"host"}}}`

// appWorld is routeWorld plus a compose project on disk and a recorded
// Docker that runs nothing.
func appWorld(t *testing.T) (*env, *bytes.Buffer, string, string, *dockerFake) {
	t.Helper()
	e, buf, conf, _ := routeWorld(t)
	dir := filepath.Join(t.TempDir(), "shop")
	for _, f := range []string{"compose.yaml", "compose.prod.yaml"} {
		touchFile(t, filepath.Join(dir, f), "services:\n  web:\n    image: shop-web\n  db:\n    image: postgres:17\n")
	}
	e.fixed.Containers = append(e.fixed.Containers,
		discover.Container{Name: "shop-web-1", Project: "shop", Service: "web", Running: true, Networks: []string{"shop_default"}, Exposed: []int{3000}},
		discover.Container{Name: "shop-db-1", Project: "shop", Service: "db", Running: true, Networks: []string{"shop_default"}})
	f := &dockerFake{config: shopConfig}
	prev, prevProbe := appRunner, probeWith
	appRunner = f
	probeWith = func(_ context.Context, t apply.Target) apply.Probe {
		return apply.Probe{Route: t.Route, Status: 200, Reached: true}
	}
	t.Cleanup(func() { appRunner, probeWith = prev, prevProbe })
	return e, buf, conf, dir, f
}

func touchFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadState(t *testing.T, e *env) *model.State {
	t.Helper()
	st, err := model.Load(e.paths)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAppLinkByFlagsAndList(t *testing.T) {
	e, buf, _, dir, _ := appWorld(t)
	out := run(t, e, buf, ExitOK, "app", "link", dir, "--file", "compose.yaml", "--file", "compose.prod.yaml", "--name", "shop")
	if !strings.Contains(out, "Linked shop") {
		t.Fatalf("link:\n%s", out)
	}
	st := loadState(t, e)
	a := st.App("shop")
	if a == nil || a.Project != "shop" || a.WorkingDir != dir || len(a.Files) != 2 || a.Files[1] != filepath.Join(dir, "compose.prod.yaml") || a.LinkedAt == "" {
		t.Fatalf("state: %+v", st.Apps)
	}
	if st.Schema != 3 {
		t.Errorf("schema %d", st.Schema)
	}
	if out := run(t, e, buf, ExitOK, "app", "link", dir); !strings.Contains(out, "already linked as shop") {
		t.Errorf("relink:\n%s", out)
	}
	// The name is deduplicated like cli/src/flows.mjs: shop-2.
	other := filepath.Join(t.TempDir(), "shop")
	touchFile(t, filepath.Join(other, "docker", "compose.dev.yml"), "services:\n  api:\n    image: x\n")
	run(t, e, buf, ExitOK, "app", "link", filepath.Join(other, "docker", "compose.dev.yml"), "--project-name", "shop-dev")
	if loadState(t, e).App("shop-dev") == nil {
		t.Errorf("second app: %+v", loadState(t, e).Apps)
	}
	out = run(t, e, buf, ExitOK, "app", "ls", "--json")
	var views []appJSON
	if err := json.Unmarshal([]byte(out), &views); err != nil || len(views) != 2 || views[0].Drift.Status != compose.NetNone {
		t.Fatalf("ls --json: %v\n%s", err, out)
	}
	ui.SetColor(false)
	out = run(t, e, buf, ExitOK, "app", "ls")
	for _, s := range []string{"APP", "NETWORK", "shop", "not attached", "/tmp/"} {
		if !strings.Contains(out, s) {
			t.Errorf("ls lacks %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Error("resolved env printed")
	}
	run(t, e, buf, ExitUsage, "app", "link") // off a terminal the path is needed
	if out := run(t, e, buf, ExitOK, "app", "show", "shop"); !strings.Contains(out, "2. "+filepath.Join(dir, "compose.prod.yaml")) || !strings.Contains(out, "SERVICE") {
		t.Errorf("show:\n%s", out)
	}
}

func TestAppActionsShowTheExactCommand(t *testing.T) {
	e, buf, _, dir, f := appWorld(t)
	run(t, e, buf, ExitOK, "app", "link", dir, "--name", "shop")
	run(t, e, buf, ExitUsage, "app", "restart", "shop") // off a terminal: --yes or nothing
	out := run(t, e, buf, ExitOK, "app", "rebuild", "shop", "--no-cache", "--yes")
	base := "docker compose -p shop --project-directory " + dir + " -f " + filepath.Join(dir, "compose.yaml")
	for _, s := range []string{"What it does", "How to undo", "Will run:", base + " build --no-cache web", base + " up -d web", "skipped: db"} {
		if !strings.Contains(out, s) {
			t.Errorf("rebuild lacks %q:\n%s", s, out)
		}
	}
	if len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" build --no-cache web")) != 1 {
		t.Errorf("build not run: %v", f.calls)
	}
	if !strings.Contains(out, "web") || !strings.Contains(out, "container Recreated") {
		t.Errorf("streamed output with the service prefix:\n%s", out)
	}
	out = run(t, e, buf, ExitOK, "app", "pull", "shop", "--yes")
	if !strings.Contains(out, base+" pull db") || !strings.Contains(out, "skipped: web") {
		t.Errorf("pull skips build services:\n%s", out)
	}
	if run(t, e, buf, ExitUsage, "app", "down", "shop", "--volumes", "--yes"); len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" down")) != 0 {
		t.Error("down --volumes ran without --force")
	}
	out = run(t, e, buf, ExitOK, "app", "down", "shop", "--volumes", "--yes", "--force")
	if !strings.Contains(out, "shop_data") || len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" down --volumes")) != 1 {
		t.Errorf("down -v lists the volumes and runs:\n%s", out)
	}
	run(t, e, buf, ExitUsage, "app", "restart", "shop", "nope", "--yes")
	if st := loadState(t, e); st.App("shop").Last == nil || st.App("shop").Last.Action != compose.Down {
		t.Errorf("last action: %+v", st.App("shop").Last)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "compose") && strings.Contains(c, " -p ") && !strings.Contains(c, "--project-directory") {
			t.Errorf("compose run without --project-directory: %s", c)
		}
	}
}

func TestAttachRouteDriftFixUnlink(t *testing.T) {
	e, buf, conf, dir, f := appWorld(t)
	run(t, e, buf, ExitOK, "app", "link", dir, "--name", "shop")
	run(t, e, buf, ExitOK, "instance", "adopt", "ctr:front", "--yes")

	// The service shares no network with the front: --connect attaches it
	// through the override and recreates only it (DOCK-16: the alias).
	out := run(t, e, buf, ExitOK, "route", "add", "shop.example.com", "--to", "app:shop/web:3000", "--connect", "--yes")
	ov := filepath.Join(e.paths.Overrides, "shop.yaml")
	body, err := os.ReadFile(ov)
	if err != nil || !strings.Contains(string(body), `aliases: ["shop-web"]`) || !strings.Contains(string(body), `"default": {}`) {
		t.Fatalf("override: %v\n%s\n%s", err, body, out)
	}
	if len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" -f "+ov+" up -d --no-deps web")) != 1 {
		t.Errorf("recreate with the override: %v", f.calls)
	}
	up, _ := os.ReadFile(filepath.Join(conf, "ngitool", "upstreams", "shop_example_com.conf"))
	if !strings.Contains(string(up), "shop-web:3000") {
		t.Errorf("upstream uses the alias:\n%s", up)
	}
	st := loadState(t, e)
	m := st.Pool("shop_example_com").Members[0]
	if m.Kind != model.KindService || m.App != "shop" || m.Ref != "shop/web" || m.Host != "shop-web" || m.Port != 3000 {
		t.Errorf("member: %+v", m)
	}

	// A plain docker compose up drops the override (DOCK-07).
	f.ps = "shop-web-1\tshop-web\trunning\tUp\tshop_default\t\tshop\tweb\t" + dir + "\t" + filepath.Join(dir, "compose.yaml") + "\n"
	out = run(t, e, buf, ExitOK, "app", "ls")
	if !strings.Contains(out, "detached") || !strings.Contains(out, "its routes return 502") || !strings.Contains(out, "ngitool app fix shop") {
		t.Errorf("ls must flag the detached app:\n%s", out)
	}
	if out := run(t, e, buf, ExitOK, "route", "ls"); !strings.Contains(out, "detached") {
		t.Errorf("route ls must flag it too:\n%s", out)
	}
	if r := checkApps(context.Background(), e); r.Status != ui.StatusFail || !strings.Contains(r.Fix, "app fix shop") {
		t.Errorf("doctor: %+v", r)
	}
	out = run(t, e, buf, ExitOK, "app", "fix", "--yes")
	if len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" -f "+ov+" up -d --force-recreate --no-deps web")) != 1 {
		t.Errorf("fix: %v\n%s", f.calls, out)
	}
	f.ps = "shop-web-1\tshop-web\trunning\tUp\tshop_default,app\t\tshop\tweb\t" + dir + "\t" + filepath.Join(dir, "compose.yaml") + "," + ov + "\n"
	if out := run(t, e, buf, ExitOK, "app", "fix"); !strings.Contains(out, "no app is detached") {
		t.Errorf("after fix:\n%s", out)
	}

	// The picker lists the app's services under Linked apps, not twice.
	rows := appRows(st, e.fixed, e.fixed.Find("ctr:front"))
	if len(rows) != 2 || rows[1].Value != "app:shop/web" || !strings.Contains(rows[1].Hint, "shop-web") {
		t.Errorf("linked app rows: %+v", rows)
	}
	for _, r := range serviceRows(st, e.fixed, e.fixed.Find("ctr:front")) {
		if strings.HasPrefix(r.Label, "shop/") {
			t.Errorf("a linked app's service listed again: %+v", r)
		}
	}

	// unlink: the cascade (route + its pool), then the override goes.
	out = run(t, e, buf, ExitOK, "app", "unlink", "shop", "--yes", "--keep-running")
	if !strings.Contains(out, "shop.example.com") || !strings.Contains(out, "Unlinked shop") {
		t.Errorf("unlink:\n%s", out)
	}
	st = loadState(t, e)
	if st.App("shop") != nil || st.Route("shop.example.com") != nil || st.Pool("shop_example_com") != nil {
		t.Errorf("after unlink: %+v %+v %+v", st.Apps, st.Routes, st.Pools)
	}
	if _, err := os.Stat(ov); !os.IsNotExist(err) {
		t.Errorf("override left behind: %v", err)
	}
}

func TestAttachRefusesNetworkModeHost(t *testing.T) {
	e, buf, _, dir, _ := appWorld(t)
	run(t, e, buf, ExitOK, "app", "link", dir, "--name", "shop")
	out := run(t, e, buf, ExitFail, "app", "attach", "shop", "tool", "--network", "app", "--yes")
	if !strings.Contains(out, "DOCK-06") || !strings.Contains(out, "host") {
		t.Errorf("network_mode host:\n%s", out)
	}
}

func TestAppsMenuGroup(t *testing.T) {
	e, buf, _, dir, _ := appWorld(t)
	run(t, e, buf, ExitOK, "app", "link", dir, "--name", "shop")
	items := groupItems(e, "apps")
	var labels []string
	for _, it := range items {
		labels = append(labels, it.Label)
	}
	if strings.Join(labels, ",") != "Link,Scan,shop,List,Fix detached" {
		t.Errorf("apps menu: %v", labels)
	}
	if strings.Join(items[2].Args, " ") != "app menu shop" {
		t.Errorf("app entry: %v", items[2].Args)
	}
	help := renderHelp(newRoot(e))
	for _, s := range []string{"APPS", "ngitool app link|scan|ls|show|up|rebuild|…"} {
		if !strings.Contains(help, s) {
			t.Errorf("help lacks %q", s)
		}
	}
}

func TestPrefixWriterCondensesAndPrefixes(t *testing.T) {
	var buf bytes.Buffer
	prev := ui.Out
	ui.Out = &buf
	t.Cleanup(func() { ui.Out = prev })
	ui.SetColor(false)
	w := newPrefixWriter("shop", false)
	_, _ = w.Write([]byte("#1 [web 1/2] FROM nginx\n#1 DONE 0.1s\n#2 sha256:abc 1MB\n Container shop-web-1  Started\nweb-1  | ready\npartial"))
	w.Flush()
	out := buf.String()
	if strings.Contains(out, "DONE") || strings.Contains(out, "sha256") || !strings.Contains(out, "web      │ FROM nginx") || !strings.Contains(out, "web      │ container Started") {
		t.Errorf("condensed:\n%s", out)
	}
	buf.Reset()
	v := newPrefixWriter("shop", true)
	_, _ = v.Write([]byte("#1 DONE 0.1s\n"))
	if !strings.Contains(buf.String(), "#1 DONE 0.1s") {
		t.Errorf("verbose keeps everything: %s", buf.String())
	}
}

func TestRunArgsResetsRepeatableFlags(t *testing.T) {
	e, buf, _, dir, _ := appWorld(t)
	root := newRoot(e)
	e.root = root
	ui.Out, ui.Errw = buf, buf
	for i := 0; i < 2; i++ { // the menu runs commands again and again in one process
		err := runArgs(root, []string{"app", "link", dir, "--file", "compose.yaml", "--name", "shop" + strconv.Itoa(i), "--no-serve"})
		if i == 0 && err != nil {
			t.Fatalf("%v\n%s", err, buf)
		}
	}
	a := loadState(t, e).App("shop0")
	if a == nil || len(a.Files) != 1 || strings.Contains(strings.Join(a.Files, ","), "[]") {
		t.Fatalf("--file after a reset: %+v", a)
	}
}

func TestExternalizeLinksCopiesAndMounts(t *testing.T) {
	e, buf, _, dir, f := appWorld(t)
	e.fixed.Instances = append(e.fixed.Instances, discover.Instance{ID: "compose:shop/proxy", Kind: discover.KindCompose, Name: "shop-proxy-1",
		Container: "shop-proxy-1", Project: "shop", Service: "proxy", State: discover.StateRunning, WorkingDir: dir,
		ComposeFile: []string{filepath.Join(dir, "compose.yaml")},
		Caps:        discover.Capabilities{Write: discover.Capability{Reason: "config is inside the image: no bind mount covers /etc/nginx/conf.d (CONF-06)"}}})
	out := run(t, e, buf, ExitOK, "instance", "externalize", "compose:shop/proxy", "--yes")
	dest := filepath.Join(e.paths.Externals, "shop")
	if len(f.ran("docker cp shop-proxy-1:/etc/nginx/. "+dest)) != 1 {
		t.Errorf("docker cp: %v\n%s", f.calls, out)
	}
	a := loadState(t, e).App("shop")
	if a == nil || len(a.Mounts["proxy"]) != 1 || a.Mounts["proxy"][0].Target != "/etc/nginx" || a.Mounts["proxy"][0].ReadOnly {
		t.Fatalf("linked with a writable mount: %+v", a)
	}
	body, _ := os.ReadFile(a.Override)
	if !strings.Contains(string(body), `source: "`+dest+`"`) || !strings.Contains(string(body), `target: "/etc/nginx"`) {
		t.Errorf("override:\n%s", body)
	}
	if len(f.ran("docker compose -p shop --project-directory "+dir+" -f "+filepath.Join(dir, "compose.yaml")+" -f "+a.Override+" up -d --no-deps proxy")) != 1 {
		t.Errorf("recreate: %v", f.calls)
	}
	if !strings.Contains(out, "ngitool instance adopt compose:shop/proxy") {
		t.Errorf("next step:\n%s", out)
	}
	// Not baked: refused with the reason.
	run(t, e, buf, ExitFail, "instance", "externalize", "ctr:front", "--yes")
}
