package compose

import (
	"regexp"
	"strings"
)

// Line is one line of compose output, attributed to a service when it can be.
type Line struct {
	Service string
	Text    string
	Keep    bool // shown in the condensed view
}

var (
	logLineRE   = regexp.MustCompile(`^(\S+?)(?:-\d+)?\s+\|\s?(.*)$`)
	buildStepRE = regexp.MustCompile(`^#\d+ \[([^\] ]+)(?: [^\]]*)?\] (.*)$`)
	stepRE      = regexp.MustCompile(`^\d+/\d+$`)
	buildNoise  = regexp.MustCompile(`^#\d+ (DONE|CACHED|sha256:|transferring|resolve|extracting|naming|exporting|writing|unpacking|\d+\.\d+ )`)
	resourceRE  = regexp.MustCompile(`^\s*(?:[✔✘⠿⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]\s+)?(Container|Network|Volume|Image|Service)\s+(\S+)\s+(.*)$`)
)

// ParseLine attributes a line of `docker compose` output to a service:
// `web-1  | …` (logs), `#7 [web 2/4] RUN …` (build), ` Container
// <project>-web-1  Started` (up, stop, down). Condensed view keeps
// service events, build steps and errors, and drops BuildKit's
// bookkeeping (DONE, sha256, transferring).
func ParseLine(project, line string) Line {
	l := strings.TrimRight(line, "\r")
	if m := buildStepRE.FindStringSubmatch(l); m != nil {
		switch {
		case m[1] == "internal" || m[1] == "auth":
			return Line{Text: m[2]} // BuildKit's own loading steps
		case stepRE.MatchString(m[1]):
			return Line{Text: m[1] + " " + m[2], Keep: true} // a one-target build: "[1/2] RUN …"
		}
		return Line{Service: m[1], Text: m[2], Keep: true}
	}
	if buildNoise.MatchString(l) {
		return Line{Text: l}
	}
	if m := resourceRE.FindStringSubmatch(l); m != nil {
		svc := ""
		switch m[1] {
		case "Container":
			svc = ServiceOf(project, m[2])
		case "Service":
			svc = m[2]
		case "Image": // <project>-web:tag, or any image a service names
			name, _, _ := strings.Cut(m[2], ":")
			if rest, ok := strings.CutPrefix(name, project+"-"); ok {
				svc = rest
			}
		}
		return Line{Service: svc, Text: strings.ToLower(m[1]) + " " + strings.TrimSpace(m[3]), Keep: true}
	}
	if m := logLineRE.FindStringSubmatch(l); m != nil && !strings.HasPrefix(l, "#") {
		return Line{Service: ServiceOf(project, m[1]), Text: m[2], Keep: true}
	}
	low := strings.ToLower(l)
	keep := strings.Contains(low, "error") || strings.Contains(low, "failed") || strings.Contains(low, "warn") || strings.Contains(low, "skipping")
	return Line{Text: l, Keep: keep && strings.TrimSpace(l) != ""}
}

// ServiceOf turns a container name (<project>-web-1, <project>_web_1) into
// its service; anything else is returned as it is.
func ServiceOf(project, name string) string {
	for _, sep := range []string{"-", "_"} {
		if rest, ok := strings.CutPrefix(name, project+sep); ok {
			if i := strings.LastIndex(rest, sep); i > 0 && allDigits(rest[i+1:]) {
				return rest[:i]
			}
			return rest
		}
	}
	if i := strings.LastIndex(name, "-"); i > 0 && allDigits(name[i+1:]) {
		return name[:i]
	}
	return name
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
