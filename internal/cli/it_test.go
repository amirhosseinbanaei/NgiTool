package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// TestIntegrationProxyAndBalance runs against the real Docker daemon,
// gated by NGITOOL_IT=1. It creates only its own network (ngitool-it), a
// front nginx (ngitool-it-front on 127.0.0.1:18080) and three backends
// (ngitool-it-b1..b3), and removes them all. It never touches "edge",
// "proxy" or any other container.
func TestIntegrationProxyAndBalance(t *testing.T) {
	if os.Getenv("NGITOOL_IT") != "1" {
		t.Skip("set NGITOOL_IT=1 to run against the real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	// DOCK-08: an inherited project name must not leak into anything.
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	os.Unsetenv("COMPOSE_PROJECT_NAME")

	const network, front = "ngitool-it", "ngitool-it-front"
	backends := []string{"ngitool-it-b1", "ngitool-it-b2", "ngitool-it-b3"}
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	all := append([]string{front}, backends...)
	for _, c := range all {
		_ = exec.Command("docker", "rm", "-f", c).Run()
	}
	// The discover integration test shares the network and may run at the
	// same time: create it unless it exists, remove it only when it is ours.
	created := exec.Command("docker", "network", "inspect", network).Run() != nil &&
		exec.Command("docker", "network", "create", network).Run() == nil
	t.Cleanup(func() {
		for _, c := range all {
			_ = exec.Command("docker", "rm", "-f", c).Run()
		}
		if created {
			_ = exec.Command("docker", "network", "rm", network).Run()
		}
	})

	dir := t.TempDir()
	conf := filepath.Join(dir, "front-conf.d")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range backends {
		script := fmt.Sprintf(`printf 'server { listen 80; location / { return 200 "%s\\n"; } }\n' > /etc/nginx/conf.d/default.conf && exec nginx -g 'daemon off;'`, b)
		docker("run", "-d", "--name", b, "--network", network, "--pull", "never", "nginx:alpine", "sh", "-c", script)
	}
	docker("run", "-d", "--name", front, "--network", network, "--pull", "never",
		"-p", "127.0.0.1:18080:80", "-v", conf+":/etc/nginx/conf.d", "nginx:stable-alpine")
	for i := 0; i < 50 && docker("inspect", "-f", "{{.State.Running}}", front) != "true"; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	// A sandbox of NgiTool's own paths; compose scanning only looks at dir.
	t.Setenv(paths.EnvRoot, filepath.Join(dir, "root"))
	t.Setenv(EnvNoCheck, "1")
	e := &env{paths: paths.Get()}
	if err := os.MkdirAll(e.paths.Etc, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.paths.Config, []byte(`{"schema":1,"scanRoots":["`+dir+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prevOut, prevErr := ui.Out, ui.Errw
	ui.Out, ui.Errw = &buf, &buf
	t.Cleanup(func() { ui.Out, ui.Errw = prevOut, prevErr })
	ngt := func(want int, args ...string) string {
		t.Helper()
		buf.Reset()
		_, code := execute(t, e, args...)
		if code != want {
			t.Fatalf("ngitool %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, buf.String())
		}
		return buf.String()
	}
	client := &http.Client{Timeout: 20 * time.Second}
	get := func(host string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:18080/", nil)
		req.Host = host
		res, err := client.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, strings.TrimSpace(string(b))
	}
	spread := func(host string, n int) map[string]int {
		got := map[string]int{}
		for i := 0; i < n; i++ {
			code, body := get(host)
			if code != 200 {
				t.Fatalf("request %d to %s: %d %s", i, host, code, body)
			}
			got[body]++
		}
		return got
	}

	ngt(ExitOK, "instance", "adopt", front, "--yes")
	ngt(ExitOK, "route", "add", "it.example.com", "--instance", front, "--method", "round_robin", "--yes",
		"--to", "container:ngitool-it-b1:80", "--to", "container:ngitool-it-b2:80", "--to", "container:ngitool-it-b3:80")
	rr := spread("it.example.com", 30)
	t.Logf("round robin over 30 requests: %v", sorted(rr))
	for _, b := range backends {
		if rr[b] == 0 {
			t.Errorf("%s never answered under round robin: %v", b, rr)
		}
	}

	ngt(ExitOK, "pool", "drain", "it_example_com", "ngitool-it-b2:80", "--yes")
	drained := spread("it.example.com", 20)
	t.Logf("with b2 drained: %v", sorted(drained))
	if drained["ngitool-it-b2"] > 0 {
		t.Errorf("drained b2 still answers: %v", drained)
	}
	ngt(ExitOK, "pool", "undrain", "it_example_com", "ngitool-it-b2:80", "--yes")

	ngt(ExitOK, "route", "edit", "it.example.com", "--method", "least_conn", "--yes")
	up, err := os.ReadFile(filepath.Join(conf, "ngitool", "upstreams", "it_example_com.conf"))
	if err != nil || !strings.Contains(string(up), "least_conn;") {
		t.Fatalf("switch to least_conn: %v\n%s", err, up)
	}

	docker("stop", "-t", "1", "ngitool-it-b1")
	after := spread("it.example.com", 10)
	t.Logf("least_conn with b1 stopped: %v", sorted(after))
	if after["ngitool-it-b1"] > 0 {
		t.Errorf("stopped b1 answered: %v", after)
	}

	out := ngt(ExitOK, "pool", "check", "it_example_com", "--json")
	var checks []memberCheck
	if err := json.Unmarshal([]byte(out), &checks); err != nil || len(checks) != 3 {
		t.Fatalf("pool check --json: %v\n%s", err, out)
	}
	for _, c := range checks {
		if (c.Member == "ngitool-it-b1:80") == c.OK {
			t.Errorf("pool check: %+v", c)
		}
	}

	// A broken file on purpose: nginx -t fails, the snapshot comes back,
	// and the previous config keeps serving (APPLY-01).
	render.Mutate = func(p, body string) string {
		if strings.HasSuffix(p, "it2.example.com.conf") {
			return strings.Replace(body, "server {", "server {\n    this_is_not_a_directive on;", 1)
		}
		return body
	}
	t.Cleanup(func() { render.Mutate = nil })
	out = ngt(ExitFail, "route", "add", "it2.example.com", "--instance", front, "--to", "container:ngitool-it-b3:80", "--yes")
	render.Mutate = nil
	if !strings.Contains(out, "this_is_not_a_directive") || !strings.Contains(out, "restored") {
		t.Errorf("failed test output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(conf, "ngitool", "servers", "it2.example.com.conf")); err == nil {
		t.Error("the broken file is still on disk")
	}
	if code, body := get("it.example.com"); code != 200 || body == "" {
		t.Errorf("previous config no longer serves: %d %s", code, body)
	}
	ngt(ExitOK, "test", front)
}

func sorted(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%d", strings.TrimPrefix(k, "ngitool-it-"), m[k]))
	}
	return strings.Join(parts, " ")
}
