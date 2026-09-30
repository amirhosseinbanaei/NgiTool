// Package ui is NgiTool's terminal kit: one theme, width-aware layout,
// spinners, panels and the interactive prompts. Commands never write colour
// codes themselves; they call the role functions below. docs/ux.md is the
// rulebook this package implements.
package ui

import (
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"golang.org/x/term"
)

// The semantic roles. This is the only place colours are defined; adaptive
// pairs keep them readable on light and dark terminals.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "#0969DA", Dark: "#58A6FF"}
	colOK     = lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#3FB950"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D29922"}
	colErr    = lipgloss.AdaptiveColor{Light: "#CF222E", Dark: "#F85149"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#6E7781", Dark: "#8B949E"}
	colKey    = lipgloss.AdaptiveColor{Light: "#8250DF", Dark: "#D2A8FF"}
	colDanger = lipgloss.AdaptiveColor{Light: "#A40E26", Dark: "#FF7B72"}
)

var (
	enabled  bool // colour on
	outTTY   bool // stdout is a terminal
	inTTY    bool // stdin is a terminal
	renderer = lipgloss.DefaultRenderer()
)

func init() { Init(false) }

// Init decides once whether output is styled. Colour is off with NO_COLOR
// (any value), TERM=dumb, --no-color, or when stdout is not a terminal; the
// text stays the same, only the styling goes.
func Init(noColor bool) {
	outTTY = term.IsTerminal(int(os.Stdout.Fd()))
	inTTY = term.IsTerminal(int(os.Stdin.Fd()))
	_, nc := os.LookupEnv("NO_COLOR")
	enabled = outTTY && !noColor && !nc && os.Getenv("TERM") != "dumb"
	if !enabled {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
	renderer = lipgloss.DefaultRenderer()
}

// SetColor forces colour on or off (tests, --json).
func SetColor(on bool) {
	enabled = on
	if on {
		lipgloss.SetColorProfile(termenv.TrueColor)
	} else {
		lipgloss.SetColorProfile(termenv.Ascii)
	}
}

// ColorEnabled reports whether styling is on.
func ColorEnabled() bool { return enabled }

func style(c lipgloss.TerminalColor) lipgloss.Style {
	if !enabled {
		return renderer.NewStyle()
	}
	return renderer.NewStyle().Foreground(c)
}

func paint(st lipgloss.Style, s string) string {
	if !enabled || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = st.Render(l)
		}
	}
	return strings.Join(lines, "\n")
}

// Role painters. Every command styles text through these.
func Accent(s string) string { return paint(style(colAccent), s) }
func OK(s string) string     { return paint(style(colOK), s) }
func Warn(s string) string   { return paint(style(colWarn), s) }
func Err(s string) string    { return paint(style(colErr), s) }
func Muted(s string) string  { return paint(style(colMuted), s) }
func Key(s string) string    { return paint(style(colKey), s) }
func Danger(s string) string { return paint(style(colDanger).Bold(true), s) }
func Bold(s string) string   { return paint(renderer.NewStyle().Bold(true), s) }

// AccentBold is the title style: the tool name, a focused option.
func AccentBold(s string) string { return paint(style(colAccent).Bold(true), s) }

// huhTheme styles the interactive prompts with the same roles.
func huhTheme() *huh.Theme {
	t := huh.ThemeBase()
	plain := renderer.NewStyle()
	for _, f := range []*huh.FieldStyles{&t.Focused, &t.Blurred} {
		f.Base = plain
		f.Title = plain
		f.Description = style(colMuted)
		f.ErrorIndicator = style(colErr).SetString(" " + SymErr)
		f.ErrorMessage = style(colErr).SetString(SymErr)
		f.SelectSelector = style(colAccent).SetString(SymPointer + " ")
		f.Option = plain
		f.NextIndicator = style(colMuted).SetString(" →")
		f.PrevIndicator = style(colMuted).SetString("← ")
		f.MultiSelectSelector = style(colAccent).SetString(SymPointer + " ")
		f.SelectedOption = style(colAccent)
		f.SelectedPrefix = style(colOK).SetString(SymChecked + " ")
		f.UnselectedOption = plain
		f.UnselectedPrefix = style(colMuted).SetString(SymUnchecked + " ")
		f.TextInput.Cursor = style(colAccent)
		f.TextInput.Placeholder = style(colMuted)
		f.TextInput.Prompt = style(colAccent)
		f.TextInput.Text = plain
		f.FocusedButton = style(colAccent).Bold(true).Padding(0, 1)
		f.BlurredButton = style(colMuted).Padding(0, 1)
		f.Card = plain
		f.NoteTitle = plain.Bold(true)
	}
	if enabled {
		t.Focused.FocusedButton = renderer.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#FFFFFF", Dark: "#0D1117"}).
			Background(colAccent).Bold(true).Padding(0, 1)
	}
	t.Blurred.SelectSelector = plain.SetString("  ")
	t.Blurred.MultiSelectSelector = plain.SetString("  ")
	t.Help.ShortKey = style(colMuted)
	t.Help.ShortDesc = style(colMuted)
	t.Help.ShortSeparator = style(colMuted)
	return t
}
