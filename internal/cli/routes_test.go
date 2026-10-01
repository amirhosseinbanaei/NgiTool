package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

type okDriver struct{ tests, reloads int }

func (d *okDriver) Dump(context.Context) (string, error) { return "", nil }
func (d *okDriver) Test(context.Context) (driver.Result, error) {
	d.tests++
	return driver.Result{OK: true}, nil
}
func (d *okDriver) Reload(context.Context) error { d.reloads++; return nil }
func (d *okDriver) Describe() driver.Methods     { return driver.Methods{} }

type okProbe struct{}

func (okProbe) Probe(_ context.Context, t apply.Target) apply.Probe {
	return apply.Probe{Route: t.Route, URL: "http://" + t.Host + t.Path, Status: 200, Reached: true}
}

// routeWorld is a container nginx whose conf.d is a temp dir, three app
// containers, and a fake driver: route commands run end to end on disk.
func routeWorld(t *testing.T) (*env, *bytes.Buffer, string, *okDriver) {
	t.Helper()
	e, buf := sandbox(t)
	conf := filepath.Join(t.TempDir(), "conf.d")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	e.fixed = &discover.Report{
		Containers: []discover.Container{
			{Name: "front", Running: true, Networks: []string{"app"}, Gateways: map[string]string{"app": "172.20.0.1"}},
			{Name: "web-1", Running: true, Networks: []string{"app"}, Exposed: []int{3000}},
			{Name: "web-2", Running: true, Networks: []string{"app"}, Exposed: []int{3000}},
			{Name: "lonely", Running: true, Networks: []string{"other"}, Exposed: []int{80}},
		},
		Instances: []discover.Instance{{ID: "ctr:front", Kind: discover.KindContainer, Name: "front", Container: "front", State: discover.StateRunning,
			Version: "nginx/1.30.5", Networks: []string{"app"}, Conf: "/etc/nginx/nginx.conf", FrontDoor: true,
			Caps:    discover.Capabilities{Write: discover.Capability{OK: true}, Reload: discover.Capability{OK: true}},
			Ports:   []discover.Published{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Proto: "tcp"}},
			Mounts:  []discover.Mount{{Type: "bind", Source: conf, Dest: "/etc/nginx/conf.d"}},
			Summary: &nginxconf.Summary{Hook: &nginxconf.Hook{Existing: "/etc/nginx/conf.d/*.conf", Dir: "/etc/nginx/conf.d"}}}},
		FrontDoor: "ctr:front",
	}
	drv := &okDriver{}
	prev := newDeps
	newDeps = func(e *env, in *discover.Instance) apply.Deps {
		return apply.Deps{Paths: e.paths, Driver: drv, Prober: okProbe{}, Ask: cliAsker{}}
	}
	t.Cleanup(func() { newDeps = prev })
	return e, buf, conf, drv
}

func run(t *testing.T, e *env, buf *bytes.Buffer, want int, args ...string) string {
	t.Helper()
	buf.Reset()
	_, code := execute(t, e, args...)
	if code != want {
		t.Fatalf("ngitool %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, buf)
	}
	return buf.String()
}

func TestRouteLifecycle(t *testing.T) {
	e, buf, conf, drv := routeWorld(t)

	out := run(t, e, buf, ExitFail, "route", "add", "app.example.com", "--to", "container:web-1:3000")
	if !strings.Contains(out, "not adopted") && !strings.Contains(out, "adopt") {
		t.Errorf("route add before adopt must say adopt first:\n%s", out)
	}

	run(t, e, buf, ExitOK, "instance", "adopt", "ctr:front", "--yes")
	if _, err := os.Stat(filepath.Join(conf, "ngitool.conf")); err != nil {
		t.Fatalf("adopt wrote no entry file: %v\n%s", err, buf)
	}

	out = run(t, e, buf, ExitOK, "route", "add", "app.example.com", "--to", "container:web-1:3000", "--to", "container:web-2:3000,weight=2", "--yes")
	if !strings.Contains(out, "Add route") || !strings.Contains(out, "least connections") || !strings.Contains(out, "added") {
		t.Errorf("plan, method default (least_conn) and diff summary:\n%s", out)
	}
	up, err := os.ReadFile(filepath.Join(conf, "ngitool/upstreams/app_example_com.conf"))
	if err != nil || !strings.Contains(string(up), "server web-2:3000 resolve weight=2;") || !strings.Contains(string(up), "least_conn;") {
		t.Fatalf("upstream file: %v\n%s", err, up)
	}

	out = run(t, e, buf, ExitOK, "route", "ls", "--json")
	var rs []routeJSON
	if err := json.Unmarshal([]byte(out), &rs); err != nil || len(rs) != 1 || len(rs[0].Members) != 2 || rs[0].Health == nil || rs[0].Health.Status != 200 {
		t.Fatalf("route ls --json: %v %s", err, out)
	}
	out = run(t, e, buf, ExitOK, "--no-color", "route", "ls")
	if !strings.Contains(out, "app.example.com") || !strings.Contains(out, "● /  → web-1:3000, web-2:3000 · least connections") {
		t.Errorf("route ls tree:\n%s", out)
	}

	out = run(t, e, buf, ExitOK, "pool", "drain", "app_example_com", "web-2:3000", "--yes")
	if !strings.Contains(out, "web-2:3000 down") {
		t.Errorf("drain shows the remaining members:\n%s", out)
	}
	up, _ = os.ReadFile(filepath.Join(conf, "ngitool/upstreams/app_example_com.conf"))
	if !strings.Contains(string(up), "server web-2:3000 resolve weight=2 down;") {
		t.Errorf("drained member not marked down:\n%s", up)
	}
	run(t, e, buf, ExitOK, "pool", "undrain", "app_example_com", "web-2:3000", "--yes")

	run(t, e, buf, ExitOK, "route", "disable", "app.example.com", "--yes")
	if _, err := os.Stat(filepath.Join(conf, "ngitool/servers/app.example.com.conf")); err == nil {
		t.Error("a disabled route keeps its server file (RP-22)")
	}
	st, _ := model.Load(e.paths)
	if r := st.Route("app.example.com"); r == nil || r.Enabled {
		t.Error("disable must keep the route in state, disabled")
	}
	run(t, e, buf, ExitOK, "route", "enable", "app.example.com", "--yes")

	out = run(t, e, buf, ExitOK, "route", "rm", "app.example.com", "--yes")
	if !strings.Contains(out, "no other route uses it") {
		t.Errorf("rm shows the cascade (LB-12):\n%s", out)
	}
	st, _ = model.Load(e.paths)
	if len(st.Routes) != 0 || len(st.Pools) != 0 {
		t.Errorf("route rm left %d routes, %d pools", len(st.Routes), len(st.Pools))
	}
	if drv.tests == 0 || drv.reloads == 0 {
		t.Error("no transaction ran")
	}
	snaps, _ := apply.Snapshots(e.paths.Backups, "ctr:front")
	if len(snaps) < 5 {
		t.Errorf("every apply snapshots: %d", len(snaps))
	}
}

func TestRouteErrorsNameTheirEdgeCase(t *testing.T) {
	e, buf, _, _ := routeWorld(t)
	run(t, e, buf, ExitOK, "instance", "adopt", "ctr:front", "--yes")

	out := run(t, e, buf, ExitFail, "route", "add", "a.example.com", "--to", "container:web-1:3000", "--to", "container:web-2:3000,backup", "--method", "ip_hash", "--yes")
	if !strings.Contains(out, "(LB-02)") {
		t.Errorf("backup with ip_hash: %s", out)
	}
	out = run(t, e, buf, ExitFail, "route", "add", "a.example.com", "--to", "container:web-1:3000", "--method", "sticky", "--yes")
	if !strings.Contains(out, "(LB-15)") {
		t.Errorf("sticky: %s", out)
	}
	out = run(t, e, buf, ExitFail, "route", "add", "a.example.com", "--to", "container:web-1:3000,slow_start=30s", "--yes")
	if !strings.Contains(out, "(LB-15)") {
		t.Errorf("slow_start: %s", out)
	}
	out = run(t, e, buf, ExitFail, "route", "add", "a.example.com", "--to", "http://10.0.0.1:80", "--to", "https://10.0.0.2", "--yes")
	if !strings.Contains(out, "(LB-16)") {
		t.Errorf("mixed schemes: %s", out)
	}
	out = run(t, e, buf, ExitUsage, "route", "add", "a.example.com", "--yes")
	if !strings.Contains(out, "--to") {
		t.Errorf("missing targets off a terminal names --to: %s", out)
	}
	out = run(t, e, buf, ExitOK, "route", "add", "b.example.com", "--to", "container:lonely:80", "--yes", "--dry-run")
	if !strings.Contains(out, "(RP-05)") || !strings.Contains(out, "docker network connect app lonely") {
		t.Errorf("container on no shared network: %s", out)
	}
	if !strings.Contains(out, "--dry-run: nothing was written") {
		t.Errorf("dry-run: %s", out)
	}
	run(t, e, buf, ExitOK, "route", "add", "c.example.com/api", "--to", "container:web-1:3000", "--strip", "--yes")
	out = run(t, e, buf, ExitFail, "route", "add", "c.example.com/api", "--to", "container:web-2:3000", "--yes")
	if !strings.Contains(out, "(RP-03)") {
		t.Errorf("duplicate route: %s", out)
	}
}

func TestPoolSwitchBlueGreen(t *testing.T) {
	e, buf, conf, _ := routeWorld(t)
	run(t, e, buf, ExitOK, "instance", "adopt", "ctr:front", "--yes")
	run(t, e, buf, ExitOK, "route", "add", "bg.example.com", "--to", "container:web-1:3000", "--yes")
	run(t, e, buf, ExitOK, "pool", "switch", "bg_example_com", "--to", "container:web-2:3000", "--yes")
	up, _ := os.ReadFile(filepath.Join(conf, "ngitool/upstreams/bg_example_com.conf"))
	if !strings.Contains(string(up), "server web-2:3000 resolve;") || !strings.Contains(string(up), "server web-1:3000 resolve backup;") {
		t.Fatalf("switch: the new member is active, the old one a backup (LB-09):\n%s", up)
	}
	run(t, e, buf, ExitOK, "pool", "switch", "bg_example_com", "--revert", "--yes")
	st, _ := model.Load(e.paths)
	if p := st.Pool("bg_example_com"); len(p.Members) != 1 || p.Members[0].Ref != "web-1" || len(p.Previous) != 0 {
		t.Fatalf("revert: %+v", p)
	}
	run(t, e, buf, ExitOK, "pool", "switch", "bg_example_com", "--to", "container:web-2:3000", "--yes")
	run(t, e, buf, ExitOK, "pool", "switch", "bg_example_com", "--confirm", "--yes")
	st, _ = model.Load(e.paths)
	if p := st.Pool("bg_example_com"); len(p.Members) != 1 || p.Members[0].Ref != "web-2" {
		t.Fatalf("confirm drops the old set: %+v", p)
	}
}
