package ui

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// Where output goes. Tests swap these for buffers.
var (
	Out  io.Writer = os.Stdout
	Errw io.Writer = os.Stderr
)

// Symbols carry meaning in plain output too, so they stay without colour.
const (
	SymOK        = "✔"
	SymErr       = "✖"
	SymWarn      = "!"
	SymInfo      = "ℹ"
	SymTip       = "★"
	SymPointer   = "❯"
	SymArrow     = "→"
	SymDot       = "●"
	SymRing      = "○"
	SymBar       = "│"
	SymChecked   = "◉"
	SymUnchecked = "◯"
	SymPen       = "✎"
	SymDash      = "–"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;:?]*[A-Za-z]|\x1b\][^\x07]*\x07`)

// Strip removes escape sequences.
func Strip(s string) string { return ansiRE.ReplaceAllString(s, "") }

// Width is the visible width in terminal cells (ANSI-aware, wide runes count 2).
func Width(s string) int {
	w := 0
	for _, l := range strings.Split(s, "\n") {
		w = max(w, lipgloss.Width(l))
	}
	return w
}

// Truncate cuts s to max visible cells, ending in "…". Styled input that has
// to be cut degrades to plain text so no escape sequence is left half-open.
func Truncate(s string, maxw int) string {
	if maxw <= 0 {
		return ""
	}
	if Width(s) <= maxw {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range Strip(s) {
		rw := lipgloss.Width(string(r))
		if w+rw > maxw-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

// Pad right-pads s with spaces to n visible cells.
func Pad(s string, n int) string {
	if d := n - Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// Columns is the terminal width (COLUMNS, then 80, off a terminal).
func Columns() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return 80
}

// Rows is the terminal height (24 off a terminal).
func Rows() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 {
		return h
	}
	return 24
}

// Narrow terminals drop hints before they drop labels.
const NarrowColumns = 60

func Narrow() bool { return Columns() < NarrowColumns }

// IsTTY reports whether stdout is a terminal.
func IsTTY() bool { return outTTY }

// CanPrompt reports whether both stdin and stdout are terminals.
func CanPrompt() bool { return inTTY && outTTY }

// wrap breaks text into lines of at most width cells on word boundaries.
func wrap(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := ""
		for _, w := range words {
			switch {
			case line == "":
				line = w
			case Width(line)+1+Width(w) <= width:
				line += " " + w
			default:
				out = append(out, line)
				line = w
			}
		}
		out = append(out, line)
	}
	return out
}

// Cursor control only makes sense on a terminal; off one it would land in a
// pipe, so these are no-ops there.
func cursorWrite(s string) {
	if outTTY {
		_, _ = io.WriteString(Out, s)
	}
}

func cursorHide() { cursorWrite("\x1b[?25l") }
func cursorShow() { cursorWrite("\x1b[?25h") }
func cursorUp(n int) {
	if n > 0 {
		cursorWrite("\x1b[" + strconv.Itoa(n) + "A")
	}
}
func clearLine() { cursorWrite("\r\x1b[2K") }
func clearDown() { cursorWrite("\r\x1b[0J") }

// RestoreTerminal shows the cursor again; main calls it on SIGINT.
func RestoreTerminal() {
	clearLine()
	cursorShow()
}
