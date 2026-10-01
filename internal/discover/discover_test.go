package discover

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

var ctx = context.Background()

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeProc builds a /proc tree: pid → (cmdline, ppid, uid, mount ns, cgroup).
type fp struct {
	pid, ppid, uid int
	cmd, ns, cg    string
	exe            string
}

func fakeProc(t *testing.T, dir string, ps []fp) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "self", "ns"), 0o700)
	if err := os.Symlink("mnt:[4026531841]", filepath.Join(dir, "self", "ns", "mnt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		d := filepath.Join(dir, itoa(p.pid))
		write(t, filepath.Join(d, "cmdline"), strings.ReplaceAll(p.cmd, " ", "\x00")+"\x00")
		write(t, filepath.Join(d, "stat"), itoa(p.pid)+" (x y) S "+itoa(p.ppid)+" 1 1")
		write(t, filepath.Join(d, "status"), "Name:\tx\nUid:\t"+itoa(p.uid)+"\t"+itoa(p.uid)+"\n")
		ns := p.ns
		if ns == "" {
			ns = "mnt:[4026531841]"
		}
		os.MkdirAll(filepath.Join(d, "ns"), 0o700)
		os.Symlink(ns, filepath.Join(d, "ns", "mnt"))
		write(t, filepath.Join(d, "cgroup"), firstNonEmpty(p.cg, "0::/user.slice")+"\n")
		if p.exe != "" {
			os.Symlink(p.exe, filepath.Join(d, "exe"))
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func lookPath(have ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, h := range have {
			if h == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
}

func baseEnv(t *testing.T, run execx.Runner, have ...string) Env {
	return Env{
		Run:      run,
		Proc:     filepath.Join(t.TempDir(), "proc"),
		Euid:     0,
		LookPath: lookPath(have...),
		Getenv:   func(string) string { return "" },
		Now:      func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		UserName: func(uid int) string { return map[int]string{0: "root", 1000: "alice"}[uid] },
		Access:   func(string, uint32) error { return nil },
	}
}

// dockerFixture is the recorded (sanitised) docker output of a server with
// an edge stack, plus hand-written containers for the cases it lacks.
func dockerFixture(t *testing.T) (Env, string) {
	t.Helper()
	tmp := t.TempDir()
	edge := filepath.Join(tmp, "edge")
	write(t, filepath.Join(edge, "compose.yaml"), "name: edge\n")
	write(t, filepath.Join(edge, "conf", "nginx.conf"), "events {}\n")
	write(t, filepath.Join(edge, "edge"), "#!/bin/sh\n")
	write(t, filepath.Join(tmp, "static", "conf.d", "site.conf"), "server {\n    listen 80;\n    server_name static.example.com;\n    root /usr/share/nginx/html;\n}\n")
	write(t, filepath.Join(tmp, "single", "nginx.conf"), "events {}\nhttp {\n    server {\n        listen 80;\n        server_name single.example.com;\n    }\n}\n")
	write(t, filepath.Join(tmp, "single", "templates", "x.conf.template"), "server { server_name ${HOST}; }\n")
	write(t, filepath.Join(tmp, "old", "conf.d", "default.conf"), "server {\n    listen 80;\n    server_name old.example.com;\n    location / { proxy_pass http://gone:3000; }\n}\n")
	os.MkdirAll(filepath.Join(tmp, "hostnet"), 0o700)
	sub := func(s string) string {
		return strings.NewReplacer("@EDGE@", edge, "@TMP@", tmp).Replace(s)
	}
	ps := sub(read(t, "testdata/docker-ps.txt") + read(t, "testdata/extra-ps.txt"))
	var a, b []json.RawMessage
	json.Unmarshal([]byte(sub(read(t, "testdata/docker-inspect.json"))), &a)
	json.Unmarshal([]byte(sub(read(t, "testdata/extra-inspect.json"))), &b)
	inspect, _ := json.Marshal(append(a, b...))
	ss := read(t, "testdata/ss.txt") + "LISTEN 0 511 0.0.0.0:8081 0.0.0.0:* users:((\"nginx\",pid=2301,fd=6))\n"
	tops := map[string]string{
		"edge-nginx-1": "top-edge.txt", "App1": "top-panel.txt", "App4-Site": "top-site.txt",
		"proxy-manager": "top-proxy-manager.txt", "hostnet": "top-hostnet.txt",
	}
	dumps := map[string]string{
		"docker exec edge-nginx-1 nginx -c /etc/nginx/edge/nginx.conf -T": "../nginxconf/testdata/edge-dump.txt",
		"docker exec App1 nginx -T":                                       "testdata/dump-app1.txt",
		"docker exec hostnet nginx -T":                                    "testdata/dump-hostnet.txt",
		"docker exec proxy-manager /usr/sbin/nginx -T":                    "testdata/dump-npm.txt",
	}
	run := &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
		k := execx.Key(name, args...)
		switch {
		case k == "docker --version":
			return execx.Result{Stdout: "Docker version 29.8.1, build abc\n"}, true
		case strings.HasPrefix(k, "docker info"):
			return execx.Result{Stdout: "29.8.1|[\"name=seccomp,profile=builtin\"]\n"}, true
		case strings.HasPrefix(k, "docker ps -a"):
			return execx.Result{Stdout: ps}, true
		case strings.HasPrefix(k, "docker inspect"):
			return execx.Result{Stdout: string(inspect)}, true
		case strings.HasPrefix(k, "docker top "):
			if f, ok := tops[args[1]]; ok {
				return execx.Result{Stdout: read(t, "testdata/"+f)}, true
			}
			return execx.Result{Stdout: "PID PPID COMMAND\n9 8 node server.js\n"}, true
		case strings.HasSuffix(k, " -v") && strings.HasPrefix(k, "docker exec"):
			v := map[string]string{"edge-nginx-1": "nginx/1.30.5", "App1": "nginx/1.29.8", "hostnet": "nginx/1.29.1", "proxy-manager": "openresty/1.27.1.2"}[args[1]]
			return execx.Result{Stderr: "nginx version: " + v + "\n"}, true
		case dumps[k] != "":
			return execx.Result{Stdout: read(t, dumps[k]), Stderr: "nginx: configuration file test is successful\n"}, true
		case k == "ss -ltnpH":
			return execx.Result{Stdout: ss}, true
		}
		return execx.Result{}, false
	}}
	env := baseEnv(t, run, "docker", "ss")
	fakeProc(t, env.Proc, []fp{{pid: 2300, ppid: 2299, cmd: "nginx: master process nginx -g daemon off;", ns: "mnt:[4026532999]"}, {pid: 2301, ppid: 2300, cmd: "nginx: worker process", ns: "mnt:[4026532999]"}})
	return env, tmp
}

func byID(t *testing.T, r *Report, id string) *Instance {
	t.Helper()
	in := r.Find(id)
	if in == nil {
		var ids []string
		for _, i := range r.Instances {
			ids = append(ids, i.ID)
		}
		t.Fatalf("no instance %s in %v", id, ids)
	}
	return in
}

func findings(r *Report, code string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func TestDockerDiscovery(t *testing.T) {
	env, tmp := dockerFixture(t)
	r := Scan(ctx, env)
	edgeID := "edge:" + filepath.Join(tmp, "edge")

	// The edge stack: recognised, front door, -c from the master, write via
	// its :ro bind mount.
	edge := byID(t, r, edgeID)
	if edge.Kind != KindEdge || !edge.FrontDoor || r.FrontDoor != edgeID || r.Instances[0].ID != edgeID {
		t.Fatalf("edge: kind %s front %v report front %s", edge.Kind, edge.FrontDoor, r.FrontDoor)
	}
	if edge.Version != "nginx/1.30.5" || edge.Conf != "/etc/nginx/edge/nginx.conf" || edge.Source != "dump" || edge.Valid == nil || !*edge.Valid {
		t.Errorf("edge config: %s %s %s %v", edge.Version, edge.Conf, edge.Source, edge.Valid)
	}
	if s, _ := edge.Counts(); s != 13 {
		t.Errorf("edge servers %d", s)
	}
	w := edge.Caps.Write
	if !w.OK || !strings.Contains(w.Note, ":ro only stops the container") || !strings.Contains(w.Note, filepath.Join(tmp, "edge", "conf", "conf.d")) {
		t.Errorf("edge write %+v", w)
	}
	if edge.Methods.Test != "docker exec edge-nginx-1 nginx -c /etc/nginx/edge/nginx.conf -t" || edge.ManagedBy != ManagedEdge {
		t.Errorf("edge methods %+v managed %s", edge.Methods, edge.ManagedBy)
	}
	if edge.Summary.Hook == nil || edge.Summary.Hook.Existing != "/etc/nginx/edge/conf.d/*.conf" {
		t.Errorf("edge hook %+v", edge.Summary.Hook)
	}
	for pos, rc := range edge.Reach {
		if rc.Status != ReachOK {
			t.Errorf("edge target %s: %+v", pos, rc)
		}
	}

	// A custom image running nginx (DISC-10), config inside the image (CONF-06).
	app := byID(t, r, "compose:app1/panel")
	if app.Version != "nginx/1.29.8" || app.Caps.Write.OK || !strings.Contains(app.Caps.Write.Reason, "CONF-06") || !app.Caps.Read.OK {
		t.Errorf("app1: %s write %+v read %+v", app.Version, app.Caps.Write, app.Caps.Read)
	}

	// Plain docker run, stopped, conf.d mounted: read from files with the
	// image's stock main (DISC-07, CONF-02), writable via the mount.
	st := byID(t, r, "ctr:static-site")
	if st.Kind != KindContainer || st.State != StateStopped || st.Source != "files" || !st.StockMain || !st.Caps.Write.OK || st.Caps.Reload.OK {
		t.Errorf("static-site %+v", st)
	}
	if !strings.Contains(st.Caps.Test.Note, "throwaway container") || !strings.Contains(st.Methods.Test, "docker run --rm --network none --pull never") {
		t.Errorf("static-site test %+v %s", st.Caps.Test, st.Methods.Test)
	}
	if n, _ := st.Counts(); n != 1 {
		t.Errorf("static-site servers %d", n)
	}

	// Managed by another tool (DISC-11): read-only.
	npm := byID(t, r, "compose:npm/app")
	if npm.ManagedBy != "other:nginx-proxy-manager" || npm.Caps.Write.OK || npm.Caps.Reload.OK || !npm.Caps.Read.OK || npm.Variant != "openresty" {
		t.Errorf("npm %+v", npm)
	}

	// Host network mode (DISC-12): owns :8081 through its worker's pid.
	hn := byID(t, r, "ctr:hostnet")
	if hn.NetworkMode != "host" || !strings.Contains(strings.Join(hn.Notes, " "), "DISC-12") {
		t.Errorf("hostnet %+v", hn)
	}
	owned := false
	for _, o := range r.Ports {
		owned = owned || (o.Port == 8081 && o.Instance == "ctr:hostnet")
	}
	if !owned {
		t.Errorf("8081 owner: %+v", r.Ports)
	}

	// Single-file mount + templates (CONF-07, CONF-08).
	sf := byID(t, r, "ctr:single-file")
	if !sf.Templates || !sf.Caps.Write.OK || !strings.Contains(sf.Caps.Write.Note, "CONF-07") || !strings.Contains(sf.Caps.Write.Note, "CONF-08") {
		t.Errorf("single-file %+v", sf.Caps.Write)
	}

	// Stopped, config baked into the image (CONF-06), found by its command.
	bk := byID(t, r, "ctr:baked")
	if bk.Caps.Read.OK || !strings.Contains(bk.Caps.Read.Reason, "inside the image") {
		t.Errorf("baked read %+v", bk.Caps.Read)
	}

	// An exited compose service (DISC-08), read through its mounts.
	old := byID(t, r, "compose:old/web")
	if old.State != StateStopped || !old.Caps.Read.OK || old.Reach[firstTarget(old)].Status != ReachErr {
		t.Errorf("old %+v reach %v", old, old.Reach)
	}

	// Findings.
	var dock11 []string
	for _, f := range findings(r, "DOCK-11") {
		dock11 = append(dock11, f.Message)
	}
	all := strings.Join(dock11, "\n")
	if len(dock11) != 3 || !strings.Contains(all, "App1 publishes 0.0.0.0:8080") || !strings.Contains(all, "proj6-postgres-1") ||
		strings.Contains(all, "edge-nginx-1") || strings.Contains(all, "App2") {
		t.Errorf("DOCK-11:\n%s", all)
	}
	if f := findings(r, "DOCK-15"); len(f) == 0 || !strings.Contains(f[0].Message, "edge-nginx-1 → App1") || !strings.Contains(f[0].Message, "terminates TLS") {
		t.Errorf("DOCK-15 %+v", f)
	}
	if f := findings(r, "RP-07"); len(f) != 2 || !strings.Contains(f[0].Message, "api-backend:9000") || !strings.Contains(f[1].Message, "http://gone:3000") {
		t.Errorf("RP-07 %+v", f)
	}
	if f := findings(r, "RP-04"); len(f) != 1 || !strings.Contains(f[0].Message, "shop.example.com") {
		t.Errorf("RP-04 %+v", f)
	}
	if f := findings(r, "RP-05"); len(f) != 1 || !strings.Contains(f[0].Message, "gone") {
		t.Errorf("RP-05 %+v", f)
	}
	if f := findings(r, "DISC-11"); len(f) != 1 {
		t.Errorf("DISC-11 %+v", f)
	}
	if r.Host == "" || !strings.HasPrefix(r.Host, "no host nginx") {
		t.Errorf("host line %q", r.Host)
	}
	// The report holds no secrets and no dump.
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "configuration file") {
		t.Error("a dump leaked into the report")
	}
}

func firstTarget(in *Instance) string {
	for _, s := range in.Summary.Servers {
		for _, l := range s.Locations {
			if l.Target != nil {
				return l.Target.Pos.String()
			}
		}
	}
	return ""
}

func TestHostDiscovery(t *testing.T) {
	tmp := t.TempDir()
	conf2 := filepath.Join(tmp, "n2", "nginx.conf")
	write(t, conf2, "events {}\nhttp {\n    include "+filepath.Join(tmp, "n2", "conf.d")+"/*.conf;\n}\n")
	write(t, filepath.Join(tmp, "n2", "conf.d", "a.conf"), "server {\n    listen 8080;\n    server_name second.example.com;\n}\n")
	run := &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
		k := execx.Key(name, args...)
		switch k {
		case "/opt/nginx/sbin/nginx -V":
			return execx.Result{Stderr: "nginx version: nginx/1.26.2\nconfigure arguments: --prefix=/opt/nginx --conf-path=/opt/nginx/conf/nginx.conf\n"}, true
		case "/opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -T":
			return execx.Result{Stdout: "# configuration file /opt/nginx/conf/nginx.conf:\nevents {}\nhttp {\n    server {\n        listen 80;\n        server_name first.example.com;\n    }\n}\n\n"}, true
		case "/usr/sbin/nginx -V", "/usr/local/openresty/bin/openresty -V":
			v := "nginx/1.24.0 (Ubuntu)"
			if strings.Contains(name, "openresty") {
				v = "openresty/1.25.3.1"
			}
			return execx.Result{Stderr: "nginx version: " + v + "\nconfigure arguments: --prefix=/usr/share/nginx --conf-path=/etc/nginx/nginx.conf\n"}, true
		case "/usr/sbin/nginx -c " + conf2 + " -g daemon on; master_process on; -T":
			return execx.Result{Code: 1, Stderr: "nginx: [alert] could not open error log file: permission denied"}, true
		case "/usr/sbin/angie -V":
			return execx.Result{Stderr: "Angie version: Angie/1.6.0\nconfigure arguments: --conf-path=/etc/angie/angie.conf\n"}, true
		case "/usr/sbin/angie -T":
			return execx.Result{Stdout: "# configuration file /etc/angie/angie.conf:\nevents {}\n\n"}, true
		case "systemctl list-unit-files --type=service --no-legend --no-pager --plain nginx* openresty* angie* tengine*":
			return execx.Result{Stdout: "nginx.service enabled enabled\nopenresty.service disabled enabled\n"}, true
		case "systemctl show -p ActiveState -p MainPID -p ExecStart -p DropInPaths -p UnitFileState openresty.service":
			return execx.Result{Stdout: "ActiveState=inactive\nMainPID=0\nUnitFileState=disabled\nDropInPaths=/etc/systemd/system/openresty.service.d/override.conf\n" +
				"ExecStart={ path=/usr/local/openresty/bin/openresty ; argv[]=/usr/local/openresty/bin/openresty -c /etc/openresty/nginx.conf -g daemon on; ; ignore_errors=no ; start_time=[n/a] }\n"}, true
		}
		return execx.Result{}, false
	}}
	env := baseEnv(t, run, "systemctl")
	fakeProc(t, env.Proc, []fp{
		{pid: 1, cmd: "/sbin/init"},
		// DISC-05: a source-built master outside PATH, no unit.
		{pid: 100, ppid: 1, cmd: "nginx: master process /opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf", exe: "/opt/nginx/sbin/nginx"},
		{pid: 101, ppid: 100, uid: 33, cmd: "nginx: worker process"},
		// DISC-02/04: a second master under systemd with -g.
		{pid: 110, ppid: 1, cmd: "nginx: master process /usr/sbin/nginx -c " + conf2 + " -g daemon on; master_process on;", exe: "/usr/sbin/nginx", cg: "0::/system.slice/nginx.service"},
		// DISC-06: angie, not root.
		{pid: 120, ppid: 1, uid: 1000, cmd: "angie: master process /usr/sbin/angie", exe: "/usr/sbin/angie"},
		// A container's master: another mount namespace, not a host instance.
		{pid: 200, ppid: 190, cmd: "nginx: master process nginx -g daemon off;", ns: "mnt:[4026532001]"},
		// DISC-16.
		{pid: 300, ppid: 1, cmd: "/usr/bin/kubelet --config=/var/lib/kubelet/config.yaml"},
	})
	r := Scan(ctx, env)
	if len(r.Instances) != 4 {
		var ids []string
		for _, i := range r.Instances {
			ids = append(ids, i.ID+" "+i.State)
		}
		t.Fatalf("instances: %v", ids)
	}
	a := byID(t, r, "host:/opt/nginx/sbin/nginx:/opt/nginx/conf/nginx.conf")
	if a.Exe != "/opt/nginx/sbin/nginx" || a.Version != "nginx/1.26.2" || a.Source != "dump" || !a.Caps.Write.OK {
		t.Errorf("source-built master %+v", a)
	}
	if a.Methods.Reload != "/opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -s reload" {
		t.Errorf("reload without unit: %s", a.Methods.Reload)
	}
	b := byID(t, r, "host:nginx.service")
	if b.Unit != "nginx.service" || b.Globals != "daemon on; master_process on;" || b.Methods.Reload != "systemctl reload nginx.service" {
		t.Errorf("systemd master %+v", b)
	}
	if b.Source != "files" || len(b.Files) != 2 || b.Summary.Servers[0].Listens[0].Port != 8080 {
		t.Errorf("files fallback: %s %+v", b.Source, b.Files)
	}
	c := byID(t, r, "host:/usr/sbin/angie:/etc/angie/angie.conf")
	if c.Variant != "angie" || c.User != "alice" || !strings.Contains(strings.Join(c.Notes, " "), "DISC-06") {
		t.Errorf("angie %+v", c)
	}
	d := byID(t, r, "host:openresty.service")
	if d.State != StateStopped || d.Conf != "/etc/openresty/nginx.conf" || d.Variant != "openresty" || !strings.Contains(strings.Join(d.Notes, " "), "DISC-18") {
		t.Errorf("stopped unit %+v", d)
	}
	if d.Caps.Reload.OK || !strings.Contains(d.Caps.Reload.Reason, "not running") {
		t.Errorf("stopped reload %+v", d.Caps.Reload)
	}
	if len(findings(r, "DISC-16")) != 1 || len(findings(r, "DISC-17")) != 1 {
		t.Errorf("findings %+v", r.Findings)
	}
	if !strings.HasPrefix(r.Host, "4 host instances") {
		t.Errorf("host line %q", r.Host)
	}
}

func TestNotRootReportsFailedReads(t *testing.T) {
	env := baseEnv(t, &execx.Fake{}, "ss")
	env.Euid = 1000
	fakeProc(t, env.Proc, []fp{{pid: 100, ppid: 1, cmd: "nginx: master process /usr/sbin/nginx"}}) // no exe link: unreadable
	r := Scan(ctx, env)
	if len(r.ReadFailures) == 0 || !strings.Contains(r.ReadFailures[0], "/proc/100/exe") {
		t.Fatalf("read failures %v", r.ReadFailures)
	}
	if f := findings(r, "DISC-14"); len(f) != 1 {
		t.Errorf("DISC-14 %+v", r.Findings)
	}
	if in := r.Instances[0]; in.Caps.Write.OK || in.Caps.Reload.OK {
		t.Errorf("not root may not write or reload: %+v", in.Caps)
	}
}

func TestNoNginxAnywhere(t *testing.T) {
	env := baseEnv(t, &execx.Fake{})
	fakeProc(t, env.Proc, []fp{{pid: 1, cmd: "/sbin/init"}})
	r := Scan(ctx, env)
	if len(r.Instances) != 0 || len(findings(r, "DISC-01")) != 1 || r.Docker.State != "missing" {
		t.Fatalf("%+v", r)
	}
}

func TestDockerStatus(t *testing.T) {
	cases := []struct {
		version, info string
		code          int
		dockerHost    string
		want          string
	}{
		{"podman version 4.9.3", "", 0, "", "podman"},
		{"Docker version 29", "", 1, "", "down"},
		{"Docker version 29", "permission denied while trying to connect", 1, "", "denied"},
		{"Docker version 29", "", 124, "", "slow"},
		{"Docker version 29", "29.0|[\"name=rootless\"]", 0, "", "rootless"},
		{"Docker version 29", "29.0|[]", 0, "unix:///run/user/1000/docker.sock", "rootless"},
		{"Docker version 29", "29.0|[]", 0, "", "ok"},
	}
	for _, c := range cases {
		run := &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
			if args[0] == "--version" {
				return execx.Result{Stdout: c.version}, true
			}
			if c.code != 0 {
				return execx.Result{Code: c.code, Stderr: c.info}, true
			}
			return execx.Result{Stdout: c.info}, true
		}}
		env := baseEnv(t, run, "docker")
		env.Getenv = func(k string) string { return map[string]string{"DOCKER_HOST": c.dockerHost}[k] }
		if got := DockerAccess(ctx, env); got.State != c.want || (got.State != "ok" && got.Hint == "") {
			t.Errorf("%+v: got %+v", c, got)
		}
	}
}

func TestComposeDefinedNeverStarted(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "alice", "shop", "compose.yaml")
	write(t, good, "services:\n  web:\n    image: nginx:1.27\n    ports: [\"8088:80\"]\n    volumes: [\"./nginx:/etc/nginx/conf.d:ro\"]\n  api:\n    image: node:22\n")
	write(t, filepath.Join(root, "alice", "shop", "nginx", "shop.conf"), "server {\n    listen 80;\n    server_name shop.example.com;\n}\n")
	broken := filepath.Join(root, "alice", "blog", "docker-compose.yml")
	write(t, broken, "name: blog\nservices:\n  proxy:\n    image: openresty/openresty:alpine\n    env_file: missing.env\n    ports:\n      - \"127.0.0.1:8443:443\"\n    networks: [front]\n")
	write(t, filepath.Join(root, "alice", "other", "compose.yaml"), "services:\n  db:\n    image: postgres:17\n")
	cfg := `{"name":"shop","services":{"api":{"image":"node:22","environment":{"SECRET":"s3cr3t"}},` +
		`"web":{"image":"nginx:1.27","environment":{"TOKEN":"hunter2"},"ports":[{"target":80,"published":"8088","protocol":"tcp"}],` +
		`"networks":{"default":null},"volumes":[{"type":"bind","source":"` + filepath.Join(root, "alice", "shop", "nginx") + `","target":"/etc/nginx/conf.d","read_only":true}]}}}`
	run := &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
		k := execx.Key(name, args...)
		switch {
		case k == "docker --version":
			return execx.Result{Stdout: "Docker version 29"}, true
		case strings.HasPrefix(k, "docker info"):
			return execx.Result{Stdout: "29.0|[]"}, true
		case strings.HasPrefix(k, "docker ps"):
			return execx.Result{}, true
		case k == "docker compose -f "+good+" config --format json":
			return execx.Result{Stdout: cfg}, true
		case k == "docker compose -f "+broken+" config --format json":
			return execx.Result{Code: 1, Stderr: "env file missing.env not found"}, true
		}
		return execx.Result{}, false
	}}
	env := baseEnv(t, run, "docker")
	env.Roots, env.Depth = []string{filepath.Join(root, "*")}, 4
	r := Scan(ctx, env)
	if r.ComposeFiles != 3 || len(r.Instances) != 2 {
		t.Fatalf("files %d instances %+v", r.ComposeFiles, r.Instances)
	}
	web := byID(t, r, "compose:shop/web")
	if web.State != StateDefined || web.Unresolved || web.Caps.Write.OK || !strings.Contains(web.Caps.Write.Reason, "docker compose up -d web") || web.Caps.Test.OK {
		t.Errorf("web %+v", web)
	}
	if !web.Caps.Read.OK || !web.StockMain || web.Ports[0].HostPort != 8088 {
		t.Errorf("web read %+v ports %+v", web.Caps.Read, web.Ports)
	}
	px := byID(t, r, "compose:blog/proxy")
	if !px.Unresolved || px.Ports[0].HostIP != "127.0.0.1" || px.Ports[0].HostPort != 8443 || px.Networks[0] != "front" {
		t.Errorf("blog %+v", px)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "hunter2") || strings.Contains(string(b), "s3cr3t") {
		t.Fatal("resolved compose environment leaked into the report")
	}
}

func TestParseSSAndOwners(t *testing.T) {
	ls := parseSS(read(t, "testdata/ss.txt") + "LISTEN 0 511 127.0.0.53%lo:53 0.0.0.0:* users:((\"systemd-resolve\",pid=9,fd=15))\n" +
		"LISTEN 0 511 *:8443 *:*\n")
	if len(ls) != 17 {
		t.Fatalf("listeners %d", len(ls))
	}
	last := ls[len(ls)-2]
	if last.Addr != "127.0.0.53" || last.Port != 53 || last.Procs[0].Name != "systemd-resolve" {
		t.Errorf("%+v", last)
	}
	if ls[16].Addr != "*" || len(ls[16].Procs) != 0 {
		t.Errorf("no owner (not root): %+v", ls[16])
	}
	if ls[4].Procs[0].PID != 304 || len(ls[4].Procs) != 2 {
		t.Errorf("two owners: %+v", ls[4])
	}
}

func TestPortConflictAndNobody(t *testing.T) {
	env := baseEnv(t, &execx.Fake{}, "ss")
	s := New(env)
	sum := func(port int) *nginxconf.Summary {
		return &nginxconf.Summary{Servers: []nginxconf.Server{{Listens: []nginxconf.Listen{{Port: port}}}}}
	}
	s.inst = []*Instance{
		{ID: "host:a", Name: "a", Kind: KindHost, State: StateRunning, Summary: sum(8080)},
		{ID: "host:b", Name: "b", Kind: KindHost, State: StateStopped, Summary: sum(8080)},
		{ID: "ctr:c", Name: "c", Kind: KindContainer, Ports: []Published{{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Proto: "tcp"}}},
	}
	s.portOwners()
	s.findings()
	var conflicts, nobody int
	for _, f := range s.rep.Findings {
		switch {
		case f.Code == "DISC-15" && strings.Contains(f.Message, "only one can bind"):
			conflicts++
		case f.Code == "DISC-15" && strings.HasPrefix(f.Message, "nothing listens"):
			nobody++
		}
	}
	if conflicts != 3 || nobody != 2 {
		t.Fatalf("conflicts %d nobody %d: %+v", conflicts, nobody, s.rep.Findings)
	}
}

func TestDuplicateNameOnOneListen(t *testing.T) {
	s := New(baseEnv(t, &execx.Fake{}))
	srv := func(line int) nginxconf.Server {
		return nginxconf.Server{Pos: nginxconf.Pos{File: "/e/a.conf", Line: line}, Listens: []nginxconf.Listen{{Port: 443, SSL: true}},
			Names: []nginxconf.ServerName{{Name: "App.example.com"}}}
	}
	s.inst = []*Instance{{ID: "x", Name: "x", Summary: &nginxconf.Summary{Servers: []nginxconf.Server{srv(1), srv(9)}}}}
	s.nameFindings()
	f := s.rep.Findings
	if len(f) != 1 || f[0].Code != "RP-03" || !strings.Contains(f[0].Message, "/e/a.conf:1 and /e/a.conf:9") {
		t.Fatalf("%+v", f)
	}
}

func TestReachFromContainerAndHost(t *testing.T) {
	s := New(baseEnv(t, &execx.Fake{}))
	s.ctrs = []*ctr{
		{Name: "api", Running: true, DNS: map[string][]string{"edge": {"api", "web"}}, IPs: map[string]string{"edge": "192.0.2.10"}},
		{Name: "db", Running: false, State: "exited", DNS: map[string][]string{"edge": {"db"}}},
		{Name: "lone", Running: true, DNS: map[string][]string{"other": {"lone"}}},
	}
	s.listeners = []listener{{Addr: "127.0.0.1", Port: 3005}}
	ctrIn := &Instance{Name: "proxy", Kind: KindContainer, Networks: []string{"edge"}}
	hostIn := &Instance{Name: "host", Kind: KindHost}
	cases := []struct {
		in   *Instance
		host string
		port int
		want string
	}{
		{ctrIn, "web", 80, ReachOK},
		{ctrIn, "192.0.2.10", 80, ReachOK},
		{ctrIn, "db", 5432, ReachErr},
		{ctrIn, "lone", 80, ReachErr},
		{ctrIn, "missing", 80, ReachErr},
		{ctrIn, "api.example.com", 443, ReachUnknown},
		{ctrIn, "127.0.0.1", 80, ReachWarn},
		{hostIn, "127.0.0.1", 3005, ReachOK},
		{hostIn, "127.0.0.1", 3006, ReachWarn},
		{hostIn, "api", 80, ReachErr},
	}
	for _, c := range cases {
		if got := s.reach(c.in, c.host, c.port, ""); got.Status != c.want || got.Why == "" {
			t.Errorf("%s → %s:%d: %+v, want %s", c.in.Name, c.host, c.port, got, c.want)
		}
	}
}

func TestCapabilityReasons(t *testing.T) {
	f := false
	hook := &nginxconf.Summary{Hook: &nginxconf.Hook{Existing: "/etc/nginx/conf.d/*.conf", Dir: "/etc/nginx/conf.d"}}
	files := []nginxconf.FileInfo{{Path: "/etc/nginx/nginx.conf"}}
	cases := []struct {
		name  string
		in    Instance
		cap   func(Capabilities) Capability
		ok    bool
		cause string
	}{
		{"baked into the image", Instance{Kind: KindContainer, State: StateRunning, Files: files, Source: "dump", Summary: hook},
			func(c Capabilities) Capability { return c.Write }, false, "config is inside the image"},
		{":ro mount is fine", Instance{Kind: KindContainer, State: StateRunning, Files: files, Source: "dump", Summary: hook,
			Mounts: []Mount{{Type: "bind", Source: "/srv/n", Dest: "/etc/nginx/conf.d"}}},
			func(c Capabilities) Capability { return c.Write }, true, ":ro only stops the container from writing; the host side stays writable"},
		{"named volume", Instance{Kind: KindContainer, State: StateRunning, Files: files, Source: "dump", Summary: hook,
			Mounts: []Mount{{Type: "volume", Name: "conf", Source: "/var/lib/docker/volumes/conf/_data", Dest: "/etc/nginx", RW: true}}},
			func(c Capabilities) Capability { return c.Write }, true, "named volume conf"},
		{"invalid config", Instance{Kind: KindContainer, State: StateRunning, Valid: &f, Files: files, Summary: hook,
			Mounts: []Mount{{Type: "bind", Dest: "/etc/nginx"}}},
			func(c Capabilities) Capability { return c.Write }, false, "CONF-03"},
		{"invalid config, reload", Instance{Kind: KindContainer, State: StateRunning, Valid: &f, Files: files, Summary: hook},
			func(c Capabilities) Capability { return c.Reload }, false, "fix it first"},
		{"non-UTF-8", Instance{Kind: KindContainer, State: StateRunning, Files: []nginxconf.FileInfo{{Path: "/etc/nginx/x.conf", NonUTF8: true}}, Summary: hook,
			Mounts: []Mount{{Type: "bind", Dest: "/etc/nginx"}}},
			func(c Capabilities) Capability { return c.Write }, false, "CONF-10"},
		{"defined", Instance{Kind: KindCompose, State: StateDefined, Service: "web", WorkingDir: "/srv/shop"},
			func(c Capabilities) Capability { return c.Write }, false, "docker compose up -d web in /srv/shop"},
		{"stopped reload", Instance{Kind: KindContainer, State: StateStopped},
			func(c Capabilities) Capability { return c.Reload }, false, "not running"},
		{"other manager", Instance{Kind: KindContainer, State: StateRunning, ManagedBy: "other:swag"},
			func(c Capabilities) Capability { return c.Write }, false, "managed by swag"},
		{"no http block", Instance{Kind: KindHost, State: StateRunning, Files: files, Source: "dump", Summary: &nginxconf.Summary{}},
			func(c Capabilities) Capability { return c.Write }, false, "no http {} block"},
	}
	for _, c := range cases {
		s := New(baseEnv(t, &execx.Fake{}))
		s.rep.Docker = DockerStatus{State: "ok"}
		in := c.in
		if in.ManagedBy == "" {
			in.ManagedBy = ManagedNone
		}
		s.capabilities(&in, nil)
		got := c.cap(in.Caps)
		text := got.Reason + got.Note
		if got.OK != c.ok || !strings.Contains(text, c.cause) {
			t.Errorf("%s: %+v, want ok=%v with %q", c.name, got, c.ok, c.cause)
		}
	}
}

func TestCmdArgsAndExecStart(t *testing.T) {
	bin, conf, prefix, g := cmdArgs("/usr/sbin/nginx -p /srv/n -c conf/n.conf -g daemon on; master_process on;")
	if bin != "/usr/sbin/nginx" || conf != "conf/n.conf" || prefix != "/srv/n" || g != "daemon on; master_process on;" {
		t.Fatalf("%q %q %q %q", bin, conf, prefix, g)
	}
	if got := confPath(conf, prefix, buildInfo{}); got != "/srv/n/conf/n.conf" {
		t.Errorf("relative -c: %s", got)
	}
	exe, conf, _, g := execStart("{ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -g daemon on; master_process on; ; ignore_errors=no ; start_time=[n/a] }")
	if exe != "/usr/sbin/nginx" || conf != "" || g != "daemon on; master_process on;" {
		t.Errorf("%q %q %q", exe, conf, g)
	}
	for img, want := range map[string]bool{
		"nginx:stable-alpine": true, "nginxinc/nginx-unprivileged:1.27": true, "bitnami/nginx": true,
		"openresty/openresty:alpine": true, "docker.angie.software/angie:1.6": true, "lscr.io/linuxserver/swag": true,
		"jc21/nginx-proxy-manager:latest": true, "nginxproxy/acme-companion": false, "postgres:17": false, "registry:5000/nginx@sha256:ab": true,
	} {
		if nginxImage(img) != want {
			t.Errorf("nginxImage(%s) = %v", img, !want)
		}
	}
}

func TestCacheRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cache", "scan.json")
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := SaveCache(p, &Report{Schema: Schema, ScannedAt: now}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if _, ok := LoadCache(p, time.Minute, now.Add(30*time.Second)); !ok {
		t.Error("fresh cache not used")
	}
	if _, ok := LoadCache(p, time.Minute, now.Add(61*time.Second)); ok {
		t.Error("stale cache used")
	}
}

func TestParseVBanners(t *testing.T) {
	for out, want := range map[string][2]string{
		"nginx version: nginx/1.24.0 (Ubuntu)":          {"nginx", "nginx/1.24.0"},
		"nginx version: openresty/1.25.3.1":             {"openresty", "openresty/1.25.3.1"},
		"Tengine version: Tengine/3.1.0\nnginx version": {"tengine", "Tengine/3.1.0"},
		"Angie version: Angie/1.6.0":                    {"angie", "Angie/1.6.0"},
	} {
		b := parseV(out + "\nconfigure arguments: --prefix=/p --conf-path=/p/n.conf")
		if b.Variant != want[0] || b.Version != want[1] || b.ConfPath != "/p/n.conf" || b.Prefix != "/p" {
			t.Errorf("%q: %+v", out, b)
		}
	}
}

// A running instance whose config fails: flagged invalid with nginx's
// error, parsed from its files anyway, read-only (CONF-03).
func TestInvalidRunningConfig(t *testing.T) {
	tmp := t.TempDir()
	conf := filepath.Join(tmp, "nginx.conf")
	write(t, conf, "events {}\nhttp {\n    server {\n        listen 80;\n        server_name bad.example.com;\n        lisen 81;\n    }\n}\n")
	run := &execx.Fake{Responses: map[string]execx.Result{
		"/usr/sbin/nginx -V":                 {Stderr: "nginx version: nginx/1.24.0\n"},
		"/usr/sbin/nginx -c " + conf + " -T": {Code: 1, Stderr: "nginx: [emerg] unknown directive \"lisen\" in " + conf + ":6\nnginx: configuration file " + conf + " test failed\n"},
	}}
	env := baseEnv(t, run)
	fakeProc(t, env.Proc, []fp{{pid: 100, ppid: 1, cmd: "nginx: master process /usr/sbin/nginx -c " + conf, exe: "/usr/sbin/nginx"}})
	r := Scan(ctx, env)
	in := r.Instances[0]
	if in.Valid == nil || *in.Valid || !strings.Contains(in.TestOutput, "unknown directive") || in.Source != "files" {
		t.Fatalf("%+v", in)
	}
	if in.Caps.Write.OK || in.Caps.Reload.OK || !in.Caps.Read.OK || len(in.Summary.Servers) != 1 {
		t.Errorf("caps %+v", in.Caps)
	}
	if f := findings(r, "CONF-03"); len(f) != 1 || !strings.Contains(f[0].Message, "unknown directive") {
		t.Errorf("%+v", r.Findings)
	}
}
