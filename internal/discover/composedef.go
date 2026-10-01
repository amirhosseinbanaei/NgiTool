package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// svc is a compose service reduced to what discovery needs: name, image,
// ports, networks, volumes (and the command, for -c). The resolved config
// also holds the project's environment and secrets; those fields are not
// decoded, never printed and never stored (cli/src/apps.mjs did the same).
type svc struct {
	Name     string
	Image    string
	Command  []string
	Ports    []Published
	Networks []string
	Volumes  []Mount
}

// composeJSON is `docker compose config --format json`, narrowed.
type composeJSON struct {
	Name     string `json:"name"`
	Services map[string]struct {
		Image   string `json:"image"`
		Command any    `json:"command"`
		Ports   []struct {
			Target    int    `json:"target"`
			Published string `json:"published"`
			HostIP    string `json:"host_ip"`
			Protocol  string `json:"protocol"`
		} `json:"ports"`
		Networks map[string]any `json:"networks"`
		Volumes  []struct {
			Type     string `json:"type"`
			Source   string `json:"source"`
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"`
		} `json:"volumes"`
	} `json:"services"`
}

// composeYAML is the raw file, for when `compose config` fails.
type composeYAML struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image    string `yaml:"image"`
		Command  any    `yaml:"command"`
		Ports    []any  `yaml:"ports"`
		Networks any    `yaml:"networks"`
		Volumes  []any  `yaml:"volumes"`
	} `yaml:"services"`
}

var imageLineRE = regexp.MustCompile(`(?m)^\s*image:\s*["']?([^\s"'#]+)`)

// scanCompose finds compose services running nginx that were never
// created (DISC-09). Containers that exist were already found by docker
// discovery through their labels.
func (s *Scanner) scanCompose(ctx context.Context) int {
	if len(s.env.Roots) == 0 {
		return 0
	}
	files, errs := compose.Find(s.env.Roots, s.env.Depth)
	s.rep.ComposeFiles = len(files)
	for _, e := range errs {
		s.fail(e.Error())
	}
	exists := map[string]bool{}
	for _, c := range s.ctrs {
		if c.Project != "" {
			exists[c.Project+"/"+c.Service] = true
		}
	}
	n := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			s.fail(f + ": " + errText(err))
			continue
		}
		// Only files that name an nginx-like image are worth a compose run.
		hit := false
		for _, m := range imageLineRE.FindAllStringSubmatch(string(raw), -1) {
			hit = hit || nginxImage(m[1])
		}
		if !hit {
			continue
		}
		project, services, unresolved := s.readCompose(ctx, f, raw)
		for _, sv := range services {
			if !nginxImage(sv.Image) || exists[project+"/"+sv.Name] {
				continue
			}
			s.addDefined(f, project, sv, unresolved)
			n++
		}
	}
	return n
}

// readCompose runs `docker compose -f <file> config --format json` in the
// file's directory; when that fails (a missing env file, say) it falls
// back to the raw YAML, and the result is marked unresolved.
func (s *Scanner) readCompose(ctx context.Context, file string, raw []byte) (string, []svc, bool) {
	dir := filepath.Dir(file)
	if s.rep.Docker.Usable() {
		res := s.env.Run.Run(ctx, "docker", []string{"compose", "-f", file, "config", "--format", "json"},
			execx.Opts{Dir: dir, Timeout: 20 * time.Second, Scrub: execx.ComposeScrub})
		var cj composeJSON
		if res.Code == 0 && json.Unmarshal([]byte(res.Stdout), &cj) == nil {
			return cj.Name, fromJSON(cj), false
		}
	}
	var cy composeYAML
	if err := yaml.Unmarshal(raw, &cy); err != nil {
		s.fail(file + ": " + firstLine(err.Error()))
		return "", nil, true
	}
	name := cy.Name
	if name == "" || strings.Contains(name, "$") {
		name = projectName(filepath.Base(dir))
	}
	return name, fromYAML(cy, dir), true
}

func fromJSON(cj composeJSON) []svc {
	var out []svc
	for name, s := range cj.Services {
		sv := svc{Name: name, Image: s.Image, Command: argv(s.Command)}
		for _, p := range s.Ports {
			hp, _ := strconv.Atoi(p.Published)
			sv.Ports = append(sv.Ports, Published{HostIP: p.HostIP, HostPort: hp, ContainerPort: p.Target, Proto: firstNonEmpty(p.Protocol, "tcp")})
		}
		for n := range s.Networks {
			sv.Networks = append(sv.Networks, n)
		}
		for _, v := range s.Volumes {
			sv.Volumes = append(sv.Volumes, Mount{Type: v.Type, Source: v.Source, Dest: v.Target, RW: !v.ReadOnly})
		}
		sort.Strings(sv.Networks)
		out = append(out, sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func fromYAML(cy composeYAML, dir string) []svc {
	var out []svc
	for name, s := range cy.Services {
		sv := svc{Name: name, Image: s.Image, Command: argv(s.Command)}
		for _, p := range s.Ports {
			if pp, ok := shortPort(fmt.Sprint(p)); ok {
				sv.Ports = append(sv.Ports, pp)
			}
		}
		switch n := s.Networks.(type) {
		case []any:
			for _, x := range n {
				sv.Networks = append(sv.Networks, fmt.Sprint(x))
			}
		case map[string]any:
			for k := range n {
				sv.Networks = append(sv.Networks, k)
			}
		}
		for _, v := range s.Volumes {
			if m, ok := shortVolume(v, dir); ok {
				sv.Volumes = append(sv.Volumes, m)
			}
		}
		sort.Strings(sv.Networks)
		out = append(out, sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func argv(v any) []string {
	switch c := v.(type) {
	case string:
		return strings.Fields(c)
	case []any:
		out := make([]string, len(c))
		for i, x := range c {
			out[i] = fmt.Sprint(x)
		}
		return out
	}
	return nil
}

// shortPort reads "8080:80", "127.0.0.1:8443:443/tcp", "80".
func shortPort(s string) (Published, bool) {
	proto := "tcp"
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s, proto = s[:i], s[i+1:]
	}
	parts := strings.Split(s, ":")
	p := Published{Proto: proto}
	switch len(parts) {
	case 1:
		p.ContainerPort, _ = strconv.Atoi(parts[0])
	case 2:
		p.HostPort, _ = strconv.Atoi(parts[0])
		p.ContainerPort, _ = strconv.Atoi(parts[1])
	default:
		p.HostIP = strings.Join(parts[:len(parts)-2], ":")
		p.HostPort, _ = strconv.Atoi(parts[len(parts)-2])
		p.ContainerPort, _ = strconv.Atoi(parts[len(parts)-1])
	}
	return p, p.ContainerPort > 0
}

// shortVolume reads "./conf:/etc/nginx/conf.d:ro" or the long map form.
func shortVolume(v any, dir string) (Mount, bool) {
	var m Mount
	switch x := v.(type) {
	case string:
		parts := strings.Split(x, ":")
		if len(parts) < 2 {
			return m, false
		}
		m = Mount{Source: parts[0], Dest: parts[1], RW: !(len(parts) > 2 && strings.Contains(parts[2], "ro"))}
	case map[string]any:
		m = Mount{Type: fmt.Sprint(x["type"]), Source: fmt.Sprint(x["source"]), Dest: fmt.Sprint(x["target"])}
		m.RW = x["read_only"] != true
	default:
		return m, false
	}
	if strings.HasPrefix(m.Source, ".") || strings.HasPrefix(m.Source, "~") || strings.HasPrefix(m.Source, "/") {
		m.Type = "bind"
		if !filepath.IsAbs(m.Source) && !strings.HasPrefix(m.Source, "~") {
			m.Source = filepath.Join(dir, m.Source)
		}
	} else if m.Type == "" || m.Type == "<nil>" {
		m.Type = "volume"
		m.Name = m.Source
	}
	return m, true
}

var projectRE = regexp.MustCompile(`[^a-z0-9_-]`)

// projectName is compose's default project name for a directory.
func projectName(dir string) string {
	return strings.TrimLeft(projectRE.ReplaceAllString(strings.ToLower(dir), ""), "_-")
}

func (s *Scanner) addDefined(file, project string, sv svc, unresolved bool) {
	in := &Instance{
		Kind:        KindCompose,
		ID:          "compose:" + project + "/" + sv.Name,
		Name:        project + "/" + sv.Name,
		State:       StateDefined,
		Image:       sv.Image,
		Project:     project,
		Service:     sv.Name,
		WorkingDir:  filepath.Dir(file),
		ComposeFile: []string{file},
		Networks:    sv.Networks,
		Ports:       sv.Ports,
		Mounts:      sv.Volumes,
		Unresolved:  unresolved,
		ManagedBy:   ManagedNone,
		Variant:     variantOfImage(sv.Image),
		Owner:       s.env.ownerOf(file),
		Conf:        confFromCmd(sv.Command),
	}
	if m := managerOf(sv.Image); m != "" {
		in.ManagedBy = "other:" + m
	}
	var fm []nginxconf.Mount
	var dm []driver.Mount
	for _, m := range sv.Volumes {
		if m.Type == "bind" || m.Type == "volume" {
			if st, err := os.Stat(m.Source); err == nil && !st.IsDir() {
				m.File = true
			}
			fm = append(fm, nginxconf.Mount{Dest: m.Dest, Source: m.Source})
			dm = append(dm, driver.Mount{Type: m.Type, Source: m.Source, Name: m.Name, Dest: m.Dest})
		}
		if strings.HasPrefix(m.Dest, "/etc/nginx/templates") {
			in.Templates = true
		}
	}
	if unresolved {
		in.Notes = append(in.Notes, "docker compose config failed; read from the raw YAML (variables not resolved)")
	}
	s.add(in, &build{
		drv:   driver.Container{Run: s.env.Run, Image: sv.Image, Conf: in.Conf, Mounts: dm},
		src:   nginxconf.FileSource{Container: true, Mounts: fm},
		stock: stockImage(sv.Image),
		files: fm,
	})
}
