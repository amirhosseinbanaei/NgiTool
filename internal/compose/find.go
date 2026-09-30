// Package compose finds and reads Docker Compose projects. This file only
// finds compose files; prompt 4 builds the full finder on top of it.
package compose

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileNames are the names `docker compose` looks for, in its order of
// preference: one directory is one project, and the first name wins.
var FileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// SkipDirs are never descended into. Hidden directories are skipped too
// (caches, .git, editor state); a project root is never hidden.
var SkipDirs = map[string]bool{"node_modules": true, "vendor": true, "dist": true, "build": true, ".git": true}

// DefaultDepth is how many directory levels below a scan root are searched.
const DefaultDepth = 4

// Find returns one compose file per project directory under roots, sorted.
// Roots may be globs ("/home/*"). Symlinks are not followed, so a project
// linked from somewhere else is found once, where it really lives. Paths
// that could not be read are returned as errors next to the results.
func Find(roots []string, depth int) ([]string, []error) {
	var dirs []string
	for _, r := range roots {
		if strings.ContainsAny(r, "*?[") {
			m, _ := filepath.Glob(r)
			dirs = append(dirs, m...)
		} else {
			dirs = append(dirs, r)
		}
	}
	seen := map[string]bool{}
	var out []string
	var errs []error
	for _, root := range dirs {
		root = filepath.Clean(root)
		st, err := os.Stat(root)
		if err != nil || !st.IsDir() {
			continue
		}
		base := strings.Count(root, string(filepath.Separator))
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, err)
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if p != root && (SkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if strings.Count(p, string(filepath.Separator))-base > depth {
				return filepath.SkipDir
			}
			for _, n := range FileNames {
				f := filepath.Join(p, n)
				if st, err := os.Lstat(f); err == nil && st.Mode().IsRegular() {
					if !seen[f] {
						seen[f] = true
						out = append(out, f)
					}
					break
				}
			}
			return nil
		})
	}
	sort.Strings(out)
	return out, errs
}
