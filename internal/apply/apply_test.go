package apply

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

type fakeDriver struct {
	tests   []driver.Result // one per Test call; the last repeats
	reloads []error
	nTest   int
	nReload int
}

func (f *fakeDriver) Dump(context.Context) (string, error) { return "", nil }
func (f *fakeDriver) Test(context.Context) (driver.Result, error) {
	r := f.tests[min(f.nTest, len(f.tests)-1)]
	f.nTest++
	return r, nil
}
func (f *fakeDriver) Reload(context.Context) error {
	var err error
	if f.nReload < len(f.reloads) {
		err = f.reloads[f.nReload]
	}
	f.nReload++
	return err
}
func (f *fakeDriver) Describe() driver.Methods { return driver.Methods{} }

type fakeAsk struct {
	confirm bool
	drift   string
	asked   []string
}

func (a *fakeAsk) Confirm(q, _ string) (bool, error) {
	a.asked = append(a.asked, q)
	return a.confirm, nil
}
func (a *fakeAsk) Drift(files []string, canAdopt bool) (string, error) {
	a.asked = append(a.asked, "drift "+strings.Join(files, ","))
	return a.drift, nil
}

type fakeProbe struct{ status int }

func (f fakeProbe) Probe(_ context.Context, t Target) Probe {
	return Probe{Route: t.Route, URL: "http://" + t.Host + t.Path, Status: f.status, Reached: f.status < 502 || f.status > 504}
}

type fixture struct {
	t     *testing.T
	conf  string // host side of /etc/nginx/conf.d
	p     paths.Paths
	rep   *discover.Report
	in    *discover.Instance
	st    *model.State
	drv   *fakeDriver
	ask   *fakeAsk
	out   *bytes.Buffer
	clock time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(paths.EnvRoot, filepath.Join(dir, "root"))
	f := &fixture{t: t, conf: filepath.Join(dir, "conf.d"), p: paths.Get(), drv: &fakeDriver{tests: []driver.Result{{OK: true}}}, ask: &fakeAsk{confirm: true},
		out: &bytes.Buffer{}, clock: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	if err := os.MkdirAll(f.conf, 0o755); err != nil {
		t.Fatal(err)
	}
	prevOut, prevErr := ui.Out, ui.Errw
	ui.Out, ui.Errw = f.out, f.out
	t.Cleanup(func() { ui.Out, ui.Errw = prevOut, prevErr })
	f.rep = &discover.Report{Containers: []discover.Container{
		{Name: "front", Running: true, Networks: []string{"app"}},
		{Name: "web-1", Running: true, Networks: []string{"app"}},
		{Name: "web-2", Running: true, Networks: []string{"app"}},
	}}
	f.rep.Instances = []discover.Instance{{ID: "ctr:front", Kind: discover.KindContainer, Name: "front", Container: "front", State: discover.StateRunning,
		Version: "nginx/1.30.5", Networks: []string{"app"}, Conf: "/etc/nginx/nginx.conf",
		Ports:   []discover.Published{{HostIP: "127.0.0.1", HostPort: 18080, ContainerPort: 80, Proto: "tcp"}},
		Mounts:  []discover.Mount{{Type: "bind", Source: f.conf, Dest: "/etc/nginx/conf.d"}},
		Summary: &nginxconf.Summary{Hook: &nginxconf.Hook{Existing: "/etc/nginx/conf.d/*.conf", Dir: "/etc/nginx/conf.d"}}}}
	f.in = &f.rep.Instances[0]
	a, err := render.Plan(f.in)
	if err != nil {
		t.Fatal(err)
	}
	f.st = &model.State{Instances: []model.Adopted{a}}
	return f
}

func (f *fixture) deps() Deps {
	return Deps{Paths: f.p, Driver: f.drv, Ask: f.ask, Prober: fakeProbe{200}, Now: func() time.Time { f.clock = f.clock.Add(time.Second); return f.clock }}
}

// next is the current state plus one route to pool members.
func (f *fixture) next(host string, members ...string) *model.State {
	n := f.st.Clone()
	p := model.Pool{Name: n.PoolName(host, ""), Instance: f.in.ID, Method: model.LeastConn, Scheme: "http"}
	for _, s := range members {
		m, _, err := model.ParseMember(s)
		if err != nil {
			f.t.Fatal(err)
		}
		p.Members = append(p.Members, m)
	}
	n.Pools = append(n.Pools, p)
	n.Routes = append(n.Routes, model.Route{ID: host, Instance: f.in.ID, Host: host, Pool: p.Name, Enabled: true, HTTP: model.HTTPServe, Options: model.DefaultOptions()})
	return n
}

func (f *fixture) run(next *model.State, o Opts) (*Result, error) {
	c := Change{Report: f.rep, Instance: f.in, Before: f.st, Next: next, Summary: "test", Probe: []string{next.Routes[len(next.Routes)-1].ID}}
	res, err := Run(context.Background(), f.deps(), c, o)
	if err == nil && !o.DryRun {
		f.st = next
	}
	return res, err
}

func (f *fixture) files() map[string]string {
	out := map[string]string{}
	_ = filepath.Walk(f.conf, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(f.conf, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func TestApplyWritesTestsReloadsAndSaves(t *testing.T) {
	f := newFixture(t)
	res, err := f.run(f.next("app.example.com", "container:web-1:3000", "container:web-2:3000"), Opts{Yes: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	files := f.files()
	for _, want := range []string{"ngitool.conf", "ngitool/proxy.conf", "ngitool/upstreams/app_example_com.conf", "ngitool/servers/app.example.com.conf"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s; have %v", want, keys(files))
		}
	}
	if res.Added != 4 || res.Changed != 0 || !res.Reloaded || f.drv.nTest != 1 || f.drv.nReload != 1 {
		t.Errorf("result %+v tests %d reloads %d", res, f.drv.nTest, f.drv.nReload)
	}
	if !strings.Contains(f.out.String(), "0 files changed, 4 added, 0 removed") {
		t.Errorf("summary line missing:\n%s", f.out)
	}
	st, err := model.Load(f.p)
	if err != nil || len(st.Routes) != 1 {
		t.Fatalf("state not saved: %v %+v", err, st)
	}
	if len(res.Probes) != 1 || res.Probes[0].Status != 200 {
		t.Errorf("probe: %+v", res.Probes)
	}
}

func TestTestFailureRestoresTheSnapshot(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	before := f.files()
	f.drv.tests = []driver.Result{{OK: false, Output: `nginx: [emerg] unknown directive "bogus" in /etc/nginx/conf.d/ngitool/servers/b.example.com.conf:9`,
		File: "/etc/nginx/conf.d/ngitool/servers/b.example.com.conf", Line: 9}}
	reloads := f.drv.nReload
	_, err := f.run(f.next("b.example.com", "container:web-2:3000"), Opts{Yes: true})
	var te *TestError
	if !errors.As(err, &te) {
		t.Fatalf("want TestError, got %v", err)
	}
	if after := f.files(); !equal(before, after) {
		t.Errorf("files not restored:\nbefore %v\nafter  %v", keys(before), keys(after))
	}
	if f.drv.nReload != reloads {
		t.Error("nothing may reload after a failed test (APPLY-01)")
	}
	st, _ := model.Load(f.p)
	if len(st.Routes) != 1 {
		t.Errorf("state must not be saved after a failed test: %d routes", len(st.Routes))
	}
	if !strings.Contains(f.out.String(), "servers/b.example.com.conf:9") || !strings.Contains(f.out.String(), ui.SymPointer) {
		t.Errorf("the offending file:line is not highlighted:\n%s", f.out)
	}
}

func TestReloadFailureRestoresAndReloadsAgain(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	before := f.files()
	f.drv.reloads = []error{nil, errors.New("bind() to 0.0.0.0:443 failed (98: Address in use)"), nil}
	_, err := f.run(f.next("b.example.com", "container:web-2:3000"), Opts{Yes: true})
	var re *ReloadError
	if !errors.As(err, &re) || !strings.Contains(re.First, "bind()") || re.Second != "" {
		t.Fatalf("want ReloadError with both outputs, got %v", err)
	}
	if f.drv.nReload != 3 {
		t.Errorf("want a second reload with the previous config, got %d reloads", f.drv.nReload)
	}
	if !equal(before, f.files()) {
		t.Error("files not restored after a failed reload (APPLY-03)")
	}
}

func TestBindFailureInTheLogIsAReloadFailure(t *testing.T) {
	f := newFixture(t)
	d := f.deps()
	d.Logs = func(context.Context, time.Time) string {
		return "2026/10/01 [emerg] 1#1: bind() to 0.0.0.0:8443 failed (98: Address in use)"
	}
	n := f.next("app.example.com", "container:web-1:3000")
	_, err := Run(context.Background(), d, Change{Report: f.rep, Instance: f.in, Before: f.st, Next: n}, Opts{Yes: true})
	var re *ReloadError
	if !errors.As(err, &re) || !strings.Contains(re.First, "bind()") {
		t.Fatalf("APPLY-10: want ReloadError, got %v", err)
	}
	if len(f.files()) != 0 {
		t.Errorf("first apply must leave nothing behind: %v", keys(f.files()))
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	res, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.files()) != 0 || f.drv.nTest != 0 || res.Added != 4 {
		t.Errorf("dry-run wrote or tested: files %v tests %d res %+v", keys(f.files()), f.drv.nTest, res)
	}
	if _, err := os.Stat(f.p.State); err == nil {
		t.Error("dry-run saved state")
	}
	if !strings.Contains(f.out.String(), "+upstream ngt_app_example_com {") {
		t.Errorf("dry-run must print the diff (APPLY-08):\n%s", f.out)
	}
}

func TestDriftIsShownAndAsked(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	srv := filepath.Join(f.conf, "ngitool/servers/app.example.com.conf")
	b, _ := os.ReadFile(srv)
	edited := strings.Replace(string(b), "proxy_send_timeout 120s;", "proxy_send_timeout 120s;\n        add_header X-Hand edited;", 1)
	if err := os.WriteFile(srv, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	f.ask.drift = DriftAbort
	_, err := f.run(f.next("b.example.com", "container:web-2:3000"), Opts{Yes: true})
	if !errors.Is(err, ErrAborted) || !strings.Contains(f.out.String(), "+        add_header X-Hand edited;") {
		t.Fatalf("abort: %v\n%s", err, f.out)
	}

	f.ask.drift = DriftAdopt
	if _, err := f.run(f.next("b.example.com", "container:web-2:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	r := f.st.Route("app.example.com")
	if len(r.Extra) != 1 || r.Extra[0] != "add_header X-Hand edited;" {
		t.Fatalf("adopt must keep the edit as an extra directive: %q", r.Extra)
	}
	b, _ = os.ReadFile(srv)
	if p := render.Parse(string(b)); p.Edited || !strings.Contains(p.Body, "add_header X-Hand edited;") {
		t.Errorf("adopted file must be re-stamped with the edit:\n%s", b)
	}

	if err := os.WriteFile(srv, []byte(strings.Replace(string(b), "120s", "99s", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.ask.drift = DriftOverwrite
	if _, err := f.run(f.st.Clone(), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(srv)
	if render.Parse(string(b)).Edited {
		t.Error("overwrite must put NgiTool's version back")
	}
}

func TestHandWrittenFileAtATargetIsRefused(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.conf, "ngitool/servers"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(f.conf, "ngitool/servers/app.example.com.conf")
	if err := os.WriteFile(mine, []byte("server { listen 80; }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true})
	if err == nil || !strings.Contains(err.Error(), "CONF-09") {
		t.Fatalf("want CONF-09, got %v", err)
	}
	if b, _ := os.ReadFile(mine); string(b) != "server { listen 80; }\n" {
		t.Error("a hand-written file was touched")
	}
}

func TestSnapshotRotation(t *testing.T) {
	f := newFixture(t)
	d := f.deps()
	d.Retain = 3
	for i := 0; i < 5; i++ {
		n := f.next("h"+string(rune('a'+i))+".example.com", "container:web-1:3000")
		if _, err := Run(context.Background(), d, Change{Report: f.rep, Instance: f.in, Before: f.st, Next: n}, Opts{Yes: true}); err != nil {
			t.Fatal(err)
		}
		f.st = n
	}
	snaps, err := Snapshots(f.p.Backups, f.in.ID)
	if err != nil || len(snaps) != 3 {
		t.Fatalf("want 3 snapshots kept, got %d (%v)", len(snaps), err)
	}
	if !strings.Contains(snaps[0].Changes, "added") || !snaps[0].State {
		t.Errorf("manifest: %+v", snaps[0])
	}
}

func TestRollbackToASnapshot(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run(f.next("a.example.com", "container:web-1:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	one := f.files()
	if _, err := f.run(f.next("b.example.com", "container:web-2:3000"), Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := Snapshots(f.p.Backups, f.in.ID)
	// snaps[0] was taken before b was added: it holds the "one route" files.
	old, err := SnapshotState(snaps[0])
	if err != nil {
		t.Fatal(err)
	}
	c := Change{Report: f.rep, Instance: f.in, Before: f.st, Next: old, Snapshot: snaps[0], Summary: "rollback"}
	if _, err := Run(context.Background(), f.deps(), c, Opts{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !equal(one, f.files()) {
		t.Errorf("rollback did not restore the files:\nwant %v\ngot  %v", keys(one), keys(f.files()))
	}
	st, _ := model.Load(f.p)
	if len(st.Routes) != 1 {
		t.Errorf("rollback must restore state too: %d routes", len(st.Routes))
	}
}

func TestFailedWriteChangesNothing(t *testing.T) {
	f := newFixture(t)
	// A file where NgiTool needs a directory: staging fails before any
	// live file changes (APPLY-07, APPLY-09).
	if err := os.WriteFile(filepath.Join(f.conf, "ngitool"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true})
	var we *WriteError
	if !errors.As(err, &we) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("want WriteError, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.conf, "ngitool.conf")); err == nil {
		t.Error("the entry file was written although staging failed")
	}
	for _, c := range []struct {
		err  error
		want string
	}{{syscall.ENOSPC, "the disk is full"}, {syscall.EROFS, "the filesystem is read-only"}} {
		if msg := (&WriteError{Path: "/x", Err: c.err}).Error(); !strings.Contains(msg, c.want) {
			t.Errorf("%v → %q", c.err, msg)
		}
	}
}

func TestStoppedInstanceTestsButDoesNotReload(t *testing.T) {
	f := newFixture(t)
	f.in.State = discover.StateStopped
	res, err := f.run(f.next("app.example.com", "container:web-1:3000"), Opts{Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.drv.nTest != 1 || f.drv.nReload != 0 || res.Reloaded || len(res.Probes) != 0 {
		t.Errorf("APPLY-02: tests %d reloads %d probes %d", f.drv.nTest, f.drv.nReload, len(res.Probes))
	}
	if !strings.Contains(f.out.String(), "docker start front") {
		t.Errorf("missing start hint:\n%s", f.out)
	}
}

func TestCausesAndCloudflareHints(t *testing.T) {
	c := Causes(502, []string{model.KindContainer, model.KindHostPort, model.KindContainer})
	if len(c) != 2 || !strings.Contains(c[0], "RP-05") || !strings.Contains(c[1], "RP-06") {
		t.Errorf("causes: %q", c)
	}
	for _, code := range []int{520, 522, 525, 526} {
		if CloudflareHints[code] == "" {
			t.Errorf("RP-21: no hint for %d", code)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func equal(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
