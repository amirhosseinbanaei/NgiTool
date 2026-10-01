// Package apply is the one way NgiTool changes nginx. Every mutating
// command builds the next state and hands it to Run, which renders the
// instance's files, shows a diff, checks for hand edits, asks, snapshots,
// writes, runs nginx -t, reloads, probes and only then saves state. Any
// failure puts the snapshot back, so nginx keeps serving the previous
// config (APPLY-01 … APPLY-10). No other package writes nginx files.
package apply

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// Asker asks the questions a transaction may need. The CLI's version
// honours --yes and --on-drift; tests answer directly.
type Asker interface {
	Confirm(question, note string) (bool, error)
	// Drift picks overwrite, adopt or abort for hand-edited files.
	Drift(files []string, canAdopt bool) (string, error)
}

// Deps are the outside world of a transaction.
type Deps struct {
	Paths  paths.Paths
	Driver driver.Driver
	Prober Prober
	Ask    Asker
	// Logs returns nginx's error output since a time ("" when unknown); a
	// bind failure after a passing test shows up only there (APPLY-10).
	Logs   func(ctx context.Context, since time.Time) string
	Now    func() time.Time
	Chroot string // host instances in tests
	Retain int
	Settle time.Duration // wait after a reload before reading logs and probing
}

// IncludeEdit is the include line in a hand-written file: added by adopt,
// removed by release.
type IncludeEdit struct {
	File    string // host path
	Content string // the whole new content
}

// Change is what one transaction applies to one instance.
type Change struct {
	Report   *discover.Report
	Instance *discover.Instance
	Before   *model.State
	Next     *model.State
	Summary  string   // "route add api.example.com"
	Probe    []string // route ids to request afterwards
	Include  *IncludeEdit
	Release  bool      // remove every NgiTool file of the instance
	Snapshot *Manifest // rollback: these files instead of rendering
}

// Opts are the flags of a mutating command.
type Opts struct {
	Yes     bool
	DryRun  bool
	OnDrift string // "", overwrite, adopt, abort
}

// Result is what happened.
type Result struct {
	Changed, Added, Removed int
	Snapshot                *Manifest
	Probes                  []Probe
	Notes                   model.Problems
	NoChange                bool
	Reloaded                bool
}

// Summary is "2 files changed, 1 added, 0 removed".
func (r *Result) Summary() string {
	return fmt.Sprintf("%d %s changed, %d added, %d removed", r.Changed, plural(r.Changed, "file"), r.Added, r.Removed)
}

func plural(n int, s string) string {
	if n == 1 {
		return s
	}
	return s + "s"
}

// ErrAborted is "abort" at the drift question, or "no" at the confirm.
var ErrAborted = errors.New("cancelled — nothing changed")

// TestError is nginx -t rejecting the new files; the snapshot is back.
type TestError struct {
	Output string
	File   string
	Line   int
}

func (e *TestError) Error() string {
	return "nginx -t rejected the change — the previous config was restored and nothing was reloaded (APPLY-01)\n" + e.Output
}

// ReloadError is a reload that failed after the test passed (APPLY-03,
// APPLY-10): the snapshot is back and nginx was reloaded again.
type ReloadError struct {
	First, Second string
}

func (e *ReloadError) Error() string {
	s := "reload failed after nginx -t passed — the previous config was restored (APPLY-03)\nfirst reload: " + e.First
	if e.Second == "" {
		return s + "\nsecond reload, with the previous config: ok"
	}
	return s + "\nsecond reload, with the previous config: " + e.Second
}

// built is the rendered file set of one state.
type built struct {
	files map[string]string // host path → content
	byNg  map[string]string // nginx path → host path
	dirs  []string          // host dirs
	notes model.Problems
}

type tx struct {
	d      Deps
	c      Change
	o      Opts
	in     *discover.Instance
	facts  render.Facts
	owned  []string
	locDir string
}

// Run applies one change. The caller holds the state lock.
func Run(ctx context.Context, d Deps, c Change, o Opts) (*Result, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	t := &tx{d: d, c: c, o: o, in: c.Instance}
	a := c.Next.Adopted(t.in.ID)
	if a == nil {
		a = c.Before.Adopted(t.in.ID)
	}
	if a == nil {
		return nil, fmt.Errorf("%s is not adopted — run: ngitool instance adopt %s", t.in.Name, t.in.ID)
	}
	t.facts = render.GatherFacts(c.Report, t.in, a)
	for _, dir := range t.facts.Layout.Owned {
		hp, err := t.host(dir)
		if err != nil {
			return nil, err
		}
		t.owned = append(t.owned, hp)
	}
	t.locDir, _ = t.host(t.facts.Layout.Locations)
	return t.run(ctx)
}

func (t *tx) host(p string) (string, error) { return render.HostPath(t.in, p, t.d.Chroot) }

func (t *tx) build(st *model.State) (*built, error) {
	b := &built{files: map[string]string{}, byNg: map[string]string{}}
	switch {
	case t.c.Release:
		return b, nil
	case t.c.Snapshot != nil:
		for _, f := range t.c.Snapshot.Files {
			if !f.Existed || f.Raw {
				continue
			}
			content, err := t.c.Snapshot.Content(f)
			if err != nil {
				return nil, err
			}
			b.files[f.Path] = content
		}
		return b, nil
	}
	out, err := render.Render(st, t.facts)
	if err != nil {
		return nil, err
	}
	for _, f := range out.Files {
		hp, err := t.host(f.Path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		b.files[hp] = f.Content()
		b.byNg[f.Path] = hp
	}
	for _, dir := range out.Dirs {
		hp, err := t.host(dir)
		if err != nil {
			return nil, err
		}
		b.dirs = append(b.dirs, hp)
	}
	b.notes = out.Notes
	return b, nil
}

func (t *tx) run(ctx context.Context) (*Result, error) {
	res := &Result{}
	disk, err := readManaged(t.owned)
	if err != nil {
		return nil, err
	}
	want, err := t.build(t.c.Next)
	if err != nil {
		return nil, err
	}
	// Never overwrite something a person wrote (CONF-09).
	for p := range want.files {
		if _, ours := disk[p]; ours {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%s exists and was not written by NgiTool — move it away first (CONF-09)", p)
		}
	}

	// Drift: managed files whose body no longer matches their hash (APPLY-05).
	var drifted []string
	for p, content := range disk {
		if render.Parse(content).Edited {
			drifted = append(drifted, p)
		}
	}
	sort.Strings(drifted)
	if len(drifted) > 0 && t.c.Snapshot == nil {
		if want, err = t.drift(drifted, disk, want); err != nil {
			return nil, err
		}
	}
	res.Notes = want.notes

	// Diff, grouped by file.
	var names []string
	seen := map[string]bool{}
	for p := range disk {
		names, seen[p] = append(names, p), true
	}
	for p := range want.files {
		if !seen[p] {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	var write = map[string]string{}
	var remove []string
	var diffs []string
	for _, p := range names {
		old, had := disk[p]
		nw, keep := want.files[p]
		switch {
		case had && keep && old == nw:
		case had && keep:
			res.Changed++
			write[p] = nw
			diffs = append(diffs, ui.UnifiedDiff(p, p, old, nw))
		case keep:
			res.Added++
			write[p] = nw
			diffs = append(diffs, ui.UnifiedDiff("/dev/null", p, "", nw))
		default:
			res.Removed++
			remove = append(remove, p)
			diffs = append(diffs, ui.UnifiedDiff(p, "/dev/null", old, ""))
		}
	}
	var rawOld string
	if inc := t.c.Include; inc != nil {
		b, err := os.ReadFile(inc.File)
		if err != nil {
			return nil, err
		}
		rawOld = string(b)
		if rawOld != inc.Content {
			res.Changed++
			diffs = append(diffs, ui.UnifiedDiff(inc.File, inc.File, rawOld, inc.Content))
		}
	}
	if len(diffs) == 0 {
		res.NoChange = true
		ui.Info("nginx files are already up to date " + ui.Muted("("+t.in.Name+")"))
		if t.o.DryRun {
			return res, nil
		}
		return res, t.save(res)
	}
	ui.Heading("Changes", t.in.Name)
	for _, d := range diffs {
		ui.Plain(indentDiff(ui.Diff(d)))
	}
	ui.Plain("  " + ui.Bold(res.Summary()))
	t.printNotes(res.Notes)
	if t.o.DryRun {
		ui.Hint("--dry-run: nothing was written (APPLY-08)")
		return res, nil
	}
	if !t.o.Yes {
		ok, err := t.d.Ask.Confirm("Apply these changes to "+t.in.Name+"?", "snapshot first; nginx -t and reload; the snapshot comes back on any failure")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrAborted
		}
	}

	// Snapshot (APPLY-06): every managed file now, every target, the include file.
	var snapFiles []string
	for _, p := range names {
		snapFiles = append(snapFiles, p)
	}
	var raw []string
	if t.c.Include != nil {
		raw = append(raw, t.c.Include.File)
	}
	m, err := takeSnapshot(t.d.Paths.Backups, t.in.ID, t.d.Now(), t.c.Summary, res.Summary(), snapFiles, raw, t.d.Paths.State)
	if err != nil {
		return nil, fmt.Errorf("snapshot failed, nothing was changed: %w", err)
	}
	res.Snapshot = m
	restore := func() error { return m.restore(t.inPlace) }

	// Write: stage everything first so a full disk fails before any live
	// file changes (APPLY-07, APPLY-09), then swap.
	st, err := stageAll(write)
	if err != nil {
		return res, fmt.Errorf("%w — nothing was changed", err)
	}
	fail := func(err error) (*Result, error) {
		if rerr := restore(); rerr != nil {
			return res, fmt.Errorf("%v; %v", err, rerr)
		}
		return res, fmt.Errorf("%w — the snapshot was restored", err)
	}
	if err := commit(st); err != nil {
		return fail(err)
	}
	for _, p := range remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(&WriteError{Path: p, Err: err})
		}
	}
	if inc := t.c.Include; inc != nil && rawOld != inc.Content {
		var err error
		if t.inPlace(inc.File) {
			err = writeInPlace(inc.File, inc.Content)
		} else {
			err = writeAtomic(inc.File, inc.Content)
		}
		if err != nil {
			return fail(err)
		}
	}
	keep := map[string]bool{}
	for _, dir := range want.dirs {
		keep[dir] = true
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fail(&WriteError{Path: dir, Err: err})
		}
	}
	if t.locDir != "" {
		removeEmptyDirs(t.locDir, keep)
	}

	// nginx -t (APPLY-01, APPLY-02 for stopped containers).
	var tr driver.Result
	err = ui.Task("Testing the new config (nginx -t)", func(*ui.TaskCtl) error {
		var terr error
		tr, terr = t.d.Driver.Test(ctx)
		if terr == nil && !tr.OK {
			terr = errors.New("nginx -t failed")
		}
		return terr
	})
	if err != nil {
		rerr := restore()
		if tr.Output == "" {
			if rerr != nil {
				return res, fmt.Errorf("nginx -t could not run: %v; %v", err, rerr)
			}
			return res, fmt.Errorf("nginx -t could not run: %w — the snapshot was restored", err)
		}
		t.showBlame(tr, want)
		if rerr != nil {
			return res, fmt.Errorf("%v; %v", &TestError{Output: tr.Output, File: tr.File, Line: tr.Line}, rerr)
		}
		return res, &TestError{Output: tr.Output, File: tr.File, Line: tr.Line}
	}

	// Reload (APPLY-03, APPLY-10).
	if t.in.State != discover.StateRunning {
		ui.Hint(t.in.Name + " is " + t.in.State + ": nothing to reload — nginx reads the new config when it starts (APPLY-02)")
		if t.in.Kind != discover.KindHost {
			ui.Hint("start it: docker start " + t.in.Container)
		}
		return res, t.save(res)
	}
	since := t.d.Now()
	err = ui.Task("Reloading "+t.in.Name, func(*ui.TaskCtl) error {
		if err := t.d.Driver.Reload(ctx); err != nil {
			return err
		}
		if t.d.Logs != nil {
			if t.d.Settle > 0 {
				time.Sleep(t.d.Settle)
			}
			if emerg := emergLines(t.d.Logs(ctx, since)); emerg != "" {
				return errors.New(emerg)
			}
		}
		return nil
	})
	if err != nil {
		first := err.Error()
		if rerr := restore(); rerr != nil {
			return res, fmt.Errorf("reload failed (%s) and %v", first, rerr)
		}
		second := ""
		if err2 := t.d.Driver.Reload(ctx); err2 != nil {
			second = err2.Error()
		}
		return res, &ReloadError{First: first, Second: second}
	}
	res.Reloaded = true

	// Probe: through the instance's own listener with the route's Host.
	if t.d.Prober != nil && len(t.c.Probe) > 0 && !t.c.Release {
		if t.d.Settle > 0 {
			time.Sleep(t.d.Settle)
		}
		res.Probes = t.probe(ctx)
	}
	return res, t.save(res)
}

var emergRE = regexp.MustCompile(`\[(emerg|alert)\][^\n]*`)

// emergLines are the fatal lines of nginx's log, e.g. a bind() failure.
func emergLines(log string) string {
	return strings.Join(emergRE.FindAllString(log, 5), "\n")
}

func (t *tx) save(res *Result) error {
	if err := model.Save(t.d.Paths, t.c.Next); err != nil {
		return fmt.Errorf("nginx has the new config but state.json could not be saved: %w — run: ngitool apply", err)
	}
	retain := t.d.Retain
	rotate(t.d.Paths.Backups, t.in.ID, retain)
	return nil
}

// inPlace: a hand-written file that is a single-file bind mount (CONF-07).
func (t *tx) inPlace(p string) bool {
	for _, m := range t.in.Mounts {
		if m.File && m.Source == p {
			return true
		}
	}
	return false
}

func writeAtomic(p, content string) error {
	st, err := stageAll(map[string]string{p: content})
	if err != nil {
		return err
	}
	return commit(st)
}

func indentDiff(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

func (t *tx) printNotes(ps model.Problems) {
	for _, p := range ps {
		switch p.Level {
		case model.LevelWarn:
			ui.Warning(p.Msg)
		default:
			ui.Info(p.Msg)
		}
		if p.Fix != "" {
			ui.Hint(p.Fix + "  " + "(" + p.Code + ")")
		} else if p.Code != "" {
			ui.Hint("(" + p.Code + ")")
		}
	}
}

// showBlame prints nginx's complaint and the offending line of the file it
// named, from what was written (it is restored by now), highlighted.
func (t *tx) showBlame(tr driver.Result, b *built) {
	ui.Fail("nginx -t rejected the new config")
	for _, l := range strings.Split(tr.Output, "\n") {
		ui.Plain("    " + ui.Muted(l))
	}
	if tr.File == "" {
		return
	}
	content, managed := "", false
	if hp, ok := b.byNg[tr.File]; ok {
		content, managed = b.files[hp], true
	} else if hp, err := t.host(tr.File); err == nil {
		if data, err := os.ReadFile(hp); err == nil {
			content = string(data)
		}
	}
	if content == "" {
		return
	}
	who := "a hand-written file"
	if managed {
		who = "a file NgiTool wrote"
	}
	ui.Plain("")
	ui.Plain("  " + ui.Bold(tr.File+":"+strconv.Itoa(tr.Line)) + "  " + ui.Muted("("+who+")"))
	lines := strings.Split(content, "\n")
	for i := max(1, tr.Line-3); i <= min(len(lines), tr.Line+2); i++ {
		num := fmt.Sprintf("%5d ", i)
		if i == tr.Line {
			ui.Plain("  " + ui.Err(ui.SymPointer+num+lines[i-1]))
		} else {
			ui.Plain("   " + ui.Muted(num) + lines[i-1])
		}
	}
	ui.Plain("")
	ui.Hint("the snapshot was restored: " + t.in.Name + " still runs its previous config")
}

func (t *tx) probe(ctx context.Context) []Probe {
	ts, out := ProbeTargets(t.c.Next, t.in, t.c.Probe, t.facts.HTTPPort, t.facts.HTTPSPort)
	h := LoadHealth(t.d.Paths.Cache)
	for _, tg := range ts {
		var p Probe
		_ = ui.Task("Requesting "+tg.Host+tg.Path, func(tc *ui.TaskCtl) error {
			p = t.d.Prober.Probe(ctx, tg)
			if !p.Reached {
				return errors.New("")
			}
			return nil
		})
		out = append(out, p)
		h.Routes[p.Route] = p
		switch {
		case p.Err != "":
			ui.Warning(p.URL + " did not answer: " + p.Err)
		case p.Reached:
			ui.Done(p.URL + " → " + strconv.Itoa(p.Status) + ui.Muted(" ("+p.Took.Round(time.Millisecond).String()+")"))
		default:
			ui.Warning(p.URL + " → " + strconv.Itoa(p.Status) + ": nginx did not reach the upstream")
			for _, c := range Causes(p.Status, tg.Kinds) {
				ui.Hint(ui.SymArrow + " " + c)
			}
		}
	}
	for _, p := range out {
		if p.Skipped != "" {
			ui.Hint(p.Route + ": " + p.Skipped)
		}
	}
	h.Save(t.d.Paths.Cache)
	return out
}

// HostRoot is where an instance's files are on this machine (adopt plans).
func HostRoot(in *discover.Instance, p string) string {
	hp, err := render.HostPath(in, p, "")
	if err != nil {
		return p
	}
	return filepath.Clean(hp)
}
