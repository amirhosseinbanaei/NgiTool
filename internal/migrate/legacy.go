// Package migrate imports an nginx-edge directory (the legacy Node `edge`
// CLI's stack) into NgiTool: it reads .env, edge.json, apps/ and the token
// file, maps them onto routes, pools, certificates, domains and linked
// apps, renders NgiTool's files into a scratch copy of the config, tests
// it with nginx -t and compares the effective config with the one running
// now. Nothing in the directory changes until the CLI's real run.
package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
)

// LegacyMarker starts every file the legacy CLI wrote (cli/src/nginx.mjs:14).
const LegacyMarker = "# Managed by edge"

// EdgeJSON is edge.json (cli/src/config.mjs:93-111).
type EdgeJSON struct {
	Version int                `json:"version"`
	Certs   map[string]LCert   `json:"certs"`
	Domains map[string]LDomain `json:"domains"`
	Sites   map[string]LSite   `json:"sites"`
	Paths   map[string]LPath   `json:"paths"`
}

// LCert is one legacy certificate.
type LCert struct {
	Type      string   `json:"type"`
	Names     []string `json:"names"`
	Challenge string   `json:"challenge,omitempty"`
	AOP       bool     `json:"aop,omitempty"`
}

// LZone is a Cloudflare zone as edge.json stores it.
type LZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LDomain is one legacy domain.
type LDomain struct {
	Cert  string `json:"cert"`
	Zone  *LZone `json:"zone"`
	Added string `json:"added,omitempty"`
}

// LSource is where a legacy route sends traffic (cli/src/targets.mjs).
type LSource struct {
	Type     string `json:"type"` // app, container, port, static
	App      string `json:"app,omitempty"`
	Service  string `json:"service,omitempty"`
	Port     int    `json:"port,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Name     string `json:"name,omitempty"`
	IP       string `json:"ip,omitempty"`
	Dir      string `json:"dir,omitempty"`
}

// LSite is a host.
type LSite struct {
	Cert    string  `json:"cert"`
	Source  LSource `json:"source"`
	DNS     string  `json:"dns,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	Added   string  `json:"added,omitempty"`
	HTTP    string  `json:"http,omitempty"`
}

// On: a missing "enabled" is enabled.
func (s LSite) On() bool { return s.Enabled == nil || *s.Enabled }

// LPath is a path route.
type LPath struct {
	Host    string  `json:"host"`
	Path    string  `json:"path"`
	Source  LSource `json:"source"`
	Strip   bool    `json:"strip,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	Added   string  `json:"added,omitempty"`
}

// On: a missing "enabled" is enabled.
func (p LPath) On() bool { return p.Enabled == nil || *p.Enabled }

// LApp is apps/<name>/: a symlink to a project's compose file and maybe
// edge.override.yaml (cli/src/apps.mjs).
type LApp struct {
	Name       string `json:"name"`
	Link       string `json:"link,omitempty"`   // apps/<name>/<compose file>
	Target     string `json:"target,omitempty"` // the real compose file
	ProjectDir string `json:"projectDir,omitempty"`
	Override   string `json:"override,omitempty"`
	Broken     string `json:"broken,omitempty"`
	Meta       *LMeta `json:"meta,omitempty"`
}

// LMeta is the override's "# edge: {...}" line (cli/src/apps.mjs:186-194).
type LMeta struct {
	Network  string               `json:"network"`
	Services map[string]LAttached `json:"services"`
}

// LAttached is one service the override attached.
type LAttached struct {
	Alias string   `json:"alias"`
	Keys  []string `json:"keys"`
}

// Legacy is everything read from one nginx-edge directory. The Cloudflare
// token is never read into it: only whether one is set.
type Legacy struct {
	Dir      string
	Env      edge.Env
	State    EdgeJSON
	Apps     []LApp
	TokenSet bool
	// Files are the config files under conf/sites, conf/locations/*/ and
	// conf/snippets/ssl: rel path → content, split by the marker.
	Managed   map[string]string
	Unmanaged []string
}

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

var metaRE = regexp.MustCompile(`(?m)^# edge: (\{.*\})$`)

// Read reads dir. A missing edge.json is an empty state (a stack that was
// never used); an unreadable one is an error naming the file.
func Read(dir string) (*Legacy, error) {
	if err := edge.Check(dir); err != nil {
		return nil, err
	}
	l := &Legacy{Dir: dir, Env: edge.ReadEnv(dir), Managed: map[string]string{}}
	l.State = EdgeJSON{Version: 1, Certs: map[string]LCert{}, Domains: map[string]LDomain{}, Sites: map[string]LSite{}, Paths: map[string]LPath{}}
	b, err := os.ReadFile(filepath.Join(dir, "edge.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &l.State); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "edge.json"), err)
		}
		if l.State.Certs == nil {
			l.State.Certs = map[string]LCert{}
		}
		if l.State.Domains == nil {
			l.State.Domains = map[string]LDomain{}
		}
		if l.State.Sites == nil {
			l.State.Sites = map[string]LSite{}
		}
		if l.State.Paths == nil {
			l.State.Paths = map[string]LPath{}
		}
	}
	l.TokenSet = edge.ReadToken(dir) != ""
	l.Apps = readApps(filepath.Join(dir, "apps"))
	if err := l.readConf(); err != nil {
		return nil, err
	}
	return l, nil
}

func readApps(root string) []LApp {
	ents, _ := os.ReadDir(root)
	var out []LApp
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		a := LApp{Name: e.Name()}
		for _, n := range composeNames {
			p := filepath.Join(dir, n)
			if st, err := os.Lstat(p); err == nil && (st.Mode()&os.ModeSymlink != 0 || st.Mode().IsRegular()) {
				a.Link = p
				break
			}
		}
		switch {
		case a.Link == "":
			a.Broken = "no compose file or symlink in the folder"
		default:
			if t, err := filepath.EvalSymlinks(a.Link); err == nil {
				a.Target, a.ProjectDir = t, filepath.Dir(t)
			} else {
				dest, _ := os.Readlink(a.Link)
				a.Broken = "symlink points to a missing file (" + dest + ")"
			}
		}
		ov := filepath.Join(dir, "edge.override.yaml")
		if b, err := os.ReadFile(ov); err == nil {
			a.Override = ov
			if m := metaRE.FindSubmatch(b); m != nil {
				var meta LMeta
				if json.Unmarshal(m[1], &meta) == nil {
					a.Meta = &meta
				}
			}
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// readConf sorts the route files into legacy-managed and hand-written.
func (l *Legacy) readConf() error {
	conf := filepath.Join(l.Dir, "conf")
	var dirs []string
	dirs = append(dirs, filepath.Join(conf, "sites"), filepath.Join(conf, "snippets", "ssl"))
	locs, _ := os.ReadDir(filepath.Join(conf, "locations"))
	for _, d := range locs {
		if d.IsDir() {
			dirs = append(dirs, filepath.Join(conf, "locations", d.Name()))
		}
	}
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			p := filepath.Join(d, e.Name())
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(l.Dir, p)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(string(b), LegacyMarker) {
				l.Managed[rel] = string(b)
			} else {
				l.Unmanaged = append(l.Unmanaged, rel)
			}
		}
	}
	sort.Strings(l.Unmanaged)
	return nil
}

// App returns apps/<name>, or nil.
func (l *Legacy) App(name string) *LApp {
	for i := range l.Apps {
		if l.Apps[i].Name == name {
			return &l.Apps[i]
		}
	}
	return nil
}

// ── the legacy renderer (cli/src/nginx.mjs buildFiles) ─────────────────────

var certTypes = map[string]string{
	certs.LetsEncrypt: "Let's Encrypt", certs.Origin: "Cloudflare Origin CA", certs.Custom: "custom", certs.SelfSigned: "self-signed",
}

// LegacyCertPaths are a legacy certificate's paths inside the container.
func LegacyCertPaths(name string, c LCert) (crt, key string) {
	if c.Type == certs.LetsEncrypt {
		return edge.LEIn + "/live/" + name + "/fullchain.pem", edge.LEIn + "/live/" + name + "/privkey.pem"
	}
	return edge.CertsIn + "/" + name + "/fullchain.pem", edge.CertsIn + "/" + name + "/privkey.pem"
}

// Addr is a legacy source's host:port (cli/src/targets.mjs upstreamOf).
func (s LSource) Addr() string {
	switch s.Type {
	case "app":
		return s.Upstream + ":" + strconv.Itoa(s.Port)
	case "container":
		return s.Name + ":" + strconv.Itoa(s.Port)
	case "port":
		return s.IP + ":" + strconv.Itoa(s.Port)
	}
	return ""
}

// Describe is the legacy description (cli/src/targets.mjs describeSource).
func (s LSource) Describe() string {
	switch s.Type {
	case "app":
		return "app " + s.App + "/" + s.Service + ":" + strconv.Itoa(s.Port)
	case "container":
		return "container " + s.Name + ":" + strconv.Itoa(s.Port)
	case "port":
		return "host port " + strconv.Itoa(s.Port)
	case "static":
		return "static www/" + s.Dir
	}
	return "?"
}

const (
	hstsOn  = `add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;`
	hstsOff = `add_header Strict-Transport-Security "max-age=0" always;  # also served over plain HTTP`
)

// Slug is cli/src/targets.mjs slugPath: /api/v1 → api-v1.
func Slug(p string) string { return strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "-") }

// BuildLegacy renders what the legacy CLI would write for s: rel path →
// content. The migration compares it with the files on disk to find hand
// edits (MIG-02).
func BuildLegacy(s EdgeJSON) (map[string]string, error) {
	files := map[string]string{}
	for name, c := range s.Certs {
		crt, key := LegacyCertPaths(name, c)
		aop := ""
		if c.AOP {
			aop = "# Authenticated Origin Pulls: only Cloudflare may connect (edge cert aop " + name + " off)\ninclude /etc/nginx/edge/snippets/cloudflare-aop.conf;"
		}
		typ := certTypes[c.Type]
		if typ == "" {
			typ = c.Type
		}
		files["conf/snippets/ssl/"+name+".conf"] = edge.Fill(edge.Template("ssl.conf.tpl"), [][2]string{
			{"CERT", name}, {"TYPE", typ}, {"NAMES", strings.Join(c.Names, ", ")}, {"CRT", crt}, {"KEY", key}, {"AOP", aop}})
	}
	for host, site := range s.Sites {
		if !site.On() {
			continue
		}
		c, ok := s.Certs[site.Cert]
		if !ok {
			return nil, fmt.Errorf("%s uses certificate %q, which is not in edge.json", host, site.Cert)
		}
		_, domain := s.Domains[host]
		apexWWW := domain && certs.Covers(c.Names, "www."+host)
		var body string
		if site.Source.Type == "static" {
			body = edge.Fill(edge.Template("body-static.tpl"), [][2]string{{"DIR", site.Source.Dir}})
		} else {
			body = edge.Fill(edge.Template("body-proxy.tpl"), [][2]string{{"UPSTREAM", site.Source.Addr()}})
		}
		desc := site.Source.Describe()
		hsts := hstsOn
		if site.HTTP == "serve" {
			desc += " (also plain HTTP)"
			hsts = hstsOff
		}
		names := host
		if apexWWW {
			names = host + " www." + host
		}
		vars := [][2]string{{"TARGET", host}, {"DESC", desc}, {"HOST", host}, {"SERVER_NAMES", names}, {"CERT", site.Cert},
			{"HSTS", hsts}, {"BODY", strings.TrimSuffix(body, "\n")}}
		conf := edge.Fill(edge.Template("site.conf.tpl"), vars)
		if site.HTTP == "serve" {
			conf += edge.Fill(edge.Template("site-http.conf.tpl"), vars)
		}
		files["conf/sites/"+host+".conf"] = conf
	}
	for key, p := range s.Paths {
		if !p.On() {
			continue
		}
		file := "conf/locations/" + p.Host + "/" + Slug(p.Path) + ".conf"
		if p.Source.Type == "static" {
			files[file] = edge.Fill(edge.Template("path-static.conf.tpl"), [][2]string{
				{"TARGET", key}, {"DESC", p.Source.Describe()}, {"PATH", p.Path}, {"DIR", p.Source.Dir}})
			continue
		}
		desc, strip := p.Source.Describe(), ""
		if p.Strip {
			desc += " (prefix stripped)"
			strip = "rewrite ^" + p.Path + "/(.*)$ /$1 break;"
		}
		files[file] = edge.Fill(edge.Template("path-proxy.conf.tpl"), [][2]string{
			{"TARGET", key}, {"DESC", desc}, {"PATH", p.Path}, {"UPSTREAM", p.Source.Addr()}, {"STRIP", strip}})
	}
	return files, nil
}

// Drift are the legacy-managed files whose content is not what edge.json
// renders: hand edits the migration would lose (MIG-02). Files edge.json
// no longer describes are listed as well.
func (l *Legacy) Drift() ([]string, error) {
	want, err := BuildLegacy(l.State)
	if err != nil {
		return nil, err
	}
	var out []string
	for rel, have := range l.Managed {
		if w, ok := want[rel]; !ok || w != have {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out, nil
}
