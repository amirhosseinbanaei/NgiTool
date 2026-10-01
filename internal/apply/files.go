package apply

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/amirhosseinbanaei/NgiTool/internal/render"
)

// File modes of what NgiTool writes into nginx's config: readable by the
// nginx master of any image, never secret.
const (
	confMode os.FileMode = 0o644
	dirMode  os.FileMode = 0o755
)

// Disk is the managed files found in the owned dirs: host path → content.
type Disk map[string]string

// readManaged lists every file carrying the NgiTool marker in dirs (one
// level, plus one level of subdirs for locations/<host>/). Hand-written
// files are never read into it, so they can never be rewritten or deleted.
func readManaged(dirs []string) (Disk, error) {
	out := Disk{}
	seen := map[string]bool{}
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if seen[dir] {
			return nil
		}
		seen[dir] = true
		ents, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil // nothing of ours there; writing reports the real problem
		}
		if err != nil {
			return err
		}
		for _, e := range ents {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if depth > 0 {
					if err := walk(p, depth-1); err != nil {
						return err
					}
				}
				continue
			}
			if !strings.HasSuffix(e.Name(), ".conf") || !e.Type().IsRegular() {
				continue
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if render.Parse(string(b)).Managed {
				out[p] = string(b)
			}
		}
		return nil
	}
	for _, d := range dirs {
		depth := 0
		if strings.HasSuffix(d, "/locations") {
			depth = 1
		}
		if err := walk(d, depth); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// WriteError is a write that failed before nginx saw anything new: disk
// full or a read-only filesystem (APPLY-07).
type WriteError struct {
	Path string
	Err  error
}

func (e *WriteError) Error() string {
	why := e.Err.Error()
	switch {
	case errors.Is(e.Err, syscall.ENOSPC):
		why = "the disk is full"
	case errors.Is(e.Err, syscall.EROFS):
		why = "the filesystem is read-only"
	case errors.Is(e.Err, syscall.EACCES), errors.Is(e.Err, syscall.EPERM):
		why = "permission denied"
	}
	return fmt.Sprintf("cannot write %s: %s (APPLY-07)", e.Path, why)
}

func (e *WriteError) Unwrap() error { return e.Err }

// staged is a temp file waiting to be renamed over its target.
type staged struct{ tmp, dst string }

// stageAll writes every file to a temp next to its target, fsynced, so a
// full disk fails here — before any live file changes (APPLY-07, APPLY-09).
func stageAll(files map[string]string) ([]staged, error) {
	var out []staged
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), dirMode); err != nil {
			cleanup(out)
			return nil, &WriteError{Path: filepath.Dir(p), Err: err}
		}
		f, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".ngt-*")
		if err != nil {
			cleanup(out)
			return nil, &WriteError{Path: p, Err: err}
		}
		out = append(out, staged{tmp: f.Name(), dst: p})
		_, err = f.WriteString(files[p])
		if err == nil {
			err = f.Chmod(confMode)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			cleanup(out)
			return nil, &WriteError{Path: p, Err: err}
		}
	}
	return out, nil
}

func cleanup(st []staged) {
	for _, s := range st {
		_ = os.Remove(s.tmp)
	}
}

// commit renames every staged file into place.
func commit(st []staged) error {
	for i, s := range st {
		if err := os.Rename(s.tmp, s.dst); err != nil {
			cleanup(st[i:])
			return &WriteError{Path: s.dst, Err: err}
		}
		syncDir(filepath.Dir(s.dst))
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// writeInPlace rewrites a file without replacing its inode: a single-file
// bind mount keeps showing the container the old inode after a rename
// (CONF-07).
func writeInPlace(p, content string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return &WriteError{Path: p, Err: err}
	}
	_, err = f.WriteString(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return &WriteError{Path: p, Err: err}
	}
	return nil
}

// removeEmptyDirs drops location dirs of hosts that are gone, only when
// nothing (hand-written or not) is left in them.
func removeEmptyDirs(locations string, keep map[string]bool) {
	ents, err := os.ReadDir(locations)
	if err != nil {
		return
	}
	for _, e := range ents {
		p := filepath.Join(locations, e.Name())
		if !e.IsDir() || keep[p] {
			continue
		}
		if sub, err := os.ReadDir(p); err == nil && len(sub) == 0 {
			_ = os.Remove(p)
		}
	}
}
