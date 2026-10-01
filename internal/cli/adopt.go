package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

func instanceCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:         "instance",
		Short:       "adopt an nginx so NgiTool may write to it, or release it",
		Long:        "adopt   once per instance, before NgiTool writes there: creates its directories and the one include\nrelease removes every NgiTool file and the include line again",
		Annotations: map[string]string{annGroup: "instances", annSynopsis: "instance adopt|release <id>"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(adoptCmd(e), releaseCmd(e))
	return c
}

func adoptCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:         "adopt [id]",
		Short:       "let NgiTool write to an instance",
		Long:        "Shows the plan — the directories, the one include, and whether conf.d/*.conf already provides it — then\nwrites the entry file through the usual transaction (nginx -t, reload). If the include has to go into a\nhand-written file, that one line gets its own confirm and a backup; `instance release` takes it out again.",
		Annotations: map[string]string{annSynopsis: "adopt <id> [--dry-run]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rep := e.freshReport(ctx, false)
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			var in *discover.Instance
			if len(args) == 1 {
				if in = lookupInstance(rep, args[0]); in == nil {
					return &UsageError{Msg: "no instance " + args[0] + " — see: ngitool instances", Cmd: "ngitool instance adopt"}
				}
			} else {
				if err := ui.Need("instance", "<id>"); err != nil {
					return err
				}
				if in, err = pickAdoptable(rep, st); err != nil {
					return err
				}
			}
			return e.adopt(cmd, rep, st, in, f)
		},
	}
	f.add(c)
	return c
}

// pickAdoptable asks for a writable instance that is not adopted yet.
func pickAdoptable(rep *discover.Report, st *model.State) (*discover.Instance, error) {
	var opts []ui.Option
	for i := range rep.Instances {
		in := &rep.Instances[i]
		o := instanceOption(in)
		switch {
		case st.Adopted(in.ID) != nil:
			o.Disabled = "already adopted"
		case !in.Caps.Write.OK:
			o.Disabled = in.Caps.Write.Reason
		}
		opts = append(opts, o)
	}
	if len(opts) == 0 {
		return nil, errors.New("no nginx found — run: ngitool scan")
	}
	id, err := ui.Select(ui.SelectOpts{Title: "Which nginx should NgiTool manage?", Options: opts})
	if err != nil {
		return nil, err
	}
	return rep.Find(id), nil
}

// adopt plans and applies the adoption of one instance (4.4).
func (e *env) adopt(cmd *cobra.Command, rep *discover.Report, st *model.State, in *discover.Instance, f txFlags) error {
	if st.Adopted(in.ID) != nil {
		ui.Info(in.Name + " is already adopted")
		return nil
	}
	if m, ok := strings.CutPrefix(in.ManagedBy, "other:"); ok {
		return fmt.Errorf("%s is managed by %s, which regenerates its files — NgiTool never writes there %s", in.Name, m, ui.Muted("(DISC-11)"))
	}
	if !in.Caps.Write.OK {
		ui.Fail(in.Name + " cannot be written: " + in.Caps.Write.Reason)
		if strings.Contains(in.Caps.Write.Reason, "CONF-06") {
			ui.Hint("What it needs: the config on the host, mounted into the container. For example:")
			ui.Hint("  docker cp " + firstNonEmpty(in.Container, in.Name) + ":/etc/nginx /srv/" + in.Name + "-nginx")
			ui.Hint("  then mount it: volumes: [\"/srv/" + in.Name + "-nginx:/etc/nginx\"] and recreate the container")
			ui.Hint("Prompt 4 writes that compose override for you (CONF-06).")
		}
		return &ExitError{Code: ExitFail}
	}
	a, err := render.Plan(in)
	if err != nil {
		return err
	}
	a.At = model.Now(time.Now())
	h := in.Summary.Hook
	var inc *apply.IncludeEdit
	pairs := [][2]string{
		{"Instance", in.Name + ui.Muted("  "+in.ID)},
		{"Layout", a.Layout + ui.Muted("  "+layoutNote(a.Layout))},
		{"Directory", a.Root + ui.Muted("  on the host: "+a.HostRoot)},
	}
	if a.Layout == model.LayoutEdge {
		pairs = append(pairs, [2]string{"Files", "sites/<host>.conf, locations/<host>/, ngitool/upstreams/, conf.d/ngitool.conf, snippets/ngitool-proxy.conf"})
	} else {
		pairs = append(pairs, [2]string{"Creates", "upstreams/, servers/, locations/, proxy.conf under " + a.Root})
	}
	if h.NeedsLine {
		l := render.LayoutOf(in, &a)
		file, err := render.HostPath(in, h.Pos.File, "")
		if err != nil {
			return fmt.Errorf("%s is inside the image: %w", h.Pos.File, err)
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		line := render.IncludeLine(l)
		content, at, err := insertAfterBrace(string(b), h.Pos.Line, line)
		if err != nil {
			return fmt.Errorf("%s:%d: %w", h.Pos.File, h.Pos.Line, err)
		}
		backup := filepath.Join(e.paths.Backups, apply.SafeName(in.ID), "adopt-include", filepath.Base(file)+".orig")
		a.Include = &model.Include{File: file, Line: at, Text: line, Backup: backup}
		inc = &apply.IncludeEdit{File: file, Content: content}
		pairs = append(pairs, [2]string{"Include", ui.Warn("needs one line in a hand-written file") + ui.Muted("  "+h.Pos.File+":"+strconv.Itoa(at))})
	} else {
		pairs = append(pairs, [2]string{"Include", ui.OK("already there") + ui.Muted("  "+h.Existing+" ("+h.Pos.String()+") picks up "+render.LayoutOf(in, &a).Entry)})
	}
	if in.Caps.Write.Note != "" {
		pairs = append(pairs, [2]string{"Note", ui.Muted(in.Caps.Write.Note)})
	}
	ui.Plan("Adopt "+in.Name, pairs)
	if inc != nil && !f.dryRun {
		ui.Explain(
			"adds this line inside http {} of "+h.Pos.File+" (line "+strconv.Itoa(a.Include.Line)+"):\n"+strings.TrimSpace(a.Include.Text),
			"the only edit NgiTool ever makes to a file it did not write; a copy is kept at "+a.Include.Backup,
			"ngitool instance release "+in.ID)
		ok, err := ui.Sure(e.yes, "Add the include line to "+filepath.Base(h.Pos.File)+"?", "")
		if err != nil {
			return err
		}
		if !ok {
			return errCancelled
		}
		if err := state.WriteFile(a.Include.Backup, []byte(mustRead(a.Include.File)), state.FileMode); err != nil {
			return fmt.Errorf("backup of %s: %w", a.Include.File, err)
		}
	}
	_, err = e.change(cmd.Context(), rep, in, f, "instance adopt "+in.ID, nil, func(next *model.State) error {
		if next.Adopted(in.ID) != nil {
			return errors.New(in.Name + " was adopted meanwhile")
		}
		next.Instances = append(next.Instances, a)
		return nil
	}, func(c *apply.Change) { c.Include = inc })
	if err == nil && !f.dryRun {
		ui.Hint("next: ngitool route add <host> --instance " + in.ID)
	}
	return err
}

func mustRead(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func layoutNote(l string) string {
	switch l {
	case model.LayoutEdge:
		return "the edge stack's own conf/sites and conf/locations; edge CLI files are left alone"
	case model.LayoutHost:
		return "files under /etc/nginx/ngitool, one managed file in conf.d"
	}
	return "a ngitool/ subdir inside the bind-mounted conf dir"
}

// insertAfterBrace puts line after the line holding the `{` of the
// directive starting at line n (1-based). It returns the line number the
// new line got.
func insertAfterBrace(content string, n int, line string) (string, int, error) {
	lines := strings.SplitAfter(content, "\n")
	for i := n - 1; i < len(lines) && i < n+5; i++ {
		if i >= 0 && strings.Contains(stripComment(lines[i]), "{") {
			nl := "\n"
			if strings.HasSuffix(lines[i], "\r\n") {
				nl = "\r\n" // keep CRLF files CRLF (CONF-10)
			}
			if !strings.HasSuffix(lines[i], "\n") {
				lines[i] += nl
			}
			out := strings.Join(lines[:i+1], "") + line + nl + strings.Join(lines[i+1:], "")
			return out, i + 2, nil
		}
	}
	return "", 0, errors.New("could not find the { of http")
}

func stripComment(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

func releaseCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:         "release <id>",
		Short:       "remove every NgiTool file from an instance",
		Long:        "Removes the routes and pools of the instance from state, deletes every file NgiTool wrote there, and takes\nout the include line adopt added to a hand-written file. Shows what goes first.",
		Annotations: map[string]string{annSynopsis: "release <id> [--dry-run]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rep := e.freshReport(ctx, true)
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			in, err := e.instanceFor(rep, st, id, true)
			if err != nil {
				return err
			}
			a := st.Adopted(in.ID)
			routes, pools := st.RoutesOn(in.ID), st.PoolsOn(in.ID)
			pairs := [][2]string{{"Instance", in.Name}}
			for _, r := range routes {
				pairs = append(pairs, [2]string{"Route", r.ID + ui.Muted(" → "+model.Describe(st.Pool(r.Pool)))})
			}
			for _, p := range pools {
				pairs = append(pairs, [2]string{"Pool", p.Name})
			}
			if a.Include != nil {
				pairs = append(pairs, [2]string{"Include", "line " + strconv.Itoa(a.Include.Line) + " of " + a.Include.File + " is taken out"})
			}
			ui.Plan("Release "+in.Name, pairs)
			var inc *apply.IncludeEdit
			if a.Include != nil {
				cur := mustRead(a.Include.File)
				out, ok := removeLine(cur, a.Include.Text)
				if !ok {
					ui.Warning("the include line is no longer in " + a.Include.File + " — nothing to take out")
				} else {
					inc = &apply.IncludeEdit{File: a.Include.File, Content: out}
				}
			}
			if len(routes) > 0 && !f.dryRun {
				ui.Explain("removes "+strconv.Itoa(len(routes))+" routes and their pools from "+in.Name, "those hosts stop being served by it", "ngitool rollback "+in.ID+" restores the files and state")
			}
			_, err = e.change(ctx, rep, in, f, "instance release "+in.ID, nil, func(next *model.State) error {
				for _, r := range next.RoutesOn(in.ID) {
					next.RemoveRoute(r.ID)
				}
				for _, p := range next.PoolsOn(in.ID) {
					next.RemovePool(p.Name)
				}
				out := next.Instances[:0]
				for _, x := range next.Instances {
					if x.ID != in.ID {
						out = append(out, x)
					}
				}
				next.Instances = out
				return nil
			}, func(c *apply.Change) { c.Release, c.Include = true, inc })
			return err
		},
	}
	f.add(c)
	return c
}

// removeLine drops the first line equal to text (ignoring the line end).
func removeLine(content, text string) (string, bool) {
	lines := strings.SplitAfter(content, "\n")
	for i, l := range lines {
		if strings.TrimRight(l, "\r\n") == text {
			return strings.Join(append(lines[:i:i], lines[i+1:]...), ""), true
		}
	}
	return content, false
}
