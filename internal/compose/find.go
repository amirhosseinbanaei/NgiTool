// Package compose finds, reads and runs Docker Compose projects for
// NgiTool: the finder (every file-name pattern, grouped by directory), the
// project model merged with what running containers' labels say, a service
// summary that never holds the resolved environment, NgiTool's network
// override, the exact argv of every lifecycle action, and the drift check
// for a project started without that override.
package compose

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Roles of a compose file inside a project directory.
const (
	RoleBase     = "base"     // compose.yaml, docker-compose.yml …
	RoleOverride = "override" // compose.override.yaml, *.override.* …
	RoleEnv      = "env"      // a middle word: compose.dev.yml, prod.compose.yaml …
	RoleExtra    = "extra"    // named only by a container label (edge.override.yaml)
)

// FileNames are the base names `docker compose` looks for, in its order of
// preference.
var FileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// SkipDirs are never descended into. Hidden directories are skipped too,
// except the ones in Special.
var SkipDirs = map[string]bool{"node_modules": true, "vendor": true, "dist": true, "build": true, ".git": true, ".next": true, ".cache": true}

// Special subfolders hold a project's compose files often enough that they
// are looked into even one level past the depth limit (DOCK-01).
var Special = map[string]bool{"docker": true, "deploy": true, ".docker": true, "infra": true}

// DefaultDepth is how many directory levels below a scan root are searched.
const DefaultDepth = 4

// File is one compose file.
type File struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Word   string `json:"word,omitempty"`   // dev, prod … for RoleEnv
	Owner  string `json:"owner,omitempty"`  // user owning the file
	Broken string `json:"broken,omitempty"` // why it cannot be used (DOCK-10)
}

// Dir is a directory holding compose files: one project candidate before
// labels are merged in. Broken is set for a directory that could not be read.
type Dir struct {
	Path   string `json:"path"`
	Files  []File `json:"files,omitempty"`
	Broken string `json:"broken,omitempty"`
}

// Classify tells whether name is a compose file and its role. Matching is
// case-sensitive, as Docker's is.
func Classify(name string) (role, word string, ok bool) {
	var stem string
	switch {
	case strings.HasSuffix(name, ".yaml"):
		stem = strings.TrimSuffix(name, ".yaml")
	case strings.HasSuffix(name, ".yml"):
		stem = strings.TrimSuffix(name, ".yml")
	default:
		return "", "", false
	}
	if stem == "compose" || stem == "docker-compose" {
		return RoleBase, "", true
	}
	for _, pre := range []string{"docker-compose.", "compose."} {
		if w, ok := strings.CutPrefix(stem, pre); ok && w != "" {
			return middle(w)
		}
	}
	if w, ok := strings.CutSuffix(stem, ".compose"); ok && w != "" && !strings.HasPrefix(w, ".") {
		return middle(w)
	}
	return "", "", false
}

func middle(w string) (string, string, bool) {
	for _, part := range strings.Split(w, ".") {
		if part == "override" {
			return RoleOverride, w, true
		}
	}
	return RoleEnv, w, true
}

// roleRank orders files the way they merge: base, override, variants.
var roleRank = map[string]int{RoleBase: 0, RoleOverride: 1, RoleEnv: 2, RoleExtra: 3}

// SortFiles puts files in merge order: base (compose.* before
// docker-compose.*), overrides, env variants by word, label-only files.
func SortFiles(fs []File) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if roleRank[a.Role] != roleRank[b.Role] {
			return roleRank[a.Role] < roleRank[b.Role]
		}
		if a.Word != b.Word {
			return a.Word < b.Word
		}
		return baseRank(a.Path) < baseRank(b.Path)
	})
}

func baseRank(p string) string {
	n := filepath.Base(p)
	for i, f := range FileNames {
		if n == f {
			return strconv.Itoa(i)
		}
	}
	return n
}

// readDir is os.ReadDir; tests make a directory unreadable with it (root
// reads through any permission bits).
var readDir = os.ReadDir

// Result is a finished scan.
type Result struct {
	Dirs   []Dir
	Errors []error
}

// Scan walks every root (globs allowed, "/home/*") down to depth levels and
// returns each directory holding compose files. Symlinked directories are
// not followed, so a symlink loop cannot trap the walk and a project linked
// from elsewhere is found once, where it lives; symlinked files are
// followed and reported broken when their target is gone (DOCK-10).
func Scan(roots []string, depth int) Result {
	var res Result
	seen := map[string]bool{}
	// pointers are directories a symlinked compose file points into: the
	// project lives there, not next to the link (the legacy apps/ folder).
	var pointers []string
	var walk func(dir string, level int, last bool)
	walk = func(dir string, level int, last bool) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			real = dir
		}
		if seen[real] {
			return
		}
		seen[real] = true
		entries, err := readDir(dir)
		if err != nil {
			res.Errors = append(res.Errors, err)
			if level > 0 {
				res.Dirs = append(res.Dirs, Dir{Path: dir, Broken: "cannot read the directory: " + errText(err)})
			}
			return
		}
		var files []File
		for _, en := range entries {
			role, word, ok := Classify(en.Name())
			if !ok || en.IsDir() {
				continue
			}
			p := filepath.Join(dir, en.Name())
			f := File{Path: p, Role: role, Word: word}
			st, err := os.Stat(p)
			switch {
			case err != nil && en.Type()&os.ModeSymlink != 0:
				to, _ := os.Readlink(p)
				f.Broken = "symlink to a missing file (" + to + ")"
			case err != nil:
				f.Broken = errText(err)
			case !st.Mode().IsRegular():
				continue
			case en.Type()&os.ModeSymlink != 0 && targetDir(p) != real:
				pointers = append(pointers, targetDir(p))
				continue
			default:
				f.Owner = Owner(st)
				if fh, err := os.Open(p); err != nil {
					f.Broken = "cannot read: " + errText(err)
				} else {
					fh.Close()
				}
			}
			files = append(files, f)
		}
		if len(files) > 0 {
			SortFiles(files)
			res.Dirs = append(res.Dirs, Dir{Path: dir, Files: files})
		}
		if last {
			return
		}
		for _, en := range entries {
			name := en.Name()
			if !en.IsDir() || SkipDirs[name] || (strings.HasPrefix(name, ".") && !Special[name]) {
				continue
			}
			switch {
			case level < depth:
				walk(filepath.Join(dir, name), level+1, false)
			case Special[name]:
				walk(filepath.Join(dir, name), level+1, true)
			}
		}
	}
	for _, root := range expand(roots) {
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		walk(root, 0, false)
	}
	for _, d := range pointers {
		walk(d, 0, true)
	}
	sort.Slice(res.Dirs, func(i, j int) bool { return res.Dirs[i].Path < res.Dirs[j].Path })
	return res
}

func targetDir(p string) string {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	return filepath.Dir(real)
}

func expand(roots []string) []string {
	var out []string
	for _, r := range roots {
		if strings.ContainsAny(r, "*?[") {
			m, _ := filepath.Glob(r)
			out = append(out, m...)
		} else {
			out = append(out, filepath.Clean(r))
		}
	}
	return out
}

// Primary is the file a directory's project is read through when only one
// is needed: the first base file, else the first usable file.
func (d Dir) Primary() string {
	for _, f := range d.Files {
		if f.Broken == "" && f.Role == RoleBase {
			return f.Path
		}
	}
	for _, f := range d.Files {
		if f.Broken == "" {
			return f.Path
		}
	}
	return ""
}

// Find returns one compose file per project directory under roots, sorted:
// discovery reads each project through it.
func Find(roots []string, depth int) ([]string, []error) {
	res := Scan(roots, depth)
	var out []string
	for _, d := range res.Dirs {
		if p := d.Primary(); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, res.Errors
}

// UserName turns a uid into a name; tests replace it.
var UserName = func(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		return u.Username
	}
	return strconv.Itoa(uid)
}

// Owner is the user name owning a file ("" when unknown).
func Owner(st os.FileInfo) string {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return UserName(int(s.Uid))
	}
	return ""
}

// OwnerOf is Owner for a path.
func OwnerOf(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return Owner(st)
}

func errText(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "does not exist"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
