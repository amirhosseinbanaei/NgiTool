package compose

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// Compose labels Docker puts on every container of a project.
const (
	LabelProject = "com.docker.compose.project"
	LabelService = "com.docker.compose.service"
	LabelWorkDir = "com.docker.compose.project.working_dir"
	LabelFiles   = "com.docker.compose.project.config_files"
)

// Port is a published port.
type Port struct {
	HostIP   string `json:"hostIp,omitempty"`
	HostPort int    `json:"hostPort,omitempty"`
	Target   int    `json:"target"`
	Proto    string `json:"proto,omitempty"`
}

// Public reports whether the port is published on every address (DOCK-11).
func (p Port) Public() bool {
	return p.HostPort > 0 && (p.HostIP == "" || p.HostIP == "0.0.0.0" || p.HostIP == "::")
}

func (p Port) String() string {
	if p.HostPort == 0 {
		return strconv.Itoa(p.Target)
	}
	ip := p.HostIP
	if ip == "" {
		ip = "0.0.0.0"
	}
	return ip + ":" + strconv.Itoa(p.HostPort) + "→" + strconv.Itoa(p.Target)
}

// Ctr is a container as its compose labels describe it. Never its env.
type Ctr struct {
	Name        string   `json:"name"`
	Image       string   `json:"image,omitempty"`
	State       string   `json:"state"`
	Running     bool     `json:"running"`
	Health      string   `json:"health,omitempty"`
	Project     string   `json:"project,omitempty"`
	Service     string   `json:"service,omitempty"`
	WorkDir     string   `json:"workingDir,omitempty"`
	ConfigFiles []string `json:"configFiles,omitempty"`
	Networks    []string `json:"networks,omitempty"`
	Ports       []Port   `json:"ports,omitempty"`
}

// psFormat is one tab-separated line per container.
var psFormat = strings.Join([]string{"{{.Names}}", "{{.Image}}", "{{.State}}", "{{.Status}}", "{{.Networks}}", "{{.Ports}}",
	`{{.Label "` + LabelProject + `"}}`, `{{.Label "` + LabelService + `"}}`, `{{.Label "` + LabelWorkDir + `"}}`, `{{.Label "` + LabelFiles + `"}}`}, "\t")

// Containers lists every compose container with its labels (`docker ps -a`).
func Containers(ctx context.Context, r execx.Runner) ([]Ctr, error) {
	res := r.Run(ctx, "docker", []string{"ps", "-a", "--no-trunc", "--filter", "label=" + LabelProject, "--format", psFormat}, execx.Opts{Timeout: 20 * time.Second})
	if res.Code != 0 {
		return nil, &execx.Error{Cmd: "docker ps", Res: res}
	}
	return ParsePS(res.Stdout), nil
}

var portRE = regexp.MustCompile(`^(?:(\[?[0-9a-fA-F:.]*\]?):)?(\d+)(?:-\d+)?->(\d+)(?:-\d+)?/(\w+)$`)

// ParsePS reads psFormat lines.
func ParsePS(out string) []Ctr {
	var cs []Ctr
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 10 || f[6] == "" {
			continue
		}
		c := Ctr{Name: f[0], Image: f[1], State: f[2], Running: f[2] == "running", Project: f[6], Service: f[7], WorkDir: f[8], ConfigFiles: splitList(f[9]), Networks: splitList(f[4])}
		if i := strings.Index(f[3], "(health: "); i >= 0 {
			c.Health = strings.TrimSuffix(f[3][i+9:], ")")
		} else if strings.Contains(f[3], "(healthy)") || strings.Contains(f[3], "(unhealthy)") {
			c.Health = strings.Trim(f[3][strings.LastIndex(f[3], "("):], "()")
		}
		for _, p := range splitList(f[5]) {
			if m := portRE.FindStringSubmatch(strings.TrimSpace(p)); m != nil {
				hp, _ := strconv.Atoi(m[2])
				tp, _ := strconv.Atoi(m[3])
				c.Ports = appendPort(c.Ports, Port{HostIP: strings.Trim(m[1], "[]"), HostPort: hp, Target: tp, Proto: m[4]})
			}
		}
		cs = append(cs, c)
	}
	return cs
}

func appendPort(ps []Port, p Port) []Port {
	for _, q := range ps {
		if q.HostPort == p.HostPort && q.Target == p.Target && q.Proto == p.Proto && (q.HostIP == p.HostIP || (q.Public() && p.Public())) {
			return ps
		}
	}
	return append(ps, p)
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// Project states.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateNever   = "never" // no container exists
)

// Candidate is one compose project found on this machine: the files of its
// directory merged with what its containers' labels say.
type Candidate struct {
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Owner string `json:"owner,omitempty"`
	Files []File `json:"files"`
	// Labeled is the -f list of the running (or last) containers, in order:
	// authoritative when it exists (DOCK-02).
	Labeled  []string `json:"labeled,omitempty"`
	State    string   `json:"state"`
	Up       int      `json:"up"`
	Total    int      `json:"total"`
	Services []string `json:"services,omitempty"`
	Networks []string `json:"networks,omitempty"`
	Ports    []Port   `json:"ports,omitempty"`
	Nginx    string   `json:"nginx,omitempty"`  // the instance id when it has an nginx (filled by the caller)
	Linked   string   `json:"linked,omitempty"` // app name when linked (filled by the caller)
	Broken   string   `json:"broken,omitempty"`
	// Reserved is why NgiTool will not link it although it could: the
	// edge stack is run by its own commands, not as an app.
	Reserved string `json:"reserved,omitempty"`
}

// ID is how a candidate is picked: dir, plus the project name when one
// directory holds several projects.
func (c Candidate) ID() string { return c.Dir + "#" + c.Name }

// Public are the ports published on every address (DOCK-11).
func (c Candidate) Public() []Port {
	var out []Port
	for _, p := range c.Ports {
		if p.Public() {
			out = append(out, p)
		}
	}
	return out
}

// Default is the -f set to pre-select: the label's, else base plus its
// override, else the only file.
func (c Candidate) Default() []string {
	if len(c.Labeled) > 0 {
		return append([]string{}, c.Labeled...)
	}
	var out []string
	for _, f := range c.Files {
		if f.Broken == "" && (f.Role == RoleBase || f.Role == RoleOverride) {
			if f.Role == RoleBase && len(out) > 0 {
				continue // docker-compose.yaml next to compose.yaml: compose takes the first
			}
			out = append(out, f.Path)
		}
	}
	if len(out) == 0 {
		var ok []string
		for _, f := range c.Files {
			if f.Broken == "" {
				ok = append(ok, f.Path)
			}
		}
		if len(ok) == 1 {
			return ok
		}
	}
	return out
}

// Merge builds the candidates: one per scanned directory, plus every
// project its containers name that the scan did not reach (outside the
// roots). Label files outside the directory (a legacy edge override) are
// added to the project's files; files a label names that are gone are
// marked broken with their last known path (DOCK-10).
func Merge(dirs []Dir, ctrs []Ctr) []Candidate {
	type proj struct {
		name, dir string
		files     []string
		cs        []Ctr
	}
	var projs []*proj
	byName := map[string]*proj{}
	for _, c := range ctrs {
		if c.Project == "" {
			continue
		}
		p := byName[c.Project]
		if p == nil {
			p = &proj{name: c.Project, dir: c.WorkDir}
			byName[c.Project] = p
			projs = append(projs, p)
		}
		p.cs = append(p.cs, c)
		if len(p.files) == 0 || (c.Running && len(c.ConfigFiles) > 0) {
			if len(c.ConfigFiles) > 0 {
				p.files = c.ConfigFiles
			}
		}
		if p.dir == "" {
			p.dir = c.WorkDir
		}
	}
	var out []Candidate
	used := map[string]bool{}
	for _, d := range dirs {
		cand := Candidate{Dir: d.Path, Files: append([]File{}, d.Files...), Broken: d.Broken, State: StateNever}
		var mine []*proj
		for _, p := range projs {
			if p.dir == d.Path {
				mine = append(mine, p)
			}
		}
		if len(mine) == 0 {
			cand.Name = nameOf(d)
			out = append(out, finish(cand))
			continue
		}
		for _, p := range mine {
			used[p.name] = true
			c := cand
			c.Files = append([]File{}, cand.Files...)
			out = append(out, finish(withLabels(c, p.name, p.files, p.cs)))
		}
	}
	for _, p := range projs {
		if used[p.name] {
			continue
		}
		c := Candidate{Dir: p.dir, State: StateNever}
		if st, err := os.Stat(p.dir); err != nil {
			c.Broken = "working directory " + p.dir + " " + errText(err)
		} else if !st.IsDir() {
			c.Broken = p.dir + " is not a directory"
		}
		out = append(out, finish(withLabels(c, p.name, p.files, p.cs)))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Dir < out[j].Dir
	})
	return out
}

func withLabels(c Candidate, name string, files []string, cs []Ctr) Candidate {
	c.Name = name
	c.Labeled = files
	have := map[string]bool{}
	for _, f := range c.Files {
		have[f.Path] = true
	}
	for _, f := range files {
		if have[f] {
			continue
		}
		role, word, ok := Classify(filepath.Base(f))
		if !ok {
			role = RoleExtra
		}
		lf := File{Path: f, Role: role, Word: word}
		if st, err := os.Stat(f); err != nil {
			lf.Broken = "missing — last known path from the container label"
		} else {
			lf.Owner = Owner(st)
		}
		c.Files = append(c.Files, lf)
	}
	SortFiles(c.Files)
	svcs := map[string]bool{}
	nets := map[string]bool{}
	for _, x := range cs {
		c.Total++
		if x.Running {
			c.Up++
		}
		if !svcs[x.Service] {
			svcs[x.Service] = true
			c.Services = append(c.Services, x.Service)
		}
		if c.Nginx == "" && NginxImage(x.Image) {
			c.Nginx = "compose:" + name + "/" + x.Service
		}
		for _, n := range x.Networks {
			if !nets[n] {
				nets[n] = true
				c.Networks = append(c.Networks, n)
			}
		}
		for _, p := range x.Ports {
			c.Ports = appendPort(c.Ports, p)
		}
	}
	switch {
	case c.Up > 0:
		c.State = StateRunning
	case c.Total > 0:
		c.State = StateStopped
	}
	return c
}

// finish fills what the files say (owner, and for a project that never
// ran, services, networks and ports from the raw YAML) and marks a
// candidate with no usable file broken.
func finish(c Candidate) Candidate {
	if c.Owner == "" {
		for _, f := range c.Files {
			if f.Owner != "" {
				c.Owner = f.Owner
				break
			}
		}
		if c.Owner == "" {
			c.Owner = OwnerOf(c.Dir)
		}
	}
	usable := 0
	for _, f := range c.Files {
		if f.Broken == "" {
			usable++
		}
	}
	if c.Broken == "" && usable == 0 {
		if len(c.Files) > 0 {
			c.Broken = c.Files[0].Broken
		} else {
			c.Broken = "no compose file found"
		}
	}
	if EdgeStack(c.Dir) {
		c.Reserved = "the edge stack — run by its own commands, not as an app"
	}
	if c.Broken == "" {
		raw := RawSummary(c.Default())
		if c.Total == 0 {
			c.Services, c.Networks, c.Ports = raw.Services, raw.Networks, raw.Ports
		}
		for _, svc := range raw.Services {
			if c.Nginx == "" && NginxImage(raw.Images[svc]) {
				c.Nginx = "compose:" + firstNonEmpty(c.Name, raw.Name) + "/" + svc
			}
		}
		if c.Name == "" && raw.Name != "" {
			c.Name = raw.Name
		}
	}
	return c
}

// ProjectName is the name compose derives from a directory: lower case,
// only a-z 0-9 _ -, starting with a letter or digit.
func ProjectName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

var nameRE = regexp.MustCompile(`(?m)^name:\s*["']?([A-Za-z0-9_-]+)`)

// nameOf is name: from the first usable file, else the folder (DOCK-03).
func nameOf(d Dir) string {
	for _, f := range d.Files {
		if f.Broken != "" {
			continue
		}
		if b, err := os.ReadFile(f.Path); err == nil {
			if m := nameRE.FindSubmatch(b); m != nil {
				return ProjectName(string(m[1]))
			}
		}
	}
	return ProjectName(filepath.Base(d.Path))
}

// Raw is what the files say without compose resolving them: enough for a
// checklist row. Never holds environment values.
type Raw struct {
	Name     string
	Services []string
	Networks []string
	Ports    []Port
	Images   map[string]string // service → image
}

type rawDoc struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image    string `yaml:"image"`
		Ports    []any  `yaml:"ports"`
		Networks any    `yaml:"networks"`
	} `yaml:"services"`
}

// RawSummary reads files as plain YAML, later files adding to earlier ones.
func RawSummary(files []string) Raw {
	r := Raw{Images: map[string]string{}}
	seen := map[string]bool{}
	nets := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var doc rawDoc
		if yaml.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc.Name != "" {
			r.Name = ProjectName(doc.Name)
		}
		names := make([]string, 0, len(doc.Services))
		for n := range doc.Services {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			s := doc.Services[n]
			if !seen[n] {
				seen[n] = true
				r.Services = append(r.Services, n)
			}
			if s.Image != "" {
				r.Images[n] = s.Image
			}
			for _, k := range netKeys(s.Networks) {
				if !nets[k] {
					nets[k] = true
					r.Networks = append(r.Networks, k)
				}
			}
			for _, p := range s.Ports {
				if pp, ok := rawPort(p); ok {
					r.Ports = appendPort(r.Ports, pp)
				}
			}
		}
	}
	return r
}

func netKeys(v any) []string {
	var out []string
	switch n := v.(type) {
	case []any:
		for _, x := range n {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	case map[string]any:
		for k := range n {
			out = append(out, k)
		}
		sort.Strings(out)
	}
	return out
}

// rawPort reads "8080:80", "127.0.0.1:8080:80/tcp", "80" or the long form.
func rawPort(v any) (Port, bool) {
	switch p := v.(type) {
	case int:
		return Port{Target: p, Proto: "tcp"}, true
	case float64:
		return Port{Target: int(p), Proto: "tcp"}, true
	case string:
		spec, proto, _ := strings.Cut(p, "/")
		if proto == "" {
			proto = "tcp"
		}
		parts := strings.Split(spec, ":")
		port := Port{Proto: proto}
		atoi := func(s string) int { n, _ := strconv.Atoi(strings.SplitN(s, "-", 2)[0]); return n }
		switch len(parts) {
		case 1:
			port.Target = atoi(parts[0])
		case 2:
			port.HostPort, port.Target = atoi(parts[0]), atoi(parts[1])
		default:
			port.HostIP = strings.Trim(strings.Join(parts[:len(parts)-2], ":"), "[]")
			port.HostPort, port.Target = atoi(parts[len(parts)-2]), atoi(parts[len(parts)-1])
		}
		return port, port.Target > 0
	case map[string]any:
		port := Port{Proto: "tcp"}
		num := func(v any) int {
			switch n := v.(type) {
			case int:
				return n
			case float64:
				return int(n)
			case string:
				i, _ := strconv.Atoi(n)
				return i
			}
			return 0
		}
		port.Target, port.HostPort = num(p["target"]), num(p["published"])
		if pr, ok := p["protocol"].(string); ok && pr != "" {
			port.Proto = pr
		}
		if ip, ok := p["host_ip"].(string); ok {
			port.HostIP = ip
		}
		return port, port.Target > 0
	}
	return Port{}, false
}

// NginxImage reports whether an image is nginx or a variant by its name:
// any path part nginx, nginx-*, openresty, angie, tengine, swag. A custom
// image that runs nginx is only found by the scan (DISC-10).
func NginxImage(image string) bool {
	if image == "" {
		return false
	}
	repo := image
	if i := strings.LastIndex(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, part := range strings.Split(strings.ToLower(repo), "/") {
		switch {
		case part == "nginx", strings.HasPrefix(part, "nginx-"), part == "openresty", part == "angie", part == "tengine", part == "swag":
			return true
		}
	}
	return false
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// psJSON is one container of `docker compose ps --format json`, narrowed
// (Labels and Command are not decoded).
type psJSON struct {
	Name       string `json:"Name"`
	Image      string `json:"Image"`
	Service    string `json:"Service"`
	Project    string `json:"Project"`
	State      string `json:"State"`
	Health     string `json:"Health"`
	Networks   string `json:"Networks"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// ParsePSJSON reads `compose ps --format json`: one object per line (v2.21+)
// or one array (older v2).
func ParsePSJSON(out string) []Ctr {
	var docs []psJSON
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "[") {
		_ = json.Unmarshal([]byte(out), &docs)
	} else {
		for _, l := range strings.Split(out, "\n") {
			var d psJSON
			if json.Unmarshal([]byte(strings.TrimSpace(l)), &d) == nil && d.Name != "" {
				docs = append(docs, d)
			}
		}
	}
	var cs []Ctr
	for _, d := range docs {
		c := Ctr{Name: d.Name, Image: d.Image, Service: d.Service, Project: d.Project, State: d.State, Running: d.State == "running", Health: d.Health, Networks: splitList(d.Networks)}
		for _, p := range d.Publishers {
			if p.PublishedPort > 0 {
				c.Ports = appendPort(c.Ports, Port{HostIP: p.URL, HostPort: p.PublishedPort, Target: p.TargetPort, Proto: p.Protocol})
			}
		}
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs
}

// EdgeStack reports whether dir is the bundled edge stack: compose.yaml,
// conf/nginx.conf and the edge CLI (discovery's rule).
func EdgeStack(dir string) bool {
	for _, p := range []string{"compose.yaml", "conf/nginx.conf"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			return false
		}
	}
	for _, p := range []string{"edge", "cli/src"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err == nil {
			return true
		}
	}
	return false
}
