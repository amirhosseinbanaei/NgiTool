package discover

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
)

// Env is everything discovery touches, so tests can use a fake /proc tree
// and recorded command output.
type Env struct {
	Run      execx.Runner
	Proc     string   // "/proc"
	Roots    []string // compose scan roots; nil skips the compose scan
	Depth    int
	Euid     int
	LookPath func(string) (string, error)
	Getenv   func(string) string
	Now      func() time.Time
	// Binaries are where a stopped host nginx is looked for besides PATH.
	Binaries []string
	// UserName turns a uid into a name.
	UserName func(uid int) string
	// Access is access(2) on a host path; nil means syscall.Access.
	Access func(path string, mode uint32) error
}

// KnownBinaries are the usual places of nginx and its variants (DISC-02..06).
var KnownBinaries = []string{
	"/usr/sbin/nginx",
	"/usr/local/sbin/nginx",
	"/usr/local/nginx/sbin/nginx",
	"/opt/nginx/sbin/nginx",
	"/usr/local/openresty/nginx/sbin/nginx",
	"/usr/local/openresty/bin/openresty",
	"/usr/sbin/angie",
	"/usr/local/angie/sbin/angie",
	"/opt/tengine/sbin/nginx",
}

// DefaultRoots are the compose scan roots when config.json has none.
var DefaultRoots = []string{"/home/*", "/root", "/opt", "/srv"}

// System is the real machine.
func System(roots []string) Env {
	return Env{
		Run:      execx.System,
		Proc:     "/proc",
		Roots:    roots,
		Depth:    compose.DefaultDepth,
		Euid:     os.Geteuid(),
		LookPath: exec.LookPath,
		Getenv:   os.Getenv,
		Now:      time.Now,
		Binaries: KnownBinaries,
		UserName: lookupUser,
		Access:   syscall.Access,
	}
}

func lookupUser(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// ownerOf is the user owning path, "" when it cannot be read.
func (e Env) ownerOf(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return e.UserName(int(s.Uid))
	}
	return ""
}

// access is Access, or the real access(2) when none is set.
func (e Env) access(path string, mode uint32) error {
	if e.Access != nil {
		return e.Access(path, mode)
	}
	return syscall.Access(path, mode)
}

func (e Env) has(name string) bool {
	_, err := e.LookPath(name)
	return err == nil
}
