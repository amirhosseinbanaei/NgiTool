package ui

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

func writeln(w io.Writer, s string) { _, _ = fmt.Fprintln(w, s) }

// Log lines: one symbol, one message.
func Plain(msg string) { writeln(Out, msg) }
func Step(msg string)  { writeln(Out, Accent("•")+" "+msg) }
func Info(msg string)  { writeln(Out, Accent(SymInfo)+" "+msg) }
func Done(msg string)  { writeln(Out, OK(SymOK)+" "+msg) }
func Warning(msg string) {
	writeln(Out, Warn(SymWarn)+" "+msg)
}
func Fail(msg string) { writeln(Errw, Err(SymErr)+" "+msg) }
func Hint(msg string) { writeln(Out, Muted("  "+msg)) }

// Banner is the first thing the root menu shows: name, version, one status line.
func Banner(version, status string) {
	writeln(Out, "")
	line := "  " + AccentBold("NgiTool") + "  " + Muted(version)
	if status != "" {
		line += "  " + status
	}
	writeln(Out, line)
	writeln(Out, "")
}

// Heading is a section title: blank line, bold title, optional muted note.
func Heading(title, note string) {
	writeln(Out, "")
	line := "  " + Bold(title)
	if note != "" {
		line += "  " + Muted(note)
	}
	writeln(Out, line)
}

// TableOpts shape a table.
type TableOpts struct {
	Indent int      // default 4
	Gap    int      // default 2
	Header []string // optional, rendered muted
}

// Table aligns rows into columns measured on visible width. The last column
// is truncated to the terminal width so a row never wraps.
func Table(rows [][]string, o TableOpts) []string {
	if o.Indent == 0 {
		o.Indent = 4
	}
	if o.Gap == 0 {
		o.Gap = 2
	}
	all := rows
	if len(o.Header) > 0 {
		h := make([]string, len(o.Header))
		for i, c := range o.Header {
			h[i] = Muted(c)
		}
		all = append([][]string{h}, rows...)
	}
	if len(all) == 0 {
		return nil
	}
	n := 0
	for _, r := range all {
		n = max(n, len(r))
	}
	widths := make([]int, n)
	for _, r := range all {
		for i := 0; i < len(r)-1; i++ {
			widths[i] = max(widths[i], Width(r[i]))
		}
	}
	cols := Columns()
	out := make([]string, 0, len(all))
	for _, r := range all {
		line := strings.Repeat(" ", o.Indent)
		for i, cell := range r {
			if i < len(r)-1 {
				line += Pad(cell, widths[i]) + strings.Repeat(" ", o.Gap)
			} else {
				line += Truncate(cell, max(8, cols-Width(line)-1))
			}
		}
		out = append(out, strings.TrimRight(line, " "))
	}
	return out
}

// PrintTable prints Table's lines.
func PrintTable(rows [][]string, o TableOpts) {
	for _, l := range Table(rows, o) {
		writeln(Out, l)
	}
}

// Plan is the "here's what will happen" block shown before a change.
func Plan(title string, pairs [][2]string) {
	Heading(title, "")
	kw := 12
	for _, p := range pairs {
		kw = max(kw, Width(p[0]))
	}
	for _, p := range pairs {
		writeln(Out, "    "+Muted(Pad(p[0], kw))+" "+p[1])
	}
	writeln(Out, "")
}

// Kind picks a callout's colour and symbol.
type Kind int

const (
	KindInfo Kind = iota
	KindWarn
	KindDanger
	KindTip
)

func (k Kind) paint(s string) string {
	switch k {
	case KindWarn:
		return Warn(s)
	case KindDanger:
		return Danger(s)
	case KindTip:
		return OK(s)
	}
	return Accent(s)
}

func (k Kind) symbol() string {
	switch k {
	case KindWarn:
		return SymWarn
	case KindDanger:
		return SymErr
	case KindTip:
		return SymTip
	}
	return SymInfo
}

func panelWidth() int { return max(20, min(Columns()-6, 76)) }

// Callout is a titled panel with a coloured bar down its left side.
func Callout(k Kind, title, body string) {
	writeln(Out, strings.Join(CalloutLines(k, title, body), "\n"))
}

// CalloutLines renders a callout without printing it.
func CalloutLines(k Kind, title, body string) []string {
	bar := k.paint(SymBar)
	lines := []string{"  " + k.paint(k.symbol()) + " " + Bold(title)}
	if body != "" {
		for _, l := range wrap(body, panelWidth()) {
			lines = append(lines, strings.TrimRight("  "+bar+" "+l, " "))
		}
	}
	return lines
}

// Explain is shown before a destructive or confusing action: what it does,
// what it touches, how to take it back.
func Explain(what, affects, undo string) {
	writeln(Out, strings.Join(ExplainLines(what, affects, undo), "\n"))
}

// ExplainLines renders an Explain panel without printing it.
func ExplainLines(what, affects, undo string) []string {
	bar := Warn(SymBar)
	rows := [][2]string{{"What it does", what}, {"What it affects", affects}, {"How to undo", undo}}
	kw := len("What it affects")
	room := max(16, panelWidth()-kw-2)
	lines := []string{"  " + Warn(SymWarn) + " " + Bold("Before you confirm")}
	for _, r := range rows {
		for i, l := range wrap(r[1], room) {
			key := ""
			if i == 0 {
				key = r[0]
			}
			lines = append(lines, strings.TrimRight("  "+bar+" "+Muted(Pad(key, kw))+"  "+l, " "))
		}
	}
	return lines
}

// Node is one entry of a Tree.
type Node struct {
	Label    string
	Hint     string
	Children []Node
}

// Tree renders server → location → upstream style hierarchies.
func Tree(root Node) []string {
	lines := []string{"  " + label(root)}
	var walk func(n Node, prefix string)
	walk = func(n Node, prefix string) {
		for i, c := range n.Children {
			last := i == len(n.Children)-1
			branch, next := "├─ ", "│  "
			if last {
				branch, next = "└─ ", "   "
			}
			lines = append(lines, "  "+Muted(prefix+branch)+label(c))
			walk(c, prefix+next)
		}
	}
	walk(root, "")
	return lines
}

func label(n Node) string {
	if n.Hint == "" || Narrow() {
		return n.Label
	}
	return n.Label + "  " + Muted(n.Hint)
}

// Status is a checklist result.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

func (s Status) String() string {
	return [...]string{"ok", "warn", "fail"}[s]
}

// CheckLine is one checklist row: ✔ / ! / ✖, label, detail; a fix hint below.
func CheckLine(s Status, label, detail, fix string) []string {
	var mark string
	switch s {
	case StatusOK:
		mark = OK(SymOK)
	case StatusWarn:
		mark = Warn(SymWarn)
	default:
		mark = Err(SymErr)
	}
	line := "  " + mark + " " + label
	if detail != "" {
		line += "  " + Muted(detail)
	}
	out := []string{line}
	if fix != "" && s != StatusOK {
		out = append(out, "    "+Muted(SymArrow+" "+fix))
	}
	return out
}

// DaysLeft is a certificate's expiry as the legacy CLI showed it
// (cli/src/ui.mjs daysLeft): "83 days left", "3 years left", "expired 2d
// ago", in Err under 14 days, Warn under 30, OK otherwise; "unknown" muted
// for a zero time.
func DaysLeft(notAfter, now time.Time) string {
	if notAfter.IsZero() {
		return Muted("unknown")
	}
	days := int(math.Floor(notAfter.Sub(now).Hours() / 24))
	if days < 0 {
		return Err(fmt.Sprintf("expired %dd ago", -days))
	}
	text := fmt.Sprintf("%d days left", days)
	if days > 730 {
		text = fmt.Sprintf("%d years left", int(math.Round(float64(days)/365)))
	}
	switch {
	case days < 14:
		return Err(text)
	case days < 30:
		return Warn(text)
	}
	return OK(text)
}
