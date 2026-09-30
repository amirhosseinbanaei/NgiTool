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
	{Group: "server", Label: "Doctor", Hint: "root, docker, compose v2, ss, state dir, updates", Args: []string{"doctor"}},
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
	items := itemsOf(id)
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
		_ = f.Value.Set(f.DefValue)
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
