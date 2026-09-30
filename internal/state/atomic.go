// Package state stores NgiTool's JSON files: atomic writes, an exclusive
// lock for mutating commands, and a schema number with migrations run on load.
package state

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	FileMode os.FileMode = 0o600
	DirMode  os.FileMode = 0o700
)

// WriteFile replaces path atomically: a temp file in the same directory is
// written, fsynced and renamed over the target, then the directory is
// fsynced. A crash leaves either the old file or the new one, never half.
func WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
