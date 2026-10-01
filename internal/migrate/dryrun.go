package migrate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
)

// Env is what the dry run needs from the machine.
type Env struct {
	// Report is the last scan; Instance the stack's nginx in it, when its
	// container exists. Without one the instance is built from the files.
	Report   *discover.Report
	Instance *discover.Instance
	// Version is the nginx version when no instance was scanned.
	Version string
	// Image is the stack's nginx image (compose.yaml); "" skips nginx -t.
	Image string
	Run   execx.Runner
	// SkipTest skips nginx -t (unit tests, no Docker).
	SkipTest bool
}

// Result is a finished dry run.
type Result struct {
	Plan     *Plan        `json:"plan"`
	Files    []string     `json:"files"`   // files NgiTool writes (nginx paths)
	Removed  []string     `json:"removed"` // legacy files it replaces or removes
	Test     string       `json:"test"`    // ok, failed, skipped
	TestOut  string       `json:"testOutput,omitempty"`
	Diffs    []Difference `json:"differences"`
	Blockers []Finding    `json:"blockers"`
	// Warnings are true today already and not changed by the migration:
	// another instance serving the same host (RP-04), a hand-written
	// server for it (RP-03).
	Warnings []Finding `json:"warnings"`
	Notes    model.Problems
}

// Safe reports the verdict.
func (r *Result) Safe() bool { return len(r.Blockers) == 0 }

// Mounts are the edge stack's nginx mounts with conf/ at confDir
// (compose.yaml).
func Mounts(dir, confDir string) []discover.Mount {
	l := edge.Layout{Dir: dir}
	return []discover.Mount{
		{Type: "bind", Source: confDir, Dest: edge.ConfIn},
		{Type: "bind", Source: l.WWW(), Dest: edge.WWWIn},
		{Type: "bind", Source: l.LE(), Dest: edge.LEIn},
		{Type: "bind", Source: l.Certs(), Dest: edge.CertsIn},
		{Type: "bind", Source: l.ACME(), Dest: edge.ACMEIn},
	}
}

func source(ms []discover.Mount) nginxconf.FileSource {
	var out []nginxconf.Mount
	for _, m := range ms {
		out = append(out, nginxconf.Mount{Source: m.Source, Dest: m.Dest})
	}
	return nginxconf.FileSource{Container: true, Mounts: out}
}

// Load reads an edge config whose conf/ is at confDir.
func Load(dir, confDir string) *nginxconf.Config {
	return nginxconf.Load(source(Mounts(dir, confDir)), edge.MainIn)
}

// Instance is the stack's nginx as rendering sees it, reading conf/ from
// confDir. Files the plan replaces count as NgiTool's own, so their
// servers do not block the routes that replace them (RP-03).
func Instance(e Env, p *Plan, confDir string) *discover.Instance {
	in := &discover.Instance{ID: p.Instance, Kind: discover.KindEdge, Name: "edge stack " + p.Dir, Conf: edge.MainIn,
		WorkingDir: p.Dir, State: discover.StateStopped, Version: e.Version, Image: e.Image}
	if e.Instance != nil {
		cp := *e.Instance
		in = &cp
	}
	in.Mounts = Mounts(p.Dir, confDir)
	cfg := nginxconf.Load(source(in.Mounts), edge.MainIn)
	sum := nginxconf.Summarize(cfg.Tree)
	in.Summary, in.Files = &sum, cfg.Files
	legacy := map[string]bool{}
	for _, rel := range p.Legacy {
		legacy[edge.ConfIn+"/"+strings.TrimPrefix(rel, "conf/")] = true
	}
	for i := range in.Files {
		if legacy[in.Files[i].Path] {
			in.Files[i].Managed = "ngitool"
		}
	}
	return in
}

// Report is a copy of rep with in in place of the scanned instance.
func Report(rep *discover.Report, in *discover.Instance) *discover.Report {
	out := &discover.Report{}
	if rep != nil {
		cp := *rep
		out = &cp
	}
	out.Instances = append([]discover.Instance{}, out.Instances...)
	for i := range out.Instances {
		if out.Instances[i].ID == in.ID {
			out.Instances[i] = *in
			return out
		}
	}
	out.Instances = append(out.Instances, *in)
	return out
}

// DryRun renders the plan into a scratch copy of conf/, tests it and
// compares it with the config running now. dir is never written to.
func DryRun(ctx context.Context, e Env, p *Plan) (*Result, error) {
	r := &Result{Plan: p, Test: "skipped"}
	scratch, err := os.MkdirTemp("", "ngitool-migrate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch)
	conf := filepath.Join(scratch, "conf")
	if err := copyTree(filepath.Join(p.Dir, "conf"), conf); err != nil {
		return nil, err
	}
	for _, rel := range p.Legacy {
		_ = os.Remove(filepath.Join(scratch, rel))
		r.Removed = append(r.Removed, edge.ConfIn+"/"+strings.TrimPrefix(rel, "conf/"))
	}
	in := Instance(e, p, conf)
	rep := Report(e.Report, in)
	in = rep.Find(in.ID)
	a, err := render.Plan(in)
	if err != nil {
		return nil, err
	}
	next := p.Next.Clone()
	if next.Adopted(in.ID) == nil {
		next.Instances = append(next.Instances, a)
	} else {
		a = *next.Adopted(in.ID)
	}
	f := render.GatherFacts(rep, in, &a)
	out, err := render.Render(next, f)
	if err != nil {
		r.Blockers = append(r.Blockers, Finding{Level: Blocker, Code: "MIG-01", Msg: "rendering failed: " + err.Error()})
		r.Blockers = append(r.Blockers, p.Blockers()...)
		return r, nil
	}
	r.Notes = out.Notes
	for _, file := range out.Files {
		hp, err := render.HostPath(in, file.Path, "")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(hp), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(hp, []byte(file.Content()), 0o644); err != nil {
			return nil, err
		}
		r.Files = append(r.Files, file.Path)
	}
	for _, d := range out.Dirs {
		if hp, err := render.HostPath(in, d, ""); err == nil {
			_ = os.MkdirAll(hp, 0o755)
		}
	}
	// Problems the model finds in the imported routes. A host another
	// nginx (or a hand-written server) serves is served that way today
	// too: the migration does not change it, so it is a warning.
	seen := map[string]bool{}
	for _, id := range p.Routes {
		rt := next.Route(id)
		if rt == nil {
			continue
		}
		ps := model.CheckRoute(next, rep, rt).Errors()
		if pl := next.Pool(rt.Pool); pl != nil {
			ps = append(ps, model.CheckPool(next, rep, pl).Errors()...)
		}
		for _, pr := range ps {
			f := Finding{Level: Blocker, Code: pr.Code, Msg: id + ": " + pr.Msg, Fix: pr.Fix}
			if pr.Code == "RP-03" || pr.Code == "RP-04" {
				f.Level, f.Fix = Warn, "true today already — the migration does not change it"
				f.Msg = id + ": " + strings.SplitN(pr.Msg, " (", 2)[0]
				if seen[f.Msg] {
					continue
				}
				seen[f.Msg] = true
				r.Warnings = append(r.Warnings, f)
				continue
			}
			r.Blockers = append(r.Blockers, f)
		}
	}
	// nginx -t with the stack's image and mounts, read-only, no network.
	switch {
	case e.SkipTest:
	case e.Image == "":
		r.Test, r.TestOut = "skipped", "no nginx image known for the stack"
	default:
		var ms []driver.Mount
		for _, m := range in.Mounts {
			ms = append(ms, driver.Mount{Type: m.Type, Source: m.Source, Dest: m.Dest})
		}
		run := e.Run
		if run == nil {
			run = execx.System
		}
		res, err := driver.Container{Run: run, Image: e.Image, Conf: edge.MainIn, Mounts: ms}.Test(ctx)
		switch {
		case err != nil:
			r.Test, r.TestOut = "failed", err.Error()
			r.Blockers = append(r.Blockers, Finding{Level: Blocker, Code: "APPLY-02", Msg: "nginx -t could not run: " + err.Error(), Fix: "docker pull " + e.Image})
		case !res.OK:
			r.Test, r.TestOut = "failed", res.Output
			r.Blockers = append(r.Blockers, Finding{Level: Blocker, Code: "APPLY-01", Msg: "nginx -t rejects the migrated config: " + firstLine(res.Output)})
		default:
			r.Test, r.TestOut = "ok", res.Output
		}
	}
	// The effective config, before and after.
	r.Diffs = Compare(Effective(Load(p.Dir, filepath.Join(p.Dir, "conf"))), Effective(Load(p.Dir, conf)))
	for _, d := range r.Diffs {
		r.Blockers = append(r.Blockers, Finding{Level: Blocker, Code: "MIG-01", Msg: d.Where + ": " + d.Was + " → " + d.Now, Fix: d.Reason})
	}
	r.Blockers = append(r.Blockers, p.Blockers()...)
	sort.SliceStable(r.Blockers, func(i, j int) bool { return r.Blockers[i].Code < r.Blockers[j].Code })
	return r, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// copyTree copies a directory tree (files, dirs, symlinks), keeping modes.
func copyTree(src, dst string) error {
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
			return os.MkdirAll(to, info.Mode().Perm()|0o700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, to)
		case info.Mode().IsRegular():
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
		}
		return nil
	})
}

// ErrNoStack is a directory without edge.json and apps/: nothing to import.
var ErrNoStack = errors.New("nothing to migrate: no edge.json and no apps/")
