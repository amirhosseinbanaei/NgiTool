package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

// sandbox points every path at a temp dir and captures output.
func sandbox(t *testing.T) (*env, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(paths.EnvRoot, filepath.Join(dir, "root"))
	t.Setenv(paths.EnvPrefix, filepath.Join(dir, "prefix"))
	t.Setenv(EnvNoCheck, "1")
	var buf bytes.Buffer
	prevOut, prevErr := ui.Out, ui.Errw
	ui.Out, ui.Errw = &buf, &buf
	t.Cleanup(func() { ui.Out, ui.Errw = prevOut, prevErr })
	return &env{paths: paths.Get()}, &buf
}

func execute(t *testing.T, e *env, args ...string) (error, int) {
	t.Helper()
	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(ui.Out)
	root.SetErr(ui.Errw)
	cmd, err := root.ExecuteC()
	return err, report(err, cmd)
}

func TestHelpRendersEveryGroup(t *testing.T) {
	e, _ := sandbox(t)
	root := newRoot(e)
	help := renderHelp(root)
	seen := 0
	for _, g := range groups {
		var cmds []string
		for _, c := range root.Commands() {
			if !c.Hidden && c.Annotations[annGroup] == g.ID {
				cmds = append(cmds, "ngitool "+synopsis(c))
			}
		}
		if len(cmds) == 0 {
			continue
		}
		seen++
		if !strings.Contains(help, g.Title) {
			t.Errorf("group %s missing from help", g.Title)
		}
		for _, c := range cmds {
			if !strings.Contains(help, c) {
				t.Errorf("%q missing from help", c)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no groups rendered")
	}
	for _, s := range []string{"USAGE", "FLAGS", "--yes", "--no-color"} {
		if !strings.Contains(help, s) {
			t.Errorf("%s missing from help", s)
		}
	}
	// Every visible command belongs to a group that exists.
	ids := map[string]bool{}
	for _, g := range groups {
		ids[g.ID] = true
	}
	for _, c := range root.Commands() {
		if !c.Hidden && c.Name() != "help" && !ids[c.Annotations[annGroup]] {
			t.Errorf("command %s has no known group", c.Name())
		}
	}
	// Menu items point at real commands.
	for _, it := range menuItems {
		if c, _, err := root.Find(it.Args); err != nil || c == root {
			t.Errorf("menu item %q runs unknown %v", it.Label, it.Args)
		}
	}
}

func TestHelpSubcommand(t *testing.T) {
	e, buf := sandbox(t)
	if err, code := execute(t, e, "help", "update"); code != 0 {
		t.Fatalf("help update: %v", err)
	}
	for _, s := range []string{"ngitool update", "--rollback", "--version vX.Y.Z", "GLOBAL FLAGS", "EXAMPLES"} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("%q missing from:\n%s", s, buf.String())
		}
	}
}

func TestNoArgsOffTTYPrintsHelp(t *testing.T) {
	e, buf := sandbox(t)
	if _, code := execute(t, e); code != 0 || !strings.Contains(buf.String(), "USAGE") {
		t.Fatalf("code %d, output:\n%s", code, buf.String())
	}
}

func TestMissingValueOffTTYNamesTheFlag(t *testing.T) {
	e, buf := sandbox(t)
	err, code := execute(t, e, "completion")
	var missing *ui.MissingError
	if !errors.As(err, &missing) || code != ExitUsage || !strings.Contains(buf.String(), "bash|zsh|fish") {
		t.Fatalf("completion: %v (exit %d)\n%s", err, code, buf.String())
	}

	// A confirmation is a value too: --yes, and --yes --force for purges.
	bin := filepath.Join(paths.BinDir(), "ngitool")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(bin, []byte("x"), 0o755)
	buf.Reset()
	if _, code := execute(t, e, "uninstall"); code != ExitUsage || !strings.Contains(buf.String(), "pass --yes") {
		t.Fatalf("uninstall without --yes: exit %d\n%s", code, buf.String())
	}
	buf.Reset()
	if _, code := execute(t, e, "uninstall", "--purge", "--yes"); code != ExitUsage || !strings.Contains(buf.String(), "--yes --force") {
		t.Fatalf("purge without --force: exit %d\n%s", code, buf.String())
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatal("binary removed without confirmation")
	}
}

func TestUnknownFlagIsUsageError(t *testing.T) {
	e, _ := sandbox(t)
	if _, code := execute(t, e, "version", "--nope"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
	if _, code := execute(t, e, "nope"); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestVersionJSON(t *testing.T) {
	e, buf := sandbox(t)
	if _, code := execute(t, e, "version", "--json"); code != 0 {
		t.Fatal(buf.String())
	}
	var info version.Info
	if err := json.Unmarshal(buf.Bytes(), &info); err != nil || info.Version == "" || info.Go == "" {
		t.Fatalf("%v: %s", err, buf.String())
	}
}

func TestUpdateCheckExitCodes(t *testing.T) {
	e, buf := sandbox(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"v0.2.0","body":"- feat: something new"},{"tag_name":"v0.3.0-rc.1","prerelease":true},{"tag_name":"v0.1.0"}]`)
	}))
	defer srv.Close()
	t.Setenv("NGITOOL_RELEASES_URL", srv.URL)
	prev := version.Version
	t.Cleanup(func() { version.Version = prev })

	version.Version = "v0.1.0"
	if _, code := execute(t, e, "update", "--check"); code != ExitUpdate {
		t.Fatalf("older: exit %d\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "v0.2.0") || !strings.Contains(buf.String(), "feat: something new") {
		t.Errorf("check output:\n%s", buf.String())
	}
	buf.Reset()
	if _, code := execute(t, e, "update", "--check", "--json"); code != ExitUpdate {
		t.Fatalf("json: exit %d", code)
	}
	var rep checkReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil || !rep.UpdateAvailable || rep.Latest != "v0.2.0" {
		t.Fatalf("json report %+v %v\n%s", rep, err, buf.String())
	}

	version.Version = "v0.2.0"
	if _, code := execute(t, e, "update", "--check"); code != 0 {
		t.Fatalf("current: exit %d", code)
	}
	if _, code := execute(t, e, "update", "--check", "--prerelease"); code != ExitUpdate {
		t.Fatalf("prerelease channel: exit %d", code)
	}
	buf.Reset()
	if _, code := execute(t, e, "update", "--version", "v9.9.9", "--yes"); code != ExitFail || !strings.Contains(buf.String(), "no release v9.9.9") {
		t.Fatalf("unknown version: exit %d\n%s", code, buf.String())
	}
}

func TestUninstallRemovesOnlyWhatItLists(t *testing.T) {
	e, buf := sandbox(t)
	bin := filepath.Join(paths.BinDir(), "ngitool")
	_ = os.MkdirAll(filepath.Dir(bin), 0o755)
	_ = os.WriteFile(bin, []byte("x"), 0o755)
	_ = os.Symlink("ngitool", filepath.Join(filepath.Dir(bin), "ngt"))
	_ = os.WriteFile(filepath.Join(filepath.Dir(bin), "other"), []byte("keep"), 0o755)
	for _, d := range e.paths.Dirs() {
		_ = os.MkdirAll(d, 0o700)
	}
	if _, code := execute(t, e, "uninstall", "--yes"); code != 0 {
		t.Fatal(buf.String())
	}
	if exists(bin) || exists(filepath.Join(filepath.Dir(bin), "ngt")) || !exists(filepath.Join(filepath.Dir(bin), "other")) {
		t.Fatal("wrong files removed")
	}
	if !exists(e.paths.Lib) {
		t.Fatal("state removed without --purge")
	}
	if _, code := execute(t, e, "uninstall", "--purge", "--yes", "--force"); code != 0 {
		t.Fatal(buf.String())
	}
	for _, d := range e.paths.Dirs() {
		if exists(d) {
			t.Errorf("%s survived --purge", d)
		}
	}
}

func TestSafeToPurge(t *testing.T) {
	t.Setenv(paths.EnvRoot, "")
	for d, want := range map[string]bool{"/etc/ngitool": true, "/var/lib/ngitool": true, "/": false, "/etc": false, "relative/ngitool": false, "/home/user": false} {
		if safeToPurge(d) != want {
			t.Errorf("safeToPurge(%q) = %v", d, !want)
		}
	}
}
