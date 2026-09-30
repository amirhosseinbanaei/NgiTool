package discover

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// proc is one process from /proc.
type proc struct {
	PID, PPID, UID int
	Cmd            string // cmdline, NULs as spaces
	Foreign        bool   // in another mount namespace or a container cgroup
}

// masterRE matches the title nginx and its variants give their master
// process (DISC-06). openresty and tengine keep "nginx:".
var masterRE = regexp.MustCompile(`^(nginx|openresty|angie|tengine): master process\s*(.*)$`)

// readProcs reads every process: pid, parent, uid and command line.
func (s *Scanner) readProcs() {
	s.procs = map[int]*proc{}
	entries, err := os.ReadDir(s.env.Proc)
	if err != nil {
		s.fail(s.env.Proc + ": " + errText(err))
		return
	}
	s.selfNS, _ = os.Readlink(filepath.Join(s.env.Proc, "self", "ns", "mnt"))
	for _, d := range entries {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join(s.env.Proc, d.Name())
		b, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue // exited, or a kernel thread
		}
		p := &proc{PID: pid, Cmd: strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))}
		if st, err := os.ReadFile(filepath.Join(dir, "stat")); err == nil {
			// "pid (comm) S ppid …": comm may hold spaces and parens.
			if i := strings.LastIndexByte(string(st), ')'); i > 0 {
				f := strings.Fields(string(st[i+1:]))
				if len(f) > 1 {
					p.PPID, _ = strconv.Atoi(f[1])
				}
			}
		}
		if st, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			for _, l := range strings.Split(string(st), "\n") {
				if f := strings.Fields(l); len(f) > 1 && f[0] == "Uid:" {
					p.UID, _ = strconv.Atoi(f[1])
				}
			}
		}
		s.procs[pid] = p
	}
}

var containerCgroupRE = regexp.MustCompile(`docker|containerd|kubepods|libpod|lxc|machine\.slice`)

// foreign reports whether p runs in a container: another mount namespace,
// or a container's cgroup when the namespace cannot be read.
func (s *Scanner) foreign(p *proc) bool {
	dir := filepath.Join(s.env.Proc, strconv.Itoa(p.PID))
	if ns, err := os.Readlink(filepath.Join(dir, "ns", "mnt")); err == nil && s.selfNS != "" {
		return ns != s.selfNS
	}
	if cg, err := os.ReadFile(filepath.Join(dir, "cgroup")); err == nil {
		return containerCgroupRE.Match(cg)
	}
	return false
}

// unitOf is the systemd service a process belongs to, from its cgroup.
func (s *Scanner) unitOf(pid int) string {
	cg, err := os.ReadFile(filepath.Join(s.env.Proc, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(cg), "\n") {
		parts := strings.SplitN(l, ":", 3)
		if len(parts) != 3 || (parts[0] != "0" && !strings.Contains(parts[1], "systemd")) {
			continue
		}
		base := path.Base(parts[2])
		if strings.HasSuffix(base, ".service") {
			return base
		}
	}
	return ""
}

// masterOf walks up from pid to an nginx master, or 0.
func (s *Scanner) masterOf(pid int) int {
	for i := 0; i < 64 && pid > 1; i++ {
		p := s.procs[pid]
		if p == nil {
			return 0
		}
		if masterRE.MatchString(p.Cmd) {
			return pid
		}
		pid = p.PPID
	}
	return 0
}

// cmdArgs reads the binary, -c, -p and -g from a master's title or an
// ExecStart line. nginx joins its argv with spaces, so -g runs until the
// next flag.
func cmdArgs(line string) (bin, conf, prefix, globals string) {
	f := strings.Fields(line)
	if len(f) > 0 && !strings.HasPrefix(f[0], "-") {
		bin, f = f[0], f[1:]
	}
	flags := map[string]bool{"-c": true, "-p": true, "-g": true, "-e": true, "-q": true, "-t": true, "-T": true, "-s": true}
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case "-c", "-p", "-e":
			if i+1 < len(f) {
				if f[i] == "-c" {
					conf = f[i+1]
				} else if f[i] == "-p" {
					prefix = f[i+1]
				}
				i++
			}
		case "-g":
			var g []string
			for i+1 < len(f) && !flags[f[i+1]] {
				i++
				g = append(g, f[i])
			}
			globals = strings.Join(g, " ")
		}
	}
	return
}

// Build info from `<exe> -V`.
type buildInfo struct {
	Variant, Version, ConfPath, Prefix string
}

var versionRE = regexp.MustCompile(`(?i)(nginx|openresty|tengine|angie)/[0-9][0-9A-Za-z.\-]*`)

func parseV(out string) buildInfo {
	var b buildInfo
	if m := versionRE.FindString(out); m != "" {
		b.Version = m
		b.Variant = strings.ToLower(m[:strings.IndexByte(m, '/')])
	}
	for _, f := range strings.Fields(out) {
		switch {
		case strings.HasPrefix(f, "--conf-path="):
			b.ConfPath = strings.TrimPrefix(f, "--conf-path=")
		case strings.HasPrefix(f, "--prefix="):
			b.Prefix = strings.TrimPrefix(f, "--prefix=")
		}
	}
	return b
}

func (s *Scanner) buildInfo(ctx context.Context, exe string) buildInfo {
	res := s.env.Run.Run(ctx, exe, []string{"-V"}, execx.Opts{Timeout: 5 * time.Second})
	return parseV(res.Stderr + "\n" + res.Stdout)
}

// confPath is the main config nginx reads: -c (relative to the prefix),
// else the compiled-in path.
func confPath(conf, prefix string, b buildInfo) string {
	if prefix == "" {
		prefix = b.Prefix
	}
	if conf == "" {
		conf = b.ConfPath
	}
	if conf != "" && !path.IsAbs(conf) && prefix != "" {
		conf = path.Join(prefix, conf)
	}
	return conf
}

// scanHost finds host masters (running) and installed-but-stopped nginx
// (binaries, systemd units), and notes masters that run in containers so
// docker discovery and the port map can use them.
func (s *Scanner) scanHost(ctx context.Context) int {
	s.readProcs()
	seenExe := map[string]bool{}
	seenUnit := map[string]bool{}
	pids := make([]int, 0, len(s.procs))
	for pid := range s.procs {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	found := 0
	for _, pid := range pids {
		p := s.procs[pid]
		base := path.Base(strings.Fields(p.Cmd + " x")[0])
		switch {
		case base == "kubelet":
			s.kubelet = true
		case strings.Contains(p.Cmd, "nginx-ingress-controller"):
			s.ingress = true
		}
		m := masterRE.FindStringSubmatch(p.Cmd)
		if m == nil {
			continue
		}
		if s.masterOf(p.PPID) != 0 {
			continue // a master's child titled master (binary upgrade): same instance
		}
		if p.Foreign = s.foreign(p); p.Foreign {
			s.foreignMasters = append(s.foreignMasters, p)
			continue
		}
		bin, conf, prefix, globals := cmdArgs(m[2])
		exe, err := os.Readlink(filepath.Join(s.env.Proc, strconv.Itoa(pid), "exe"))
		if err != nil {
			s.fail("/proc/" + strconv.Itoa(pid) + "/exe (" + m[1] + " master): " + errText(err))
			exe = bin
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		b := s.buildInfo(ctx, exe)
		conf = confPath(conf, prefix, b)
		unit := s.unitOf(pid)
		in := &Instance{
			Kind:      KindHost,
			State:     StateRunning,
			PID:       pid,
			Exe:       exe,
			Unit:      unit,
			Prefix:    prefix,
			Globals:   globals,
			Conf:      conf,
			Variant:   firstNonEmpty(b.Variant, m[1]),
			Version:   b.Version,
			ManagedBy: ManagedNone,
		}
		in.Owner = s.env.UserName(p.UID)
		if p.UID != 0 {
			in.User = in.Owner
			in.Notes = append(in.Notes, "master runs as "+in.Owner+", not root (DISC-06)")
		}
		in.ID, in.Name = hostID(unit, exe, conf)
		s.add(in, &build{
			drv: driver.Host{Run: s.env.Run, Exe: exe, Conf: conf, Prefix: prefix, Globals: globals, Unit: unit},
			src: nginxconf.FileSource{},
		})
		seenExe[realPath(exe)] = true
		if unit != "" {
			seenUnit[unit] = true
		}
		found++
	}
	found += s.stoppedUnits(ctx, seenUnit, seenExe)
	found += s.stoppedBinaries(ctx, seenExe)
	for _, in := range s.inst {
		if in.Kind == KindHost && in.Exe != "" {
			in.Package = s.packageOf(ctx, in.Exe)
		}
	}
	s.rep.Host = s.hostSummary(found)
	return found
}

func (s *Scanner) hostSummary(found int) string {
	if found > 0 {
		return strconv.Itoa(found) + " host instance" + plural(found)
	}
	why := "no nginx master process, no nginx binary on PATH or in the usual prefixes"
	if s.env.has("systemctl") {
		why += ", no nginx systemd unit"
	}
	return "no host nginx: " + why
}

func hostID(unit, exe, conf string) (id, name string) {
	if unit != "" {
		return "host:" + unit, strings.TrimSuffix(unit, ".service")
	}
	if conf == "" {
		return "host:" + exe, exe
	}
	return "host:" + exe + ":" + conf, exe + " -c " + conf
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

var unitRE = regexp.MustCompile(`^(nginx|openresty|angie|tengine)[\w@.\-]*\.service$`)

// stoppedUnits lists nginx-like systemd units with no running master:
// installed but stopped or disabled (DISC-03). The effective ExecStart,
// drop-ins included, gives -c and -g (DISC-18).
func (s *Scanner) stoppedUnits(ctx context.Context, seenUnit, seenExe map[string]bool) int {
	if !s.env.has("systemctl") {
		return 0
	}
	res := s.env.Run.Run(ctx, "systemctl", []string{"list-unit-files", "--type=service", "--no-legend", "--no-pager", "--plain",
		"nginx*", "openresty*", "angie*", "tengine*"}, execx.Opts{Timeout: 10 * time.Second})
	n := 0
	for _, l := range strings.Split(res.Stdout, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 || !unitRE.MatchString(f[0]) || seenUnit[f[0]] || strings.Contains(f[0], "@.") {
			continue
		}
		unit := f[0]
		show := s.env.Run.Run(ctx, "systemctl", []string{"show", "-p", "ActiveState", "-p", "MainPID", "-p", "ExecStart", "-p", "DropInPaths", "-p", "UnitFileState", unit}, execx.Opts{Timeout: 10 * time.Second})
		props := map[string]string{}
		for _, kv := range strings.Split(show.Stdout, "\n") {
			if i := strings.IndexByte(kv, '='); i > 0 {
				props[kv[:i]] = kv[i+1:]
			}
		}
		if pid, _ := strconv.Atoi(props["MainPID"]); pid > 0 && s.procs[pid] != nil {
			continue // running, but its master was not readable: reported under read failures
		}
		exe, conf, prefix, globals := execStart(props["ExecStart"])
		if exe == "" {
			continue
		}
		b := s.buildInfo(ctx, exe)
		conf = confPath(conf, prefix, b)
		in := &Instance{
			Kind: KindHost, State: StateStopped, Unit: unit, Exe: exe, Conf: conf, Prefix: prefix, Globals: globals,
			Variant: firstNonEmpty(b.Variant, "nginx"), Version: b.Version, Owner: "root", ManagedBy: ManagedNone,
		}
		in.ID, in.Name = hostID(unit, exe, conf)
		in.Notes = append(in.Notes, "systemd unit "+unit+" is "+firstNonEmpty(props["ActiveState"], "inactive")+" ("+firstNonEmpty(props["UnitFileState"], "?")+")")
		if d := strings.TrimSpace(props["DropInPaths"]); d != "" {
			in.Notes = append(in.Notes, "drop-ins: "+d+" (their ExecStart is the one used, DISC-18)")
		}
		s.add(in, &build{
			drv: driver.Host{Run: s.env.Run, Exe: exe, Conf: conf, Prefix: prefix, Globals: globals, Unit: unit},
			src: nginxconf.FileSource{},
		})
		seenExe[realPath(exe)] = true
		n++
	}
	return n
}

// execStart parses systemctl's ExecStart value:
// "{ path=/usr/sbin/nginx ; argv[]=/usr/sbin/nginx -g daemon on; master_process on; ; ignore_errors=no ; … }".
func execStart(v string) (exe, conf, prefix, globals string) {
	i := strings.Index(v, "argv[]=")
	if i < 0 {
		return "", "", "", ""
	}
	argv := v[i+len("argv[]="):]
	if j := strings.Index(argv, " ; ignore_errors="); j >= 0 {
		argv = argv[:j]
	}
	return cmdArgs(argv)
}

// stoppedBinaries are nginx binaries with no running master and no unit.
func (s *Scanner) stoppedBinaries(ctx context.Context, seenExe map[string]bool) int {
	var cands []string
	for _, name := range []string{"nginx", "openresty", "angie", "tengine"} {
		if p, err := s.env.LookPath(name); err == nil {
			cands = append(cands, p)
		}
	}
	cands = append(cands, s.env.Binaries...)
	n := 0
	for _, c := range cands {
		st, err := os.Stat(c)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		r := realPath(c)
		if seenExe[r] {
			continue
		}
		seenExe[r] = true
		b := s.buildInfo(ctx, c)
		if b.Version == "" {
			continue // not an nginx after all
		}
		conf := confPath("", "", b)
		in := &Instance{
			Kind: KindHost, State: StateStopped, Exe: c, Conf: conf, Variant: b.Variant, Version: b.Version,
			Owner: "root", ManagedBy: ManagedNone,
		}
		in.ID, in.Name = hostID("", c, conf)
		in.Notes = append(in.Notes, "installed, no master process and no systemd unit")
		s.add(in, &build{drv: driver.Host{Run: s.env.Run, Exe: c, Conf: conf}, src: nginxconf.FileSource{}})
		n++
	}
	return n
}

// packageOf is the package that installed a binary: dpkg, rpm or apk.
func (s *Scanner) packageOf(ctx context.Context, exe string) string {
	o := execx.Opts{Timeout: 5 * time.Second}
	switch {
	case s.env.has("dpkg"):
		if r := s.env.Run.Run(ctx, "dpkg", []string{"-S", exe}, o); r.Code == 0 {
			if i := strings.Index(r.Stdout, ":"); i > 0 {
				return strings.TrimSpace(r.Stdout[:i]) + " (dpkg)"
			}
		}
	case s.env.has("rpm"):
		if r := s.env.Run.Run(ctx, "rpm", []string{"-qf", exe}, o); r.Code == 0 {
			return strings.TrimSpace(firstLine(r.Stdout)) + " (rpm)"
		}
	case s.env.has("apk"):
		if r := s.env.Run.Run(ctx, "apk", []string{"info", "-W", exe}, o); r.Code == 0 {
			if i := strings.Index(r.Stdout, "owned by "); i >= 0 {
				return strings.TrimSpace(r.Stdout[i+len("owned by "):]) + " (apk)"
			}
		}
	}
	return ""
}

func errText(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	}
	return err.Error()
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
