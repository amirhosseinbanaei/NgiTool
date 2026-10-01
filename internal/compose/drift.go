package compose

import (
	"path/filepath"
	"sort"
	"strings"
)

// Network statuses of a linked app (app ls, route ls, doctor).
const (
	NetOK       = "ok"           // every attached service runs on its network, with the override
	NetDetached = "detached"     // started without the override: routes answer 502 (DOCK-07)
	NetNone     = "not attached" // nothing attached (no route needed it)
	NetStopped  = "stopped"      // nothing running to check
	NetMissing  = "missing"      // its files are gone (DOCK-10)
)

// DetachedWhy is the one-line explanation of NetDetached.
const DetachedWhy = "started without NgiTool's override (plain docker compose up?) — its routes return 502"

// Drift is how a linked app's containers compare with what NgiTool runs.
type Drift struct {
	Status string `json:"status"`
	Why    string `json:"why,omitempty"`
	// Services are the attached services that are off their network.
	Services []string `json:"services,omitempty"`
	Up       int      `json:"up"`
	Total    int      `json:"total"`
}

// Check compares the app's containers' config_files labels and networks
// with the app's files plus override (DOCK-07). Containers are matched by
// the project label, so a copy started under another project name is not
// this app's.
func Check(a App, ctrs []Ctr) Drift {
	var d Drift
	var mine []Ctr
	for _, c := range ctrs {
		if c.Project == a.Project {
			mine = append(mine, c)
			d.Total++
			if c.Running {
				d.Up++
			}
		}
	}
	if miss := a.Missing(); len(miss) > 0 {
		d.Status, d.Why = NetMissing, "files gone: "+strings.Join(miss, ", ")+" — re-link: ngitool app unlink "+a.Name+" && ngitool app link <new path> (DOCK-10)"
		return d
	}
	if len(a.Attached) == 0 {
		d.Status = NetNone
		return d
	}
	if d.Up == 0 {
		d.Status = NetStopped
		return d
	}
	expected := a.Expected()
	off := map[string]bool{}
	for _, c := range mine {
		at, ok := a.Attached[c.Service]
		if !ok || !c.Running {
			continue
		}
		if !at.Manual && !sameFiles(c.ConfigFiles, expected) && !contains(c.ConfigFiles, a.Override) {
			off[c.Service] = true
		}
		if !contains(c.Networks, at.Network) {
			off[c.Service] = true
		}
	}
	if len(off) == 0 {
		d.Status = NetOK
		return d
	}
	for s := range off {
		d.Services = append(d.Services, s)
	}
	sort.Strings(d.Services)
	d.Status, d.Why = NetDetached, DetachedWhy
	return d
}

func sameFiles(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if filepath.Clean(a[i]) != filepath.Clean(b[i]) {
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
