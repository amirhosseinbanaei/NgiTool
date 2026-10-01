package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// fakeMachine is one host nginx and no docker.
func fakeMachine(t *testing.T) {
	t.Helper()
	proc := filepath.Join(t.TempDir(), "proc")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(proc, "self", "ns"), 0o700))
	must(os.Symlink("mnt:[1]", filepath.Join(proc, "self", "ns", "mnt")))
	d := filepath.Join(proc, "100")
	must(os.MkdirAll(filepath.Join(d, "ns"), 0o700))
	must(os.Symlink("mnt:[1]", filepath.Join(d, "ns", "mnt")))
	must(os.Symlink("/usr/sbin/nginx", filepath.Join(d, "exe")))
	must(os.WriteFile(filepath.Join(d, "cmdline"), []byte("nginx: master process /usr/sbin/nginx\x00"), 0o600))
	must(os.WriteFile(filepath.Join(d, "stat"), []byte("100 (nginx) S 1 1"), 0o600))
	must(os.WriteFile(filepath.Join(d, "cgroup"), []byte("0::/system.slice/nginx.service\n"), 0o600))
	dump := "# configuration file /etc/nginx/nginx.conf:\nevents {}\nhttp {\n    include /etc/nginx/conf.d/*.conf;\n    upstream app { server 127.0.0.1:3000; }\n" +
		"    server {\n        listen 80;\n        server_name app.example.com;\n        location / { proxy_pass http://app; }\n        location /x { return 200 \"a\\nb\"; }\n    }\n}\n\n"
	run := &execx.Fake{Responses: map[string]execx.Result{
		"/usr/sbin/nginx -V":                          {Stderr: "nginx version: nginx/1.24.0\nconfigure arguments: --conf-path=/etc/nginx/nginx.conf\n"},
		"/usr/sbin/nginx -c /etc/nginx/nginx.conf -T": {Stdout: dump},
		"ss -ltnpH": {Stdout: "LISTEN 0 511 0.0.0.0:80 0.0.0.0:* users:((\"nginx\",pid=100,fd=6))\n"},
	}}
	prev := newScanEnv
	newScanEnv = func(*env) discover.Env {
		return discover.Env{
			Run: run, Proc: proc, Euid: 0, Now: time.Now, Getenv: func(string) string { return "" },
			LookPath: func(n string) (string, error) {
				if n == "ss" {
					return "/usr/bin/ss", nil
				}
				return "", errors.New("no")
			},
			UserName: func(int) string { return "root" },
		}
	}
	t.Cleanup(func() { newScanEnv = prev })
}

func TestScanInstancesInspect(t *testing.T) {
	e, buf := sandbox(t)
	fakeMachine(t)

	if err, code := execute(t, e, "scan"); code != 0 {
		t.Fatalf("scan: %v\n%s", err, buf)
	}
	out := buf.String()
	for _, want := range []string{"✔ Host processes · 1 host instance", "Instances  1 found · front door nginx", "⌂ nginx  ● running  nginx/1.24.0  [front door]",
		"host:nginx.service · owner root · 1 server · 1 upstream", "✔ read  ✔ test  ✔ reload  ✔ write", "docker  docker is not installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan output lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(e.paths.ScanCache); err != nil {
		t.Errorf("cache not written: %v", err)
	}

	buf.Reset()
	e.memo = scanMemo{}
	if _, code := execute(t, e, "instances", "--json"); code != 0 {
		t.Fatal(buf.String())
	}
	var list []compactInstance
	if err := json.Unmarshal(buf.Bytes(), &list); err != nil || len(list) != 1 || list[0].ID != "host:nginx.service" || !list[0].FrontDoor || !list[0].Write {
		t.Fatalf("instances --json: %v %s", err, buf)
	}

	buf.Reset()
	e.memo = scanMemo{}
	if _, code := execute(t, e, "inspect", "host:nginx"); code != 0 {
		t.Fatal(buf.String())
	}
	out = buf.String()
	for _, want := range []string{"hook point   include /etc/nginx/conf.d/*.conf (/etc/nginx/nginx.conf:3)", "reload       systemctl reload nginx.service",
		"listen 80", "app.example.com  /etc/nginx/nginx.conf:5", "nginx  1 server · 1 upstream", "→ proxy_pass http://app", "127.0.0.1:3000", `return 200 a\nb`} {
		if !strings.Contains(out, want) {
			t.Errorf("inspect lacks %q:\n%s", want, out)
		}
	}

	buf.Reset()
	if _, code := execute(t, e, "inspect", "--raw", "nginx"); code != 0 || !strings.HasPrefix(buf.String(), "# configuration file /etc/nginx/nginx.conf:") {
		t.Errorf("inspect --raw:\n%s", buf)
	}

	buf.Reset()
	if _, code := execute(t, e, "inspect"); code != ExitUsage || !strings.Contains(buf.String(), "missing instance") {
		t.Errorf("inspect without id off a terminal: %d %s", code, buf)
	}
	buf.Reset()
	if _, code := execute(t, e, "inspect", "nope"); code != ExitUsage || !strings.Contains(buf.String(), `no instance "nope"`) {
		t.Errorf("unknown id: %d %s", code, buf)
	}
}

func TestInstancesGroupInMenuAndHelp(t *testing.T) {
	var g *Group
	for i := range groups {
		if groups[i].ID == "instances" {
			g = &groups[i]
		}
	}
	if g == nil || len(itemsOf("instances")) != 6 {
		t.Fatal("Instances menu group with scan, list, inspect, adopt, release, externalize")
	}
	e, _ := sandbox(t)
	help := renderHelp(newRoot(e))
	for _, s := range []string{"INSTANCES", "ngitool scan [--json]", "ngitool instances [--json] [--fresh]", "ngitool inspect [id] [--raw] [--json]"} {
		if !strings.Contains(help, s) {
			t.Errorf("help lacks %q", s)
		}
	}
}
