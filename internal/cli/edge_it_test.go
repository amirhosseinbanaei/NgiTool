package cli

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// TestIntegrationEdgeStack creates, runs and removes an edge stack on the
// real Docker daemon, gated by NGITOOL_IT=1. Everything is its own: the
// stack in a temp dir (project ngitool-it-edge, ports 18080/18443),
// network ngitool-it-edge, backend ngitool-it-edge-b1. It never touches
// the live stack, "edge" or "proxy".
func TestIntegrationEdgeStack(t *testing.T) {
	if os.Getenv("NGITOOL_IT") != "1" {
		t.Skip("set NGITOOL_IT=1 to run against the real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	const project, network, backend = "ngitool-it-edge", "ngitool-it-edge", "ngitool-it-edge-b1"
	dir := t.TempDir()
	stack := filepath.Join(dir, "edge")
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	_ = exec.Command("docker", "rm", "-f", backend).Run()
	t.Cleanup(func() {
		c := exec.Command("docker", "compose", "-p", project, "--project-directory", stack, "-f", filepath.Join(stack, "compose.yaml"), "down", "--remove-orphans")
		c.Env = append(os.Environ(), "COMPOSE_PROJECT_NAME=")
		_ = c.Run()
		_ = exec.Command("docker", "rm", "-f", backend).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})

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

	// The stack commands run with COMPOSE_PROJECT_NAME set (the legacy
	// tests found it leaking into app runs); edge init takes its project
	// from it, and every compose run scrubs it and passes -p (DOCK-08).
	t.Setenv("COMPOSE_PROJECT_NAME", project)
	ngt(ExitOK, "edge", "init", stack, "--network", network, "--email", "it@example.com", "--no-token",
		"--server-ip", "203.0.113.10", "--http-port", "18080", "--https-port", "18443", "--start", "--yes")
	os.Unsetenv("COMPOSE_PROJECT_NAME")
	for _, f := range []string{"compose.yaml", "conf/nginx.conf", ".ngitool-edge", "secrets/cloudflare.ini", ".env"} {
		if _, err := os.Stat(filepath.Join(stack, f)); err != nil {
			t.Fatalf("edge init did not write %s: %v", f, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(stack, "secrets")); st.Mode().Perm() != 0o700 {
		t.Errorf("secrets/ is %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(stack, "secrets", "cloudflare.ini")); st.Mode().Perm() != 0o600 {
		t.Errorf("cloudflare.ini is %v", st.Mode().Perm())
	}
	env := edge.ReadEnv(stack)
	if env["COMPOSE_PROJECT_NAME"] != project || env["HTTP_PORT"] != "18080" || env["EDGE_NETWORK"] != network {
		t.Fatalf(".env: %v", env)
	}
	st := loadState(t, e)
	id := model.EdgeInstanceID(stack)
	if st.EdgeAt(stack) == nil || st.Adopted(id) == nil {
		t.Fatalf("stack not registered or not adopted: edges %+v instances %+v\n%s", st.Edges, st.Instances, buf.String())
	}
	if out := ngt(ExitOK, "edge", "status", "--dir", stack, "--json"); !strings.Contains(out, `"adopted": true`) {
		t.Errorf("edge status:\n%s", out)
	}

	// A self-signed certificate, a static route and a proxied route.
	ngt(ExitOK, "cert", "add", "it.example.com", "--kind", "self-signed", "--instance", id, "--yes")
	if out := ngt(ExitOK, "cert", "ls", "--json"); !strings.Contains(out, `"name": "it.example.com"`) {
		t.Errorf("cert ls:\n%s", out)
	}
	key := filepath.Join(stack, "data", "certs", "it.example.com", "privkey.pem")
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("key %s: %v %v", key, st, err)
	}
	ngt(ExitOK, "route", "add", "it.example.com", "--instance", id, "--to", "static:site", "--cert", "it.example.com", "--yes")
	docker("run", "-d", "--name", backend, "--network", network, "--pull", "never", "nginx:alpine", "sh", "-c",
		`printf 'server { listen 80; location / { return 200 "backend\\n"; } }\n' > /etc/nginx/conf.d/default.conf && exec nginx -g 'daemon off;'`)
	ngt(ExitOK, "route", "add", "api.example.com", "--instance", id, "--to", "container:"+backend+":80", "--http-only", "--yes")

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "it.example.com"}}, //nolint:gosec // self-signed on purpose
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(url, host string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Host = host
		var res *http.Response
		var err error
		for i := 0; i < 20; i++ {
			if res, err = client.Do(req); err == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	if code, body := get("https://127.0.0.1:18443/", "it.example.com"); code != 200 || !strings.Contains(body, "it.example.com") {
		t.Errorf("static route over https: %d %s", code, body)
	}
	if code, body := get("http://127.0.0.1:18080/", "it.example.com"); code != 301 {
		t.Errorf("static route on :80 should redirect: %d %s", code, body)
	}
	if code, body := get("http://127.0.0.1:18080/", "api.example.com"); code != 200 || strings.TrimSpace(body) != "backend" {
		t.Errorf("proxied route: %d %s", code, body)
	}
	if out := ngt(ExitOK, "www", "ls", "--dir", stack, "--json"); !strings.Contains(out, `"dir": "site"`) {
		t.Errorf("www ls:\n%s", out)
	}

	ngt(ExitOK, "edge", "down", "--dir", stack, "--yes")
	if out := docker("ps", "-a", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.Names}}"); out != "" {
		t.Errorf("edge down left %s", out)
	}
}
