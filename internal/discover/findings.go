package discover

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

func (s *Scanner) find(sev, code, msg, fix string, ids ...string) {
	s.rep.Findings = append(s.rep.Findings, Finding{Severity: sev, Code: code, Message: msg, Fix: fix, Instances: ids})
}

// findings runs every cross-instance rule.
func (s *Scanner) findings() {
	s.systemFindings()
	s.nameFindings()
	s.targetFindings()
	s.portFindings()
	s.doubleProxy()
	for _, in := range s.inst {
		if in.Valid != nil && !*in.Valid {
			s.find(SevError, "CONF-03", in.Name+": config fails nginx -t: "+firstLine(in.TestOutput),
				"fix the file nginx names, then run ngitool inspect "+in.ID, in.ID)
		}
		if in.Summary != nil && in.Summary.Stream != nil {
			s.find(SevInfo, "RP-14", in.Name+" has a stream {} block ("+in.Summary.Stream.String()+")",
				"TCP/UDP proxying is shown read-only; NgiTool never changes it", in.ID)
		}
		if strings.HasPrefix(in.ManagedBy, "other:") {
			s.find(SevInfo, "DISC-11", in.Name+" is managed by "+strings.TrimPrefix(in.ManagedBy, "other:"),
				"NgiTool only reads it; change it with its own tool", in.ID)
		}
	}
	rank := map[string]int{SevError: 0, SevWarn: 1, SevInfo: 2}
	sort.SliceStable(s.rep.Findings, func(i, j int) bool {
		return rank[s.rep.Findings[i].Severity] < rank[s.rep.Findings[j].Severity]
	})
}

func (s *Scanner) systemFindings() {
	if len(s.inst) == 0 {
		s.find(SevInfo, "DISC-01", "no nginx found on this machine (only this machine is scanned, DISC-17)",
			"the bundled edge stack (compose.yaml in the NgiTool repository) can be your front door")
	}
	if s.env.Euid != 0 {
		s.find(SevWarn, "DISC-14", "not running as root: this is a partial view ("+strconv.Itoa(len(s.rep.ReadFailures))+" reads failed)",
			"run with sudo to see every process, socket owner and config file")
	}
	if d := s.rep.Docker; !d.Usable() && d.State != "missing" {
		s.find(SevWarn, "DISC-13", "containers not scanned: "+d.Detail, d.Hint)
	} else if d.State == "rootless" {
		s.find(SevInfo, "DISC-13", "rootless Docker: "+d.Detail, d.Hint)
	}
	if s.kubelet || s.ingress {
		what := "a kubelet runs on this machine"
		if s.ingress {
			what = "Kubernetes ingress-nginx runs on this machine"
		}
		s.find(SevInfo, "DISC-16", what, "Kubernetes ingress is out of scope; NgiTool never touches it")
	}
	for _, p := range s.foreignMasters {
		if s.containerOfPID(p.PID) == nil {
			s.find(SevInfo, "DISC-17", "nginx master pid "+strconv.Itoa(p.PID)+" runs in a namespace Docker does not own (Kubernetes, LXC or podman?)",
				"only host nginx and Docker containers are managed")
		}
	}
}

// nameFindings: the same server_name twice on one listen of one instance
// (RP-03), or on two instances (RP-04).
func (s *Scanner) nameFindings() {
	type use struct {
		in  *Instance
		pos nginxconf.Pos
	}
	across := map[string][]use{}
	for _, in := range s.inst {
		if in.Summary == nil {
			continue
		}
		seen := map[string]nginxconf.Pos{}
		reported := map[string]bool{}
		for _, srv := range in.Summary.Servers {
			for _, n := range srv.Names {
				if n.Name == "_" || n.Name == "" {
					continue
				}
				for _, l := range srv.Listens {
					k := l.Key() + " " + strings.ToLower(n.Name)
					if first, dup := seen[k]; dup && !reported[k] {
						reported[k] = true
						s.find(SevWarn, "RP-03", fmt.Sprintf("%s: %s is defined twice on %s (%s and %s); nginx uses the first",
							in.Name, n.Name, l.Key(), first, srv.Pos), "merge the two server blocks or remove one", in.ID)
					} else if !dup {
						seen[k] = srv.Pos
					}
				}
				if n.Name == "localhost" {
					continue
				}
				k := strings.ToLower(n.Name)
				if us := across[k]; len(us) == 0 || us[len(us)-1].in != in {
					across[k] = append(across[k], use{in, srv.Pos})
				}
			}
		}
	}
	names := make([]string, 0, len(across))
	for n := range across {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		us := across[n]
		if len(us) < 2 {
			continue
		}
		var where, ids []string
		for _, u := range us {
			where = append(where, u.in.Name+" ("+u.pos.String()+")")
			ids = append(ids, u.in.ID)
		}
		s.find(SevWarn, "RP-04", n+" is served by "+strings.Join(where, " and "),
			"decide which one answers: chain the front door to the other, or remove one", ids...)
	}
}

// targetFindings: proxy targets that cannot be reached (missing container,
// no shared network) and static names that stop nginx from starting when
// the container is down (RP-07).
func (s *Scanner) targetFindings() {
	for _, in := range s.inst {
		if in.Summary == nil {
			continue
		}
		bad := map[string]bool{}
		for _, srv := range in.Summary.Servers {
			nginxconf.Walk(srv.Locations, func(l *nginxconf.Location) {
				t := l.Target
				if t == nil {
					return
				}
				if r := in.Reach[t.Pos.String()]; r.Status == ReachErr && !bad[t.Host] {
					bad[t.Host] = true
					s.find(SevError, "RP-05", in.Name+": "+t.Directive+" "+t.Effective()+" ("+t.Pos.String()+"): "+r.Why,
						"put nginx and "+t.Host+" on one network, or fix the name", in.ID)
				}
				if !t.Variable && t.Upstream == "" && staticName(t.Host) {
					s.find(SevWarn, "RP-07", in.Name+": "+t.Directive+" "+t.Raw+" ("+t.Pos.String()+") is resolved once at start: nginx will not start while "+t.Host+" is down",
						"use a variable with a resolver (set $u "+t.Host+"; proxy_pass …$u) or an upstream server with resolve", in.ID)
				}
			})
		}
		for _, u := range in.Summary.Upstreams {
			for _, m := range u.Servers {
				if r := in.Reach[m.Pos.String()]; r.Status == ReachErr && !bad[m.Host] {
					bad[m.Host] = true
					s.find(SevError, "RP-05", in.Name+": upstream "+u.Name+" server "+m.Addr+" ("+m.Pos.String()+"): "+r.Why,
						"put nginx and "+m.Host+" on one network, or fix the name", in.ID)
				}
				if staticName(m.Host) && !m.Resolve {
					s.find(SevWarn, "RP-07", in.Name+": upstream "+u.Name+" server "+m.Addr+" ("+m.Pos.String()+") has no resolve: nginx will not start while "+m.Host+" is down",
						"add resolve (nginx ≥ 1.27.3, with a resolver and a zone) or use a variable proxy_pass", in.ID)
				}
			}
		}
	}
}

// staticName is a DNS name nginx resolves once at startup.
func staticName(host string) bool {
	return host != "" && net.ParseIP(host) == nil && host != "localhost" && !strings.Contains(host, "$")
}

// portFindings: nobody on :80/:443, two instances wanting one host port,
// and published 0.0.0.0 ports that bypass the front door (DOCK-11).
func (s *Scanner) portFindings() {
	if s.rep.PortsStatus == "" || s.env.Euid != 0 {
		for _, port := range []int{80, 443} {
			owned := false
			for _, o := range s.rep.Ports {
				owned = owned || o.Port == port
			}
			if !owned && s.env.has("ss") {
				s.find(SevInfo, "DISC-15", "nothing listens on :"+strconv.Itoa(port), "a front door needs :80 and :443")
			}
		}
		for _, o := range s.rep.Ports {
			if (o.Port == 80 || o.Port == 443) && o.Instance == "" && !loopback(o.Addr) && o.Process != "" {
				who := o.Process
				if o.Container != "" {
					who = "container " + o.Container
				}
				s.find(SevInfo, "DISC-15", ":"+strconv.Itoa(o.Port)+" is owned by "+who+", not an nginx", "only one program can own a port; the front door must be an nginx NgiTool knows")
			}
		}
	}
	type claim struct {
		in   *Instance
		addr string
	}
	wants := map[int][]claim{}
	for _, in := range s.inst {
		for port, addrs := range s.hostPorts(in) {
			for _, a := range addrs {
				wants[port] = append(wants[port], claim{in, a})
			}
		}
	}
	ports := make([]int, 0, len(wants))
	for p := range wants {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, p := range ports {
		cs := wants[p]
		for i := 0; i < len(cs); i++ {
			for j := i + 1; j < len(cs); j++ {
				a, b := cs[i], cs[j]
				if a.in == b.in || !(wildcard(a.addr) || wildcard(b.addr) || a.addr == b.addr) {
					continue
				}
				s.find(SevWarn, "DISC-15", fmt.Sprintf("%s and %s are both configured for host port %d; only one can bind it", a.in.Name, b.in.Name, p),
					"move one to another port, or let the front door proxy to the other", a.in.ID, b.in.ID)
			}
		}
	}
	front := s.frontDoorContainer()
	for _, c := range s.ctrs {
		if !c.Running || c.NetworkMode == "host" {
			continue
		}
		var open []string
		for _, p := range c.Ports {
			if p.Public() && !(c == front && (p.ContainerPort == 80 || p.ContainerPort == 443)) {
				open = appendUnique(open, strconv.Itoa(p.HostPort))
			}
		}
		if len(open) == 0 {
			continue
		}
		var ids []string
		if c.instance != "" {
			ids = append(ids, c.instance)
		}
		s.find(SevWarn, "DOCK-11", "container "+c.Name+" publishes 0.0.0.0:"+strings.Join(open, ", :")+", bypassing the front door",
			"publish on 127.0.0.1 only (\"127.0.0.1:"+open[0]+":…\") or use expose: and a shared network", ids...)
	}
}

func (s *Scanner) frontDoorContainer() *ctr {
	for _, c := range s.ctrs {
		if c.instance != "" && c.instance == s.rep.FrontDoor {
			return c
		}
	}
	return nil
}

// doubleProxy: the front door proxies to another nginx instance (DOCK-15).
func (s *Scanner) doubleProxy() {
	var front *Instance
	for _, in := range s.inst {
		if in.ID == s.rep.FrontDoor {
			front = in
		}
	}
	if front == nil || front.Summary == nil {
		return
	}
	seen := map[string]bool{}
	for _, srv := range front.Summary.Servers {
		tls := false
		for _, l := range srv.Listens {
			tls = tls || l.SSL
		}
		nginxconf.Walk(srv.Locations, func(l *nginxconf.Location) {
			t := l.Target
			if t == nil || t.Host == "" {
				return
			}
			inner := s.instanceAt(front, t.Host, t.Port)
			if inner == nil || inner == front || seen[inner.ID+srv.NameList()] {
				return
			}
			seen[inner.ID+srv.NameList()] = true
			who := front.Name + " terminates TLS; " + inner.Name + " gets plain HTTP"
			if !tls {
				who = "neither terminates TLS for " + srv.NameList()
			}
			s.find(SevInfo, "DOCK-15", "double proxy: "+front.Name+" → "+inner.Name+" for "+srv.NameList()+" ("+who+")",
				"make sure "+inner.Name+" trusts X-Forwarded-Proto from the front door", front.ID, inner.ID)
		})
	}
}

// instanceAt is the nginx instance a target reaches: a container by name
// or IP, or a host port an instance owns.
func (s *Scanner) instanceAt(from *Instance, host string, port int) *Instance {
	byID := map[string]*Instance{}
	for _, in := range s.inst {
		byID[in.ID] = in
	}
	if c := s.containerNamed(host); c != nil && c.instance != "" {
		return byID[c.instance]
	}
	for _, c := range s.ctrs {
		for _, ip := range c.IPs {
			if ip == host && c.instance != "" {
				return byID[c.instance]
			}
		}
	}
	if net.ParseIP(host) == nil && host != "localhost" {
		return nil
	}
	for _, o := range s.rep.Ports {
		if o.Port == port && o.Instance != "" && o.Instance != from.ID {
			return byID[o.Instance]
		}
	}
	return nil
}
