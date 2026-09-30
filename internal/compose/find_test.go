package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFind(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "alice/shop/compose.yaml"))
	touch(t, filepath.Join(root, "alice/shop/docker-compose.yml")) // same project: compose.yaml wins
	touch(t, filepath.Join(root, "alice/a/b/c/docker-compose.yml"))
	touch(t, filepath.Join(root, "alice/a/b/c/d/e/compose.yaml")) // too deep
	touch(t, filepath.Join(root, "alice/web/node_modules/x/compose.yaml"))
	touch(t, filepath.Join(root, "alice/.cache/compose.yaml"))
	touch(t, filepath.Join(root, "bob/api/compose.yml"))
	if err := os.Symlink(filepath.Join(root, "bob/api"), filepath.Join(root, "alice/link")); err != nil {
		t.Fatal(err)
	}
	got, errs := Find([]string{filepath.Join(root, "*"), filepath.Join(root, "missing")}, DefaultDepth)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	want := []string{
		filepath.Join(root, "alice/a/b/c/docker-compose.yml"),
		filepath.Join(root, "alice/shop/compose.yaml"),
		filepath.Join(root, "bob/api/compose.yml"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
}
