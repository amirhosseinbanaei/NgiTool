package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// confRoot is the directory externalize copies out and mounts back.
const confRoot = "/etc/nginx"

// externalizeCmd is CONF-06's opt-in: an nginx inside a compose project
// whose config is baked into the image gets that config copied to the host
// and bind-mounted back through the app's override (the same writer that
// attaches services), so it becomes writable and adoptable.
func externalizeCmd(e *env) *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use:   "externalize [id]",
		Short: "copy a config baked into an image to the host and mount it (CONF-06)",
		Long: "For an nginx service of a compose project with no bind mount over " + confRoot + ":\n" +
			"  1. docker cp <container>:" + confRoot + " → /var/lib/ngitool/externalized/<app>/\n" +
			"  2. the app's override bind-mounts it over " + confRoot + " (writable: images that write their\n" +
			"     config at start, like the official image's templates/, keep working)\n" +
			"  3. the service is recreated (up -d --no-deps) on confirm\n" +
			"The project is linked first when it is not yet. Afterwards `ngitool instance adopt <id>` works.",
		Annotations: map[string]string{annSynopsis: "externalize <id>"},
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
					return &UsageError{Msg: "no instance " + args[0] + " — see: ngitool instances"}
				}
			} else {
				var opts []ui.Option
				for i := range rep.Instances {
					x := &rep.Instances[i]
					if baked(x) {
						opts = append(opts, instanceOption(x))
					}
				}
				if len(opts) == 0 {
					ui.Info("no compose nginx has its config baked into its image")
					return nil
				}
				if err := ui.Need("instance", "<id>"); err != nil {
					return err
				}
				id, err := ui.Select(ui.SelectOpts{Title: "Externalize which nginx?", Options: opts})
				if err != nil {
					return err
				}
				in = rep.Find(id)
			}
			if !baked(in) {
				switch {
				case in.Kind != discover.KindCompose:
					return errors.New(in.Name + " is not a compose service: copy its config out and mount it with docker run -v (CONF-06)")
				case in.Container == "":
					return errors.New(in.Name + " was never started: there is no container to copy the config from — start it once: docker compose up -d " + in.Service)
				}
				return errors.New(in.Name + "'s config is not baked into its image: " + firstNonEmpty(in.Caps.Write.Reason, "it is writable already"))
			}
			app := st.AppOfProject(in.Project)
			link := app == nil
			if link {
				files := in.ComposeFile
				if len(files) == 0 || in.WorkingDir == "" {
					return errors.New(in.Name + " has no compose labels to link its project from — link it first: ngitool app link <dir>")
				}
				name := compose.UniqueName(in.Project, func(n string) bool { return st.App(n) != nil })
				app = &compose.App{Name: name, Project: in.Project, WorkingDir: in.WorkingDir, Files: files, Owner: compose.OwnerOf(in.WorkingDir), LinkedAt: stamp()}
			}
			for svc := range app.Mounts {
				if svc != in.Service {
					return errors.New(app.Name + " already has " + svc + " externalized to " + filepath.Join(e.paths.Externals, app.Name))
				}
			}
			dest := filepath.Join(e.paths.Externals, app.Name)
			pairs := [][2]string{{"Instance", in.Name + ui.Muted("  "+in.ID)}}
			if link {
				pairs = append(pairs, [2]string{"Link app", app.Name + ui.Muted("  "+app.WorkingDir+" · "+strings.Join(app.Files, ", "))})
			}
			pairs = append(pairs,
				[2]string{"Copy", in.Container + ":" + confRoot + " → " + dest},
				[2]string{"Override", compose.OverridePath(e.paths.Overrides, app.Name) + ui.Muted("  bind "+dest+" → "+confRoot)},
				[2]string{"Recreate", in.Service + ui.Muted("  up -d --no-deps")})
			ui.Plan("Externalize "+in.Name, pairs)
			ui.Explain("Copies the config out of the image and mounts it back, so NgiTool can write it from the host; the image is unchanged",
				in.Service+" is recreated: a few seconds of downtime; afterwards its config lives in "+dest,
				"remove the mount from "+compose.OverridePath(e.paths.Overrides, app.Name)+" and recreate: the image's config comes back")
			if ok, err := ui.Sure(e.yes, "Externalize "+in.Name+"?", ""); err != nil || !ok {
				if err == nil {
					err = errCancelled
				}
				return err
			}
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			if err := ui.Task("Copying "+confRoot+" out of "+in.Container, func(*ui.TaskCtl) error {
				res := appRunner.Run(ctx, "docker", []string{"cp", in.Container + ":" + confRoot + "/.", dest}, execx.Opts{Timeout: 60 * time.Second})
				if res.Code != 0 {
					return errors.New("docker cp failed\n" + execx.Tail(res, 3))
				}
				return nil
			}); err != nil {
				return err
			}
			if link {
				if err := e.withLock(ctx, true, func() error {
					cur, err := model.Load(e.paths)
					if err != nil {
						return err
					}
					cur.Apps = append(cur.Apps, *app)
					return model.Save(e.paths, cur)
				}); err != nil {
					return err
				}
				ui.Done("Linked " + app.Name)
			}
			updated, err := e.updateApp(ctx, app.Name, func(a *compose.App) error {
				if a.Mounts == nil {
					a.Mounts = map[string][]compose.Mount{}
				}
				a.Mounts[in.Service] = []compose.Mount{{Source: dest, Target: confRoot}}
				return nil
			})
			if err != nil {
				return err
			}
			ui.Done("Wrote " + updated.Override)
			b, err := e.composeBin(ctx)
			if err != nil {
				return err
			}
			steps := compose.Steps(compose.Up, compose.Opts{NoDeps: true}, []string{in.Service})
			printWillRun(b, updated.Compose(), steps)
			if err := e.runSteps(ctx, b, *updated, "externalize", steps, []string{in.Service}, verbose); err != nil {
				return err
			}
			ui.Hint(ui.SymArrow + " now NgiTool may write to it: ngitool instance adopt " + in.ID)
			return nil
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "raw compose output")
	return c
}

// baked reports whether an instance is a created compose service whose
// config is inside its image (CONF-06).
func baked(in *discover.Instance) bool {
	return in.Kind == discover.KindCompose && in.Container != "" && !in.Caps.Write.OK && strings.Contains(in.Caps.Write.Reason, "CONF-06")
}
