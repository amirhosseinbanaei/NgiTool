// Package paths is the one place that knows where NgiTool keeps things.
//
//	/etc/ngitool/config.json        settings
//	/var/lib/ngitool/state.json     what NgiTool manages
//	/var/lib/ngitool/.lock          held by every mutating command
//	/var/lib/ngitool/backups/       snapshots taken before every apply
//	/var/lib/ngitool/overrides/     compose overrides
//	/var/cache/ngitool/             update-check and scan caches
//
// NGITOOL_ROOT=/some/dir moves all of it under that dir (etc/, lib/, cache/)
// so tests never touch the real machine.
package paths

import (
	"os"
	"path/filepath"
)

const (
	EnvRoot   = "NGITOOL_ROOT"
	EnvPrefix = "NGITOOL_PREFIX"
)

// Paths are absolute.
type Paths struct {
	Root        string // "" on a real machine, NGITOOL_ROOT otherwise
	Etc         string
	Config      string
	Lib         string
	State       string
	Lock        string
	Backups     string
	Overrides   string
	Cache       string
	UpdateCheck string
}

// Get resolves every path, honouring NGITOOL_ROOT.
func Get() Paths {
	root := os.Getenv(EnvRoot)
	etc, lib, cache := "/etc/ngitool", "/var/lib/ngitool", "/var/cache/ngitool"
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		etc, lib, cache = filepath.Join(root, "etc"), filepath.Join(root, "lib"), filepath.Join(root, "cache")
	}
	return Paths{
		Root:        root,
		Etc:         etc,
		Config:      filepath.Join(etc, "config.json"),
		Lib:         lib,
		State:       filepath.Join(lib, "state.json"),
		Lock:        filepath.Join(lib, ".lock"),
		Backups:     filepath.Join(lib, "backups"),
		Overrides:   filepath.Join(lib, "overrides"),
		Cache:       cache,
		UpdateCheck: filepath.Join(cache, "update-check.json"),
	}
}

// Dirs are the directories `uninstall --purge` removes.
func (p Paths) Dirs() []string { return []string{p.Etc, p.Lib, p.Cache} }

// BinDir is where the installer puts the binary: $NGITOOL_PREFIX/bin or
// /usr/local/bin.
func BinDir() string {
	if prefix := os.Getenv(EnvPrefix); prefix != "" {
		return filepath.Join(prefix, "bin")
	}
	return "/usr/local/bin"
}

// Executable is the running binary with symlinks resolved, so updating
// through the `ngt` alias replaces the real file.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
