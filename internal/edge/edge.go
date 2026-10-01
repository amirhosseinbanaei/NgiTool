// Package edge is NgiTool's own nginx stack: nginx plus certbot in Docker
// Compose, written out from assets embedded in the binary (assets/). It
// knows the stack's directory layout, its .env and Cloudflare token file,
// and how to run compose against it with an explicit project name.
//
//	<dir>/compose.yaml            nginx + certbot
//	<dir>/.env                    ACME_EMAIL, ports, EDGE_NETWORK, SERVER_IP …
//	<dir>/.ngitool-edge           marks the directory as an edge stack
//	<dir>/conf/                   nginx config, mounted at /etc/nginx/edge
//	<dir>/www/                    static sites, mounted at /var/www
//	<dir>/data/{letsencrypt,certs,acme}/
//	<dir>/secrets/cloudflare.ini  the Cloudflare token (0600, dir 0700)
//
// nginx files of routes are not written here: internal/apply does that.
package edge

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
)

//go:embed all:assets
var assets embed.FS

// DefaultDir is where `ngitool edge init` puts a new stack (open decision,
// recorded in AGENTS.md): /root is private, and /opt is the conventional
// place for a self-contained stack.
const DefaultDir = "/opt/ngitool/edge"

// DefaultProject is the compose project name of a new stack.
const DefaultProject = "edge"

// Paths inside the nginx container (compose.yaml's mounts).
const (
	ConfIn    = "/etc/nginx/edge"
	MainIn    = "/etc/nginx/edge/nginx.conf"
	WWWIn     = "/var/www"
	LEIn      = "/etc/letsencrypt"
	CertsIn   = "/etc/edge-certs"
	ACMEIn    = "/var/acme"
	AOPCAIn   = "/etc/nginx/edge/certs/cloudflare-origin-pull-ca.pem"
	TokenIn   = "/secrets/cloudflare.ini" // in the certbot container
	Placehold = "PASTE_TOKEN_HERE"
)

// Layout is where things are in a stack directory on this machine.
type Layout struct{ Dir string }

func (l Layout) p(parts ...string) string {
	return filepath.Join(append([]string{l.Dir}, parts...)...)
}

func (l Layout) Compose() string     { return l.p("compose.yaml") }
func (l Layout) Env() string         { return l.p(".env") }
func (l Layout) Marker() string      { return l.p(compose.EdgeMarker) }
func (l Layout) Conf() string        { return l.p("conf") }
func (l Layout) Sites() string       { return l.p("conf", "sites") }
func (l Layout) Locations() string   { return l.p("conf", "locations") }
func (l Layout) SSLSnippets() string { return l.p("conf", "snippets", "ssl") }
func (l Layout) RealIP() string      { return l.p("conf", "conf.d", "cloudflare-realip.conf") }
func (l Layout) AOPCA() string       { return l.p("conf", "certs", "cloudflare-origin-pull-ca.pem") }
func (l Layout) WWW() string         { return l.p("www") }
func (l Layout) LE() string          { return l.p("data", "letsencrypt") }
func (l Layout) Certs() string       { return l.p("data", "certs") }
func (l Layout) ACME() string        { return l.p("data", "acme") }
func (l Layout) Secrets() string     { return l.p("secrets") }
func (l Layout) Token() string       { return l.p("secrets", "cloudflare.ini") }
func (l Layout) LegacyState() string { return l.p("edge.json") }

// Dirs are the directories a stack needs, empty or not.
func (l Layout) Dirs() []string {
	return []string{l.Sites(), l.Locations(), l.p("conf", "certs"), l.WWW(), l.LE(), l.Certs(), l.ACME()}
}

// Asset is one embedded file of the stack.
type Asset struct {
	Path string // relative to the stack dir
	Mode os.FileMode
	Body []byte
}

// MachineState are paths of a stack that belong to this machine and are
// never replaced by upgrade-assets: what routes, certificates, cf-sync and
// people wrote.
var MachineState = []string{
	"conf/sites", "conf/locations", "conf/snippets/ssl", "conf/certs",
	"conf/conf.d/cloudflare-realip.conf", "conf/conf.d/ngitool.conf", "conf/snippets/ngitool-proxy.conf",
	"conf/ngitool", "data", "secrets", "www", ".env",
}

// IsMachineState reports whether rel (slash-separated) is machine state.
func IsMachineState(rel string) bool {
	for _, m := range MachineState {
		if rel == m || strings.HasPrefix(rel, m+"/") {
			return true
		}
	}
	return false
}

// Assets are the files `edge init` writes, sorted. The legacy templates,
// env.example and the token example are not written: NgiTool renders from
// its own templates and writes .env and the token file itself.
func Assets() []Asset {
	var out []Asset
	_ = fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimPrefix(p, "assets/")
		if strings.HasPrefix(rel, "templates/") || strings.HasPrefix(rel, "secrets/") || rel == "env.example" {
			return nil
		}
		b, _ := assets.ReadFile(p)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		out = append(out, Asset{Path: rel, Mode: mode, Body: b})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Upgradable are the assets upgrade-assets may replace: every asset that
// is not machine state (00-default.conf lives in conf/sites and is left
// alone once written).
func Upgradable() []Asset {
	var out []Asset
	for _, a := range Assets() {
		if !IsMachineState(a.Path) {
			out = append(out, a)
		}
	}
	return out
}

// Template is one embedded template (templates/<name>), for the legacy
// renderer the migration compares against and for placeholder pages.
func Template(name string) string {
	b, err := assets.ReadFile("assets/templates/" + name)
	if err != nil {
		panic("edge: no template " + name)
	}
	return string(b)
}

// File is an embedded asset by its relative path.
func File(rel string) []byte {
	b, _ := assets.ReadFile(path.Join("assets", rel))
	return b
}

// Fill replaces {{KEY}}s like the legacy CLI's render(): a line holding
// nothing but an empty {{KEY}} is dropped entirely.
func Fill(text string, vars [][2]string) string {
	out := text
	for _, kv := range vars {
		key, val := kv[0], kv[1]
		if val == "" {
			re := regexp.MustCompile(`(?m)^[ \t]*\{\{` + regexp.QuoteMeta(key) + `\}\}[ \t]*\n`)
			out = re.ReplaceAllString(out, "")
		}
		out = strings.ReplaceAll(out, "{{"+key+"}}", val)
	}
	return out
}

// IndexHTML is the placeholder page a new static folder gets.
func IndexHTML(name, dir string) string {
	return Fill(Template("index.html.tpl"), [][2]string{{"NAME", name}, {"DIR", dir}})
}

// ── .env ────────────────────────────────────────────────────────────────────

// EnvDefaults are the stack settings when .env does not say (the legacy
// cli/src/config.mjs defaults).
var EnvDefaults = map[string]string{
	"ACME_EMAIL":             "",
	"HTTP_PORT":              "80",
	"HTTPS_PORT":             "443",
	"EDGE_NETWORK":           "edge",
	"SERVER_IP":              "",
	"CF_PROPAGATION_SECONDS": "30",
}

var envLineRE = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)

// ParseEnv reads KEY=value lines, strips one pair of quotes, skips
// comments.
func ParseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		m := envLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v := m[2]
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		out[m[1]] = v
	}
	return out
}

// SetEnvText replaces KEY=… in place (keeping comments and order),
// uncomments a commented-out KEY, or appends it.
func SetEnvText(text, key, value string) string {
	re := regexp.MustCompile(`(?m)^(\s*#?\s*)` + regexp.QuoteMeta(key) + `\s*=.*$`)
	line := key + "=" + value
	if loc := re.FindStringIndex(text); loc != nil {
		return text[:loc[0]] + line + text[loc[1]:]
	}
	return strings.TrimRight(text, "\n") + strings.Repeat("\n", min(1, len(text))) + line + "\n"
}

// Env is the stack's settings: .env over the defaults.
type Env map[string]string

// ReadEnv reads <dir>/.env with the defaults filled in.
func ReadEnv(dir string) Env {
	e := Env{}
	for k, v := range EnvDefaults {
		e[k] = v
	}
	if b, err := os.ReadFile(Layout{dir}.Env()); err == nil {
		for k, v := range ParseEnv(string(b)) {
			e[k] = v
		}
	}
	return e
}

// WriteEnv sets values in <dir>/.env, starting from the embedded example
// when the file does not exist yet. Written 0600: it names the ACME email.
func WriteEnv(dir string, values [][2]string) error {
	l := Layout{dir}
	text := string(File("env.example"))
	if b, err := os.ReadFile(l.Env()); err == nil {
		text = string(b)
	}
	for _, kv := range values {
		text = SetEnvText(text, kv[0], kv[1])
	}
	return state.WriteFile(l.Env(), []byte(text), state.FileMode)
}

// ── Cloudflare token ────────────────────────────────────────────────────────

var tokenRE = regexp.MustCompile(`(?m)^\s*dns_cloudflare_api_token\s*=\s*(\S+)`)

// ReadToken returns the token in secrets/cloudflare.ini, or "" when there
// is none or it is the placeholder. Callers never print or store it.
func ReadToken(dir string) string {
	b, err := os.ReadFile(Layout{dir}.Token())
	if err != nil {
		return ""
	}
	m := tokenRE.FindSubmatch(b)
	if m == nil || string(m[1]) == Placehold {
		return ""
	}
	return string(m[1])
}

// WriteToken writes secrets/cloudflare.ini (0600 in a 0700 dir); an empty
// token writes the placeholder, so certbot's mount is always a file.
func WriteToken(dir, token string) error {
	l := Layout{dir}
	if err := os.MkdirAll(l.Secrets(), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(l.Secrets(), 0o700); err != nil {
		return err
	}
	if token == "" {
		token = Placehold
	}
	body := "# Cloudflare API token — used by certbot (DNS-01) and by NgiTool (DNS records, Origin CA).\n" +
		"dns_cloudflare_api_token = " + token + "\n"
	return state.WriteFile(l.Token(), []byte(body), 0o600)
}

// ── writing the stack ──────────────────────────────────────────────────────

// WriteResult is what Write did per file.
type WriteResult struct {
	Written, Kept, Same []string
}

// Write puts every asset into dir: missing files are written, identical
// ones counted as Same, different ones kept (upgrade-assets replaces them,
// after showing the diff). Then the directories, the marker, a placeholder
// token file and .env from the example when missing.
func Write(dir string) (WriteResult, error) {
	var r WriteResult
	l := Layout{dir}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return r, err
	}
	for _, a := range Assets() {
		dst := filepath.Join(dir, filepath.FromSlash(a.Path))
		if old, err := os.ReadFile(dst); err == nil {
			if bytes.Equal(old, a.Body) {
				r.Same = append(r.Same, a.Path)
			} else {
				r.Kept = append(r.Kept, a.Path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return r, err
		}
		if err := state.WriteFile(dst, a.Body, a.Mode); err != nil {
			return r, err
		}
		r.Written = append(r.Written, a.Path)
	}
	for _, d := range l.Dirs() {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return r, err
		}
	}
	if _, err := os.Stat(l.Marker()); err != nil {
		if err := WriteMarker(dir); err != nil {
			return r, err
		}
	}
	if _, err := os.Stat(l.Token()); err != nil {
		if err := WriteToken(dir, ""); err != nil {
			return r, err
		}
	}
	if _, err := os.Stat(l.Env()); err != nil {
		if err := WriteEnv(dir, nil); err != nil {
			return r, err
		}
	}
	return r, nil
}

// WriteMarker marks dir as an edge stack NgiTool runs (discovery's rule
// after the legacy CLI is gone).
func WriteMarker(dir string) error {
	return state.WriteFile(Layout{dir}.Marker(), []byte("NgiTool edge stack: run it with `ngitool edge …`, not as an app.\n"), 0o644)
}

// AssetDiff is one upgradable asset that differs from the embedded one.
type AssetDiff struct {
	Path     string
	Old, New string
	Missing  bool
}

// Outdated lists upgradable assets that differ on disk.
func Outdated(dir string) []AssetDiff {
	var out []AssetDiff
	for _, a := range Upgradable() {
		dst := filepath.Join(dir, filepath.FromSlash(a.Path))
		old, err := os.ReadFile(dst)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			out = append(out, AssetDiff{Path: a.Path, New: string(a.Body), Missing: true})
		case err == nil && !bytes.Equal(old, a.Body):
			out = append(out, AssetDiff{Path: a.Path, Old: string(old), New: string(a.Body)})
		}
	}
	return out
}

// SafeWWW returns www/<dir> on disk, refusing anything that escapes www/.
func SafeWWW(stackDir, dir string) (string, error) {
	if !ValidWWW(dir) {
		return "", fmt.Errorf("invalid folder: %s", dir)
	}
	root := Layout{stackDir}.WWW()
	abs := filepath.Join(root, filepath.FromSlash(dir))
	if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to touch %s", abs)
	}
	return abs, nil
}

var wwwRE = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ValidWWW: letters, digits . _ - and slashes, never "..".
func ValidWWW(dir string) bool {
	if !wwwRE.MatchString(dir) {
		return false
	}
	for _, part := range strings.Split(dir, "/") {
		if part == ".." || part == "." {
			return false
		}
	}
	return true
}

// NormalizeWWW turns "www/blog/" into "blog".
func NormalizeWWW(s string) string {
	return strings.TrimRight(strings.TrimPrefix(strings.TrimSpace(s), "www/"), "/")
}

// EnsureStatic creates www/<dir> with a placeholder page when it is empty.
func EnsureStatic(stackDir, dir, name string) (bool, error) {
	abs, err := SafeWWW(stackDir, dir)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return false, err
	}
	ents, err := os.ReadDir(abs)
	if err != nil || len(ents) > 0 {
		return false, err
	}
	return true, os.WriteFile(filepath.Join(abs, "index.html"), []byte(IndexHTML(name, dir)), 0o644)
}

// WWWDir is a folder in www/ and the routes that serve it.
type WWWDir struct {
	Dir    string   `json:"dir"`
	Routes []string `json:"routes"`
}

// ListWWW lists the folders in www/ (top level), with users from used.
func ListWWW(stackDir string, used map[string][]string) []WWWDir {
	ents, _ := os.ReadDir(Layout{stackDir}.WWW())
	seen := map[string]bool{}
	var out []WWWDir
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		seen[e.Name()] = true
		out = append(out, WWWDir{Dir: e.Name(), Routes: used[e.Name()]})
	}
	for d, rs := range used {
		if !seen[d] && !strings.Contains(d, "/") {
			out = append(out, WWWDir{Dir: d, Routes: rs})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}
