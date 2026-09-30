package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvRoot, dir)
	p := Get()
	for _, got := range []string{p.Config, p.State, p.Lock, p.Backups, p.Overrides, p.UpdateCheck} {
		if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
			t.Errorf("%s escapes NGITOOL_ROOT %s", got, dir)
		}
	}
	if p.Config != filepath.Join(dir, "etc", "config.json") || p.State != filepath.Join(dir, "lib", "state.json") {
		t.Errorf("unexpected layout: %+v", p)
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv(EnvRoot, "")
	p := Get()
	if p.Config != "/etc/ngitool/config.json" || p.State != "/var/lib/ngitool/state.json" || p.Cache != "/var/cache/ngitool" {
		t.Errorf("unexpected defaults: %+v", p)
	}
}

func TestBinDir(t *testing.T) {
	t.Setenv(EnvPrefix, "/opt/x")
	if BinDir() != "/opt/x/bin" {
		t.Fatal(BinDir())
	}
	t.Setenv(EnvPrefix, "")
	if BinDir() != "/usr/local/bin" {
		t.Fatal(BinDir())
	}
}
