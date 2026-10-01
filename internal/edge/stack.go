package edge

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

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// Stack runs compose against one edge stack directory, always with an
// explicit -p, --project-directory and -f, with COMPOSE_PROJECT_NAME and
// COMPOSE_FILE scrubbed (DOCK-08): a variable left over in the shell can
// never point a stack command at another project.
type Stack struct {
	Dir     string
	Project string
	Run     execx.Runner
	Bin     compose.Bin
}

// Layout is the stack's directory layout.
func (s Stack) Layout() Layout { return Layout{s.Dir} }

// Compose is the stack as a compose project.
func (s Stack) Compose() compose.Project {
	return compose.Project{Name: s.Project, Dir: s.Dir, Files: []string{s.Layout().Compose()}}
}

// Argv is the full argument list of one compose step.
func (s Stack) Argv(step ...string) []string { return compose.Argv(s.bin(), s.Compose(), step) }

// CommandLine is the step as a person would type it.
func (s Stack) CommandLine(step ...string) string {
	return compose.CommandLine(s.bin(), s.Compose(), step)
}

func (s Stack) bin() compose.Bin {
	if s.Bin.Name == "" {
		return compose.V2
	}
	return s.Bin
}

// Opts are the run options: the stack dir as cwd, compose variables
// scrubbed, a generous timeout (pulling images on `up`).
func (s Stack) Opts() execx.Opts {
	o := s.Compose().Opts()
	o.Timeout = 10 * time.Minute
	return o
}

// Exec runs one compose step, captured.
func (s Stack) Exec(ctx context.Context, step ...string) execx.Result {
	return s.Run.Run(ctx, s.bin().Name, s.Argv(step...), s.Opts())
}

// Service is one service of the stack as `compose ps` reports it.
type Service struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	State     string `json:"state"`
	Health    string `json:"health,omitempty"`
}

// Status lists the stack's services (none when it was never started).
func (s Stack) Status(ctx context.Context) (map[string]Service, error) {
	res := s.Exec(ctx, "ps", "-a", "--format", "json")
	if res.Code != 0 {
		return nil, &execx.Error{Cmd: "docker compose ps", Res: res}
	}
	out := map[string]Service{}
	for _, c := range compose.ParsePSJSON(res.Stdout) {
		out[c.Service] = Service{Name: c.Service, Container: c.Name, State: c.State, Health: c.Health}
	}
	return out, nil
}

// Running reports whether the stack's nginx runs.
func (s Stack) Running(ctx context.Context) bool {
	st, err := s.Status(ctx)
	return err == nil && st["nginx"].State == "running"
}

// Duplicates are compose projects other than s.Project whose containers
// run from this directory: the stack started by hand with another project
// name (EDGE-03). Two stacks would fight over :80/:443 and the certs.
func (s Stack) Duplicates(ctrs []compose.Ctr) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range ctrs {
		if c.WorkDir == s.Dir && c.Project != s.Project && !seen[c.Project] {
			seen[c.Project] = true
			out = append(out, c.Project)
		}
	}
	sort.Strings(out)
	return out
}

// Network is a Docker network as `docker network inspect` reports it.
type Network struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Subnet     string   `json:"subnet,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	Bridge     string   `json:"bridge,omitempty"`
	Containers []string `json:"containers,omitempty"`
	Compose    string   `json:"compose,omitempty"` // the compose project that created it
}

type networkDoc struct {
	Name   string `json:"Name"`
	ID     string `json:"Id"`
	Driver string `json:"Driver"`
	IPAM   struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
	Containers map[string]struct {
		Name string `json:"Name"`
	} `json:"Containers"`
}

// ParseNetworks reads `docker network inspect` output.
func ParseNetworks(out string) []Network {
	var docs []networkDoc
	if json.Unmarshal([]byte(out), &docs) != nil {
		return nil
	}
	var ns []Network
	for _, d := range docs {
		n := Network{Name: d.Name, Driver: d.Driver, Bridge: d.Options["com.docker.network.bridge.name"], Compose: d.Labels["com.docker.compose.project"]}
		for _, c := range d.IPAM.Config {
			if strings.Contains(c.Subnet, ".") || n.Subnet == "" {
				n.Subnet, n.Gateway = c.Subnet, c.Gateway
			}
		}
		if n.Bridge == "" && len(d.ID) >= 12 {
			n.Bridge = "br-" + d.ID[:12]
		}
		for _, c := range d.Containers {
			n.Containers = append(n.Containers, c.Name)
		}
		sort.Strings(n.Containers)
		ns = append(ns, n)
	}
	return ns
}

// NetworkInfo inspects one network; nil when it does not exist (EDGE-02).
func NetworkInfo(ctx context.Context, r execx.Runner, name string) *Network {
	res := r.Run(ctx, "docker", []string{"network", "inspect", name}, execx.Opts{Timeout: 15 * time.Second})
	if res.Code != 0 {
		return nil
	}
	ns := ParseNetworks(res.Stdout)
	if len(ns) == 0 {
		return nil
	}
	return &ns[0]
}

// Networks are the user-defined bridge networks, by name.
func Networks(ctx context.Context, r execx.Runner) []Network {
	res := r.Run(ctx, "docker", []string{"network", "ls", "--filter", "driver=bridge", "--format", "{{.Name}}"}, execx.Opts{Timeout: 15 * time.Second})
	if res.Code != 0 {
		return nil
	}
	var names []string
	for _, n := range strings.Fields(res.Stdout) {
		if n != "bridge" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil
	}
	ins := r.Run(ctx, "docker", append([]string{"network", "inspect"}, names...), execx.Opts{Timeout: 15 * time.Second})
	ns := ParseNetworks(ins.Stdout)
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	return ns
}

// DefaultSubnet is what a new "edge" network gets, as the legacy CLI made it.
const DefaultSubnet = "172.30.0.0/24"

// CreateNetwork makes a bridge network; "edge" gets br-edge as its
// interface name so firewall rules can name it.
func CreateNetwork(ctx context.Context, r execx.Runner, name, subnet string) error {
	args := []string{"network", "create", "--driver", "bridge"}
	if subnet != "" {
		args = append(args, "--subnet", subnet)
	}
	if name == "edge" {
		args = append(args, "--opt", "com.docker.network.bridge.name=br-edge")
	}
	args = append(args, name)
	_, err := execx.Must(ctx, "docker", args, execx.Opts{Timeout: 20 * time.Second})
	return err
}

// Precondition is one thing `edge up` needs before compose runs (the
// legacy stackUp's checks, cli/src/main.mjs:388-395).
type Precondition struct {
	Name  string
	OK    bool
	Fixed bool   // made right on the spot
	Why   string // when not OK
	Fix   string
}

// Preconditions checks the network, the token file and data/acme. The two
// files are created when missing — certbot mounts cloudflare.ini and
// nginx mounts data/acme; a path Docker invents is a root-owned directory.
func (s Stack) Preconditions(ctx context.Context, network string) []Precondition {
	l := s.Layout()
	var out []Precondition
	if NetworkInfo(ctx, s.Run, network) == nil {
		out = append(out, Precondition{Name: "network " + network, Why: "does not exist (EDGE-02)", Fix: "ngitool edge up --create-network, or ngitool edge init to pick another"})
	} else {
		out = append(out, Precondition{Name: "network " + network, OK: true})
	}
	p := Precondition{Name: "secrets/cloudflare.ini", OK: true}
	if st, err := os.Stat(l.Token()); err != nil {
		if werr := WriteToken(s.Dir, ""); werr != nil {
			p = Precondition{Name: p.Name, Why: werr.Error()}
		} else {
			p.Fixed = true
		}
	} else if st.IsDir() {
		p = Precondition{Name: p.Name, Why: "is a directory (Docker made it when the file was missing)", Fix: "rmdir " + l.Token() + " && ngitool edge up"}
	}
	out = append(out, p)
	a := Precondition{Name: "data/acme", OK: true}
	if _, err := os.Stat(l.ACME()); err != nil {
		if err := os.MkdirAll(l.ACME(), 0o755); err != nil {
			a = Precondition{Name: a.Name, Why: err.Error()}
		} else {
			a.Fixed = true
		}
	}
	return append(out, a)
}

// PortOwner is who holds a host port the stack wants (EDGE-01).
type PortOwner struct {
	Port  int
	Owner string
}

// Conflicts are the stack's ports held by something other than its own
// nginx container, from (port, owner description, container) triples.
func Conflicts(env Env, own string, held map[int][2]string) []PortOwner {
	var out []PortOwner
	for _, k := range []string{"HTTP_PORT", "HTTPS_PORT"} {
		p, err := strconv.Atoi(env[k])
		if err != nil {
			continue
		}
		if h, ok := held[p]; ok && (h[1] == "" || h[1] != own) {
			out = append(out, PortOwner{Port: p, Owner: h[0]})
		}
	}
	return out
}

// ErrNotStack is a directory that is not an edge stack.
var ErrNotStack = errors.New("not an edge stack: no compose.yaml and conf/nginx.conf (run: ngitool edge init <dir>)")

// Check reports whether dir holds a stack.
func Check(dir string) error {
	for _, p := range []string{"compose.yaml", "conf/nginx.conf"} {
		if _, err := os.Stat(Layout{dir}.p(p)); err != nil {
			return fmt.Errorf("%s: %w", dir, ErrNotStack)
		}
	}
	return nil
}

// Image is the nginx image compose.yaml names (nginx -t runs it), or "".
func Image(dir string) string {
	b, err := os.ReadFile(Layout{dir}.Compose())
	if err != nil {
		return ""
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.Services["nginx"].Image
}
