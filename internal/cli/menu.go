package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

// Group is a heading in the help text and an entry in the root menu. Later
// prompts append their groups here, in the order they should appear.
type Group struct {
	ID    string // referenced by commands' "group" annotation and MenuItems
	Title string // help heading, upper case
	Label string // root-menu label
	Note  string // muted note after the heading / menu hint
}

var groups = []Group{
	{ID: "routes", Title: "ROUTES", Label: "Routes", Note: "hosts and paths to containers, services and ports"},
	{ID: "lb", Title: "LOAD BALANCING", Label: "Load balancing", Note: "pools, members, drain, blue/green, health"},
	{ID: "apps", Title: "APPS", Label: "Apps", Note: "compose projects: link, start, rebuild, fix"},
	{ID: "apply", Title: "APPLY", Label: "Apply & roll back", Note: "diff, apply, snapshots, nginx -t, reload"},
	{ID: "instances", Title: "INSTANCES", Label: "Instances", Note: "every nginx on this server: scan, list, inspect, adopt"},
	{ID: "server", Title: "SERVER", Label: "This server", Note: "check what this machine has"},
	{ID: "tool", Title: "NGITOOL", Label: "NgiTool itself", Note: "version, update, completion, uninstall"},
}

// MenuItem is one action inside a group's menu: it runs Args as if typed.
type MenuItem struct {
	Group string
	Label string
	Hint  string
	Args  []string
}

var menuItems = []MenuItem{
	{Group: "routes", Label: "Add a route", Hint: "hostname → containers, services or ports; two or more make a pool", Args: []string{"route", "add"}},
	{Group: "routes", Label: "List routes", Hint: "by instance and host, with the last probe", Args: []string{"route", "ls"}},
	{Group: "routes", Label: "Edit a route", Hint: "the wizard again, filled in", Args: []string{"route", "edit"}},
	{Group: "routes", Label: "Disable a route", Hint: "stop serving it, keep it in state", Args: []string{"route", "disable"}},
	{Group: "routes", Label: "Enable a route", Hint: "serve a disabled route again", Args: []string{"route", "enable"}},
	{Group: "routes", Label: "Remove a route", Hint: "shows what goes with it first", Args: []string{"route", "rm"}},
	{Group: "lb", Label: "List pools", Hint: "method, members, routes", Args: []string{"pool", "ls"}},
	{Group: "lb", Label: "Show a pool", Hint: "members with their last health check", Args: []string{"pool", "show"}},
	{Group: "lb", Label: "Check member health", Hint: "request every member from the instance's network", Args: []string{"pool", "check"}},
	{Group: "lb", Label: "Drain a member", Hint: "take it out of rotation for a deploy", Args: []string{"pool", "drain"}},
	{Group: "lb", Label: "Bring a member back", Hint: "undrain", Args: []string{"pool", "undrain"}},
	{Group: "lb", Label: "Blue/green switch", Hint: "make another set active, keep the old one as backup", Args: []string{"pool", "switch"}},
	{Group: "lb", Label: "Add members", Hint: "from the same checklist as route add", Args: []string{"pool", "member", "add"}},
	{Group: "lb", Label: "Remove a member", Hint: "", Args: []string{"pool", "member", "rm"}},
	{Group: "lb", Label: "Set a weight", Hint: "more traffic to bigger members", Args: []string{"pool", "member", "weight"}},
	{Group: "lb", Label: "Make a member a backup", Hint: "used only when the others fail", Args: []string{"pool", "member", "backup"}},
	{Group: "lb", Label: "Remove a pool", Hint: "and the routes that use it", Args: []string{"pool", "rm"}},
	{Group: "apps", Label: "Link", Hint: "pick compose projects from every one on this server", Args: []string{"app", "link"}},
	{Group: "apps", Label: "Scan", Hint: "the same checklist, several at once", Args: []string{"app", "scan"}},
	{Group: "apps", Label: "List", Hint: "state, network, routes of every linked app", Args: []string{"app", "ls"}},
	{Group: "apps", Label: "Fix detached", Hint: "recreate apps started without NgiTool's override (DOCK-07)", Args: []string{"app", "fix"}},
	{Group: "apply", Label: "Diff", Hint: "what apply would change, hand edits", Args: []string{"diff"}},
	{Group: "apply", Label: "Apply", Hint: "re-render from state", Args: []string{"apply"}},
	{Group: "apply", Label: "Roll back", Hint: "pick a snapshot", Args: []string{"rollback"}},
	{Group: "apply", Label: "Test", Hint: "nginx -t", Args: []string{"test"}},
	{Group: "apply", Label: "Reload", Hint: "nginx -t, then reload", Args: []string{"reload"}},
	{Group: "instances", Label: "Scan", Hint: "find every nginx and what NgiTool may do to it", Args: []string{"scan"}},
	{Group: "instances", Label: "List", Hint: "one line per instance", Args: []string{"instances"}},
	{Group: "instances", Label: "Inspect", Hint: "servers, locations and targets of one instance", Args: []string{"inspect"}},
	{Group: "instances", Label: "Adopt", Hint: "let NgiTool write to an instance", Args: []string{"instance", "adopt"}},
	{Group: "instances", Label: "Release", Hint: "remove every NgiTool file from an instance", Args: []string{"instance", "release"}},
	{Group: "instances", Label: "Externalize", Hint: "copy a baked-in config out of the image so it can be adopted (CONF-06)", Args: []string{"instance", "externalize"}},
	{Group: "server", Label: "Doctor", Hint: "root, docker, compose v2, ss, front door, configs, state dir, updates", Args: []string{"doctor"}},
	{Group: "tool", Label: "Update", Hint: "install the latest release", Args: []string{"update"}},
	{Group: "tool", Label: "Check for updates", Hint: "compare with the latest release", Args: []string{"update", "--check"}},
	{Group: "tool", Label: "Roll back", Hint: "restore the binary from before the last update", Args: []string{"update", "--rollback"}},
	{Group: "tool", Label: "Version", Hint: "build details", Args: []string{"version"}},
	{Group: "tool", Label: "Shell completion", Hint: "bash, zsh or fish", Args: []string{"completion"}},
	{Group: "tool", Label: "Uninstall", Hint: "remove the binary, optionally all data", Args: []string{"uninstall"}},
}

func itemsOf(group string) []MenuItem {
	var out []MenuItem
	for _, it := range menuItems {
		if it.Group == group {
			out = append(out, it)
		}
	}
	return out
}

// dynamicItems add entries computed when a group opens: one per linked
// app in Apps, between Scan and List.
var dynamicItems = map[string]func(e *env) []MenuItem{"apps": appMenuItems}

// groupItems are a group's static items with its dynamic ones after the
// first two.
func groupItems(e *env, group string) []MenuItem {
	items := itemsOf(group)
	if f := dynamicItems[group]; f != nil {
		if more := f(e); len(more) > 0 {
			at := min(2, len(items))
			items = append(append(append([]MenuItem{}, items[:at]...), more...), items[at:]...)
		}
	}
	return items
}

// runMenu is `ngitool` on a terminal: banner, then groups, then actions.
// Esc goes back one level; at the top it quits.
func runMenu(e *env) error {
	ui.Banner(version.Version, bannerStatus())
	for {
		opts := []ui.Option{}
		for _, g := range groups {
			if len(itemsOf(g.ID)) > 0 {
				opts = append(opts, ui.Option{Value: g.ID, Label: g.Label, Hint: g.Note})
			}
		}
		opts = append(opts, ui.Option{Value: "help", Label: "Help", Hint: "every command and flag"}, ui.Option{Value: "quit", Label: "Quit"})
		choice, err := ui.Select(ui.SelectOpts{Title: "What do you want to do?", Options: opts})
		switch {
		case errors.Is(err, ui.ErrBack):
			return nil
		case err != nil:
			return err
		case choice == "quit":
			return nil
		case choice == "help":
			if err := e.root.Help(); err != nil {
				return err
			}
			continue
		}
		if err := runGroup(e, choice); err != nil {
			return err
		}
	}
}

func runGroup(e *env, id string) error {
	var g Group
	for _, gg := range groups {
		if gg.ID == id {
			g = gg
		}
	}
	items := groupItems(e, id)
	opts := make([]ui.Option, len(items))
	for i, it := range items {
		opts[i] = ui.Option{Value: fmt.Sprint(i), Label: it.Label, Hint: it.Hint}
	}
	choice, err := ui.Select(ui.SelectOpts{Title: g.Label, Note: "esc goes back", Options: opts})
	if errors.Is(err, ui.ErrBack) {
		return nil
	}
	if err != nil {
		return err
	}
	var idx int
	fmt.Sscan(choice, &idx)
	err = runArgs(e.root, items[idx].Args)
	// A failed action is reported and the menu comes back; only Ctrl-C ends it.
	if errors.Is(err, ui.ErrInterrupted) {
		return err
	}
	if err != nil && !errors.Is(err, ui.ErrBack) {
		report(err, nil)
	}
	fmt.Fprintln(ui.Out)
	return nil
}

// runArgs runs a subcommand in-process with fresh flag values.
func runArgs(root *cobra.Command, args []string) error {
	cmd, rest, err := root.Find(args)
	if err != nil {
		return err
	}
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "yes" || f.Name == "force" || f.Name == "no-color" {
			return // global flags keep what was given on the command line
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil) // Set("[]") would append a literal "[]"
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	})
	if err := cmd.ParseFlags(rest); err != nil {
		return err
	}
	ctx := root.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.SetContext(ctx)
	return cmd.RunE(cmd, cmd.Flags().Args())
}

func bannerStatus() string {
	parts := ""
	if !ui.Narrow() {
		parts = ui.Muted("reverse proxy · load balancing") + "  "
	}
	if os.Geteuid() == 0 {
		parts += ui.OK(ui.SymDot) + " root"
	} else {
		parts += ui.Warn(ui.SymDot) + " not root " + ui.Muted("(partial view)")
	}
	if !execx.Has("docker") {
		parts += "  " + ui.Muted(ui.SymRing+" no docker")
	}
	return parts
}
