package nginxconf

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Source is where config files come from: an `nginx -T` dump, or files on
// this machine (host paths, a container's bind mounts, /proc/<pid>/root).
type Source interface {
	Read(p string) ([]byte, error)
	// Glob returns the matches of an include pattern, sorted as nginx sorts
	// them (byte order, like glob(3) in the C locale).
	Glob(pattern string) ([]string, error)
	Kind() string // "dump" or "files"
}

// SplitDump turns `nginx -T` output into path → content. nginx prints every
// file once, each after a "# configuration file <path>:" line and followed
// by one blank line. The first file is the main one.
func SplitDump(out string) (main string, files map[string]string) {
	files = map[string]string{}
	const head = "# configuration file "
	var cur string
	var body []string
	flush := func() {
		if cur == "" {
			return
		}
		text := strings.Join(body, "\n")
		// nginx adds one blank line after each file.
		text = strings.TrimRight(text, "\n")
		if text != "" {
			text += "\n"
		}
		if _, seen := files[cur]; !seen {
			files[cur] = text
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, head) && strings.HasSuffix(line, ":") {
			flush()
			cur = strings.TrimSuffix(strings.TrimPrefix(line, head), ":")
			body = nil
			if main == "" {
				main = cur
			}
			continue
		}
		if cur != "" {
			body = append(body, line)
		}
	}
	flush()
	return main, files
}

// DumpSource serves files from an `nginx -T` dump.
type DumpSource map[string]string

func (d DumpSource) Kind() string { return "dump" }

func (d DumpSource) Read(p string) ([]byte, error) {
	if s, ok := d[p]; ok {
		return []byte(s), nil
	}
	return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
}

func (d DumpSource) Glob(pattern string) ([]string, error) {
	var out []string
	for p := range d {
		if ok, err := path.Match(pattern, p); err != nil {
			return nil, err
		} else if ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Mount maps a path as nginx sees it (inside a container) to the host.
type Mount struct {
	Dest   string // inside
	Source string // on the host
}

// FileSource reads files from this machine. Paths as nginx sees them are
// mapped through Mounts (longest destination first), else prefixed with
// Root ("" for the host, /proc/<pid>/root for a running container or a
// chrooted master). With Root empty and no mount covering a path, a
// container path cannot be read: the config is inside the image (CONF-06).
type FileSource struct {
	Root      string
	Mounts    []Mount
	Container bool // paths not under a mount are not on the host
}

func (f FileSource) Kind() string { return "files" }

// ErrInImage is a path only the container's image has.
var ErrInImage = errors.New("inside the image, not on a mount")

// HostPath is where p really is on this machine.
func (f FileSource) HostPath(p string) (string, error) {
	best := -1
	for i, m := range f.Mounts {
		if under(p, m.Dest) && (best < 0 || len(m.Dest) > len(f.Mounts[best].Dest)) {
			best = i
		}
	}
	if best >= 0 {
		m := f.Mounts[best]
		return filepath.Join(m.Source, strings.TrimPrefix(p, m.Dest)), nil
	}
	if f.Container && f.Root == "" {
		return "", &fs.PathError{Op: "read", Path: p, Err: ErrInImage}
	}
	return filepath.Join(f.Root, p), nil
}

func (f FileSource) Read(p string) ([]byte, error) {
	_, hp, err := f.Resolve(p)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(hp)
}

// Resolve follows symlinks the way nginx would see them: an absolute link
// target is a path inside the container (or chroot) and goes through the
// mounts again, so a Debian sites-enabled link resolves correctly even
// when read from outside. It returns the final path as nginx sees it and
// where that is on this machine.
func (f FileSource) Resolve(p string) (string, string, error) {
	if !f.Container && f.Root == "" && len(f.Mounts) == 0 {
		return p, p, nil
	}
	cur := "/"
	rest := strings.Split(strings.TrimPrefix(path.Clean(p), "/"), "/")
	for hops := 0; len(rest) > 0; {
		cur = path.Join(cur, rest[0])
		rest = rest[1:]
		hp, err := f.HostPath(cur)
		if err != nil {
			continue // not on this machine (inside the image): checked at the end
		}
		st, err := os.Lstat(hp)
		if err != nil || st.Mode()&fs.ModeSymlink == 0 {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", "", err
			}
			continue
		}
		if hops++; hops > 40 {
			return "", "", &fs.PathError{Op: "read", Path: p, Err: errors.New("too many symlinks")}
		}
		link, err := os.Readlink(hp)
		if err != nil {
			return "", "", err
		}
		if !path.IsAbs(link) {
			link = path.Join(path.Dir(cur), link)
		}
		rest = append(strings.Split(strings.TrimPrefix(path.Clean(link), "/"), "/"), rest...)
		cur = "/"
	}
	hp, err := f.HostPath(cur)
	return cur, hp, err
}

// Link is the target of p when p is a symlink (sites-enabled → sites-available).
func (f FileSource) Link(p string) string {
	if real, _, err := f.Resolve(p); err == nil && real != p {
		return real
	}
	if f.Root == "" && !f.Container && len(f.Mounts) == 0 {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != p {
			return real
		}
	}
	return ""
}

func (f FileSource) Glob(pattern string) ([]string, error) {
	dir, rest := pattern, ""
	for i := 0; i < len(pattern); i++ {
		if strings.ContainsRune("*?[", rune(pattern[i])) {
			j := strings.LastIndexByte(pattern[:i], '/')
			dir, rest = pattern[:j], pattern[j:]
			break
		}
	}
	hdir, err := f.HostPath(dir)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(hdir + rest)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, dir+strings.TrimPrefix(m, hdir))
	}
	sort.Strings(out)
	return out, nil
}

func under(p, dir string) bool {
	dir = strings.TrimSuffix(dir, "/")
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// Config is a whole configuration: the main file with every include
// expanded in place, in the order nginx reads it.
type Config struct {
	Main   string              `json:"main"`
	Source string              `json:"source"` // dump or files
	Tree   []Directive         `json:"-"`
	Files  []FileInfo          `json:"files"` // every file read, in include order
	Errors []*Error            `json:"errors,omitempty"`
	byPath map[string]fileData // parsed once, even when included twice
}

type fileData struct {
	ds   []Directive
	errs []*Error
}

const maxIncludeDepth = 32

// Load parses main and follows its includes through src. Relative include
// paths are relative to the main file's directory, as with `nginx -c`.
// Problems are collected in Errors; Load itself does not fail.
func Load(src Source, main string) *Config {
	c := &Config{Main: main, Source: src.Kind(), byPath: map[string]fileData{}}
	c.Tree = c.file(src, main, path.Dir(main), 0, nil)
	return c
}

func (c *Config) file(src Source, p, base string, depth int, from *Directive) []Directive {
	if fd, ok := c.byPath[p]; ok {
		return c.expand(src, fd.ds, base, depth)
	}
	data, err := src.Read(p)
	if err != nil {
		e := &Error{File: p, Msg: readError(err)}
		if from != nil {
			e = &Error{File: from.File, Line: from.Line, Msg: "include " + p + ": " + readError(err)}
		}
		c.Errors = append(c.Errors, e)
		c.byPath[p] = fileData{}
		return nil
	}
	ds, info, errs := Parse(p, data)
	if l, ok := src.(interface{ Link(string) string }); ok {
		info.Link = l.Link(p)
	}
	c.Files = append(c.Files, info)
	c.Errors = append(c.Errors, errs...)
	c.byPath[p] = fileData{ds: ds, errs: errs}
	return c.expand(src, ds, base, depth)
}

func readError(err error) string {
	switch {
	case errors.Is(err, ErrInImage):
		return "inside the image, not on a mount"
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	}
	return err.Error()
}

// expand copies ds with includes resolved: each include directive stays in
// place (with Included set) and the included directives follow it.
func (c *Config) expand(src Source, ds []Directive, base string, depth int) []Directive {
	out := make([]Directive, 0, len(ds))
	for _, d := range ds {
		if d.Name == "include" && !d.HasBlock && len(d.Args) == 1 {
			if depth >= maxIncludeDepth {
				c.Errors = append(c.Errors, &Error{File: d.File, Line: d.Line, Msg: "includes nested too deep (a loop?)"})
				out = append(out, d)
				continue
			}
			pat := d.Args[0]
			if !path.IsAbs(pat) {
				pat = path.Join(base, pat)
			}
			var files []string
			if strings.ContainsAny(pat, "*?[") {
				m, err := src.Glob(pat)
				if err != nil {
					c.Errors = append(c.Errors, &Error{File: d.File, Line: d.Line, Msg: "include " + pat + ": " + readError(err)})
				}
				files = m
			} else {
				files = []string{pat}
			}
			d.Included = files
			out = append(out, d)
			for _, f := range files {
				dd := d
				out = append(out, c.file(src, f, base, depth+1, &dd)...)
			}
			continue
		}
		if d.HasBlock && len(d.Block) > 0 {
			d.Block = c.expand(src, d.Block, base, depth)
		}
		out = append(out, d)
	}
	return out
}
