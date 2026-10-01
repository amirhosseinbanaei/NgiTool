package apply

import (
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// Drift choices (APPLY-05).
const (
	DriftOverwrite = "overwrite"
	DriftAdopt     = "adopt"
	DriftAbort     = "abort"
)

// drift shows each hand edit (what NgiTool wrote → what is on disk) and
// asks: overwrite, adopt the added lines into state as extra directives,
// or abort. It returns the file set to write.
func (t *tx) drift(files []string, disk Disk, want *built) (*built, error) {
	wrote, err := t.build(t.c.Before)
	if err != nil {
		return nil, err
	}
	ui.Heading("Edited by hand", "managed files whose body no longer matches their hash (APPLY-05)")
	canAdopt := true
	for _, p := range files {
		ui.Plain(indentDiff(ui.Diff(ui.UnifiedDiff(p+" (as NgiTool wrote it)", p+" (on disk)", wrote.files[p], disk[p]))))
		if t.owner(render.Parse(disk[p]).ID) == nil {
			canAdopt = false
		}
	}
	if t.o.DryRun {
		ui.Hint("--dry-run: the diff below assumes overwrite")
		return want, nil
	}
	choice := t.o.OnDrift
	if choice == "" {
		if t.d.Ask == nil {
			choice = DriftAbort
		} else if choice, err = t.d.Ask.Drift(files, canAdopt); err != nil {
			return nil, err
		}
	}
	switch choice {
	case DriftOverwrite:
		return want, nil
	case DriftAdopt:
		if !canAdopt {
			ui.Fail("the entry file and the shared snippet cannot be adopted — overwrite or abort")
			return nil, ErrAborted
		}
		for _, p := range files {
			added, removed := lineDiff(render.Parse(wrote.files[p]).Body, render.Parse(disk[p]).Body)
			x := t.owner(render.Parse(disk[p]).ID)
			x.add(added)
			if len(removed) > 0 {
				ui.Warning(p + ": removed lines cannot be adopted and come back: " + strings.Join(trimAll(removed), " · "))
			}
		}
		return t.build(t.c.Next)
	}
	return nil, ErrAborted
}

// extra is where adopted directives go.
type extra struct {
	add func(lines []scoped)
}

type scoped struct {
	text  string
	depth int // 1: server/upstream level, 2+: inside a location
}

// owner finds the route or pool a managed file belongs to.
func (t *tx) owner(id string) *extra {
	kind, name, _ := strings.Cut(id, ":")
	switch kind {
	case "route":
		r := t.c.Next.Route(name)
		if r == nil {
			return nil
		}
		path := r.Path != ""
		return &extra{add: func(ls []scoped) {
			for _, l := range ls {
				if !path && l.depth <= 1 {
					r.Extra = append(r.Extra, "server:"+l.text)
				} else {
					r.Extra = append(r.Extra, l.text)
				}
			}
		}}
	case "pool":
		p := t.c.Next.Pool(name)
		if p == nil {
			return nil
		}
		return &extra{add: func(ls []scoped) {
			for _, l := range ls {
				p.Extra = append(p.Extra, l.text)
			}
		}}
	}
	return nil
}

// lineDiff is the directive lines added to b and removed from a, with the
// block depth each added line sits at. Comments and blank lines are left
// out: they are not directives.
func lineDiff(a, b string) ([]scoped, []string) {
	count := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		count[strings.TrimSpace(l)]++
	}
	var added []scoped
	depth := 0
	for _, l := range strings.Split(b, "\n") {
		s := strings.TrimSpace(l)
		at := depth
		depth += strings.Count(s, "{") - strings.Count(s, "}")
		if count[s] > 0 {
			count[s]--
			continue
		}
		if s == "" || strings.HasPrefix(s, "#") || strings.ContainsAny(s, "{}") {
			continue
		}
		added = append(added, scoped{text: s, depth: at})
	}
	var removed []string
	for l, n := range count {
		if n > 0 && l != "" && !strings.HasPrefix(l, "#") {
			removed = append(removed, l)
		}
	}
	return added, removed
}

func trimAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = strings.TrimSpace(x)
	}
	return out
}
