package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func withColor(t *testing.T, on bool) {
	t.Helper()
	prev := enabled
	SetColor(on)
	t.Cleanup(func() { SetColor(prev) })
}

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevErr := Out, Errw
	Out, Errw = &buf, &buf
	t.Cleanup(func() { Out, Errw = prevOut, prevErr })
	return &buf
}

func TestWidthIgnoresANSI(t *testing.T) {
	withColor(t, true)
	s := Accent("hello") + " " + Bold("world")
	if !strings.Contains(s, "\x1b[") {
		t.Fatal("expected styled text with colour on")
	}
	if Width(s) != 11 {
		t.Fatalf("Width = %d, want 11", Width(s))
	}
	if Width("日本") != 4 {
		t.Fatalf("wide runes: Width = %d, want 4", Width("日本"))
	}
}

func TestTruncate(t *testing.T) {
	withColor(t, true)
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{Accent("hello world"), 5, "hell…"},
		{"short", 10, "short"},
		{"日本語テキスト", 5, "日本…"},
		{"abc", 0, ""},
	}
	for _, c := range cases {
		got := Truncate(c.in, c.max)
		if got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", Strip(c.in), c.max, got, c.want)
		}
		if Width(got) > c.max {
			t.Errorf("Truncate(%q, %d) is %d wide", Strip(c.in), c.max, Width(got))
		}
	}
	// Untruncated styled text keeps its style.
	if s := Accent("ok"); Truncate(s, 10) != s {
		t.Error("styled text changed without truncation")
	}
}

func TestPad(t *testing.T) {
	withColor(t, true)
	if got := Pad(OK("ab"), 5); Width(got) != 5 || !strings.HasSuffix(got, "   ") {
		t.Fatalf("Pad = %q", got)
	}
}

func TestNoColorIsPlain(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	Init(false)
	t.Cleanup(func() { SetColor(false) })
	buf := capture(t)
	for _, f := range []func(string) string{Accent, OK, Warn, Err, Muted, Key, Danger, Bold, AccentBold} {
		if got := f("text"); got != "text" {
			t.Errorf("styled under NO_COLOR: %q", got)
		}
	}
	Heading("Title", "note")
	PrintTable([][]string{{Accent("a"), "b"}}, TableOpts{})
	Callout(KindWarn, "Careful", "body text")
	Explain("does", "affects", "undo")
	Plan("Plan", [][2]string{{"key", "value"}})
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatalf("escape codes in NO_COLOR output:\n%q", buf.String())
	}
	if !strings.Contains(buf.String(), "What it affects") || !strings.Contains(buf.String(), "! Careful") {
		t.Fatalf("content missing:\n%s", buf.String())
	}
}

func TestTableTruncatesLastColumn(t *testing.T) {
	withColor(t, true)
	t.Setenv("COLUMNS", "30")
	lines := Table([][]string{
		{"name", Muted("a note that is much too long for thirty columns")},
		{"longer-name", "short"},
	}, TableOpts{})
	for _, l := range lines {
		if Width(l) > 29 {
			t.Errorf("line is %d wide: %q", Width(l), Strip(l))
		}
	}
	if !strings.HasPrefix(Strip(lines[1]), "    longer-name  short") {
		t.Errorf("columns not aligned: %q", Strip(lines[1]))
	}
	if !strings.HasPrefix(Strip(lines[0]), "    name         a note") {
		t.Errorf("columns not aligned: %q", Strip(lines[0]))
	}
}

func TestNarrowDropsHints(t *testing.T) {
	t.Setenv("COLUMNS", "50")
	opts := huhOptions([]Option{{Value: "a", Label: "Alpha", Hint: "a long hint"}})
	if strings.Contains(opts[0].Key, "hint") {
		t.Fatal("hint kept on a narrow terminal")
	}
	t.Setenv("COLUMNS", "100")
	opts = huhOptions([]Option{{Value: "a", Label: "Alpha", Hint: "a long hint"}, {Value: "b", Label: "Beta", Disabled: "not running"}})
	if !strings.Contains(opts[0].Key, "a long hint") || !strings.Contains(opts[1].Key, "not running") {
		t.Fatalf("hint or disabled reason missing: %q / %q", opts[0].Key, opts[1].Key)
	}
}

func TestUnifiedDiff(t *testing.T) {
	a := "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\n"
	b := "one\ntwo\nthree\nfour\nFIVE\nsix\nseven\neight\nnine\nten\neleven\n"
	want := `--- a.conf
+++ b.conf
@@ -2,9 +2,10 @@
 two
 three
 four
-five
+FIVE
 six
 seven
 eight
 nine
 ten
+eleven
`
	if got := UnifiedDiff("a.conf", "b.conf", a, b); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
	if UnifiedDiff("x", "y", a, a) != "" {
		t.Fatal("equal inputs gave a diff")
	}
	// Changes far apart become separate hunks.
	long := strings.Repeat("same\n", 20)
	got := UnifiedDiff("a", "b", "first\n"+long+"last\n", "FIRST\n"+long+"LAST\n")
	if strings.Count(got, "@@ ") != 2 {
		t.Fatalf("want two hunks:\n%s", got)
	}
	withColor(t, false)
	if Diff(want) != strings.TrimSuffix(want, "\n") {
		t.Fatal("Diff changed text without colour")
	}
}

func TestTree(t *testing.T) {
	withColor(t, false)
	t.Setenv("COLUMNS", "100")
	lines := Tree(Node{Label: "example.com", Children: []Node{
		{Label: "/", Children: []Node{{Label: "app:3000", Hint: "up"}}},
		{Label: "/api", Children: []Node{{Label: "api:8000"}}},
	}})
	want := []string{
		"  example.com",
		"  ├─ /",
		"  │  └─ app:3000  up",
		"  └─ /api",
		"     └─ api:8000",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("tree:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTaskAndStepsOffTTY(t *testing.T) {
	withColor(t, false)
	buf := capture(t)
	_ = Task("Doing it", func(c *TaskCtl) error { c.Update("Did it"); return nil })
	err := Steps("", []StepFunc{
		{Label: "first", Run: func(*TaskCtl) error { return nil }},
		{Label: "second", Run: func(*TaskCtl) error { return errors.New("boom") }},
		{Label: "third", Run: func(*TaskCtl) error { t.Fatal("ran after a failure"); return nil }},
	})
	if err == nil {
		t.Fatal("Steps swallowed the error")
	}
	want := "✔ Did it\n  ✔ first\n  ✖ second\n  – third (skipped)\n"
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}

func TestNonInteractiveNamesTheFlag(t *testing.T) {
	// Tests run without a terminal on stdin.
	var missing *MissingError
	if _, err := Sure(false, "Sure?", ""); !errors.As(err, &missing) || missing.Flag != "--yes" {
		t.Fatalf("Sure off a TTY: %v", err)
	}
	if ok, err := Sure(true, "Sure?", ""); !ok || err != nil {
		t.Fatal("--yes did not skip the confirm")
	}
	if err := SureDanger(true, false, "Purge?", "x"); !errors.As(err, &missing) || missing.Flag != "--yes --force" {
		t.Fatalf("danger confirm with --yes only: %v", err)
	}
	if err := SureDanger(true, true, "Purge?", "x"); err != nil {
		t.Fatal("--yes --force did not skip the typed confirm")
	}
	if _, err := Select(SelectOpts{Title: "Pick"}); !errors.Is(err, ErrNoTTY) {
		t.Fatalf("Select off a TTY: %v", err)
	}
}
