package edge

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
)

// legacy: "parseEnv reads values, strips quotes, skips comments"
func TestParseEnv(t *testing.T) {
	got := ParseEnv("# c\nA=1\nB=\"two\"\n  C = x \n")
	if !reflect.DeepEqual(got, map[string]string{"A": "1", "B": "two", "C": "x"}) {
		t.Errorf("%v", got)
	}
}

// legacy: "setEnvText replaces in place, uncomments, or appends"
func TestSetEnvText(t *testing.T) {
	for _, c := range [][4]string{
		{"# top\nA=1\nB=2\n", "A", "9", "# top\nA=9\nB=2\n"},
		{"#A=1\n", "A", "2", "A=2\n"},
		{"B=2", "A", "1", "B=2\nA=1\n"},
	} {
		if got := SetEnvText(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("SetEnvText(%q, %s=%s) = %q, want %q", c[0], c[1], c[2], got, c[3])
		}
	}
}

// legacy: "render drops lines that hold only an empty token"
func TestFillDropsLinesOfEmptyTokens(t *testing.T) {
	if got := Fill("a\n    {{X}}\nb {{Y}}\n", [][2]string{{"X", ""}, {"Y", "$1 & $&"}}); got != "a\nb $1 & $&\n" {
		t.Errorf("%q", got)
	}
}

// legacy: "shared snippets: no HSTS in security-headers, scheme-aware X-Forwarded-Proto"
func TestSharedSnippets(t *testing.T) {
	if regexp.MustCompile(`(?m)^add_header Strict-Transport-Security`).Match(File("conf/snippets/security-headers.conf")) {
		t.Error("HSTS is per site, never in security-headers.conf")
	}
	if !strings.Contains(string(File("conf/snippets/proxy.conf")), "X-Forwarded-Proto $scheme;") {
		t.Error("proxy.conf forwards the real scheme")
	}
	if !strings.Contains(string(File("conf/conf.d/websocket.conf")), "''      '';") {
		t.Error(`websocket.conf gives "" without an Upgrade header (LB-06)`)
	}
}

func TestAssetsAndMachineState(t *testing.T) {
	var paths []string
	for _, a := range Assets() {
		paths = append(paths, a.Path)
		if strings.HasPrefix(a.Path, "templates/") || strings.HasPrefix(a.Path, "secrets/") {
			t.Errorf("%s is not written into a stack", a.Path)
		}
		if strings.Contains(string(a.Body), "Managed by edge") && !strings.HasPrefix(a.Path, "templates/") {
			t.Errorf("%s carries the legacy marker", a.Path)
		}
	}
	for _, want := range []string{"compose.yaml", "conf/nginx.conf", "conf/start.sh", "conf/sites/00-default.conf", "conf/snippets/acme-challenge.conf"} {
		found := false
		for _, p := range paths {
			found = found || p == want
		}
		if !found {
			t.Errorf("missing asset %s", want)
		}
	}
	for _, a := range Upgradable() {
		if IsMachineState(a.Path) {
			t.Errorf("%s is machine state", a.Path)
		}
	}
	for _, p := range []string{"conf/sites/00-default.conf", "conf/locations/x/y.conf", "conf/snippets/ssl/a.conf", "data/certs/x", "secrets/cloudflare.ini", "www/site", ".env", "conf/conf.d/cloudflare-realip.conf"} {
		if !IsMachineState(p) {
			t.Errorf("%s must be machine state", p)
		}
	}
	if Image(t.TempDir()) != "" {
		t.Error("no compose.yaml, no image")
	}
}

func TestWriteKeepsFilesAndMarksTheStack(t *testing.T) {
	dir := t.TempDir()
	r, err := Write(dir)
	if err != nil || len(r.Written) != len(Assets()) {
		t.Fatalf("write: %+v %v", r, err)
	}
	if !compose.EdgeStack(dir) {
		t.Error("discovery does not recognise the stack (marker)")
	}
	if Image(dir) != "nginx:stable-alpine" {
		t.Errorf("image: %q", Image(dir))
	}
	for _, d := range []string{"www", "data/acme", "data/certs", "data/letsencrypt", "conf/locations"} {
		if st, err := os.Stat(filepath.Join(dir, d)); err != nil || !st.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(dir, "secrets")); st.Mode().Perm() != 0o700 {
		t.Errorf("secrets/ %v", st.Mode().Perm())
	}
	if ReadToken(dir) != "" {
		t.Error("the placeholder is no token")
	}
	if len(Outdated(dir)) != 0 {
		t.Errorf("fresh stack outdated: %+v", Outdated(dir))
	}
	// A changed nginx.conf is kept by Write and shown by Outdated; a
	// changed site is machine state and never shown.
	conf := filepath.Join(dir, "conf", "nginx.conf")
	_ = os.WriteFile(conf, []byte("# mine\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "conf", "sites", "00-default.conf"), []byte("# mine\n"), 0o644)
	r, _ = Write(dir)
	if len(r.Kept) != 2 || len(r.Written) != 0 {
		t.Errorf("rewrite: %+v", r)
	}
	if b, _ := os.ReadFile(conf); string(b) != "# mine\n" {
		t.Error("Write replaced a changed file")
	}
	d := Outdated(dir)
	if len(d) != 1 || d[0].Path != "conf/nginx.conf" || d[0].Old != "# mine\n" {
		t.Errorf("outdated: %+v", d)
	}
}

func TestEnvAndToken(t *testing.T) {
	dir := t.TempDir()
	if e := ReadEnv(dir); e["HTTP_PORT"] != "80" || e["EDGE_NETWORK"] != "edge" || e["CF_PROPAGATION_SECONDS"] != "30" {
		t.Errorf("defaults: %v", e)
	}
	if err := WriteEnv(dir, [][2]string{{"ACME_EMAIL", "a@example.com"}, {"HTTP_PORT", "8080"}}); err != nil {
		t.Fatal(err)
	}
	e := ReadEnv(dir)
	if e["ACME_EMAIL"] != "a@example.com" || e["HTTP_PORT"] != "8080" || e["HTTPS_PORT"] != "443" {
		t.Errorf("env: %v", e)
	}
	if st, _ := os.Stat(filepath.Join(dir, ".env")); st.Mode().Perm() != 0o600 {
		t.Errorf(".env %v", st.Mode().Perm())
	}
	if err := WriteToken(dir, "secret-token"); err != nil {
		t.Fatal(err)
	}
	if ReadToken(dir) != "secret-token" {
		t.Error("token")
	}
	if st, _ := os.Stat(filepath.Join(dir, "secrets", "cloudflare.ini")); st.Mode().Perm() != 0o600 {
		t.Errorf("token file %v", st.Mode().Perm())
	}
}

// legacy: sourceFromFlags' static dir rules ("www/blog/" → blog, no escapes)
func TestStaticFolders(t *testing.T) {
	if NormalizeWWW("www/blog/") != "blog" {
		t.Error("NormalizeWWW")
	}
	for _, bad := range []string{"../etc", "a/../b", "/abs", "a b", ""} {
		if ValidWWW(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	dir := t.TempDir()
	made, err := EnsureStatic(dir, "example.com", "example.com")
	if err != nil || !made {
		t.Fatalf("EnsureStatic: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "www", "example.com", "index.html"))
	if !strings.Contains(string(b), "example.com is served by NgiTool") {
		t.Errorf("placeholder: %s", b)
	}
	if made, _ := EnsureStatic(dir, "example.com", "x"); made {
		t.Error("a folder with files keeps them")
	}
	if _, err := SafeWWW(dir, "../x"); err == nil {
		t.Error("SafeWWW escape")
	}
	got := ListWWW(dir, map[string][]string{"example.com": {"example.com"}, "gone": {"x.example.com"}})
	if len(got) != 2 || got[0].Dir != "example.com" || got[1].Dir != "gone" {
		t.Errorf("ListWWW: %+v", got)
	}
}

func TestNetworksDuplicatesAndConflicts(t *testing.T) {
	ns := ParseNetworks(`[{"Name":"edge","Id":"0123456789abcdef","Driver":"bridge","IPAM":{"Config":[{"Subnet":"172.30.0.0/24","Gateway":"172.30.0.1"}]},"Options":{},"Labels":{"com.docker.compose.project":"shop"},"Containers":{"a":{"Name":"web"},"b":{"Name":"api"}}}]`)
	if len(ns) != 1 || ns[0].Gateway != "172.30.0.1" || ns[0].Bridge != "br-0123456789ab" || ns[0].Compose != "shop" || !reflect.DeepEqual(ns[0].Containers, []string{"api", "web"}) {
		t.Errorf("networks: %+v", ns)
	}
	s := Stack{Dir: "/srv/edge", Project: "edge"}
	dups := s.Duplicates([]compose.Ctr{{WorkDir: "/srv/edge", Project: "edge"}, {WorkDir: "/srv/edge", Project: "copy"}, {WorkDir: "/srv/other", Project: "x"}})
	if !reflect.DeepEqual(dups, []string{"copy"}) {
		t.Errorf("EDGE-03: %v", dups)
	}
	env := Env{"HTTP_PORT": "80", "HTTPS_PORT": "443"}
	got := Conflicts(env, "edge-nginx-1", map[int][2]string{80: {"nginx (pid 12)", ""}, 443: {"container edge-nginx-1", "edge-nginx-1"}})
	if len(got) != 1 || got[0].Port != 80 {
		t.Errorf("EDGE-01: %+v", got)
	}
	if c := (Stack{Dir: "/srv/edge", Project: "edge"}).CommandLine("up", "-d"); c != "docker compose -p edge --project-directory /srv/edge -f /srv/edge/compose.yaml up -d" {
		t.Errorf("command: %s", c)
	}
}
