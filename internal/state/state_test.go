package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
)

func TestWriteFileAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lib")
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte("one"), FileMode); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("two"), FileMode); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "two" {
		t.Fatalf("got %q", b)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != FileMode {
		t.Errorf("file mode %v, want %v", st.Mode().Perm(), FileMode)
	}
	dst, _ := os.Stat(dir)
	if dst.Mode().Perm() != DirMode {
		t.Errorf("dir mode %v, want %v", dst.Mode().Perm(), DirMode)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
}

func TestLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lib", ".lock")
	first, _, err := TryLock(path)
	if err != nil || first == nil {
		t.Fatalf("first lock: %v", err)
	}

	var saw Holder
	start := time.Now()
	_, err = Acquire(context.Background(), path, 300*time.Millisecond, func(h Holder) { saw = h })
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("want LockedError, got %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("gave up before the wait ran out")
	}
	if saw.PID != os.Getpid() || !strings.Contains(locked.Error(), "pid") {
		t.Errorf("holder not reported: %+v / %v", saw, locked)
	}

	// Released while a second run waits: the waiter gets it.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = first.Release()
	}()
	second, err := Acquire(context.Background(), path, 2*time.Second, nil)
	if err != nil || second == nil {
		t.Fatalf("second lock after release: %v", err)
	}
	_ = second.Release()
}

func TestSchemaMigrationHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	// A schema-1 file with a field that schema 2 renamed and schema 3 nested.
	if err := os.WriteFile(path, []byte(`{"schema":1,"hosts":["a.example.com"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ran []int
	s := Store{Path: path, Schema: 3, Migrations: map[int]Migration{
		1: func(d map[string]any) error {
			ran = append(ran, 1)
			d["routes"] = d["hosts"]
			delete(d, "hosts")
			return nil
		},
		2: func(d map[string]any) error {
			ran = append(ran, 2)
			d["routes"] = map[string]any{"list": d["routes"]}
			return nil
		},
	}}
	var doc struct {
		Schema int `json:"schema"`
		Routes struct {
			List []string `json:"list"`
		} `json:"routes"`
	}
	migrated, err := s.Migrate(&doc)
	if err != nil || !migrated {
		t.Fatalf("migrate: %v %v", migrated, err)
	}
	if len(ran) != 2 || doc.Schema != 3 || len(doc.Routes.List) != 1 {
		t.Fatalf("ran %v, doc %+v", ran, doc)
	}
	if _, err := os.Stat(path + ".schema-1.bak"); err != nil {
		t.Error("original not kept:", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"schema": 3`) {
		t.Errorf("not saved at schema 3: %s", b)
	}

	// A file from the future is refused, not rewritten.
	_ = os.WriteFile(path, []byte(`{"schema":9}`), 0o600)
	var newer *NewerSchemaError
	if _, err := s.Load(&doc); !errors.As(err, &newer) {
		t.Fatalf("want NewerSchemaError, got %v", err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv(paths.EnvRoot, t.TempDir())
	p := paths.Get()
	c, err := LoadConfig(p)
	if err != nil || !c.UpdateCheck || c.Channel != "stable" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	if err := os.MkdirAll(p.Etc, 0o700); err != nil {
		t.Fatal(err)
	}
	// Missing keys keep their defaults; a file without "schema" migrates.
	_ = os.WriteFile(p.Config, []byte("\xef\xbb\xbf{\"updateCheck\": false}"), 0o600)
	c, err = LoadConfig(p)
	if err != nil || c.UpdateCheck || c.Channel != "stable" || c.Schema != ConfigSchema {
		t.Fatalf("partial: %+v %v", c, err)
	}
}

func TestInvalidJSONNamesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte(`{"updateCheck": fal`), 0o600)
	s := Store{Path: path, Schema: 1}
	var c Config
	_, err := s.Load(&c)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("want an error naming %s, got %v", path, err)
	}
	if b, _ := os.ReadFile(path); string(b) != `{"updateCheck": fal` {
		t.Fatal("a broken file was rewritten")
	}
}
