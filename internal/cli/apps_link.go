package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// linkOpts are app link / app scan's flags: one per step.
type linkOpts struct {
	files, profiles, envFiles []string
	name, project             string
	all, includeOthers        bool
	asJSON, noServe           bool
}

func appLinkCmd(e *env, scan bool) *cobra.Command {
	o := &linkOpts{}
	c := &cobra.Command{
		Use:   "link [path]",
		Short: "link a compose project: pick it from every one on this server",
		Long: "Finds every compose project (any file name, any folder layout, any owner) and shows them as a\n" +
			"checklist grouped Running now, Stopped, Never started, Other users' projects, Already linked.\n" +
			"Then, per project: which compose files make it up (pre-selected from what runs now), its\n" +
			"profiles, its name, and \"Serve it now?\" — straight into the route wizard.\n" +
			"With a path (a directory or a compose file) only that project is linked.",
		Example:     "ngitool app link\nngitool app link ~/shop --file compose.yaml --file compose.prod.yaml --name shop",
		Annotations: map[string]string{annSynopsis: "link [path] [--file F]… [--name N] [--profile P]…"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.appLink(cmd, o, args, scan)
		},
	}
	if scan {
		c.Use = "scan"
		c.Short = "the link checklist, linking several projects at once"
		c.Long = "The same checklist as app link, in multi-link mode. --all links every unlinked project owned by\n" +
			"root without asking; other users' projects only with --include-others (each owner is confirmed)."
		c.Example = "ngitool app scan\nngitool app scan --all --yes\nngitool app scan --json"
		c.Annotations = map[string]string{annSynopsis: "scan [--all [--include-others]] [--json]"}
		c.Args = noArgs
		c.Flags().BoolVar(&o.all, "all", false, "link every unlinked project owned by root")
		c.Flags().BoolVar(&o.includeOthers, "include-others", false, "with --all: other users' projects too (DOCK-09)")
		c.Flags().BoolVar(&o.asJSON, "json", false, "print the candidates and link nothing")
		return c
	}
	fl := c.Flags()
	fl.StringArrayVar(&o.files, "file", nil, "a compose file, in merge order (repeat); default: what runs now, else base + override")
	fl.StringArrayVar(&o.profiles, "profile", nil, "a profile to enable (repeat)")
	fl.StringArrayVar(&o.envFiles, "env-file", nil, "an --env-file for every compose run (repeat)")
	fl.StringVar(&o.name, "name", "", "the app's name in NgiTool (default: the project name)")
	fl.StringVar(&o.project, "project-name", "", "the compose project name (-p); default: the running label, name:, or the folder")
	fl.BoolVar(&o.noServe, "no-serve", false, "do not ask \"Serve it now?\"")
	return c
}

func (e *env) appLink(cmd *cobra.Command, o *linkOpts, args []string, scan bool) error {
	ctx := cmd.Context()
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	ctrs, cerr := appContainers(ctx)
	if cerr != nil && !o.asJSON {
		ui.Warning("running projects are not merged in: " + firstLine(cerr.Error()))
	}
	var picked []compose.Candidate
	if len(args) == 1 {
		c, err := candidateAt(args[0], ctrs)
		if err != nil {
			return err
		}
		markCandidates(e, st, []*compose.Candidate{&c})
		if c.Linked != "" {
			ui.Info(c.Dir + " is already linked as " + ui.Bold(c.Linked))
			return nil
		}
		if c.Reserved != "" {
			return errors.New(c.Dir + ": " + c.Reserved)
		}
		picked = []compose.Candidate{c}
	} else {
		if !scan && !o.asJSON {
			if err := ui.Need("project", "a path argument (ngitool app link ~/my-project)"); err != nil {
				return err
			}
		}
		cands := e.scanCandidates(ctx, st, ctrs, !o.asJSON)
		if o.asJSON {
			if cands == nil {
				cands = []compose.Candidate{}
			}
			return printJSON(cands)
		}
		if scan && o.all {
			me := currentUser()
			for _, c := range cands {
				if c.Linked != "" || c.Broken != "" || c.Reserved != "" {
					continue
				}
				if c.Owner != me && !o.includeOthers {
					continue
				}
				picked = append(picked, c)
			}
			if len(picked) == 0 {
				ui.Info("nothing to link: every project owned by " + me + " is linked already")
				return nil
			}
		} else {
			if err := ui.Need("projects", "--all (or run on a terminal to pick)"); err != nil {
				return err
			}
			if picked, err = pickCandidates(cands, ctrs, scan); err != nil {
				return err
			}
		}
	}
	var linked []compose.App
	for _, c := range picked {
		a, err := e.linkOne(ctx, st, c, o, scan && o.all)
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			if len(picked) > 1 && !errors.Is(err, ui.ErrInterrupted) {
				ui.Fail(c.Name + ": " + firstLine(err.Error()))
				continue
			}
			return err
		}
		linked = append(linked, *a)
		st.Apps = append(st.Apps, *a) // so the next name is deduplicated against it
	}
	if scan || o.noServe || e.yes || !ui.CanPrompt() {
		if len(linked) > 0 {
			ui.Hint("serve one: ngitool route add — its services are under Linked apps")
		}
		return nil
	}
	for _, a := range linked {
		if err := e.serveNow(cmd, a); err != nil {
			return err
		}
	}
	return nil
}

var errSkip = errors.New("skipped")

// currentUser is who runs NgiTool (root, normally).
func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return strconv.Itoa(os.Geteuid())
}

// scanRoots are config.json scanRoots, else the defaults.
func scanRoots(e *env) []string {
	if cfg, err := state.LoadConfig(e.paths); err == nil && len(cfg.ScanRoots) > 0 {
		return cfg.ScanRoots
	}
	return discover.DefaultRoots
}

// scanCandidates is step a's spinner: the finder over every root, merged
// with the containers' labels.
func (e *env) scanCandidates(ctx context.Context, st *model.State, ctrs []compose.Ctr, show bool) []compose.Candidate {
	roots := scanRoots(e)
	var cands []compose.Candidate
	run := func() {
		res := compose.Scan(roots, compose.DefaultDepth)
		cands = compose.Merge(res.Dirs, ctrs)
	}
	label := fmt.Sprintf("Scanning %d %s for compose projects", len(roots), either(len(roots), "root", "roots"))
	if show {
		_ = ui.Task(label, func(t *ui.TaskCtl) error {
			run()
			t.Update(fmt.Sprintf("Found %d compose %s in %d %s", len(cands), either(len(cands), "project", "projects"), len(roots), either(len(roots), "root", "roots")))
			return nil
		})
	} else {
		run()
	}
	ptrs := make([]*compose.Candidate, len(cands))
	for i := range cands {
		ptrs[i] = &cands[i]
	}
	markCandidates(e, st, ptrs)
	return cands
}

// markCandidates fills Linked from state and Nginx from the last scan (a
// custom image that runs nginx is only known there, DISC-10).
func markCandidates(e *env, st *model.State, cs []*compose.Candidate) {
	var rep *discover.Report
	if r, ok := discover.LoadCache(e.paths.ScanCache, 24*time.Hour, time.Now()); ok {
		rep = r
	}
	for _, c := range cs {
		for _, a := range st.Apps {
			if a.Project == c.Name { // -p is what Docker knows a project by
				c.Linked = a.Name
			}
		}
		if rep == nil {
			continue
		}
		for _, in := range rep.Instances {
			if in.Project != "" && in.Project == c.Name {
				c.Nginx = in.ID
			}
		}
	}
}

// candidateAt is a typed path: a directory, or one compose file in it.
func candidateAt(arg string, ctrs []compose.Ctr) (compose.Candidate, error) {
	p, err := expandPath(arg)
	if err != nil {
		return compose.Candidate{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return compose.Candidate{}, fmt.Errorf("%s does not exist", arg)
	}
	dir, file := p, ""
	if !st.IsDir() {
		dir, file = filepath.Dir(p), p
	}
	res := compose.Scan([]string{dir}, 0)
	var dirs []compose.Dir
	for _, d := range res.Dirs {
		if d.Path == dir {
			dirs = append(dirs, d)
		}
	}
	if file != "" {
		if len(dirs) == 0 {
			dirs = []compose.Dir{{Path: dir}}
		}
		found := false
		for _, f := range dirs[0].Files {
			found = found || f.Path == file
		}
		if !found {
			dirs[0].Files = append(dirs[0].Files, compose.File{Path: file, Role: compose.RoleBase, Owner: compose.OwnerOf(file)})
		}
	}
	if len(dirs) == 0 {
		return compose.Candidate{}, fmt.Errorf("no compose file in %s (compose.yaml, docker-compose.yml, compose.<env>.yaml, *.compose.yaml …)", arg)
	}
	var mine []compose.Ctr
	for _, c := range ctrs {
		if c.WorkDir == dir {
			mine = append(mine, c)
		}
	}
	cs := compose.Merge(dirs, mine)
	if len(cs) == 0 {
		return compose.Candidate{}, fmt.Errorf("no compose project in %s", arg)
	}
	c := cs[0]
	if file != "" && len(c.Labeled) == 0 {
		c.Labeled = []string{file} // the file given is the default
	}
	return c, nil
}

func expandPath(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "~" || strings.HasPrefix(s, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		s = filepath.Join(home, strings.TrimPrefix(s, "~"))
	}
	return filepath.Abs(s)
}

// candidate groups, in the order the checklist shows them.
const (
	grpRunning = "Running now"
	grpStopped = "Stopped"
	grpNever   = "Never started"
	grpOthers  = "Other users' projects"
	grpLinked  = "Already linked"
)

func groupOf(c compose.Candidate, me string) string {
	switch {
	case c.Linked != "":
		return grpLinked
	case c.Owner != "" && c.Owner != me:
		return grpOthers
	case c.State == compose.StateRunning:
		return grpRunning
	case c.State == compose.StateStopped:
		return grpStopped
	}
	return grpNever
}

// candidateRow is one checklist row: state dot and name, a badge (owner
// or nginx), then services, a ports warning chip and the path, muted.
func candidateRow(c compose.Candidate, me string) ui.Option {
	dot := ui.Muted(ui.SymRing)
	state := ""
	switch c.State {
	case compose.StateRunning:
		dot = ui.OK(ui.SymDot)
		if c.Up < c.Total {
			state = fmt.Sprintf("%d/%d up", c.Up, c.Total)
		}
	case compose.StateStopped:
		dot = ui.Warn(ui.SymDot)
	}
	o := ui.Option{Value: c.ID(), Label: dot + " " + c.Name}
	switch {
	case c.Owner != "" && c.Owner != me:
		o.Badge = c.Owner
	case c.Nginx != "":
		o.Badge = "nginx"
	}
	var parts []string
	if state != "" {
		parts = append(parts, state)
	}
	if n := len(c.Services); n > 0 {
		svcs := strings.Join(c.Services[:min(2, n)], ", ")
		if n > 2 {
			svcs += " +" + strconv.Itoa(n-2)
		}
		parts = append(parts, svcs)
	}
	if c.Nginx != "" && o.Badge != "nginx" {
		parts = append(parts, "nginx")
	}
	if pub := c.Public(); len(pub) > 0 {
		parts = append(parts, ui.Warn("! "+pub[0].String()))
	}
	parts = append(parts, shortPath(c.Dir))
	o.Hint = strings.Join(parts, " · ")
	switch {
	case c.Linked != "":
		o.Disabled = "already linked as " + c.Linked
	case c.Broken != "":
		o.Disabled = c.Broken + " (DOCK-10)"
	case c.Reserved != "":
		o.Disabled = c.Reserved
	}
	return o
}

// shortPath writes a home directory as ~user, so the path fits the row.
func shortPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "/home/"); ok {
		return "~" + rest
	}
	if rest, ok := strings.CutPrefix(p, "/root/"); ok {
		return "~root/" + rest
	}
	return p
}

// pickCandidates is step a: the grouped checklist, then a confirm per
// owner for other users' projects (DOCK-09). The last row types a path.
func pickCandidates(cands []compose.Candidate, ctrs []compose.Ctr, multi bool) ([]compose.Candidate, error) {
	me := currentUser()
	groups := map[string][]compose.Candidate{}
	for _, c := range cands {
		g := groupOf(c, me)
		groups[g] = append(groups[g], c)
	}
	var opts []ui.Option
	byID := map[string]compose.Candidate{}
	for _, g := range []string{grpRunning, grpStopped, grpNever, grpOthers, grpLinked} {
		if len(groups[g]) == 0 {
			continue
		}
		opts = append(opts, ui.Sep(g))
		for _, c := range groups[g] {
			byID[c.ID()] = c
			opts = append(opts, candidateRow(c, me))
		}
	}
	opts = append(opts, ui.Option{Value: manualRow, Label: ui.SymPen + " Enter a compose file path manually…", Hint: "a directory or a file; ~ is expanded"})
	title := "Which project should be linked?"
	if multi {
		title = "Which projects should be linked?"
	}
	for {
		ids, err := ui.MultiSelect(ui.MultiOpts{Title: title, Note: fmt.Sprintf("%d projects · / filters · other users' projects ask first", len(cands)), Options: opts})
		if err != nil {
			return nil, err
		}
		var out []compose.Candidate
		owners := map[string]bool{}
		for _, id := range ids {
			if id == manualRow {
				c, err := askPath(ctrs)
				if errors.Is(err, ui.ErrBack) {
					continue
				}
				if err != nil {
					return nil, err
				}
				out = append(out, c)
				continue
			}
			c := byID[id]
			if c.Owner != "" && c.Owner != me && !owners[c.Owner] {
				ok, err := ui.Confirm(fmt.Sprintf("%s belongs to %s. Link it anyway?", c.Name, c.Owner),
					"NgiTool will run docker compose on it as "+me+"; its files and their owner stay as they are (DOCK-09)", false)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				owners[c.Owner] = true
			}
			out = append(out, c)
		}
		if len(out) == 0 {
			ui.Warning("pick at least one project, or press esc")
			continue
		}
		return out, nil
	}
}

// askPath is the manual row: a validated input that shows what it found
// as it is typed.
func askPath(ctrs []compose.Ctr) (compose.Candidate, error) {
	v, err := ui.Input(ui.InputOpts{
		Title:       "Compose file or project directory",
		Placeholder: "~/my-project or /srv/app/docker/compose.prod.yml",
		Live: func(v string) string {
			if v == "" {
				return ui.Muted("a directory with compose files, or one compose file")
			}
			c, err := candidateAt(v, ctrs)
			if err != nil {
				return ui.Muted(err.Error())
			}
			var names []string
			for _, f := range c.Files {
				names = append(names, filepath.Base(f.Path))
			}
			return ui.OK(ui.SymOK) + " " + c.Name + ui.Muted(" · "+strings.Join(names, ", "))
		},
		Validate: func(v string) error {
			if v == "" {
				return errors.New("type a path")
			}
			_, err := candidateAt(v, ctrs)
			return err
		},
	})
	if err != nil {
		return compose.Candidate{}, err
	}
	return candidateAt(v, ctrs)
}

// linkOne is steps b–e for one project: files, profiles, name, plan, save.
func (e *env) linkOne(ctx context.Context, st *model.State, c compose.Candidate, o *linkOpts, quiet bool) (*compose.App, error) {
	tty := ui.CanPrompt() && !quiet
	if c.Broken != "" {
		return nil, problemErr(model.Problem{Code: "DOCK-10", Msg: c.Dir + ": " + c.Broken})
	}
	if tty {
		ui.Heading(c.Name, c.Dir)
	}
	// b. files, in merge order
	files := c.Default()
	if len(o.files) > 0 {
		files = nil
		for _, f := range o.files {
			if !filepath.IsAbs(f) {
				f = filepath.Join(c.Dir, f)
			}
			if _, err := os.Stat(f); err != nil {
				return nil, fmt.Errorf("%s does not exist", f)
			}
			files = append(files, f)
		}
	} else {
		var usable []compose.File
		for _, f := range c.Files {
			if f.Broken == "" {
				usable = append(usable, f)
			}
		}
		if len(usable) > 1 && tty {
			var opts []ui.Option
			for _, f := range c.Files {
				opts = append(opts, fileRow(c, f))
			}
			from := "pre-selected from what runs now"
			if len(c.Labeled) == 0 {
				from = "pre-selected: base plus override"
			}
			for {
				picked, err := ui.MultiSelect(ui.MultiOpts{Title: "Which files make up " + c.Name + "?",
					Note: "merged in this order — later files override earlier ones · " + from, Options: opts, Selected: files})
				if err != nil {
					return nil, err
				}
				if len(picked) > 0 {
					files = picked
					break
				}
				ui.Warning("pick at least one file")
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%s: no compose file to use — pass --file", c.Dir)
	}
	project := c.Name
	if o.project != "" {
		project = compose.ProjectName(o.project)
	}
	b, err := e.composeBin(ctx)
	if err != nil {
		return nil, err
	}
	p := compose.Project{Name: project, Dir: c.Dir, Files: files}
	// c. profiles: none by default
	profiles := o.profiles
	if len(profiles) == 0 && tty {
		if all := compose.Profiles(ctx, appRunner, b, p); len(all) > 0 {
			var opts []ui.Option
			for _, pr := range all {
				opts = append(opts, ui.Option{Value: pr, Label: pr})
			}
			if profiles, err = ui.MultiSelect(ui.MultiOpts{Title: "Profiles to enable", Note: "none by default: services with a profile only start when it is on", Options: opts}); err != nil {
				return nil, err
			}
		}
	}
	p.Profiles, p.EnvFiles = profiles, o.envFiles
	cfg, err := e.readProject(ctx, b, p, tty)
	if err != nil {
		return nil, err
	}
	// d. name
	taken := func(n string) bool { return st.App(n) != nil }
	name := o.name
	if name == "" {
		name = compose.UniqueName(project, taken)
		if tty {
			if name, err = ui.Input(ui.InputOpts{Title: "Name in NgiTool", Default: name, Validate: func(v string) error {
				if err := compose.ValidName(v); err != nil {
					return err
				}
				if taken(v) {
					return errors.New(v + " is taken")
				}
				return nil
			}}); err != nil {
				return nil, err
			}
		}
	} else if err := compose.ValidName(name); err != nil {
		return nil, &UsageError{Msg: "--name: " + err.Error()}
	} else if taken(name) {
		return nil, &UsageError{Msg: "an app named " + name + " exists already"}
	}
	a := compose.App{Name: name, Project: project, WorkingDir: c.Dir, Files: files, Profiles: profiles, EnvFiles: o.envFiles, Owner: c.Owner, LinkedAt: stamp()}
	// e. plan, then save
	pairs := [][2]string{{"Project", project + ui.Muted("  -p "+project)}, {"Directory", c.Dir}}
	for i, f := range files {
		k := ""
		if i == 0 {
			k = "Files"
		}
		pairs = append(pairs, [2]string{k, strconv.Itoa(i+1) + ". " + f})
	}
	if len(profiles) > 0 {
		pairs = append(pairs, [2]string{"Profiles", strings.Join(profiles, ", ")})
	}
	var svcs []string
	for _, s := range cfg.Services {
		x := s.Name
		if s.Replicas > 1 {
			x += " ×" + strconv.Itoa(s.Replicas) // LB-04: N members behind one name
		}
		svcs = append(svcs, x)
	}
	pairs = append(pairs, [2]string{"Services", strings.Join(svcs, ", ")})
	if c.Owner != "" {
		pairs = append(pairs, [2]string{"Owner", c.Owner})
	}
	if tty || !quiet {
		ui.Plan("Link "+name, pairs)
	}
	for _, s := range cfg.Services {
		for _, pp := range s.Published {
			if pp.Public() {
				ui.Warning(s.Name + " publishes " + pp.String() + " on every address: reachable without the proxy " + ui.Muted("(DOCK-11)"))
				ui.Hint(ui.SymArrow + " bind it to 127.0.0.1 or use expose:, then route it through nginx")
				break
			}
		}
	}
	if err := e.withLock(ctx, true, func() error {
		cur, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		if cur.App(name) != nil {
			return errors.New("an app named " + name + " exists already")
		}
		cur.Apps = append(cur.Apps, a)
		return model.Save(e.paths, cur)
	}); err != nil {
		return nil, err
	}
	ui.Done("Linked " + ui.Bold(name) + ui.Muted(" → "+c.Dir))
	return &a, nil
}

func fileRow(c compose.Candidate, f compose.File) ui.Option {
	label := filepath.Base(f.Path)
	if filepath.Dir(f.Path) != c.Dir {
		label = f.Path
	}
	o := ui.Option{Value: f.Path, Label: label}
	switch f.Role {
	case compose.RoleBase:
		o.Hint = "base"
	case compose.RoleOverride:
		o.Hint = "override"
	case compose.RoleEnv:
		o.Hint = f.Word + " variant"
	case compose.RoleExtra:
		o.Hint = "from the running containers' label"
	}
	if contains(c.Labeled, f.Path) {
		o.Badge = "running"
	}
	if f.Owner != "" && f.Owner != currentUser() {
		o.Hint += " · owner " + f.Owner
	}
	if f.Broken != "" {
		o.Disabled = f.Broken + " (DOCK-10)"
	}
	return o
}

// serveNow is step f (cli/src/flows.mjs:778-791): a service into the route
// wizard with it pre-selected, into an existing pool, or not now.
func (e *env) serveNow(cmd *cobra.Command, a compose.App) error {
	ctx := cmd.Context()
	b, err := e.composeBin(ctx)
	if err != nil {
		return err
	}
	cfg, err := compose.Read(ctx, appRunner, b, a.Compose())
	if err != nil {
		if cfg, err = compose.ReadRaw(a.Compose()); err != nil {
			return nil
		}
	}
	var opts []ui.Option
	for _, s := range cfg.Services {
		if s.OneOff() {
			continue
		}
		o := ui.Option{Value: "svc:" + s.Name, Label: "Serve " + s.Name, Hint: serviceHint(s, a) + " · route wizard"}
		if why := s.Detached(); why != "" {
			o.Disabled = why
		}
		opts = append(opts, o)
	}
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	if len(st.Pools) > 0 {
		opts = append(opts, ui.Option{Value: "pool", Label: "Add it to an existing pool", Hint: "as one more member"})
	}
	opts = append(opts, ui.Option{Value: "no", Label: "Not now", Hint: "later: ngitool route add — it is under Linked apps"})
	choice, err := ui.Select(ui.SelectOpts{Title: "Serve " + a.Name + " now?", Options: opts})
	if errors.Is(err, ui.ErrBack) || choice == "no" {
		return nil
	}
	if err != nil {
		return err
	}
	svc := strings.TrimPrefix(choice, "svc:")
	if choice == "pool" {
		var sopts []ui.Option
		for _, s := range cfg.Services {
			if !s.OneOff() && s.Detached() == "" {
				sopts = append(sopts, ui.Option{Value: s.Name, Label: s.Name, Hint: serviceHint(s, a)})
			}
		}
		if svc, err = ui.Select(ui.SelectOpts{Title: "Which service?", Options: sopts}); err != nil {
			return err
		}
	}
	s := cfg.Service(svc)
	ports := s.Ports
	if len(ports) == 0 {
		// Not in the files: the image's EXPOSE, as the running container says.
		rep := e.report(ctx, false, ui.IsTTY())
		for _, c := range rep.Containers {
			if c.Project == a.Project && c.Service == svc {
				for _, p := range c.Exposed {
					if !slices.Contains(ports, p) {
						ports = append(ports, p)
					}
				}
			}
		}
		slices.Sort(ports)
	}
	port, err := pickPort(pickRow{Label: a.Name + "/" + svc, Ports: ports})
	if err != nil {
		return err
	}
	spec := "app:" + a.Name + "/" + svc + ":" + strconv.Itoa(port)
	if choice == "pool" {
		pool, err := poolArg(st, nil, 0, "Add it to which pool?")
		if err != nil {
			return err
		}
		return runArgs(e.root, []string{"pool", "member", "add", pool, spec})
	}
	return runArgs(e.root, []string{"route", "add", "--to", spec})
}

// appRows are the "Linked apps" member source: <app>/<service>:<port>
// with state and network chips; a service off the instance's network is
// attached through the override when picked (route add asks).
func appRows(st *model.State, rep *discover.Report, in *discover.Instance) []pickRow {
	var rows []pickRow
	for _, a := range st.Apps {
		type svc struct {
			up, total int
			ports     map[int]bool
			nets      []string
			image     string
		}
		by := map[string]*svc{}
		var names []string
		get := func(n string) *svc {
			if by[n] == nil {
				by[n] = &svc{ports: map[int]bool{}}
				names = append(names, n)
			}
			return by[n]
		}
		for _, c := range rep.Containers {
			if c.Project != a.Project || c.Name == in.Container {
				continue
			}
			s := get(c.Service)
			s.total++
			if c.Running {
				s.up++
			}
			s.image = c.Image
			for _, p := range c.Exposed {
				s.ports[p] = true
			}
			for _, n := range c.Networks {
				s.nets = appendOnce(s.nets, n)
			}
		}
		raw := compose.RawSummary(a.Files)
		for _, n := range raw.Services {
			s := get(n)
			if s.image == "" {
				s.image = raw.Images[n]
			}
		}
		sort.Strings(names)
		for _, n := range names {
			s := by[n]
			var ps []int
			for p := range s.ports {
				ps = append(ps, p)
			}
			sort.Ints(ps)
			state := ui.Muted(ui.SymRing + " not created")
			switch {
			case s.up > 0:
				state = ui.OK(ui.SymDot) + " running"
			case s.total > 0:
				state = ui.Warn(ui.SymDot) + " stopped"
			}
			net := "not on " + firstNonEmpty(instanceNetwork(in), "a shared network") + " — attached when picked"
			shared := false
			for _, x := range in.Networks {
				shared = shared || contains(s.nets, x)
			}
			if at, ok := a.Attached[n]; ok {
				net = "on " + at.Network + " as " + at.Alias + " " + ui.SymOK
				if !contains(s.nets, at.Network) && s.up > 0 {
					net = at.Network + " " + ui.Err("detached") + " (DOCK-07)"
				}
			} else if shared {
				net = "on " + instanceNetwork(in) + " " + ui.SymOK
			}
			label := a.Name + "/" + n
			r := pickRow{Value: "app:" + label, Label: label, Hint: state + " · " + portsText(ps) + " · " + net, Ports: ps, Disabled: hostNetBlocked(in)}
			if s.total > 1 {
				r.Badge = strconv.Itoa(s.total) + " replicas" // LB-04: one name, N members
			}
			rows = append(rows, r)
		}
	}
	return rows
}
