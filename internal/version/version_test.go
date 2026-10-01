package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},
		{"v0.1.0", "v0.2.0", -1},
		{"v1.0.0", "v0.9.9", 1},
		{"v0.10.0", "v0.9.0", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0-rc.1", 1},
		{"v1.0.0-alpha", "v1.0.0-alpha.1", -1},
		{"v1.0.0-alpha.1", "v1.0.0-alpha.beta", -1},
		{"v1.0.0-beta.2", "v1.0.0-beta.11", -1},
		{"v1.0.0-rc.1", "v1.0.0-beta.11", 1},
		{"v1.0.0+build.5", "v1.0.0", 0},
		{"dev", "v0.1.0", -1},
		{"v0.1.0", "dev", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "v1", "v1.2", "v1.2.x", "v1.2.3-", "latest"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted", s)
		}
	}
}

func TestIsPrerelease(t *testing.T) {
	if !IsPrerelease("v0.2.0-rc.1") || IsPrerelease("v0.2.0") || IsPrerelease("dev") {
		t.Fatal("IsPrerelease wrong")
	}
}
