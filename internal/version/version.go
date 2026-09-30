// Package version holds the build information injected with -ldflags and a
// small semantic-version comparison used by the updater.
package version

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

// Set at build time:
//
//	-X github.com/amirhosseinbanaei/NgiTool/internal/version.Version=v0.1.0
//	-X github.com/amirhosseinbanaei/NgiTool/internal/version.Commit=abc1234
//	-X github.com/amirhosseinbanaei/NgiTool/internal/version.Date=2026-09-30T12:00:00Z
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info is what `ngitool version --json` prints.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func Get() Info {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return Info{
		Version: Version,
		Commit:  orUnknown(Commit),
		Date:    orUnknown(Date),
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    arch,
	}
}

// IsDev reports a build without an injected release version.
func IsDev() bool { return Version == "" || Version == "dev" }

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Semver is a parsed vMAJOR.MINOR.PATCH[-PRERELEASE][+BUILD].
type Semver struct {
	Major, Minor, Patch int
	Pre                 []string
}

// Parse accepts "v1.2.3", "1.2.3", "v1.2.3-rc.1" and ignores +build metadata.
func Parse(s string) (Semver, error) {
	raw := s
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Semver
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		core = s[:i]
		if s[i+1:] == "" {
			return v, fmt.Errorf("invalid version %q", raw)
		}
		v.Pre = strings.Split(s[i+1:], ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("invalid version %q (want vMAJOR.MINOR.PATCH)", raw)
	}
	nums := [3]*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("invalid version %q", raw)
		}
		*nums[i] = n
	}
	return v, nil
}

// Compare returns -1, 0 or 1. A dev build or an unparsable version sorts
// below every release, so a dev build always sees updates.
func Compare(a, b string) int {
	va, ea := Parse(a)
	vb, eb := Parse(b)
	switch {
	case ea != nil && eb != nil:
		return 0
	case ea != nil:
		return -1
	case eb != nil:
		return 1
	}
	for _, d := range [][2]int{{va.Major, vb.Major}, {va.Minor, vb.Minor}, {va.Patch, vb.Patch}} {
		if d[0] != d[1] {
			return sign(d[0] - d[1])
		}
	}
	return comparePre(va.Pre, vb.Pre)
}

// comparePre follows semver §11: a release outranks its prereleases; numeric
// identifiers compare numerically and rank below alphanumeric ones.
func comparePre(a, b []string) int {
	if len(a) == 0 || len(b) == 0 {
		return sign(len(b) - len(a))
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, errA := strconv.Atoi(a[i])
		nb, errB := strconv.Atoi(b[i])
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				return sign(na - nb)
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return sign(len(a) - len(b))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// IsPrerelease reports whether s carries a -prerelease part.
func IsPrerelease(s string) bool {
	v, err := Parse(s)
	return err == nil && len(v.Pre) > 0
}
