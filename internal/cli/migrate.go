package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/migrate"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

func migrateCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:         "migrate",
		Short:       "import an nginx-edge directory (the legacy edge CLI) into NgiTool",
		Long:        "edge <dir>  read .env, edge.json, apps/ and the token file; show the mapping; with --dry-run render the\n            new files into a scratch copy, nginx -t it and compare the effective config with the running one",
		Annotations: map[string]string{annGroup: "server", annSynopsis: "migrate edge <dir> [--dry-run]"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(migrateEdgeCmd(e))
	return c
}

type migrateOpts struct {
	txFlags
	asJSON bool
}

func migrateEdgeCmd(e *env) *cobra.Command {
	o := &migrateOpts{}
	c := &cobra.Command{
		Use:   "edge <dir>",
		Short: "import an nginx-edge directory; --dry-run proves the result is equivalent",
		Long: "Maps edge.json's sites and paths to routes (app → a service of a linked app, container, port → host\n" +
			"port, static → www folder), registers its certificates with their files (nothing is reissued), keeps the\n" +
			"domains' Cloudflare zones, links apps/ with the same compose files and moves each edge.override.yaml to\n" +
			"/var/lib/ngitool/overrides/. Hand-written files stay. The token is used in place, never copied.\n\n" +
			"--dry-run renders NgiTool's files into a scratch copy of conf/, runs nginx -t on it with the stack's image\n" +
			"and mounts, compares servers, locations, upstream targets and certificate paths with the current config,\n" +
			"and ends with \"safe to migrate\" or \"N blockers\". The real run backs up conf/ and edge.json, rewrites the\n" +
			"legacy files with NgiTool's markers in one transaction (nginx -t, reload, probe of every route).",
		Example:     "ngitool migrate edge /srv/nginx-edge --dry-run\nngitool migrate edge /srv/nginx-edge",
		Annotations: map[string]string{annSynopsis: "edge <dir> [--dry-run] [--json]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				if err := ui.Need("directory", "<dir>"); err != nil {
					return err
				}
				v, err := ui.Input(ui.InputOpts{Title: "The nginx-edge directory", Placeholder: "/srv/nginx-edge", Validate: func(v string) error { return edge.Check(v) }})
				if err != nil {
					return err
				}
				args = []string{v}
			}
			return e.migrateEdge(cmd, o, args[0])
		},
	}
	c.Flags().BoolVar(&o.asJSON, "json", false, "print the mapping and the dry run as JSON")
	o.txFlags.add(c)
	return c
}

func (e *env) migrateEdge(cmd *cobra.Command, o *migrateOpts, dir string) error {
	ctx := cmd.Context()
	dir, _ = filepath.Abs(dir)
	l, err := migrate.Read(dir)
	if err != nil {
		return err
	}
	rep := e.freshReport(ctx, true)
	id := model.EdgeInstanceID(dir)
	in := rep.Find(id)
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	ctrs, _ := compose.Containers(ctx, edgeRunner)
	projectOf := func(wd string) string {
		for _, c := range ctrs {
			if c.WorkDir == wd {
				return c.Project
			}
		}
		return ""
	}
	project := firstNonEmpty(projectOf(dir), edge.DefaultProject)
	if in != nil && in.Project != "" {
		project = in.Project
	}
	opts := migrate.Options{Instance: id, Project: project, OverrideDir: e.paths.Overrides, ProjectOf: projectOf, Now: time.Now()}
	p := migrate.Map(l, st, opts)
	env := migrate.Env{Report: rep, Instance: in, Image: edge.Image(dir), Run: edgeRunner}
	if in != nil {
		env.Version = in.Version
	} else {
		env.Version = imageVersion(ctx, env.Image)
	}
	var res *migrate.Result
	if o.asJSON {
		// --json prints only JSON (docs/ux.md §6): no task line.
		if res, err = migrate.DryRun(ctx, env, p); err != nil {
			return err
		}
		if err := printJSON(res); err != nil {
			return err
		}
		if !res.Safe() {
			return &ExitError{Code: ExitFail}
		}
		return nil
	}
	err = ui.Task("Rendering into a scratch copy, nginx -t, comparing", func(*ui.TaskCtl) error {
		var derr error
		res, derr = migrate.DryRun(ctx, env, p)
		return derr
	})
	if err != nil {
		return err
	}
	printMigration(dir, p, res)
	if o.dryRun {
		if !res.Safe() {
			return &ExitError{Code: ExitFail}
		}
		return nil
	}

	// The real run.
	var stop []migrate.Finding
	for _, b := range res.Blockers {
		if b.Code == "MIG-02" && o.onDrift == apply.DriftOverwrite {
			continue
		}
		stop = append(stop, b)
	}
	if len(stop) > 0 {
		return fmt.Errorf("%d %s — nothing was changed\n%s fix them, then run the dry run again: ngitool migrate edge %s --dry-run",
			len(stop), either(len(stop), "blocker", "blockers"), ui.SymArrow, dir)
	}
	if in == nil {
		return errors.New("the stack's nginx container was not found — start it first (docker compose up -d in " + dir + "), then migrate")
	}
	backup := filepath.Join(e.paths.Backups, "migrate-"+time.Now().UTC().Format("20060102T150405Z"))
	ui.Explain("writes NgiTool's files over the legacy ones ("+strconv.Itoa(len(p.Legacy))+" files), registers the routes, certificates, domains and apps in state.json, moves the app overrides to "+e.paths.Overrides,
		"nginx of "+dir+" (tested, reloaded, every route probed); hand-written files and the token stay; edge.json is renamed edge.json.migrated",
		"ngitool rollback "+id+" puts the old files back; a full copy of conf/ and edge.json is kept in "+backup)
	if ok, err := ui.Sure(e.yes, "Migrate "+dir+" to NgiTool?", ""); err != nil || !ok {
		if err == nil {
			err = errCancelled
		}
		return err
	}
	if err := backupDir(dir, backup); err != nil {
		return fmt.Errorf("backup failed, nothing was changed: %w", err)
	}
	ui.Done("Backup " + ui.Muted(backup))
	// The transaction sees the legacy files as NgiTool's to replace.
	conf := filepath.Join(dir, "conf")
	pin := migrate.Instance(migrate.Env{Instance: in}, p, conf)
	prep := migrate.Report(rep, pin)
	pin = prep.Find(pin.ID)
	var legacy []string
	for _, rel := range p.Legacy {
		legacy = append(legacy, filepath.Join(dir, filepath.FromSlash(rel)))
	}
	a, err := render.Plan(pin)
	if err != nil {
		return err
	}
	a.At = model.Now(time.Now())
	_, err = e.change(ctx, prep, pin, o.txFlags, "migrate edge "+dir, p.Routes, func(next *model.State) error {
		q := migrate.Map(l, next, opts)
		*next = *q.Next
		if next.Adopted(id) == nil {
			next.Instances = append(next.Instances, a)
		}
		return nil
	}, func(c *apply.Change) { c.Legacy = legacy })
	if err != nil {
		return err
	}
	// After nginx took it: overrides, the marker, edge.json out of the way.
	st, _ = model.Load(e.paths)
	for _, name := range p.Apps {
		if a := st.App(name); a != nil && a.NeedsOverride() {
			if err := state.WriteFile(a.Override, []byte(compose.RenderOverride(*a)), state.FileMode); err != nil {
				ui.Warning("override of " + name + ": " + err.Error())
			}
		}
	}
	_ = edge.WriteMarker(dir)
	if _, err := os.Stat(filepath.Join(dir, "edge.json")); err == nil {
		_ = os.Rename(filepath.Join(dir, "edge.json"), filepath.Join(dir, "edge.json.migrated"))
	}
	ui.Done("Migrated " + dir + ui.Muted(" — NgiTool manages it now"))
	for _, f := range p.Findings {
		if f.Code == "MIG-04" {
			ui.Hint(ui.SymArrow + " " + f.Msg)
		}
	}
	if _, err := os.Lstat("/usr/local/bin/edge"); err == nil {
		ui.Hint(ui.SymArrow + " the old edge command is still installed: rm /usr/local/bin/edge (see docs/migration.md)")
	}
	return nil
}

// imageVersion asks the image for its nginx version when no container
// was scanned ("" when Docker cannot tell).
func imageVersion(ctx context.Context, image string) string {
	if image == "" {
		return ""
	}
	res := edgeRunner.Run(ctx, "docker", []string{"run", "--rm", "--pull", "never", "--network", "none", "--entrypoint", "nginx", image, "-v"}, execx.Opts{Timeout: 30 * time.Second})
	out := strings.TrimSpace(res.Stderr + res.Stdout)
	if i := strings.Index(out, "nginx/"); i >= 0 {
		return strings.Fields(out[i:])[0]
	}
	return ""
}

func printMigration(dir string, p *migrate.Plan, r *migrate.Result) {
	ui.Heading("Migration", dir)
	var rows [][]string
	for _, x := range p.Rows {
		rows = append(rows, []string{ui.Muted(x.Kind), x.Legacy, x.NgiTool, ui.Muted(x.Note)})
	}
	ui.PrintTable(rows, ui.TableOpts{Indent: 2, Header: []string{"KIND", "NGINX-EDGE", "NGITOOL", "NOTE"}})
	for _, f := range p.Findings {
		if f.Level == migrate.Blocker {
			continue // listed with the blockers below
		}
		msg := f.Msg + " " + ui.Muted("("+f.Code+")")
		if f.Level == migrate.Warn {
			ui.Warning(msg)
		} else {
			ui.Info(msg)
		}
		if f.Fix != "" {
			ui.Hint(ui.SymArrow + " " + f.Fix)
		}
	}
	ui.Plan("Dry run", [][2]string{
		{"Writes", strconv.Itoa(len(r.Files)) + " files" + ui.Muted("  sites/<host>.conf, ngitool/upstreams/, conf.d/ngitool.conf, snippets/ngitool-proxy.conf")},
		{"Replaces", strconv.Itoa(len(r.Removed)) + " legacy files" + ui.Muted("  (# Managed by edge)")},
		{"Leaves", strconv.Itoa(len(p.Unmanaged)) + " hand-written files"},
		{"nginx -t", testWord(r.Test)},
		{"Differences", strconv.Itoa(len(r.Diffs))},
	})
	if r.Test == "failed" && r.TestOut != "" {
		for _, l := range strings.Split(r.TestOut, "\n") {
			ui.Plain("    " + ui.Muted(l))
		}
	}
	for _, d := range r.Diffs {
		ui.Plain("  " + ui.Warn(ui.SymWarn) + " " + ui.Bold(d.Where))
		ui.Plain("      " + ui.Muted("now:  ") + d.Was)
		ui.Plain("      " + ui.Muted("then: ") + d.Now)
		ui.Plain("      " + ui.Muted("why:  "+d.Reason))
	}
	for _, w := range r.Warnings {
		ui.Warning(w.Msg + " " + ui.Muted("("+w.Code+")"))
		ui.Hint(ui.SymArrow + " " + w.Fix)
	}
	for _, b := range r.Blockers {
		if b.Code == "MIG-01" && strings.Contains(b.Msg, " → ") {
			continue // the differences above
		}
		ui.Plain("  " + ui.Err(ui.SymErr) + " " + b.Msg + " " + ui.Muted("("+b.Code+")"))
		if b.Fix != "" {
			ui.Hint(ui.SymArrow + " " + b.Fix)
		}
	}
	ui.Plain("")
	if r.Safe() {
		ui.Done(ui.Bold("safe to migrate") + ui.Muted(" — the effective config is the same; nginx -t "+r.Test))
	} else {
		ui.Fail(ui.Bold(strconv.Itoa(len(r.Blockers)) + " " + either(len(r.Blockers), "blocker", "blockers")))
	}
}

func testWord(s string) string {
	switch s {
	case "ok":
		return ui.OK("ok")
	case "failed":
		return ui.Err("failed")
	}
	return ui.Muted(s)
}

// backupDir copies conf/ and edge.json (and apps/ with its overrides) of
// dir into dst before anything changes.
func backupDir(dir, dst string) error {
	if err := os.MkdirAll(dst, state.DirMode); err != nil {
		return err
	}
	for _, name := range []string{"conf", "apps", "edge.json", ".env"} {
		src := filepath.Join(dir, name)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if err := copyPath(src, filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	return nil
}

func copyPath(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, to)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
