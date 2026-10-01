package apply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
)

// DefaultRetain is how many snapshots each instance keeps (open decision,
// recorded in AGENTS.md; config.json "snapshots" changes it).
const DefaultRetain = 20

// SnapFile is one file in a snapshot: its host path and whether it existed.
type SnapFile struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
	Blob    string `json:"blob,omitempty"` // name inside files/
	Raw     bool   `json:"raw,omitempty"`  // a hand-written file (the adopt include)
}

// Manifest describes a snapshot (APPLY-06).
type Manifest struct {
	Instance string     `json:"instance"`
	At       time.Time  `json:"at"`
	Summary  string     `json:"summary"`
	Changes  string     `json:"changes"` // "2 files changed, 1 added, 0 removed"
	Files    []SnapFile `json:"files"`
	State    bool       `json:"state"` // state.json was copied as state.json
	Dir      string     `json:"-"`
}

// SafeName turns an instance id into a directory name.
func SafeName(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// takeSnapshot copies files (and state.json) into
// backups/<instance>/<timestamp>/ before anything is written.
func takeSnapshot(backups, instance string, now time.Time, summary, changes string, managed []string, raw []string, statePath string) (*Manifest, error) {
	base := filepath.Join(backups, SafeName(instance))
	dir := filepath.Join(base, now.UTC().Format("20060102T150405.000Z"))
	for n := 2; ; n++ {
		if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			break
		}
		dir = filepath.Join(base, now.UTC().Format("20060102T150405.000Z")+"-"+strconv.Itoa(n))
	}
	if err := os.MkdirAll(filepath.Join(dir, "files"), state.DirMode); err != nil {
		return nil, &WriteError{Path: dir, Err: err}
	}
	m := &Manifest{Instance: instance, At: now.UTC(), Summary: summary, Changes: changes, Dir: dir}
	add := func(p string, isRaw bool) error {
		b, err := os.ReadFile(p)
		f := SnapFile{Path: p, Raw: isRaw}
		switch {
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		case err != nil:
			return err
		default:
			f.Existed = true
			f.Blob = strconv.Itoa(len(m.Files))
			if err := state.WriteFile(filepath.Join(dir, "files", f.Blob), b, state.FileMode); err != nil {
				return &WriteError{Path: dir, Err: err}
			}
		}
		m.Files = append(m.Files, f)
		return nil
	}
	seen := map[string]bool{}
	for _, list := range [][]string{managed, raw} {
		for _, p := range list {
			if seen[p] {
				continue
			}
			seen[p] = true
			if err := add(p, contains(raw, p)); err != nil {
				return nil, err
			}
		}
	}
	if b, err := os.ReadFile(statePath); err == nil {
		if err := state.WriteFile(filepath.Join(dir, "state.json"), b, state.FileMode); err != nil {
			return nil, &WriteError{Path: dir, Err: err}
		}
		m.State = true
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := state.WriteFile(filepath.Join(dir, "manifest.json"), b, state.FileMode); err != nil {
		return nil, &WriteError{Path: dir, Err: err}
	}
	return m, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Content is a snapshot file's bytes.
func (m *Manifest) Content(f SnapFile) (string, error) {
	b, err := os.ReadFile(filepath.Join(m.Dir, "files", f.Blob))
	return string(b), err
}

// restore puts every file back as it was: atomic writes for managed files,
// in place for hand-written ones, removal of files that did not exist.
func (m *Manifest) restore(inPlace func(string) bool) error {
	var errs []string
	for _, f := range m.Files {
		if !f.Existed {
			if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err.Error())
			}
			continue
		}
		c, err := m.Content(f)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if inPlace(f.Path) {
			err = writeInPlace(f.Path, c)
		} else {
			err = state.WriteFile(f.Path, []byte(c), confMode)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("restoring the snapshot %s failed: %s", m.Dir, strings.Join(errs, "; "))
	}
	return nil
}

// Snapshots lists an instance's snapshots, newest first.
func Snapshots(backups, instance string) ([]*Manifest, error) {
	base := filepath.Join(backups, SafeName(instance))
	ents, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			continue
		}
		var m Manifest
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		m.Dir = dir
		out = append(out, &m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir > out[j].Dir })
	return out, nil
}

// rotate keeps the newest n snapshots of an instance.
func rotate(backups, instance string, n int) {
	if n <= 0 {
		n = DefaultRetain
	}
	all, err := Snapshots(backups, instance)
	if err != nil {
		return
	}
	for i, m := range all {
		if i >= n {
			_ = os.RemoveAll(m.Dir)
		}
	}
}

// Name is the snapshot's directory name, which `rollback` takes.
func (m *Manifest) Name() string { return filepath.Base(m.Dir) }

// SnapshotState is the state.json saved with a snapshot (empty when the
// snapshot was taken before any state existed).
func SnapshotState(m *Manifest) (*model.State, error) {
	st := &model.State{Schema: state.StateSchema}
	if !m.State {
		return st, nil
	}
	s := state.Store{Path: filepath.Join(m.Dir, "state.json"), Schema: state.StateSchema, Migrations: state.StateMigrations}
	if _, err := s.Load(st); err != nil {
		return nil, err
	}
	return st, nil
}
