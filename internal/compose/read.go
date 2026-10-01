package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// Bin is how compose is run: the v2 plugin (`docker compose …`) or the old
// standalone v1 (`docker-compose …`, DOCK-05) with the same arguments.
type Bin struct {
	Name string   `json:"name"` // docker or docker-compose
	Pre  []string `json:"pre"`  // ["compose"] for v2
	V1   bool     `json:"v1"`
}

// V2 is the plugin; tests and the command builder use it.
var V2 = Bin{Name: "docker", Pre: []string{"compose"}}

// Argv is the full argument list after the program name.
func (b Bin) Argv(args []string) []string { return append(append([]string{}, b.Pre...), args...) }

// String is the command as a person types it.
func (b Bin) String() string { return strings.TrimSpace(b.Name + " " + strings.Join(b.Pre, " ")) }

// ErrNoCompose is neither the plugin nor docker-compose.
var ErrNoCompose = errors.New("Docker Compose is not installed — install the plugin: apt install docker-compose-plugin")

// Detect finds compose: the v2 plugin, else v1 (DOCK-05).
func Detect(ctx context.Context, r execx.Runner) (Bin, error) {
	o := execx.Opts{Timeout: 10 * time.Second}
	if r.Run(ctx, "docker", []string{"compose", "version"}, o).Code == 0 {
		return V2, nil
	}
	if r.Run(ctx, "docker-compose", []string{"version"}, o).Code == 0 {
		return Bin{Name: "docker-compose", V1: true}, nil
	}
	return Bin{}, ErrNoCompose
}

// V1Warning is said once when only compose v1 exists (DOCK-05).
const V1Warning = "only the old docker-compose (v1) is installed: NgiTool runs it with the same arguments, but v1 is end-of-life — install the plugin: apt install docker-compose-plugin (DOCK-05)"

// Project is what every compose run of one app needs.
type Project struct {
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Files    []string `json:"files"`
	Profiles []string `json:"profiles,omitempty"`
	EnvFiles []string `json:"envFiles,omitempty"`
	Override string   `json:"override,omitempty"` // NgiTool's file, always the last -f
}

// Args are the global arguments every compose call gets: explicit -p,
// --project-directory and every -f, the override last (DOCK-02, DOCK-03).
func (p Project) Args() []string {
	a := []string{"-p", p.Name, "--project-directory", p.Dir}
	for _, f := range p.Files {
		a = append(a, "-f", f)
	}
	if p.Override != "" {
		a = append(a, "-f", p.Override)
	}
	for _, e := range p.EnvFiles {
		a = append(a, "--env-file", e)
	}
	for _, pr := range p.Profiles {
		a = append(a, "--profile", pr)
	}
	return a
}

// Opts are the run options for p: its directory as cwd, and the variables
// that would redirect compose to another project scrubbed (DOCK-08).
func (p Project) Opts() execx.Opts {
	return execx.Opts{Dir: p.Dir, Scrub: execx.ComposeScrub, Timeout: 60 * time.Second}
}

// Service is a compose service reduced to what NgiTool shows and routes
// to. The resolved config also holds environment values and secrets; those
// fields are not even decoded (DOCK-04).
type Service struct {
	Name          string              `json:"name"`
	Image         string              `json:"image,omitempty"`
	Build         bool                `json:"build,omitempty"`
	Ports         []int               `json:"ports,omitempty"` // container ports: expose + ports targets
	Published     []Port              `json:"published,omitempty"`
	NetworkKeys   []string            `json:"networkKeys,omitempty"`
	Networks      []string            `json:"networks,omitempty"` // real names
	Aliases       map[string][]string `json:"aliases,omitempty"`  // real network name → aliases
	ContainerName string              `json:"containerName,omitempty"`
	NetworkMode   string              `json:"networkMode,omitempty"`
	Profiles      []string            `json:"profiles,omitempty"`
	Restart       string              `json:"restart,omitempty"`
	Replicas      int                 `json:"replicas,omitempty"`
}

// OneOff is a service that is not meant to keep running (restart: "no").
func (s Service) OneOff() bool { return s.Restart == "no" }

// Detached explains why a service cannot join a network (DOCK-06): its
// network_mode is host, none, service:x or container:x.
func (s Service) Detached() string {
	switch {
	case s.NetworkMode == "":
		return ""
	case s.NetworkMode == "host":
		return "network_mode: host — it uses the host's network and cannot join a Docker network; route to it as a host port instead (DOCK-06)"
	case strings.HasPrefix(s.NetworkMode, "service:"), strings.HasPrefix(s.NetworkMode, "container:"):
		return "network_mode: " + s.NetworkMode + " — it shares another container's network; route to that one instead (DOCK-06)"
	case s.NetworkMode == "none":
		return "network_mode: none — it has no network at all (DOCK-06)"
	}
	return ""
}

// Config is a read project.
type Config struct {
	Name       string    `json:"name"`
	Services   []Service `json:"services"`
	Unresolved bool      `json:"unresolved,omitempty"` // raw YAML: include/extends/env not applied
}

// Service returns the service named name, or nil.
func (c *Config) Service(name string) *Service {
	for i := range c.Services {
		if c.Services[i].Name == name {
			return &c.Services[i]
		}
	}
	return nil
}

// configDoc is `docker compose config --format json`, narrowed: no
// environment, no secrets, no labels.
type configDoc struct {
	Name     string                `json:"name" yaml:"name"`
	Networks map[string]networkDoc `json:"networks" yaml:"networks"`
	Services map[string]serviceDoc `json:"services" yaml:"services"`
}

type networkDoc struct {
	Name     string `json:"name" yaml:"name"`
	External any    `json:"external" yaml:"external"`
}

type serviceDoc struct {
	Image         string         `json:"image" yaml:"image"`
	Build         any            `json:"build" yaml:"build"`
	Expose        []any          `json:"expose" yaml:"expose"`
	Ports         []any          `json:"ports" yaml:"ports"`
	Networks      any            `json:"networks" yaml:"networks"`
	ContainerName string         `json:"container_name" yaml:"container_name"`
	NetworkMode   string         `json:"network_mode" yaml:"network_mode"`
	Profiles      []string       `json:"profiles" yaml:"profiles"`
	Restart       string         `json:"restart" yaml:"restart"`
	Deploy        map[string]any `json:"deploy" yaml:"deploy"`
	Scale         int            `json:"scale" yaml:"scale"`
}

// ReadError is a failed `compose config`: the last 3 lines of its error.
type ReadError struct {
	Tail string
}

func (e *ReadError) Error() string { return "docker compose could not read the project:\n" + e.Tail }

// Read runs `compose -p … --project-directory … -f … config --format json`
// in the project's directory. On failure the error carries compose's last
// three lines; ReadRaw is the fallback.
func Read(ctx context.Context, r execx.Runner, b Bin, p Project) (*Config, error) {
	args := append(p.Args(), "config")
	if !b.V1 {
		args = append(args, "--format", "json")
	}
	res := r.Run(ctx, b.Name, b.Argv(args), p.Opts())
	if res.Code != 0 {
		return nil, &ReadError{Tail: execx.Tail(res, 3)}
	}
	var doc configDoc
	var err error
	if b.V1 {
		err = yaml.Unmarshal([]byte(res.Stdout), &doc)
	} else {
		err = json.Unmarshal([]byte(res.Stdout), &doc)
	}
	if err != nil {
		return nil, &ReadError{Tail: "unreadable output: " + err.Error()}
	}
	cfg := Summarize(doc)
	if cfg.Name == "" {
		cfg.Name = p.Name
	}
	return cfg, nil
}

// ReadRaw reads the files as plain YAML, later files replacing earlier
// services' keys: include, extends, env interpolation and profiles are not
// applied, and the result says so.
func ReadRaw(p Project) (*Config, error) {
	merged := configDoc{Networks: map[string]networkDoc{}, Services: map[string]serviceDoc{}}
	read := 0
	for _, f := range p.Files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var doc configDoc
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
		read++
		if doc.Name != "" {
			merged.Name = doc.Name
		}
		for k, v := range doc.Networks {
			merged.Networks[k] = v
		}
		for k, v := range doc.Services {
			old, ok := merged.Services[k]
			if !ok {
				merged.Services[k] = v
				continue
			}
			if v.Image != "" {
				old.Image = v.Image
			}
			if v.Build != nil {
				old.Build = v.Build
			}
			old.Ports = append(old.Ports, v.Ports...)
			old.Expose = append(old.Expose, v.Expose...)
			if v.Networks != nil {
				old.Networks = v.Networks
			}
			if v.NetworkMode != "" {
				old.NetworkMode = v.NetworkMode
			}
			if v.ContainerName != "" {
				old.ContainerName = v.ContainerName
			}
			merged.Services[k] = old
		}
	}
	if read == 0 {
		return nil, errors.New("no file to read")
	}
	merged.Name = firstNonEmpty(p.Name, merged.Name)
	cfg := Summarize(merged)
	cfg.Unresolved = true
	return cfg, nil
}

// Profiles lists the profiles the project's services use (`config --profiles`).
func Profiles(ctx context.Context, r execx.Runner, b Bin, p Project) []string {
	q := p
	q.Profiles = nil
	res := r.Run(ctx, b.Name, b.Argv(append(q.Args(), "config", "--profiles")), q.Opts())
	if res.Code != 0 {
		return nil
	}
	return splitLines(res.Stdout)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Summarize ports cli/src/apps.mjs summarizeServices: names, images, build,
// ports, networks with aliases, container_name, network_mode, profiles,
// restart and replicas. Long-running services first, then by name.
func Summarize(doc configDoc) *Config {
	cfg := &Config{Name: doc.Name}
	netName := func(key string) string {
		if n, ok := doc.Networks[key]; ok && n.Name != "" {
			return n.Name
		}
		if ext, ok := doc.Networks[key]; ok && truthy(ext.External) {
			return key
		}
		return doc.Name + "_" + key
	}
	for name, s := range doc.Services {
		svc := Service{Name: name, Image: s.Image, Build: s.Build != nil, ContainerName: s.ContainerName, NetworkMode: s.NetworkMode,
			Profiles: s.Profiles, Restart: s.Restart, Aliases: map[string][]string{}}
		ports := map[int]bool{}
		for _, e := range s.Expose {
			if n := atoiPort(fmt.Sprint(e)); n > 0 {
				ports[n] = true
			}
		}
		for _, p := range s.Ports {
			if pp, ok := rawPort(p); ok {
				ports[pp.Target] = true
				if pp.HostPort > 0 {
					svc.Published = append(svc.Published, pp)
				}
			}
		}
		for n := range ports {
			svc.Ports = append(svc.Ports, n)
		}
		sort.Ints(svc.Ports)
		keys := netKeys(s.Networks)
		if len(keys) == 0 && s.NetworkMode == "" {
			keys = []string{"default"}
		}
		svc.NetworkKeys = keys
		for _, k := range keys {
			real := netName(k)
			svc.Networks = append(svc.Networks, real)
			if m, ok := s.Networks.(map[string]any); ok {
				if v, ok := m[k].(map[string]any); ok {
					for _, a := range anyList(v["aliases"]) {
						svc.Aliases[real] = append(svc.Aliases[real], a)
					}
				}
			}
		}
		if len(svc.Aliases) == 0 {
			svc.Aliases = nil
		}
		svc.Replicas = s.Scale
		if r, ok := s.Deploy["replicas"]; ok {
			switch n := r.(type) {
			case float64:
				svc.Replicas = int(n)
			case int:
				svc.Replicas = n
			case string:
				svc.Replicas, _ = strconv.Atoi(n)
			}
		}
		cfg.Services = append(cfg.Services, svc)
	}
	sort.Slice(cfg.Services, func(i, j int) bool {
		a, b := cfg.Services[i], cfg.Services[j]
		if a.OneOff() != b.OneOff() {
			return !a.OneOff()
		}
		return a.Name < b.Name
	})
	return cfg
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case map[string]any:
		return true
	}
	return false
}

func anyList(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// atoiPort reads "3000", "3000/tcp", "3000-3005" (the first).
func atoiPort(s string) int {
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, "-")
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
