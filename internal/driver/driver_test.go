package driver

import (
	"context"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

var ctx = context.Background()

func TestCleanDropsNoise(t *testing.T) {
	in := "nginx: [warn] 4096 worker_connections exceed open file resource limit: 1024\n\n" +
		"2026/01/01 [notice] 1#1: signal process started\nnginx: configuration file /x test is successful\n"
	if got := Clean(in); got != "nginx: configuration file /x test is successful" {
		t.Fatalf("got %q", got)
	}
}

func TestHostDumpTestReload(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{
		"/opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -T": {Stdout: "# configuration file /opt/nginx/conf/nginx.conf:\nevents {}\n"},
		"/opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -t": {Code: 1, Stderr: "nginx: [emerg] unknown directive \"lisen\" in /opt/nginx/conf/nginx.conf:12\nnginx: configuration file /opt/nginx/conf/nginx.conf test failed\n"},
		"systemctl reload nginx.service":                         {},
	}}
	h := Host{Run: f, Exe: "/opt/nginx/sbin/nginx", Conf: "/opt/nginx/conf/nginx.conf"}
	out, err := h.Dump(ctx)
	if err != nil || !strings.HasPrefix(out, "# configuration file") {
		t.Fatalf("dump %q %v", out, err)
	}
	r, err := h.Test(ctx)
	if err != nil || r.OK || r.File != "/opt/nginx/conf/nginx.conf" || r.Line != 12 {
		t.Fatalf("test %+v %v", r, err)
	}
	// Without a unit: nginx -s reload with the same -c.
	if got := h.Describe().Reload; got != "/opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -s reload" {
		t.Errorf("reload method %q", got)
	}
	err = h.Reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "reload via /opt/nginx/sbin/nginx -c /opt/nginx/conf/nginx.conf -s reload failed") {
		t.Errorf("reload error %v", err)
	}
	// With a unit: systemctl reload.
	h.Unit = "nginx.service"
	if err := h.Reload(ctx); err != nil {
		t.Errorf("systemctl reload: %v", err)
	}
	if !f.Called("systemctl reload nginx.service") {
		t.Error("systemctl reload not used")
	}
}

func TestHostGlobalsAndPrefix(t *testing.T) {
	h := Host{Exe: "/usr/sbin/nginx", Prefix: "/srv/n", Conf: "/srv/n/n.conf", Globals: "daemon on; master_process on;"}
	if got := h.Describe().Test; got != "/usr/sbin/nginx -p /srv/n -c /srv/n/n.conf -g daemon on; master_process on; -t" {
		t.Fatalf("got %q", got)
	}
}

func TestRunningContainer(t *testing.T) {
	f := &execx.Fake{Responses: map[string]execx.Result{
		"docker exec web-1 nginx -c /etc/nginx/edge/nginx.conf -T":        {Stdout: "# configuration file /etc/nginx/edge/nginx.conf:\n"},
		"docker exec web-1 nginx -c /etc/nginx/edge/nginx.conf -t":        {Stderr: "nginx: the configuration file /etc/nginx/edge/nginx.conf syntax is ok\n"},
		"docker exec web-1 nginx -c /etc/nginx/edge/nginx.conf -s reload": {Stderr: "2026/01/01 [notice] 7#7: signal process started\n"},
	}}
	c := Container{Run: f, Name: "web-1", Image: "nginx:stable-alpine", Conf: "/etc/nginx/edge/nginx.conf", Running: true}
	if _, err := c.Dump(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := c.Test(ctx)
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStoppedContainerTestsInThrowawayContainer(t *testing.T) {
	var got string
	f := &execx.Fake{Match: func(name string, args []string) (execx.Result, bool) {
		got = execx.Key(name, args...)
		return execx.Result{Stderr: "nginx: configuration file /etc/nginx/nginx.conf test is successful"}, true
	}}
	c := Container{Run: f, Name: "web-1", Image: "nginx:1.27", Mounts: []Mount{
		{Type: "bind", Source: "/srv/app/nginx", Dest: "/etc/nginx/conf.d"},
		{Type: "volume", Name: "app_static", Dest: "/usr/share/nginx/html"},
	}}
	r, err := c.Test(ctx)
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	want := "docker run --rm --network none --pull never -v /srv/app/nginx:/etc/nginx/conf.d:ro -v app_static:/usr/share/nginx/html:ro --entrypoint nginx nginx:1.27 -t"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if _, err := c.Dump(ctx); err != ErrStopped {
		t.Errorf("dump of a stopped container: %v", err)
	}
	if err := c.Reload(ctx); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("reload of a stopped container: %v", err)
	}
}

func TestStoppedContainerImageMissing(t *testing.T) {
	f := &execx.Fake{Match: func(string, []string) (execx.Result, bool) {
		return execx.Result{Code: 125, Stderr: "docker: Error response from daemon: No such image: nginx:9"}, true
	}}
	_, err := Container{Run: f, Name: "x", Image: "nginx:9"}.Test(ctx)
	if err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("err %v", err)
	}
}
