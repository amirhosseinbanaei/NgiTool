package discover

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// listener is one listening TCP socket from `ss -ltnpH`.
type listener struct {
	Addr  string // "0.0.0.0", "::", "127.0.0.1", "*"
	Port  int
	Procs []sockProc
}

type sockProc struct {
	Name string
	PID  int
}

var usersRE = regexp.MustCompile(`\("([^"]*)",pid=(\d+),fd=\d+\)`)

// parseSS reads `ss -ltnpH` output.
func parseSS(out string) []listener {
	var ls []listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(local[i+1:])
		if err != nil {
			continue
		}
		addr := strings.Trim(local[:i], "[]")
		if j := strings.IndexByte(addr, '%'); j >= 0 {
			addr = addr[:j]
		}
		l := listener{Addr: addr, Port: port}
		for _, m := range usersRE.FindAllStringSubmatch(line, -1) {
			pid, _ := strconv.Atoi(m[2])
			l.Procs = append(l.Procs, sockProc{Name: m[1], PID: pid})
		}
		ls = append(ls, l)
	}
	return ls
}

func wildcard(addr string) bool {
	return addr == "" || addr == "*" || addr == "0.0.0.0" || addr == "::"
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// scanPorts reads every listening socket and who owns it.
func (s *Scanner) scanPorts(ctx context.Context) int {
	if !s.env.has("ss") {
		s.rep.PortsStatus = "ss is not installed (apt install iproute2): port owners unknown"
		return 0
	}
	res := s.env.Run.Run(ctx, "ss", []string{"-ltnpH"}, execx.Opts{Timeout: 10 * time.Second})
	if res.Code != 0 {
		s.rep.PortsStatus = "ss failed: " + firstLine(res.Stderr)
		return 0
	}
	s.listeners = parseSS(res.Stdout)
	if s.env.Euid != 0 {
		s.rep.PortsStatus = "not root: sockets of other users' processes show no owner"
	}
	return len(s.listeners)
}

// owner resolves one listener: a host nginx (master or worker), a
// container through docker-proxy or its own pid (host network mode), or
// just a process.
func (s *Scanner) owner(l listener) PortOwner {
	o := PortOwner{Addr: l.Addr, Port: l.Port}
	for _, p := range l.Procs {
		if o.PID == 0 {
			o.PID, o.Process = p.PID, p.Name
		}
		if p.Name == "docker-proxy" {
			if c := s.proxied(p.PID, l.Port); c != nil {
				o.Container, o.Instance = c.Name, c.instance
				return o
			}
			continue
		}
		if m := s.masterOf(p.PID); m != 0 {
			for _, in := range s.inst {
				if in.Kind == KindHost && in.PID == m {
					o.Instance = in.ID
					return o
				}
			}
		}
		if c := s.containerOfPID(p.PID); c != nil {
			o.Container, o.Instance = c.Name, c.instance
			return o
		}
	}
	return o
}

// proxied maps a docker-proxy to the container publishing its host port.
func (s *Scanner) proxied(pid, port int) *ctr {
	if b, err := os.ReadFile(filepath.Join(s.env.Proc, strconv.Itoa(pid), "cmdline")); err == nil {
		args := strings.Split(string(b), "\x00")
		for i, a := range args {
			if a == "-host-port" && i+1 < len(args) {
				port, _ = strconv.Atoi(args[i+1])
			}
		}
	}
	for _, c := range s.ctrs {
		for _, p := range c.Ports {
			if p.HostPort == port && p.Proto == "tcp" && c.Running {
				return c
			}
		}
	}
	return nil
}

// containerOfPID finds the container a process runs in, by walking up to
// a container's main process.
func (s *Scanner) containerOfPID(pid int) *ctr {
	byPid := map[int]*ctr{}
	for _, c := range s.ctrs {
		if c.Pid > 0 {
			byPid[c.Pid] = c
		}
	}
	for i := 0; i < 64 && pid > 1; i++ {
		if c := byPid[pid]; c != nil {
			return c
		}
		p := s.procs[pid]
		if p == nil {
			return nil
		}
		pid = p.PPID
	}
	return nil
}

// hostPorts are the host ports an instance wants: its listens for host
// masters and host-network containers, its published ports otherwise.
func (s *Scanner) hostPorts(in *Instance) map[int][]string {
	out := map[int][]string{}
	if in.Kind == KindHost || in.NetworkMode == "host" {
		if in.Summary != nil {
			for _, srv := range in.Summary.Servers {
				for _, l := range srv.Listens {
					if l.Unix == "" && l.Port > 0 {
						out[l.Port] = appendUnique(out[l.Port], l.Addr)
					}
				}
			}
		}
		return out
	}
	for _, p := range in.Ports {
		if p.HostPort > 0 && p.Proto == "tcp" {
			out[p.HostPort] = appendUnique(out[p.HostPort], p.HostIP)
		}
	}
	return out
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}

// portOwners fills the report's port table: :80, :443 and every port an
// instance wants; then picks the front door (owner of :443, else :80).
func (s *Scanner) portOwners() {
	want := map[int]bool{80: true, 443: true}
	for _, in := range s.inst {
		for p := range s.hostPorts(in) {
			want[p] = true
		}
	}
	for _, l := range s.listeners {
		if want[l.Port] {
			s.rep.Ports = append(s.rep.Ports, s.owner(l))
		}
	}
	sort.SliceStable(s.rep.Ports, func(i, j int) bool {
		a, b := s.rep.Ports[i], s.rep.Ports[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Addr < b.Addr
	})
	for _, port := range []int{443, 80} {
		for _, o := range s.rep.Ports {
			if o.Port == port && o.Instance != "" && !loopback(o.Addr) {
				s.rep.FrontDoor = o.Instance
				break
			}
		}
		if s.rep.FrontDoor != "" {
			break
		}
	}
	for _, in := range s.inst {
		in.FrontDoor = in.ID == s.rep.FrontDoor
	}
}

// listening reports whether something on the host listens on port at host.
func (s *Scanner) listening(host string, port int) bool {
	for _, l := range s.listeners {
		if l.Port != port {
			continue
		}
		if wildcard(l.Addr) || l.Addr == host || (loopback(host) && loopback(l.Addr)) {
			return true
		}
	}
	return false
}
