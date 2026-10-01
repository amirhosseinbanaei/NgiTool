package discover

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// Compose labels docker puts on every container of a project.
const (
	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	labelWorkDir = "com.docker.compose.project.working_dir"
	labelFiles   = "com.docker.compose.project.config_files"
)

// psFormat is one tab-separated line per container with the compose
// labels (the legacy CLI's format plus config_files and the ID).
var psFormat = strings.Join([]string{
	"{{.ID}}", "{{.Names}}", "{{.Image}}", "{{.State}}", "{{.Status}}", "{{.Networks}}", "{{.Ports}}",
	`{{.Label "` + labelProject + `"}}`, `{{.Label "` + labelService + `"}}`,
	`{{.Label "` + labelWorkDir + `"}}`, `{{.Label "` + labelFiles + `"}}`,
}, "\t")

// ctr is one container as discovery needs it. Its environment is never
// decoded: it holds the project's secrets.
type ctr struct {
	ID, Name, Image, State, Status string
	Networks                       []string
	Project, Service, WorkDir      string
	ConfigFiles                    []string

	Running     bool
	Pid         int
	Entrypoint  []string
	Cmd         []string
	NetworkMode string
	Mounts      []Mount
	DNS         map[string][]string // network → names that resolve to it
	IPs         map[string]string   // network → IP
	Gateways    map[string]string   // network → gateway
	Ports       []Published
	Master      string // the nginx master's title, from docker top
	Exposed     []int  // container ports from EXPOSE and publishes
	instance    string // id when it is an nginx instance
}

// inspectJSON is the part of `docker inspect` discovery reads.
type inspectJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Pid     int    `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image      string            `json:"Image"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		Labels     map[string]string `json:"Labels"`
		Exposed    map[string]any    `json:"ExposedPorts"`
	} `json:"Config"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			Aliases   []string `json:"Aliases"`
			DNSNames  []string `json:"DNSNames"`
			IPAddress string   `json:"IPAddress"`
			Gateway   string   `json:"Gateway"`
		} `json:"Networks"`
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// dockerStatus says how far Docker can be used (DISC-13).
func (s *Scanner) dockerStatus(ctx context.Context) DockerStatus {
	if !s.env.has("docker") {
		return DockerStatus{State: "missing", Detail: "docker is not installed", Hint: "only host nginx is scanned; install Docker to manage nginx in containers"}
	}
	o := execx.Opts{Timeout: 8 * time.Second}
	if v := s.env.Run.Run(ctx, "docker", []string{"--version"}, o); strings.Contains(strings.ToLower(v.Stdout), "podman") {
		return DockerStatus{State: "podman", Version: firstLine(v.Stdout), Detail: "`docker` is podman", Hint: "podman is reported, not driven: its containers are not scanned"}
	}
	res := s.env.Run.Run(ctx, "docker", []string{"info", "--format", "{{.ServerVersion}}|{{json .SecurityOptions}}"}, o)
	if res.Code == 0 {
		ver, sec, _ := strings.Cut(strings.TrimSpace(res.Stdout), "|")
		if strings.Contains(sec, "rootless") || strings.Contains(s.env.Getenv("DOCKER_HOST"), "/run/user/") {
			return DockerStatus{State: "rootless", Version: ver, Detail: "rootless daemon " + s.env.Getenv("DOCKER_HOST"),
				Hint: "only this user's containers are visible; the system daemon's are not"}
		}
		return DockerStatus{State: "ok", Version: ver}
	}
	msg := strings.ToLower(res.Stderr)
	switch {
	case strings.Contains(msg, "permission denied"):
		return DockerStatus{State: "denied", Detail: "no permission on the docker socket", Hint: "run as root, or add this user to the docker group"}
	case res.Code == 124:
		return DockerStatus{State: "slow", Detail: "the daemon did not answer within 8s", Hint: "check systemctl status docker"}
	}
	return DockerStatus{State: "down", Detail: "the daemon is not reachable", Hint: "start it: systemctl start docker"}
}

// scanDocker lists every container, inspects them, and turns the nginx
// ones into instances.
func (s *Scanner) scanDocker(ctx context.Context) int {
	s.rep.Docker = s.dockerStatus(ctx)
	if !s.rep.Docker.Usable() {
		return 0
	}
	res := s.env.Run.Run(ctx, "docker", []string{"ps", "-a", "--no-trunc", "--format", psFormat}, execx.Opts{Timeout: 20 * time.Second})
	if res.Code != 0 {
		s.rep.Docker.Detail = "docker ps failed: " + firstLine(res.Stderr)
		return 0
	}
	s.ctrs = parsePS(res.Stdout)
	if len(s.ctrs) == 0 {
		return 0
	}
	ids := make([]string, len(s.ctrs))
	for i, c := range s.ctrs {
		ids[i] = c.ID
	}
	ins := s.env.Run.Run(ctx, "docker", append([]string{"inspect"}, ids...), execx.Opts{Timeout: 30 * time.Second})
	var docs []inspectJSON
	if err := json.Unmarshal([]byte(ins.Stdout), &docs); err != nil && ins.Code == 0 {
		s.fail("docker inspect: " + err.Error())
	}
	byID := map[string]*ctr{}
	for _, c := range s.ctrs {
		byID[c.ID] = c
		s.ctrByName[c.Name] = c
	}
	for _, d := range docs {
		if c := byID[d.ID]; c != nil {
			applyInspect(c, d)
		}
	}
	s.top(ctx)
	n := 0
	for _, c := range s.ctrs {
		if !s.candidate(c) {
			continue
		}
		s.addContainer(ctx, c)
		n++
	}
	return n
}

func parsePS(out string) []*ctr {
	var cs []*ctr
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 11 {
			continue
		}
		c := &ctr{ID: f[0], Name: f[1], Image: f[2], State: f[3], Status: f[4], Project: f[7], Service: f[8], WorkDir: f[9]}
		for _, n := range strings.Split(f[5], ",") {
			if n != "" {
				c.Networks = append(c.Networks, n)
			}
		}
		for _, p := range strings.Split(f[10], ",") {
			if p != "" {
				c.ConfigFiles = append(c.ConfigFiles, p)
			}
		}
		c.Running = c.State == "running"
		cs = append(cs, c)
	}
	return cs
}

func applyInspect(c *ctr, d inspectJSON) {
	c.Running = d.State.Running
	c.Pid = d.State.Pid
	c.State = firstNonEmpty(d.State.Status, c.State)
	c.Entrypoint, c.Cmd = d.Config.Entrypoint, d.Config.Cmd
	c.NetworkMode = d.HostConfig.NetworkMode
	for _, m := range d.Mounts {
		mt := Mount{Type: m.Type, Name: m.Name, Source: m.Source, Dest: m.Destination, RW: m.RW}
		if st, err := os.Stat(m.Source); err == nil && !st.IsDir() {
			mt.File = true
		}
		c.Mounts = append(c.Mounts, mt)
	}
	sort.Slice(c.Mounts, func(i, j int) bool { return c.Mounts[i].Dest < c.Mounts[j].Dest })
	c.DNS, c.IPs, c.Gateways = map[string][]string{}, map[string]string{}, map[string]string{}
	var nets []string
	for name, n := range d.NetworkSettings.Networks {
		nets = append(nets, name)
		names := append(append([]string{}, n.DNSNames...), n.Aliases...)
		names = append(names, strings.TrimPrefix(d.Name, "/"))
		c.DNS[name] = names
		c.IPs[name] = n.IPAddress
		c.Gateways[name] = n.Gateway
	}
	if len(nets) > 0 {
		sort.Strings(nets)
		c.Networks = nets
	}
	exposed := map[int]bool{}
	for key := range d.Config.Exposed {
		if port, proto, _ := strings.Cut(key, "/"); proto == "tcp" {
			if n, err := strconv.Atoi(port); err == nil {
				exposed[n] = true
			}
		}
	}
	for key, binds := range d.NetworkSettings.Ports {
		port, proto, _ := strings.Cut(key, "/")
		cp, _ := strconv.Atoi(port)
		if proto == "tcp" && cp > 0 {
			exposed[cp] = true
		}
		for _, b := range binds {
			hp, _ := strconv.Atoi(b.HostPort)
			c.Ports = append(c.Ports, Published{HostIP: b.HostIP, HostPort: hp, ContainerPort: cp, Proto: proto})
		}
	}
	c.Exposed = c.Exposed[:0]
	for p := range exposed {
		c.Exposed = append(c.Exposed, p)
	}
	sort.Ints(c.Exposed)
	sort.Slice(c.Ports, func(i, j int) bool {
		if c.Ports[i].HostPort != c.Ports[j].HostPort {
			return c.Ports[i].HostPort < c.Ports[j].HostPort
		}
		return c.Ports[i].HostIP < c.Ports[j].HostIP
	})
}

// top reads each running container's process list: it finds nginx in
// images not named nginx (DISC-10) and the master's -c.
func (s *Scanner) top(ctx context.Context) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, c := range s.ctrs {
		if !c.Running {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := s.env.Run.Run(ctx, "docker", []string{"top", c.Name, "-eo", "pid,ppid,args"}, execx.Opts{Timeout: 10 * time.Second})
			c.Master = masterFromTop(res.Stdout)
		}()
	}
	wg.Wait()
}

func masterFromTop(out string) string {
	for i, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if i == 0 || len(f) < 3 {
			continue
		}
		args := strings.Join(f[2:], " ")
		if masterRE.MatchString(args) {
			return args
		}
	}
	return ""
}

// repoOf is an image reference without registry tag or digest.
func repoOf(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndexByte(image, ':'); i > strings.LastIndexByte(image, '/') {
		image = image[:i]
	}
	return image
}

// nginxImage matches nginx, openresty, angie, tengine, nginx-unprivileged,
// bitnami/nginx and the proxy managers built on nginx (DISC-06, DISC-11).
func nginxImage(image string) bool {
	for _, c := range strings.Split(repoOf(image), "/") {
		switch {
		case c == "nginx", c == "openresty", c == "angie", c == "tengine", c == "swag":
			return true
		case strings.HasPrefix(c, "nginx-"):
			return true
		}
	}
	return false
}

// managers are tools that regenerate nginx's files themselves (DISC-11).
var managers = []struct{ suffix, name string }{
	{"jc21/nginx-proxy-manager", "nginx-proxy-manager"},
	{"nginxproxy/nginx-proxy", "nginx-proxy"},
	{"jwilder/nginx-proxy", "nginx-proxy"},
	{"linuxserver/swag", "swag"},
}

func managerOf(image string) string {
	r := repoOf(image)
	for _, m := range managers {
		if r == m.suffix || strings.HasSuffix(r, "/"+m.suffix) {
			return m.name
		}
	}
	return ""
}

var nginxWordRE = regexp.MustCompile(`(^|[/\s"'])(nginx|openresty|angie)(\s|$|")`)

func (s *Scanner) candidate(c *ctr) bool {
	switch {
	case nginxImage(c.Image):
		return true
	case c.Running:
		return c.Master != ""
	}
	return nginxWordRE.MatchString(strings.Join(append(append([]string{}, c.Entrypoint...), c.Cmd...), " "))
}

// edgeLayout reports whether dir is NgiTool's edge stack (compose's rule:
// compose.yaml, conf/nginx.conf and the marker or the legacy edge CLI).
func edgeLayout(dir string) bool { return compose.EdgeStack(dir) }

func (s *Scanner) addContainer(ctx context.Context, c *ctr) {
	in := &Instance{
		Name:        c.Name,
		Container:   c.Name,
		Image:       c.Image,
		State:       StateStopped,
		Project:     c.Project,
		Service:     c.Service,
		WorkingDir:  c.WorkDir,
		ComposeFile: c.ConfigFiles,
		NetworkMode: c.NetworkMode,
		Networks:    c.Networks,
		Ports:       c.Ports,
		Mounts:      c.Mounts,
		ManagedBy:   ManagedNone,
		Variant:     variantOfImage(c.Image),
	}
	if c.Running {
		in.State = StateRunning
	}
	switch {
	case c.Project != "" && edgeLayout(c.WorkDir):
		in.Kind, in.ID, in.ManagedBy = KindEdge, "edge:"+c.WorkDir, ManagedEdge
	case c.Project != "":
		in.Kind, in.ID = KindCompose, "compose:"+c.Project+"/"+c.Service
	default:
		in.Kind, in.ID = KindContainer, "ctr:"+c.Name
	}
	if m := managerOf(c.Image); m != "" {
		in.ManagedBy = "other:" + m
	}
	if c.WorkDir != "" {
		in.Owner = s.env.ownerOf(c.WorkDir)
	}
	for _, m := range c.Mounts {
		if m.Dest == "/etc/nginx/templates" || strings.HasPrefix(m.Dest, "/etc/nginx/templates/") {
			in.Templates = true
		}
	}
	if c.NetworkMode == "host" {
		in.Notes = append(in.Notes, "host network mode: its ports are the host's (DISC-12)")
	}
	if c.Master != "" {
		m := masterRE.FindStringSubmatch(c.Master)
		bin, conf, _, _ := cmdArgs(m[2])
		in.Bin, in.Conf = bin, conf
		if m[1] != "nginx" {
			in.Variant = m[1]
		}
	}
	if in.Conf == "" {
		in.Conf = confFromCmd(append(append([]string{}, c.Entrypoint...), c.Cmd...))
	}
	if c.Running {
		bin := firstNonEmpty(in.Bin, "nginx")
		res := s.env.Run.Run(ctx, "docker", []string{"exec", c.Name, bin, "-v"}, execx.Opts{Timeout: 10 * time.Second})
		if b := parseV(res.Stderr + res.Stdout); b.Version != "" {
			in.Version, in.Variant = b.Version, b.Variant
		}
	}
	var dm []driver.Mount
	var fm []nginxconf.Mount
	for _, m := range c.Mounts {
		if m.Type == "bind" || m.Type == "volume" {
			dm = append(dm, driver.Mount{Type: m.Type, Source: m.Source, Name: m.Name, Dest: m.Dest})
			fm = append(fm, nginxconf.Mount{Dest: m.Dest, Source: m.Source})
		}
	}
	src := nginxconf.FileSource{Container: true, Mounts: fm}
	if c.Running && c.Pid > 0 && s.env.Euid == 0 {
		// The container's own view, mounts included.
		src = nginxconf.FileSource{Container: true, Root: filepath.Join(s.env.Proc, strconv.Itoa(c.Pid), "root")}
	}
	c.instance = s.add(in, &build{
		drv:   driver.Container{Run: s.env.Run, Name: c.Name, Image: c.Image, Bin: in.Bin, Conf: in.Conf, Running: c.Running, Mounts: dm},
		src:   src,
		stock: stockImage(c.Image),
		files: fm,
		ctr:   c,
	})
}

// confFromCmd finds -c in a stopped container's entrypoint and command.
func confFromCmd(argv []string) string {
	for i, a := range argv {
		if a == "-c" && i+1 < len(argv) && strings.HasSuffix(argv[i+1], ".conf") {
			return argv[i+1]
		}
	}
	return ""
}

func variantOfImage(image string) string {
	r := repoOf(image)
	for _, v := range []string{"openresty", "angie", "tengine"} {
		if strings.Contains(r, v) {
			return v
		}
	}
	return "nginx"
}

// stockImage reports whether the image ships the official nginx.conf that
// includes /etc/nginx/conf.d/*.conf, so mounting only conf.d is common.
func stockImage(image string) bool {
	for _, c := range strings.Split(repoOf(image), "/") {
		if c == "nginx" || c == "nginx-unprivileged" {
			return true
		}
	}
	return false
}

// stockMain is the official image's /etc/nginx/nginx.conf, used when only
// conf.d is mounted and the main file cannot be read (stopped container).
const stockMain = `user  nginx;
worker_processes  auto;
error_log  /var/log/nginx/error.log notice;
pid        /run/nginx.pid;
events {
    worker_connections  1024;
}
http {
    include       /etc/nginx/mime.types;
    default_type  application/octet-stream;
    sendfile        on;
    keepalive_timeout  65;
    include /etc/nginx/conf.d/*.conf;
}
`

// overlay serves a few files from memory over another source.
type overlay struct {
	files map[string]string
	base  nginxconf.Source
}

func (o overlay) Kind() string { return o.base.Kind() }

func (o overlay) Read(p string) ([]byte, error) {
	if s, ok := o.files[p]; ok {
		return []byte(s), nil
	}
	return o.base.Read(p)
}

func (o overlay) Glob(pattern string) ([]string, error) {
	m, err := o.base.Glob(pattern)
	for p := range o.files {
		if ok, _ := path.Match(pattern, p); ok {
			m = append(m, p)
		}
	}
	sort.Strings(m)
	return m, err
}
