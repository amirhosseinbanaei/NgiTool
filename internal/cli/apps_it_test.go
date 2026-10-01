package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

const (
	itApp1 = "ngitool-it-app1"
	itApp2 = "ngitool-it-app2"
)

// TestIntegrationComposeApps runs against the real Docker daemon, gated by
// NGITOOL_IT=1. It creates only its own things: network ngitool-it, the
// front ngitool-it-front on 127.0.0.1:18080, and two compose projects in a
// temp dir (ngitool-it-app1: compose.yaml + compose.prod.yaml;
// ngitool-it-app2: docker/compose.dev.yml with a tiny build). It links
// both through flags, attaches app1's web through the override, routes it,
// breaks it with a plain `docker compose up` (DOCK-07), fixes it, rebuilds
// app2 with --no-cache and takes both down with -v. No other project,
// network or container is touched.
func TestIntegrationComposeApps(t *testing.T) {
	if os.Getenv("NGITOOL_IT") != "1" {
		t.Skip("set NGITOOL_IT=1 to run against the real Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	os.Unsetenv("COMPOSE_PROJECT_NAME")
	os.Unsetenv("COMPOSE_FILE")

	const network, front = "ngitool-it", "ngitool-it-front"
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	dir := t.TempDir()
	app1, app2 := filepath.Join(dir, "app1"), filepath.Join(dir, "app2")
	files := map[string]string{
		"app1/compose.yaml":           "name: " + itApp1 + "\nservices:\n  web:\n    image: nginx:alpine\n    pull_policy: never\n    command:\n      - sh\n      - -c\n      - |\n        printf 'server { listen 80; location / { return 200 \"app1\\n\"; } }\\n' > /etc/nginx/conf.d/default.conf && exec nginx -g 'daemon off;'\n",
		"app1/compose.prod.yaml":      "services:\n  web:\n    labels:\n      it.variant: prod\n    volumes:\n      - data:/data\nvolumes:\n  data: {}\n",
		"app2/docker/compose.dev.yml": "name: " + itApp2 + "\nservices:\n  web:\n    build: .\n    image: " + itApp2 + "-web:it\n",
		"app2/docker/Dockerfile":      "FROM nginx:alpine\nRUN echo app2 > /usr/share/nginx/html/index.html\n",
	}
	for p, body := range files {
		touchFile(t, filepath.Join(dir, p), body)
	}
	cleanup := func() {
		for _, pr := range []struct{ name, dir, file string }{{itApp1, app1, "compose.yaml"}, {itApp2, filepath.Join(app2, "docker"), "compose.dev.yml"}} {
			c := exec.Command("docker", "compose", "-p", pr.name, "--project-directory", pr.dir, "-f", filepath.Join(pr.dir, pr.file), "down", "-v", "--remove-orphans")
			c.Dir = pr.dir
			_ = c.Run()
		}
		_ = exec.Command("docker", "rm", "-f", front).Run()
		_ = exec.Command("docker", "image", "rm", "-f", itApp2+"-web:it").Run()
	}
	cleanup()
	// The discover test may create the shared network between the inspect
	// and the create (packages run in parallel): "already exists" is fine.
	if exec.Command("docker", "network", "inspect", network).Run() != nil {
		if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil && !strings.Contains(string(out), "already exists") {
			t.Fatalf("docker network create %s: %v\n%s", network, err, out)
		}
	}
	t.Cleanup(func() {
		cleanup()
		// The discover test shares the network and usually finishes first,
		// unable to remove it while this one is attached: try anyway, it
		// fails harmlessly while anything else still uses it.
		_ = exec.Command("docker", "network", "rm", network).Run()
	})
	conf := filepath.Join(dir, "front-conf.d")
	if err := os.MkdirAll(conf, 0o755); err != nil {
		t.Fatal(err)
	}
	docker("run", "-d", "--name", front, "--network", network, "--pull", "never",
		"-p", "127.0.0.1:18080:80", "-v", conf+":/etc/nginx/conf.d", "nginx:stable-alpine")
	for i := 0; i < 50 && docker("inspect", "-f", "{{.State.Running}}", front) != "true"; i++ {
		time.Sleep(100 * time.Millisecond)
	}

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
		e.memo.rep = nil // every command sees the containers as they are now
		_, code := execute(t, e, args...)
		if code != want {
			t.Fatalf("ngitool %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, buf.String())
		}
		t.Logf("$ ngitool %s\n%s", strings.Join(args, " "), buf.String())
		return buf.String()
	}
	client := &http.Client{Timeout: 10 * time.Second}
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
	// waitFor polls until the route answers want (nginx re-resolves names
	// every 10 s).
	waitFor := func(host string, want int) (int, string) {
		t.Helper()
		var code int
		var body string
		for i := 0; i < 40; i++ {
			if code, body = get(host); code == want {
				return code, body
			}
			time.Sleep(500 * time.Millisecond)
		}
		return code, body
	}
	lsDrift := func(name string) string {
		t.Helper()
		var views []appJSON
		out := ngt(ExitOK, "app", "ls", "--json")
		if err := json.Unmarshal([]byte(out), &views); err != nil {
			t.Fatalf("app ls --json: %v\n%s", err, out)
		}
		for _, v := range views {
			if v.Name == name {
				return v.Drift.Status
			}
		}
		return "absent"
	}

	// Link both through flags.
	ngt(ExitOK, "app", "link", app1, "--file", "compose.yaml", "--file", "compose.prod.yaml", "--name", "app1", "--yes")
	ngt(ExitOK, "app", "link", filepath.Join(app2, "docker", "compose.dev.yml"), "--name", "app2", "--yes")
	out := ngt(ExitOK, "app", "scan", "--json")
	var cands []compose.Candidate
	_ = json.Unmarshal([]byte(out), &cands)
	// Projects known only from labels (outside the roots) are listed too.
	linked := 0
	for _, c := range cands {
		if strings.HasPrefix(c.Name, "ngitool-it-app") && c.Linked != "" {
			linked++
		}
	}
	if linked != 2 {
		t.Errorf("scan sees both sandbox projects linked: %d of %d candidates", linked, len(cands))
	}
	out = ngt(ExitOK, "app", "up", "app1", "--yes")
	if !strings.Contains(out, "Will run:") || !strings.Contains(out, "-f "+filepath.Join(app1, "compose.prod.yaml")) {
		t.Errorf("up shows the exact command:\n%s", out)
	}

	// Route it: web is not on ngitool-it, so --connect attaches it through
	// the override and recreates only web.
	ngt(ExitOK, "instance", "adopt", front, "--yes")
	ngt(ExitOK, "route", "add", "app1.it.example.com", "--instance", front, "--to", "app:app1/web:80", "--connect", "--yes")
	if code, body := waitFor("app1.it.example.com", 200); code != 200 || body != "app1" {
		t.Fatalf("through the front: %d %q", code, body)
	}
	if s := lsDrift("app1"); s != compose.NetOK {
		t.Errorf("drift after attach: %s", s)
	}

	// A plain docker compose up without the override: drift, then 502 (DOCK-07).
	plain := exec.Command("docker", "compose", "-p", itApp1, "--project-directory", app1, "-f", filepath.Join(app1, "compose.yaml"), "-f", filepath.Join(app1, "compose.prod.yaml"), "up", "-d", "--force-recreate")
	plain.Dir = app1
	if b, err := plain.CombinedOutput(); err != nil {
		t.Fatalf("plain up: %v\n%s", err, b)
	}
	if s := lsDrift("app1"); s != compose.NetDetached {
		t.Errorf("drift after a plain up: %s", s)
	}
	if code, _ := waitFor("app1.it.example.com", 502); code != 502 {
		t.Errorf("detached route answers %d, want 502", code)
	}
	if r := checkApps(t.Context(), e); r.Status != ui.StatusFail {
		t.Errorf("doctor: %+v", r)
	}

	// app fix recreates with the override: 200 again.
	out = ngt(ExitOK, "app", "fix", "app1", "--yes")
	if !strings.Contains(out, "--force-recreate --no-deps web") || !strings.Contains(out, filepath.Join(e.paths.Overrides, "app1.yaml")) {
		t.Errorf("fix command:\n%s", out)
	}
	if code, body := waitFor("app1.it.example.com", 200); code != 200 || body != "app1" {
		t.Errorf("after fix: %d %q", code, body)
	}
	if s := lsDrift("app1"); s != compose.NetOK {
		t.Errorf("drift after fix: %s", s)
	}

	// Rebuild a tiny build context with --no-cache.
	out = ngt(ExitOK, "app", "rebuild", "app2", "--no-cache", "--yes")
	if !strings.Contains(out, "build --no-cache") || !strings.Contains(out, "web") {
		t.Errorf("rebuild:\n%s", out)
	}
	if got := docker("inspect", "-f", "{{.State.Running}}", itApp2+"-web-1"); got != "true" {
		t.Errorf("app2 not running after rebuild: %s", got)
	}

	// down -v needs --yes --force off a terminal.
	ngt(ExitUsage, "app", "down", "app1", "--volumes", "--yes")
	ngt(ExitOK, "app", "down", "app1", "--volumes", "--yes", "--force")
	ngt(ExitOK, "app", "down", "app2", "--volumes", "--yes", "--force")
	if out := docker("volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+itApp1); out != "" {
		t.Errorf("volumes left after down -v: %s", out)
	}
	// 502 once nginx re-resolves; 504 while it still holds the old address.
	if code, _ := get("app1.it.example.com"); code != 502 && code != 504 {
		t.Errorf("route of a downed app answers %d", code)
	}
}
