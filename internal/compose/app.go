package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// App is a linked compose project (state.json "apps"). It replaces the
// legacy apps/ symlink folder: the files are referenced where they live,
// in merge order, and NgiTool's override is passed last.
type App struct {
	Name       string   `json:"name"`
	Project    string   `json:"projectName"`
	WorkingDir string   `json:"workingDir"`
	Files      []string `json:"files"`
	Profiles   []string `json:"profiles,omitempty"`
	EnvFiles   []string `json:"envFiles,omitempty"` // only when explicitly chosen
	// Override is NgiTool's file, /var/lib/ngitool/overrides/<app>.yaml;
	// it exists while a service is attached or a config is externalized.
	Override string            `json:"overridePath,omitempty"`
	Attached map[string]Attach `json:"attachedServices,omitempty"`
	// Mounts are externalized nginx configs (CONF-06), by service.
	Mounts   map[string][]Mount `json:"mounts,omitempty"`
	Owner    string             `json:"owner,omitempty"`
	LinkedAt string             `json:"linkedAt"`
	Last     *LastAction        `json:"lastAction,omitempty"`
}

// Attach is a service joined to an nginx instance's network.
type Attach struct {
	Network string   `json:"network"`
	Alias   string   `json:"alias"` // <app>-<service>: what routes use, never an IP (DOCK-16)
	Keys    []string `json:"keys"`  // the service's own network keys, kept so it stays on them
	// Manual: the user pasted the snippet into their own file; the
	// override leaves the service out.
	Manual bool `json:"manual,omitempty"`
}

// Mount is a host directory bind-mounted over a path in the container.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// LastAction is the last lifecycle command NgiTool ran on the app.
type LastAction struct {
	Action   string   `json:"action"`
	At       string   `json:"at"`
	OK       bool     `json:"ok"`
	Services []string `json:"services,omitempty"`
}

// NeedsOverride reports whether the app has anything for the override.
func (a App) NeedsOverride() bool { return len(a.overridden()) > 0 || len(a.Mounts) > 0 }

// overridden are the attachments the override carries.
func (a App) overridden() map[string]Attach {
	out := map[string]Attach{}
	for s, at := range a.Attached {
		if !at.Manual {
			out[s] = at
		}
	}
	return out
}

// Compose is the app as every compose run sees it: the override is always
// included when the app has one (DOCK-07).
func (a App) Compose() Project {
	p := Project{Name: a.Project, Dir: a.WorkingDir, Files: a.Files, Profiles: a.Profiles, EnvFiles: a.EnvFiles}
	if a.NeedsOverride() {
		p.Override = a.Override
	}
	return p
}

// Expected is the -f list a container of the app should carry.
func (a App) Expected() []string {
	out := append([]string{}, a.Files...)
	if a.NeedsOverride() && a.Override != "" {
		out = append(out, a.Override)
	}
	return out
}

// Alias is the name a service of app gets on an nginx network (DOCK-16).
func Alias(app, service string) string { return app + "-" + service }

var appNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidName checks an app name: lower case, digits, . _ -.
func ValidName(s string) error {
	if !appNameRE.MatchString(s) {
		return errors.New("lower-case letters, digits, . _ - (starting with a letter or digit)")
	}
	return nil
}

// UniqueName is base, or base-2, base-3 … when taken (cli/src/flows.mjs:829-833).
func UniqueName(base string, taken func(string) bool) string {
	base = ProjectName(base)
	if base == "" {
		base = "app"
	}
	name := base
	for i := 2; taken(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// ---- override -------------------------------------------------------------

// Meta is the JSON in the override's "# ngitool:" comment line, so the
// file says on its own what it does and can be read back.
type Meta struct {
	App      string             `json:"app"`
	Services map[string]Attach  `json:"services,omitempty"`
	Mounts   map[string][]Mount `json:"mounts,omitempty"`
}

const metaPrefix = "# ngitool: "

// NetKey is the network's key inside the override.
func NetKey(network string) string {
	var b strings.Builder
	for _, r := range network {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "ngt_" + b.String()
}

// q quotes a scalar for YAML (a JSON string is a valid YAML scalar).
func q(s string) string { b, _ := json.Marshal(s); return string(b) }

// RenderOverride ports cli/src/apps.mjs overrideYaml: each attached
// service keeps its own network keys (so it does not fall off its default
// network) and joins the instance's network with its alias; externalized
// configs are bind mounts. The project's own files are never touched.
func RenderOverride(a App) string {
	attached := a.overridden()
	meta, _ := json.Marshal(Meta{App: a.Name, Services: attached, Mounts: a.Mounts})
	var b strings.Builder
	b.WriteString("# Managed by NgiTool — the override of app \"" + a.Name + "\".\n")
	b.WriteString("# Passed as the last -f by every `ngitool app` command; the project's own compose files are never edited.\n")
	b.WriteString("# A plain `docker compose up` in the project leaves it out: run `ngitool app up " + a.Name + "` instead (DOCK-07).\n")
	b.WriteString(metaPrefix + string(meta) + "\n")
	b.WriteString("services:\n")
	names := map[string]bool{}
	for s := range attached {
		names[s] = true
	}
	for s := range a.Mounts {
		names[s] = true
	}
	var svcs []string
	for s := range names {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	nets := map[string]bool{}
	for _, s := range svcs {
		b.WriteString("  " + q(s) + ":\n")
		if at, ok := attached[s]; ok {
			key := NetKey(at.Network)
			nets[at.Network] = true
			b.WriteString("    networks:\n")
			for _, k := range at.Keys {
				if k != key {
					b.WriteString("      " + q(k) + ": {}\n")
				}
			}
			b.WriteString("      " + q(key) + ":\n        aliases: [" + q(at.Alias) + "]\n")
		}
		if ms := a.Mounts[s]; len(ms) > 0 {
			b.WriteString("    volumes:\n")
			for _, m := range ms {
				b.WriteString("      - type: bind\n        source: " + q(m.Source) + "\n        target: " + q(m.Target) + "\n")
				if m.ReadOnly {
					b.WriteString("        read_only: true\n")
				}
			}
		}
	}
	if len(nets) > 0 {
		var ns []string
		for n := range nets {
			ns = append(ns, n)
		}
		sort.Strings(ns)
		b.WriteString("networks:\n")
		for _, n := range ns {
			b.WriteString("  " + q(NetKey(n)) + ":\n    name: " + q(n) + "\n    external: true\n")
		}
	}
	return b.String()
}

// ParseMeta reads the "# ngitool:" line back.
func ParseMeta(text string) (Meta, error) {
	for _, l := range strings.Split(text, "\n") {
		if j, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), metaPrefix); ok {
			var m Meta
			if err := json.Unmarshal([]byte(j), &m); err != nil {
				return m, fmt.Errorf("the ngitool: line is not valid JSON: %v", err)
			}
			return m, nil
		}
	}
	return Meta{}, errors.New("no ngitool: line — not an override NgiTool wrote")
}

// ManualSnippet ports cli/src/apps.mjs manualSnippet: the lines to paste
// into the project's own compose file instead of using the override.
func ManualSnippet(service string, at Attach) string {
	lines := []string{"services:", "  " + service + ":", "    networks:"}
	for _, k := range at.Keys {
		lines = append(lines, "      "+k+":")
	}
	key := NetKey(at.Network)
	lines = append(lines, "      "+key+":", "        aliases: ["+at.Alias+"]", "networks:", "  "+key+":", "    name: "+at.Network, "    external: true")
	return strings.Join(lines, "\n")
}

// OverridePath is where app's override lives.
func OverridePath(dir, app string) string { return filepath.Join(dir, app+".yaml") }

// Missing are the app's files that no longer exist (DOCK-10).
func (a App) Missing() []string {
	var out []string
	for _, f := range a.Files {
		if _, err := os.Stat(f); err != nil {
			out = append(out, f)
		}
	}
	if _, err := os.Stat(a.WorkingDir); err != nil {
		out = append(out, a.WorkingDir)
	}
	return out
}
