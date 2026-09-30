package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// Commands describe themselves for the help page with two annotations:
//
//	group     a Group.ID from menu.go
//	synopsis  the usage shown in the list, e.g. "update [--check]"
//	usage     the full usage line of `help <command>` (default: synopsis)
const (
	annGroup    = "group"
	annSynopsis = "synopsis"
	annUsage    = "usage"
)

func installHelp(root *cobra.Command) {
	cobra.AddTemplateFunc("ngitoolHelp", renderHelp)
	root.SetHelpTemplate(`{{ngitoolHelp .}}`)
	root.SetUsageTemplate(`{{ngitoolHelp .}}`)
	root.SetHelpCommand(&cobra.Command{
		Use:   "help [command]",
		Short: "help for a command",
		RunE: func(c *cobra.Command, args []string) error {
			target, _, err := c.Root().Find(args)
			if err != nil || target == nil {
				return &UsageError{Msg: fmt.Sprintf("unknown command %q", strings.Join(args, " ")), Cmd: "ngitool"}
			}
			return target.Help()
		},
	})
}

// renderHelp is the whole help page for cmd, in the layout of the legacy
// CLI: USAGE, then groups with a muted note, then FLAGS.
func renderHelp(cmd *cobra.Command) string {
	if cmd.HasParent() {
		return commandHelp(cmd)
	}
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	title := ui.AccentBold("ngitool") + " — " + cmd.Short
	if !ui.Narrow() {
		title += "  " + ui.Muted("(NgiTool · reverse proxy and load balancing)")
	}
	w(title)
	w("")
	w(ui.Bold("USAGE"))
	rows := [][]string{
		{"ngitool", ui.Muted("interactive menu")},
		{"ngitool <command> [args] [flags]", ui.Muted("run one thing and exit")},
	}
	b.WriteString(listing(rows))

	// One left column for every group, so the notes line up down the page.
	left := 0
	for _, c := range cmd.Commands() {
		if !c.Hidden && c.Annotations[annGroup] != "" {
			left = max(left, len("ngitool "+synopsis(c)))
		}
	}
	for _, g := range groups {
		var rows [][]string
		for _, c := range cmd.Commands() {
			if c.Hidden || c.Annotations[annGroup] != g.ID {
				continue
			}
			rows = append(rows, []string{ui.Pad("ngitool "+synopsis(c), left), ui.Muted(c.Short)})
		}
		if len(rows) == 0 {
			continue
		}
		w("")
		head := ui.Bold(g.Title)
		if g.Note != "" {
			head += "  " + ui.Muted(g.Note)
		}
		w(head)
		b.WriteString(listing(rows))
	}
	w("")
	w(ui.Bold("FLAGS"))
	b.WriteString(flagTable(cmd.PersistentFlags()))
	w("")
	w(ui.Muted("Run ") + "ngitool help <command>" + ui.Muted(" for its flags. Every prompt has a flag equivalent."))
	return b.String()
}

func commandHelp(cmd *cobra.Command) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + "\n") }
	w(ui.AccentBold(cmd.CommandPath()) + " — " + cmd.Short)
	w("")
	w(ui.Bold("USAGE"))
	syn := cmd.Annotations[annUsage]
	if syn == "" {
		syn = cmd.Annotations[annSynopsis]
	}
	if syn == "" {
		syn = cmd.Use
	}
	w("  " + strings.TrimSpace(strings.TrimSuffix(cmd.CommandPath(), cmd.Name())) + " " + syn)
	if cmd.Long != "" {
		w("")
		for _, l := range strings.Split(strings.TrimSpace(cmd.Long), "\n") {
			w(strings.TrimRight("  "+l, " "))
		}
	}
	if cmd.HasAvailableLocalFlags() {
		w("")
		w(ui.Bold("FLAGS"))
		b.WriteString(flagTable(cmd.LocalNonPersistentFlags()))
	}
	w("")
	w(ui.Bold("GLOBAL FLAGS"))
	b.WriteString(flagTable(cmd.Root().PersistentFlags()))
	if cmd.Example != "" {
		w("")
		w(ui.Bold("EXAMPLES"))
		for _, l := range strings.Split(strings.TrimSpace(cmd.Example), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "#") {
				w("  " + ui.Muted(strings.TrimSpace(l)))
			} else {
				w("  " + strings.TrimSpace(l))
			}
		}
	}
	return b.String()
}

func flagTable(fs *pflag.FlagSet) string {
	var rows [][]string
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		name := "    --" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", --" + f.Name
		}
		varname, usage := pflag.UnquoteUsage(f)
		if varname != "" {
			name += " " + varname
		}
		rows = append(rows, []string{ui.Key(name), ui.Muted(usage)})
	})
	rows = append(rows, []string{ui.Key("-h, --help"), ui.Muted("this text")})
	return listing(rows)
}

func synopsis(c *cobra.Command) string {
	if s := c.Annotations[annSynopsis]; s != "" {
		return s
	}
	return c.Name()
}

// listing lays out [left, note] rows: two columns, or on a narrow terminal
// the note on its own line below, so nothing is cut.
func listing(rows [][]string) string {
	if !ui.Narrow() {
		return strings.Join(ui.Table(rows, ui.TableOpts{Indent: 2, Gap: 3}), "\n") + "\n"
	}
	var b strings.Builder
	for _, r := range rows {
		b.WriteString("  " + strings.TrimSpace(r[0]) + "\n")
		if len(r) > 1 && ui.Strip(r[1]) != "" {
			b.WriteString("      " + r[1] + "\n")
		}
	}
	return b.String()
}
