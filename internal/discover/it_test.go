package discover

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegrationScan runs against the real Docker daemon, gated by
// NGITOOL_IT=1. It creates only its own container (ngitool-it-scan) and
// network (ngitool-it), never touches "edge" or "proxy", and removes both.
func TestIntegrationScan(t *testing.T) {
	if os.Getenv("NGITOOL_IT") != "1" {
		t.Skip("set NGITOOL_IT=1 to run against the real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	const name, network = "ngitool-it-scan", "ngitool-it"
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	conf := t.TempDir()
	if err := os.WriteFile(filepath.Join(conf, "it.conf"), []byte("server {\n    listen 80;\n    server_name it.example.com;\n    location / { return 200 \"ok\\n\"; }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if exec.Command("docker", "network", "inspect", network).Run() != nil {
		docker("network", "create", network)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})
	docker("run", "-d", "--name", name, "--network", network, "--pull", "never",
		"-p", "127.0.0.1:18080:80", "-v", conf+":/etc/nginx/conf.d:ro", "nginx:stable-alpine")
	for i := 0; i < 50 && docker("inspect", "-f", "{{.State.Running}}", name) != "true"; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	env := System(nil)
	r := Scan(ctx, env)
	in := r.Find("ctr:" + name)
	if in == nil {
		t.Fatalf("%s not found", name)
	}
	if in.State != StateRunning || in.Source != "dump" || !in.Caps.Write.OK || !strings.Contains(in.Caps.Write.Note, conf) {
		t.Fatalf("running: state %s source %s write %+v", in.State, in.Source, in.Caps.Write)
	}
	if in.Summary.Servers[len(in.Summary.Servers)-1].NameList() != "it.example.com" {
		t.Errorf("servers %+v", in.Summary.Servers)
	}
	res, err := Test(ctx, env, in)
	if err != nil || !res.OK {
		t.Fatalf("nginx -t: %+v %v", res, err)
	}

	// Stopped: read through the mount (stock main), tested in a throwaway
	// container with no network.
	docker("stop", "-t", "2", name)
	r = Scan(ctx, env)
	in = r.Find("ctr:" + name)
	if in == nil || in.State != StateStopped || in.Source != "files" || !in.StockMain || !in.Caps.Write.OK {
		t.Fatalf("stopped: %+v", in)
	}
	res, err = Test(ctx, env, in)
	if err != nil || !res.OK {
		t.Fatalf("stopped nginx -t: %+v %v", res, err)
	}
}
