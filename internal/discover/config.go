package discover

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"path"
	"strconv"
	"strings"
	"syscall"

	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// build is what loading an instance's config needs; it is not reported.
type build struct {
	drv   driver.Driver
	src   nginxconf.Source
	stock bool              // the image ships the stock nginx.conf
	files []nginxconf.Mount // bind/volume mounts, for the stopped-file fallback
	ctr   *ctr
}

// defaultConf is nginx's main file when nothing else says.
const defaultConf = "/etc/nginx/nginx.conf"

// loadConfig reads an instance's configuration: `nginx -T` first, then the
// files themselves (CONF-01, CONF-02). `-T` also validates: a running
// instance whose dump fails is invalid (CONF-03) and is parsed from its
// files instead. The dump is parsed in memory and dropped (CONF-11).
func (s *Scanner) loadConfig(ctx context.Context, in *Instance, b *build) {
	var cfg *nginxconf.Config
	if in.State == StateRunning && b.drv != nil && (in.Kind == KindHost || s.rep.Docker.Usable()) {
		out, err := b.drv.Dump(ctx)
		if err == nil {
			main, files := nginxconf.SplitDump(out)
			if main != "" {
				cfg = nginxconf.Load(nginxconf.DumpSource(files), main)
				t := true
				in.Valid = &t
			}
		} else if strings.Contains(err.Error(), "[emerg]") || strings.Contains(err.Error(), "test failed") {
			f := false
			in.Valid, in.TestOutput = &f, err.Error()
		} else {
			in.Notes = append(in.Notes, "nginx -T did not run: "+err.Error())
		}
	}
	if cfg == nil {
		main := firstNonEmpty(in.Conf, defaultConf)
		cfg = nginxconf.Load(b.src, main)
		if len(cfg.Files) == 0 && b.stock && main == defaultConf {
			// Only conf.d is mounted: the main file is the image's own.
			src := b.src
			if fsrc, ok := src.(nginxconf.FileSource); ok && fsrc.Root != "" {
				src = nginxconf.FileSource{Container: true, Mounts: b.files}
			}
			alt := nginxconf.Load(overlay{files: map[string]string{defaultConf: stockMain, "/etc/nginx/mime.types": ""}, base: src}, main)
			if len(alt.Files) > 2 {
				cfg, in.StockMain = alt, true
				in.Notes = append(in.Notes, "main nginx.conf is inside the image; assumed to be the stock one that includes conf.d/*.conf")
			}
		}
	}
	in.Conf, in.Source = cfg.Main, cfg.Source
	for _, f := range cfg.Files {
		if !(in.StockMain && (f.Path == defaultConf || f.Path == "/etc/nginx/mime.types")) {
			in.Files = append(in.Files, f)
		}
	}
	for _, e := range cfg.Errors {
		in.ConfigErrors = append(in.ConfigErrors, e.Error())
		if strings.Contains(e.Msg, "permission denied") {
			s.fail(e.Error())
		}
	}
	if len(in.ConfigErrors) > 20 {
		in.ConfigErrors = append(in.ConfigErrors[:20], "…")
	}
	sum := nginxconf.Summarize(cfg.Tree)
	in.Summary = &sum
	if b.drv != nil {
		in.Methods = b.drv.Describe()
	}
}

// capabilities decides read, test, reload and write, each with a reason.
func (s *Scanner) capabilities(in *Instance, b *build) {
	c := &in.Caps
	other := strings.TrimPrefix(in.ManagedBy, "other:")
	isOther := strings.HasPrefix(in.ManagedBy, "other:")
	container := in.Kind != KindHost
	invalid := in.Valid != nil && !*in.Valid

	// read
	switch {
	case len(in.Files) > 0 && in.Source == "dump":
		c.Read = yes("from nginx -T")
	case len(in.Files) > 0:
		c.Read = yes("from the files (" + filesNote(in) + ")")
	case len(in.ConfigErrors) > 0:
		c.Read = no(strings.TrimPrefix(in.ConfigErrors[0], "include "))
		if strings.Contains(in.ConfigErrors[0], "inside the image") {
			c.Read = no("config is inside the image and the container is not running (CONF-06)")
		}
	default:
		c.Read = no("config not found")
	}

	// test
	switch {
	case in.State == StateDefined:
		c.Test = no("never started — nothing to test against yet")
	case container && !s.rep.Docker.Usable():
		c.Test = no("docker is not usable: " + s.rep.Docker.Detail)
	case in.Kind == KindHost && in.Exe == "":
		c.Test = no("nginx binary not found")
	case in.State == StateStopped && container:
		c.Test = yes("in a throwaway container: same image and mounts, read-only, no network")
	case in.Kind == KindHost && s.env.Euid != 0:
		c.Test = yes("without root nginx -t may not read every file")
	default:
		c.Test = yes("")
	}

	// reload
	switch {
	case isOther:
		c.Reload = no("managed by " + other + ", which reloads it itself")
	case in.State == StateDefined:
		c.Reload = no("never started")
	case in.State == StateStopped:
		c.Reload = no("not running — nginx reads its config when it starts")
	case container && !s.rep.Docker.Usable():
		c.Reload = no("docker is not usable: " + s.rep.Docker.Detail)
	case invalid:
		c.Reload = no("config fails nginx -t — fix it first (CONF-03)")
	case in.Kind == KindHost && s.env.Euid != 0:
		c.Reload = no("needs root: run ngitool with sudo")
	default:
		c.Reload = yes(in.Methods.Reload)
	}

	c.Write = s.writable(in, isOther, other, invalid)
}

func filesNote(in *Instance) string {
	switch {
	case in.State == StateDefined:
		return "its bind mounts"
	case in.Kind == KindHost:
		return "nginx -T unavailable"
	case in.State == StateStopped:
		return "the container is stopped; read through its mounts"
	}
	return "nginx -T failed"
}

// writable is the write capability (CONF-06, CONF-07, CONF-08, CONF-10):
// true only when the directory NgiTool would add its file to (the hook)
// is on the host — a host path, or a bind mount / volume of the container.
func (s *Scanner) writable(in *Instance, isOther bool, other string, invalid bool) Capability {
	switch {
	case isOther:
		return no("managed by " + other + ": it regenerates the files, so NgiTool never writes there (DISC-11)")
	case in.State == StateDefined:
		return no("defined, not created — read-only until it has run once: docker compose up -d " + in.Service + " in " + in.WorkingDir)
	case invalid:
		return no("config fails nginx -t — nothing is written on top of a broken config (CONF-03)")
	case in.Caps.Read.OK == false:
		return no("config could not be read")
	case in.Summary == nil || in.Summary.Hook == nil:
		return no("no http {} block to hook into")
	}
	for _, f := range in.Files {
		if f.NonUTF8 {
			return no(f.Path + " is not UTF-8 — read-only (CONF-10)")
		}
	}
	h := in.Summary.Hook
	target := h.Dir
	if h.NeedsLine {
		target = h.Pos.File
	}
	if in.Kind == KindHost {
		if s.env.Euid != 0 {
			return no("needs root: run ngitool with sudo")
		}
		dir := target
		if h.NeedsLine {
			dir = path.Dir(target)
		}
		if err := syscall.Access(dir, 2); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return no(dir + " is not writable: " + err.Error())
		}
		return yes("host files under " + dir)
	}
	var best *Mount
	for i := range in.Mounts {
		m := &in.Mounts[i]
		if (m.Type == "bind" || m.Type == "volume") && under(target, m.Dest) && (best == nil || len(m.Dest) > len(best.Dest)) {
			best = m
		}
	}
	if best == nil {
		return no("config is inside the image: no bind mount covers " + target + " (CONF-06)")
	}
	var notes []string
	host := best.Source + strings.TrimPrefix(target, best.Dest)
	switch {
	case best.Type == "volume":
		notes = append(notes, "named volume "+best.Name+", written on the host at "+host)
	case best.File:
		notes = append(notes, "single-file mount: edited in place so the container keeps seeing the same inode (CONF-07)")
	default:
		notes = append(notes, "bind mount: NgiTool writes the host side, "+host)
	}
	if !best.RW {
		notes = append(notes, ":ro only stops the container from writing; the host side stays writable")
	}
	if in.Templates {
		notes = append(notes, "/etc/nginx/templates is mounted: edits go to the templates, ${VAR} kept (CONF-08)")
	}
	if in.ManagedBy == ManagedEdge {
		notes = append(notes, "the legacy edge CLI's files (# Managed by edge) are left alone until ngitool migrate edge imports them")
	}
	return yes(strings.Join(notes, "; "))
}

func under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// reachability colours every proxy target: a container on a shared
// network is ok, a missing one is err; a host port that listens is ok,
// otherwise warn.
func (s *Scanner) reachability(in *Instance) {
	if in.Summary == nil {
		return
	}
	ups := map[string]nginxconf.Upstream{}
	for _, u := range in.Summary.Upstreams {
		ups[u.Name] = u
	}
	in.Reach = map[string]Reach{}
	for _, srv := range in.Summary.Servers {
		nginxconf.Walk(srv.Locations, func(l *nginxconf.Location) {
			t := l.Target
			if t == nil {
				return
			}
			key := t.Pos.String()
			switch {
			case t.Upstream != "":
				for _, m := range ups[t.Upstream].Servers {
					in.Reach[m.Pos.String()] = s.reach(in, m.Host, m.Port, m.Unix)
				}
				in.Reach[key] = Reach{Status: ReachOK, Why: "upstream " + t.Upstream}
			case t.Host == "" && t.Unix == "":
				in.Reach[key] = Reach{Status: ReachUnknown, Why: "decided per request (" + t.Raw + ")"}
			default:
				in.Reach[key] = s.reach(in, t.Host, t.Port, t.Unix)
			}
		})
	}
}

func (s *Scanner) reach(in *Instance, host string, port int, unix string) Reach {
	if unix != "" {
		return Reach{Status: ReachUnknown, Why: "unix socket " + unix}
	}
	hostNet := in.Kind == KindHost || in.NetworkMode == "host"
	ip := net.ParseIP(host)
	portText := ":" + strconv.Itoa(port)
	listen := func(h string) Reach {
		if s.listening(h, port) {
			return Reach{Status: ReachOK, Why: "something listens on the host at " + h + portText}
		}
		return Reach{Status: ReachWarn, Why: "nothing listens on the host at " + h + portText}
	}
	if hostNet {
		if loopback(host) || ip != nil {
			return listen(host)
		}
		if c := s.containerNamed(host); c != nil {
			return Reach{Status: ReachErr, Why: host + " is a container name, which does not resolve on the host — publish a port on 127.0.0.1"}
		}
		return Reach{Status: ReachUnknown, Why: "outside Docker, not checked"}
	}
	if loopback(host) {
		return Reach{Status: ReachWarn, Why: "loopback inside the container itself, not the host"}
	}
	mine := map[string]bool{}
	for _, n := range in.Networks {
		mine[n] = true
	}
	if ip != nil {
		for _, c := range s.ctrs {
			for n, a := range c.IPs {
				if a == host && mine[n] {
					return runningReach(c, "container "+c.Name+" on "+n)
				}
			}
			for n, g := range c.Gateways {
				if g == host && mine[n] {
					return listen(host)
				}
			}
		}
		return Reach{Status: ReachUnknown, Why: "address outside Docker, not checked"}
	}
	var elsewhere []string
	for _, c := range s.ctrs {
		for n, names := range c.DNS {
			for _, dn := range names {
				if !strings.EqualFold(dn, host) {
					continue
				}
				if mine[n] {
					return runningReach(c, "container "+c.Name+" on "+n)
				}
				elsewhere = appendUnique(elsewhere, c.Name+" ("+n+")")
			}
		}
	}
	switch {
	case len(elsewhere) > 0:
		return Reach{Status: ReachErr, Why: host + " is on " + strings.Join(elsewhere, ", ") + ", no network shared with " + in.Name}
	case !strings.Contains(host, "."):
		return Reach{Status: ReachErr, Why: "no container or alias named " + host}
	}
	return Reach{Status: ReachUnknown, Why: "outside Docker, not checked"}
}

func runningReach(c *ctr, where string) Reach {
	if c.Running {
		return Reach{Status: ReachOK, Why: where}
	}
	return Reach{Status: ReachErr, Why: where + " is " + c.State}
}

func (s *Scanner) containerNamed(name string) *ctr {
	for _, c := range s.ctrs {
		for _, names := range c.DNS {
			for _, n := range names {
				if strings.EqualFold(n, name) {
					return c
				}
			}
		}
	}
	return nil
}
