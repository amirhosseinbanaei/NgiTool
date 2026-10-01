package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/migrate"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// cli/src/main.mjs:464-469: 5xx red, 4xx yellow, 3xx cyan, 2xx green.
func TestColorLogLine(t *testing.T) {
	ui.SetColor(true)
	defer ui.SetColor(false)
	line := `203.0.113.7 api.example.com "GET / HTTP/1.1" 502 157 0.003s up=172.30.0.5:3000 "-" "curl" ray=-`
	got := ColorLogLine(line)
	if got == line || !strings.Contains(got, ui.Err("502")) {
		t.Errorf("5xx not coloured: %q", got)
	}
	if got := ColorLogLine(strings.Replace(line, " 502 ", " 404 ", 1)); !strings.Contains(got, ui.Warn("404")) {
		t.Errorf("4xx: %q", got)
	}
	if got := ColorLogLine("no status here"); got != "no status here" {
		t.Errorf("plain line changed: %q", got)
	}
	var f logFilter
	f.host = "api.example.com"
	out := captureOut(t, func() {
		_, _ = f.Write([]byte(line + "\n" + strings.Replace(line, "api.example.com", "www.example.com", 1) + "\n"))
	})
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "api.example.com") {
		t.Errorf("host filter:\n%s", out)
	}
}

func captureOut(t *testing.T, fn func()) string {
	t.Helper()
	var b strings.Builder
	prev := ui.Out
	ui.Out = &b
	defer func() { ui.Out = prev }()
	fn()
	return b.String()
}

// cli/src/remove.mjs flowRemove off a terminal: a cascade needs --force,
// reset needs --yes --force; nothing changes when refused.
func TestRemovalNeedsForceOffATerminal(t *testing.T) {
	e, buf := sandbox(t)
	st := &model.State{Schema: 4}
	in := "edge:/srv/edge"
	st.Pools = []model.Pool{
		{Name: "a", Instance: in, Method: model.RoundRobin, Scheme: "http", Members: []model.Member{{Kind: model.KindContainer, Ref: "web", Port: 80}}},
		{Name: "b", Instance: in, Method: model.RoundRobin, Scheme: "http", Members: []model.Member{{Kind: model.KindContainer, Ref: "api", Port: 80}}},
	}
	st.Routes = []model.Route{
		{ID: "example.com", Instance: in, Host: "example.com", Pool: "a", Enabled: true, HTTP: model.HTTPServe},
		{ID: "example.com/api", Instance: in, Host: "example.com", Path: "/api", Pool: "b", Enabled: true, HTTP: model.HTTPServe},
	}
	if err := model.Save(e.paths, st); err != nil {
		t.Fatal(err)
	}
	if _, code := execute(t, e, "rm", "example.com", "--yes"); code != ExitUsage || !strings.Contains(buf.String(), "add --force") {
		t.Errorf("cascade without --force: %d\n%s", code, buf.String())
	}
	buf.Reset()
	if _, code := execute(t, e, "reset", "--yes"); code != ExitUsage || !strings.Contains(buf.String(), "--yes --force") {
		t.Errorf("reset without --force: %d\n%s", code, buf.String())
	}
	buf.Reset()
	if _, code := execute(t, e, "rm", "--www", "../etc"); code != ExitUsage || !strings.Contains(buf.String(), "invalid folder") {
		t.Errorf("www escape: %d\n%s", code, buf.String())
	}
	after, _ := model.Load(e.paths)
	if len(after.Routes) != 2 {
		t.Error("a refused removal changed state")
	}
}

func TestEdgeAndCertCommandsAreInTheHelp(t *testing.T) {
	e, _ := sandbox(t)
	help := renderHelp(newRoot(e))
	for _, want := range []string{"EDGE STACK", "CERTIFICATES & DNS", "ngitool edge init", "ngitool cert ls", "ngitool certbot", "ngitool www ls", "ngitool rm", "ngitool reset", "ngitool migrate edge"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	// Every menu group has items, and every item names a real command.
	root := newRoot(e)
	for _, g := range groups {
		if len(itemsOf(g.ID)) == 0 {
			t.Errorf("group %s has no menu items", g.ID)
		}
	}
	for _, it := range menuItems {
		if c, _, err := root.Find(it.Args); err != nil || c == root {
			t.Errorf("menu item %q runs no command (%v)", it.Label, it.Args)
		}
	}
}

// okRunner answers every docker call with success and no output.
type okRunner struct{}

func (okRunner) Run(context.Context, string, []string, execx.Opts) execx.Result {
	return execx.Result{}
}

// `migrate edge --dry-run --json` prints only JSON (docs/ux.md §6), and
// a stack that renders the same is safe.
func TestMigrateDryRunJSONIsOnlyJSON(t *testing.T) {
	e, buf := sandbox(t)
	e.fixed = &discover.Report{}
	prev := edgeRunner
	edgeRunner = okRunner{}
	t.Cleanup(func() { edgeRunner = prev })
	dir := filepath.Join(t.TempDir(), "nginx-edge")
	if _, err := edge.Write(dir); err != nil {
		t.Fatal(err)
	}
	st := migrate.EdgeJSON{Version: 1,
		Certs: map[string]migrate.LCert{"example.com": {Type: "self-signed", Names: []string{"example.com"}}},
		Sites: map[string]migrate.LSite{"example.com": {Cert: "example.com", Source: migrate.LSource{Type: "static", Dir: "site"}}}}
	files, err := migrate.BuildLegacy(st)
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		touchFile(t, filepath.Join(dir, rel), body)
	}
	b, _ := json.Marshal(st)
	touchFile(t, filepath.Join(dir, "edge.json"), string(b))
	chain, key, _ := certs.SelfSignedPair([]string{"example.com"}, 30, time.Now())
	if err := certs.Store(filepath.Join(dir, "data", "certs", "example.com"), chain, key); err != nil {
		t.Fatal(err)
	}
	touchFile(t, filepath.Join(dir, "www", "site", "index.html"), "hi\n")
	_, code := execute(t, e, "migrate", "edge", dir, "--dry-run", "--json")
	var res struct {
		Test     string            `json:"test"`
		Blockers []json.RawMessage `json:"blockers"`
		Diffs    []json.RawMessage `json:"differences"`
	}
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("not only JSON: %v\n%s", err, buf.String())
	}
	if code != ExitOK || res.Test != "ok" || len(res.Blockers) != 0 || len(res.Diffs) != 0 {
		t.Errorf("exit %d, %+v\n%s", code, res, buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "conf", "conf.d", "ngitool.conf")); err == nil {
		t.Error("a dry run wrote into the directory")
	}
}
