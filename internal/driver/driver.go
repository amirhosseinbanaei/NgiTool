// Package driver talks to one nginx instance: dump its config (`nginx -T`),
// test it (`nginx -t`) and reload it. Each kind of instance has its own way
// of doing that; callers only see the Driver interface. Everything runs
// through an execx.Runner, so tests use recorded output.
package driver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// Timeout bounds every nginx call; a stuck docker daemon must not hang a scan.
const Timeout = 20 * time.Second

// Driver is how NgiTool reaches one instance.
type Driver interface {
	// Dump returns `nginx -T` output: every file of the config, each after
	// a "# configuration file <path>:" line. It is parsed in memory and
	// never logged or cached whole (CONF-11).
	Dump(ctx context.Context) (string, error)
	// Test runs `nginx -t`. The error is for "could not run the test at
	// all"; a config that fails is a Result with OK false.
	Test(ctx context.Context) (Result, error)
	// Reload makes nginx re-read its config. Its error says which method
	// was used and what it printed.
	Reload(ctx context.Context) error
	// Describe says how this driver dumps, tests and reloads, for inspect.
	Describe() Methods
}

// Methods are the commands a driver uses, as they would be typed.
type Methods struct {
	Dump   string `json:"dump"`
	Test   string `json:"test"`
	Reload string `json:"reload"`
}

// Result is a finished `nginx -t`.
type Result struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"` // noise removed
	// Where is the file:line nginx blamed, when it named one.
	File string `json:"file,omitempty"`
	Line int    `json:"line,omitempty"`
}

// ErrStopped is Dump or Reload on an instance that is not running.
var ErrStopped = errors.New("not running")

var noiseRE = regexp.MustCompile(`signal process started|worker_connections exceed`)

// Clean drops blank lines and the harmless noise nginx prints on every run
// ("signal process started", the worker_connections/ulimit warning).
func Clean(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" && !noiseRE.MatchString(l) {
			keep = append(keep, strings.TrimRight(l, "\r"))
		}
	}
	return strings.Join(keep, "\n")
}

var blameRE = regexp.MustCompile(`\[(?:emerg|alert|crit)\] .* in (\S+):(\d+)`)

func result(res execx.Result) Result {
	r := Result{OK: res.Code == 0, Output: Clean(res.Stderr + "\n" + res.Stdout)}
	if m := blameRE.FindStringSubmatch(r.Output); m != nil {
		r.File = m[1]
		r.Line, _ = strconv.Atoi(m[2])
	}
	return r
}

// Host is a host-installed nginx (or openresty, angie, tengine) master.
type Host struct {
	Run     execx.Runner
	Exe     string // /proc/<pid>/exe, or the binary found for a stopped one
	Conf    string // -c, "" for the compiled-in default
	Prefix  string // -p, "" for the default
	Globals string // -g from the command line or the systemd unit (DISC-18)
	Unit    string // systemd unit the master belongs to, "" if none
}

func (h Host) args(flag ...string) []string {
	var a []string
	if h.Prefix != "" {
		a = append(a, "-p", h.Prefix)
	}
	if h.Conf != "" {
		a = append(a, "-c", h.Conf)
	}
	if h.Globals != "" {
		a = append(a, "-g", h.Globals)
	}
	return append(a, flag...)
}

func (h Host) Dump(ctx context.Context) (string, error) {
	res := h.Run.Run(ctx, h.Exe, h.args("-T"), execx.Opts{Timeout: Timeout})
	if res.Code != 0 {
		return "", fmt.Errorf("%s -T failed: %s", h.Exe, firstLines(Clean(res.Stderr), 3))
	}
	return res.Stdout, nil
}

func (h Host) Test(ctx context.Context) (Result, error) {
	res := h.Run.Run(ctx, h.Exe, h.args("-t"), execx.Opts{Timeout: Timeout})
	if res.Code == 127 || res.Code == 124 {
		return Result{}, fmt.Errorf("%s -t could not run: %s", h.Exe, firstLines(res.Stderr, 2))
	}
	return result(res), nil
}

func (h Host) Reload(ctx context.Context) error {
	name, args := h.reloadCmd()
	res := h.Run.Run(ctx, name, args, execx.Opts{Timeout: Timeout})
	if res.Code != 0 {
		return fmt.Errorf("reload via %s failed (exit %d): %s", execx.Key(name, args...), res.Code, firstLines(Clean(res.Stderr+"\n"+res.Stdout), 5))
	}
	return nil
}

func (h Host) reloadCmd() (string, []string) {
	if h.Unit != "" {
		return "systemctl", []string{"reload", h.Unit}
	}
	return h.Exe, h.args("-s", "reload")
}

func (h Host) Describe() Methods {
	name, args := h.reloadCmd()
	return Methods{
		Dump:   execx.Key(h.Exe, h.args("-T")...),
		Test:   execx.Key(h.Exe, h.args("-t")...),
		Reload: execx.Key(name, args...),
	}
}

// Mount is a container mount as `docker run -v` needs it.
type Mount struct {
	Type   string // bind, volume
	Source string // host path (bind)
	Name   string // volume name (volume)
	Dest   string
}

// Container is nginx in a Docker container, compose-managed or not.
type Container struct {
	Run     execx.Runner
	Name    string
	Image   string
	Bin     string // nginx binary inside the container; "" = nginx
	Conf    string // -c from the master's command line, "" for the default
	Running bool
	Mounts  []Mount
}

func (c Container) bin() string {
	if c.Bin == "" {
		return "nginx"
	}
	return c.Bin
}

func (c Container) flags(flag ...string) []string {
	var a []string
	if c.Conf != "" {
		a = append(a, "-c", c.Conf)
	}
	return append(a, flag...)
}

func (c Container) exec(flag ...string) []string {
	return append([]string{"exec", c.Name, c.bin()}, c.flags(flag...)...)
}

// runArgs tests a stopped container: a throwaway container from the same
// image with the same mounts, read-only, no network, never pulling.
func (c Container) runArgs(flag ...string) []string {
	a := []string{"run", "--rm", "--network", "none", "--pull", "never"}
	for _, m := range c.Mounts {
		src := m.Source
		if m.Type == "volume" {
			src = m.Name
		}
		if src == "" || m.Dest == "" {
			continue
		}
		a = append(a, "-v", src+":"+m.Dest+":ro")
	}
	a = append(a, "--entrypoint", c.bin(), c.Image)
	return append(a, c.flags(flag...)...)
}

func (c Container) Dump(ctx context.Context) (string, error) {
	if !c.Running {
		return "", ErrStopped
	}
	res := c.Run.Run(ctx, "docker", c.exec("-T"), execx.Opts{Timeout: Timeout})
	if res.Code != 0 {
		return "", fmt.Errorf("docker exec %s %s -T failed: %s", c.Name, c.bin(), firstLines(Clean(res.Stderr), 3))
	}
	return res.Stdout, nil
}

func (c Container) Test(ctx context.Context) (Result, error) {
	args := c.exec("-t")
	if !c.Running {
		args = c.runArgs("-t")
	}
	res := c.Run.Run(ctx, "docker", args, execx.Opts{Timeout: Timeout})
	if res.Code == 127 || res.Code == 124 || (res.Code == 125 && !c.Running) {
		// 125: docker itself failed (image not present and --pull never).
		return Result{}, fmt.Errorf("docker %s could not run: %s", args[0], firstLines(Clean(res.Stderr), 2))
	}
	return result(res), nil
}

func (c Container) Reload(ctx context.Context) error {
	if !c.Running {
		return fmt.Errorf("%s is %w: nginx reads its config when the container starts", c.Name, ErrStopped)
	}
	args := c.exec("-s", "reload")
	res := c.Run.Run(ctx, "docker", args, execx.Opts{Timeout: Timeout})
	if res.Code != 0 {
		return fmt.Errorf("reload via docker %s failed (exit %d): %s", strings.Join(args, " "), res.Code, firstLines(Clean(res.Stderr+"\n"+res.Stdout), 5))
	}
	return nil
}

func (c Container) Describe() Methods {
	m := Methods{
		Dump:   "docker " + strings.Join(c.exec("-T"), " "),
		Test:   "docker " + strings.Join(c.exec("-t"), " "),
		Reload: "docker " + strings.Join(c.exec("-s", "reload"), " "),
	}
	if !c.Running {
		m.Dump = "read the mounted files (the container is stopped)"
		m.Test = "docker " + strings.Join(c.runArgs("-t"), " ")
		m.Reload = "none while stopped: nginx reads its config when the container starts"
	}
	return m
}

func firstLines(s string, n int) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " · ")
}
