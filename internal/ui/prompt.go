package ui

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
)

var (
	// ErrBack is Esc: the caller goes back one level.
	ErrBack = errors.New("back")
	// ErrInterrupted is Ctrl-C: main restores the terminal and exits 130.
	ErrInterrupted = errors.New("interrupted")
	// ErrNoTTY is a prompt reached off a terminal (callers should use Need first).
	ErrNoTTY = errors.New("this step needs an interactive terminal — pass the value as a flag instead")
)

const (
	manualValue = "\x00manual"
	sepPrefix   = "\x00sep:"
	// ManualLabel ends every list that finds things automatically.
	ManualLabel = SymPen + " Enter it manually…"
)

// Option is one row of a select or multiselect.
type Option struct {
	Value    string
	Label    string
	Hint     string // muted column after the label; dropped first on narrow terminals
	Badge    string // short tag after the label ("running", "managed")
	Disabled string // non-empty: not selectable, and this is why
	sep      bool
}

// Sep is a non-selectable group heading.
func Sep(title string) Option { return Option{Value: sepPrefix + title, Label: title, sep: true} }

// Manual turns on the "✎ Enter it manually…" row: choosing it opens a
// validated input. Typing is the last resort, never the first step.
type Manual struct {
	Title       string
	Placeholder string
	Validate    func(string) error
}

// SelectOpts configure Select.
type SelectOpts struct {
	Title   string
	Note    string
	Options []Option
	Default string
	Filter  bool    // start with the type-to-filter box open
	Manual  *Manual // append the manual-entry row
}

// Select asks for one option and returns its Value.
func Select(o SelectOpts) (string, error) {
	opts := o.Options
	if o.Manual != nil {
		opts = append(append([]Option{}, opts...), Option{Value: manualValue, Label: ManualLabel})
	}
	for {
		value := o.Default
		byValue := map[string]Option{}
		for _, op := range opts {
			byValue[op.Value] = op
		}
		field := huh.NewSelect[string]().
			Title(title(o.Title)).
			Options(huhOptions(opts)...).
			Value(&value).
			Filtering(o.Filter).
			Validate(func(v string) error {
				op := byValue[v]
				switch {
				case op.sep:
					return errors.New("that is a group heading — pick an item under it")
				case op.Disabled != "":
					return fmt.Errorf("%s is unavailable: %s", op.Label, op.Disabled)
				}
				return nil
			})
		if o.Note != "" {
			field.Description(o.Note)
		}
		if h := Rows() - 6; len(opts) > h && h > 3 {
			field.Height(h)
		}
		summary := func() string {
			if value == manualValue {
				return "" // the input that follows prints the answer
			}
			return answered(o.Title, byValue[value].Label)
		}
		footer := Muted("↑↓ move · ↵ select · / filter · esc back")
		if err := runField(field, field, summary, func() string { return footer }, nil); err != nil {
			return "", err
		}
		if value != manualValue {
			return value, nil
		}
		m := o.Manual
		v, err := Input(InputOpts{Title: cmpStr(m.Title, o.Title), Placeholder: m.Placeholder, Validate: m.Validate})
		if errors.Is(err, ErrBack) {
			continue // back to the list
		}
		return v, err
	}
}

// MultiOpts configure MultiSelect.
type MultiOpts struct {
	Title    string
	Note     string
	Options  []Option
	Selected []string
}

// MultiSelect is a checklist with a/n for all/none and a live counter. A
// group heading toggles every item under it; disabled items never tick.
func MultiSelect(o MultiOpts) ([]string, error) {
	sel := map[string]bool{}
	for _, v := range o.Selected {
		sel[v] = true
	}
	// group[i] is the index of the heading option i sits under, or -1.
	group := make([]int, len(o.Options))
	cur := -1
	for i, op := range o.Options {
		if op.sep {
			cur = i
		}
		group[i] = cur
	}
	selectable := func(i int) bool { return !o.Options[i].sep && o.Options[i].Disabled == "" }
	values := []string{}
	field := huh.NewMultiSelect[string]().Title(title(o.Title)).Value(&values)
	if o.Note != "" {
		field.Description(o.Note)
	}
	if h := Rows() - 7; len(o.Options) > h && h > 3 {
		field.Height(h)
	}

	// apply pushes sel into the field: headings show ticked when their whole
	// group is, and the cursor is put back where it was.
	apply := func(hovered int) []tea.Msg {
		for i, op := range o.Options {
			if op.sep {
				all, any := true, false
				for j := i + 1; j < len(o.Options) && group[j] == i; j++ {
					if selectable(j) {
						any = true
						all = all && sel[o.Options[j].Value]
					}
				}
				sel[op.Value] = any && all
			}
		}
		values = values[:0]
		opts := huhOptions(o.Options)
		for i, op := range o.Options {
			opts[i] = opts[i].Selected(sel[op.Value])
			if sel[op.Value] {
				values = append(values, op.Value)
			}
		}
		field.Options(opts...)
		msgs := []tea.Msg{tea.KeyMsg{Type: tea.KeyHome}}
		for i := 0; i < hovered; i++ {
			msgs = append(msgs, tea.KeyMsg{Type: tea.KeyDown})
		}
		return msgs
	}
	apply(0)

	real := func() []string {
		out := []string{}
		for i, op := range o.Options {
			if selectable(i) && sel[op.Value] {
				out = append(out, op.Value)
			}
		}
		return out
	}
	hoveredIndex := func() int {
		v, ok := field.Hovered()
		if !ok {
			return -1
		}
		for i, op := range o.Options {
			if op.Value == v {
				return i
			}
		}
		return -1
	}
	intercept := func(k tea.KeyMsg) []tea.Msg {
		h := hoveredIndex()
		switch k.String() {
		case " ", "x":
			if h < 0 {
				return []tea.Msg{}
			}
			switch {
			case o.Options[h].sep:
				on := !sel[o.Options[h].Value]
				for j := h + 1; j < len(o.Options) && group[j] == h; j++ {
					if selectable(j) {
						sel[o.Options[j].Value] = on
					}
				}
			case selectable(h):
				sel[o.Options[h].Value] = !sel[o.Options[h].Value]
			default:
				return []tea.Msg{} // disabled: its reason is already on the row
			}
		case "a":
			for i, op := range o.Options {
				if selectable(i) {
					sel[op.Value] = true
				}
			}
		case "n":
			for k := range sel {
				delete(sel, k)
			}
		default:
			return nil
		}
		return apply(max(h, 0))
	}
	footer := func() string {
		n := len(real())
		count := Muted("nothing selected")
		if n > 0 {
			count = OK(fmt.Sprintf("%d selected", n))
		}
		return count + "\n" + Muted("↑↓ move · space toggle · a all · n none · / filter · ↵ confirm · esc back")
	}
	summary := func() string {
		n := len(real())
		if n == 0 {
			return answered(o.Title, "none")
		}
		return answered(o.Title, fmt.Sprintf("%d selected", n))
	}
	if err := runField(field, field, summary, footer, intercept); err != nil {
		return nil, err
	}
	return real(), nil
}

// InputOpts configure Input.
type InputOpts struct {
	Title       string
	Note        string
	Placeholder string
	Default     string // used when the answer is left empty
	Validate    func(string) error
	Secret      bool
	Suggestions []string // completed with tab as the prefix matches
}

// Input asks for text, validating as it is typed.
func Input(o InputOpts) (string, error) {
	value := ""
	validate := func(v string) error {
		v = strings.TrimSpace(v)
		if v == "" {
			v = o.Default
		}
		if o.Validate != nil {
			return o.Validate(v)
		}
		return nil
	}
	placeholder := o.Placeholder
	if placeholder == "" {
		placeholder = o.Default
	}
	field := huh.NewInput().Title(title(o.Title)).Prompt("› ").Placeholder(placeholder).Value(&value).Validate(validate)
	if o.Note != "" {
		field.Description(o.Note)
	}
	if o.Secret {
		field.EchoMode(huh.EchoModePassword)
	}
	if len(o.Suggestions) > 0 {
		field.Suggestions(o.Suggestions)
	}
	final := func() string {
		v := strings.TrimSpace(value)
		if v == "" {
			return o.Default
		}
		return v
	}
	summary := func() string {
		if o.Secret {
			return answered(o.Title, "••••••")
		}
		return answered(o.Title, final())
	}
	footer := "↵ confirm · esc back"
	if len(o.Suggestions) > 0 {
		footer = "tab complete · ↵ confirm · esc back"
	}
	if err := runField(field, nil, summary, func() string { return Muted(footer) }, nil); err != nil {
		return "", err
	}
	return final(), nil
}

// Confirm is a yes/no question.
func Confirm(question, note string, def bool) (bool, error) {
	v := def
	field := huh.NewConfirm().Title(title(question)).Affirmative("Yes").Negative("No").Value(&v)
	if note != "" {
		field.Description(note)
	}
	summary := func() string {
		if v {
			return answered(question, "yes")
		}
		return answered(question, "no")
	}
	if err := runField(field, nil, summary, func() string { return Muted("←→ choose · y/n · ↵ confirm · esc back") }, nil); err != nil {
		return false, err
	}
	return v, nil
}

// ConfirmTyped is the danger confirm: the answer is the resource's name,
// typed out. Used for irreversible actions.
func ConfirmTyped(question, name string) error {
	_, err := Input(InputOpts{
		Title: question,
		Note:  "This cannot be undone. Type " + name + " to confirm.",
		Validate: func(v string) error {
			if v != name {
				return fmt.Errorf("type %s exactly to confirm", name)
			}
			return nil
		},
	})
	return err
}

// ---- plumbing -------------------------------------------------------------

func title(t string) string { return Accent("?") + " " + Bold(t) }

func answered(question, answer string) string {
	return OK(SymOK) + " " + question + " " + Muted("›") + " " + Accent(answer)
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// huhOptions renders label, badge and hint into one aligned option line.
func huhOptions(opts []Option) []huh.Option[string] {
	lw, bw := 0, 0
	for _, o := range opts {
		if !o.sep {
			lw = max(lw, Width(o.Label))
			bw = max(bw, Width(o.Badge))
		}
	}
	lw = min(lw, 34)
	badge := func(o Option) string {
		if bw == 0 {
			return ""
		}
		return " " + Pad(Warn(o.Badge), bw)
	}
	narrow := Narrow()
	out := make([]huh.Option[string], len(opts))
	for i, o := range opts {
		var text string
		switch {
		case o.sep:
			text = Muted("── " + o.Label + " ──")
		case o.Disabled != "":
			text = Muted(Pad(o.Label, lw)) + badge(o)
			if !narrow {
				text += "  " + Muted(o.Disabled)
			}
		default:
			text = Pad(o.Label, lw) + badge(o)
			if o.Hint != "" && !narrow {
				text += "  " + Muted(o.Hint)
			}
		}
		// One row per option: the hint is cut, never wrapped (rule 3).
		out[i] = huh.NewOption(Truncate(strings.TrimRight(text, " "), max(20, min(Columns(), 100)-10)), o.Value)
	}
	return out
}

type outcome int

const (
	running outcome = iota
	finished
	back
	interrupted
)

// promptModel wraps a one-field huh form so Esc means "back", Ctrl-C means
// "exit 130", and a finished prompt collapses to one summary line.
type promptModel struct {
	form      *huh.Form
	filterer  interface{ GetFiltering() bool }
	summary   func() string
	footer    func() string
	intercept func(tea.KeyMsg) []tea.Msg
	state     outcome
}

func (m *promptModel) Init() tea.Cmd { return m.form.Init() }

func (m *promptModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		filtering := m.filterer != nil && m.filterer.GetFiltering()
		switch k.String() {
		case "ctrl+c":
			m.state = interrupted
			return m, tea.Quit
		case "esc":
			if !filtering {
				m.state = back
				return m, tea.Quit
			}
		}
		if m.intercept != nil && !filtering {
			if msgs := m.intercept(k); msgs != nil {
				var cmds []tea.Cmd
				for _, mm := range msgs {
					_, c := m.form.Update(mm)
					cmds = append(cmds, c)
				}
				return m, tea.Batch(cmds...)
			}
		}
	}
	_, cmd := m.form.Update(msg)
	if m.form.State == huh.StateCompleted {
		m.state = finished
		return m, tea.Quit
	}
	return m, cmd
}

func (m *promptModel) View() string {
	switch m.state {
	case finished:
		if s := m.summary(); s != "" {
			return s + "\n"
		}
		return ""
	case back, interrupted:
		return ""
	}
	v := strings.TrimRight(m.form.View(), "\n")
	if m.footer != nil {
		v += "\n" + m.footer()
	}
	return v + "\n"
}

func runField(field huh.Field, filterer interface{ GetFiltering() bool }, summary, footer func() string,
	intercept func(tea.KeyMsg) []tea.Msg) error {
	if !CanPrompt() {
		return ErrNoTTY
	}
	km := huh.NewDefaultKeyMap()
	km.Quit = key.NewBinding(key.WithKeys("ctrl+c"))
	// MultiSelect owns its selection (see intercept), so huh's ctrl+a is off.
	km.MultiSelect.SelectAll = key.NewBinding(key.WithDisabled())
	km.MultiSelect.SelectNone = key.NewBinding(key.WithDisabled())
	form := huh.NewForm(huh.NewGroup(field)).
		WithTheme(huhTheme()).
		WithKeyMap(km).
		WithShowHelp(false).
		WithWidth(min(Columns(), 100))
	form.SubmitCmd, form.CancelCmd = nil, nil
	m := &promptModel{form: form, filterer: filterer, summary: summary, footer: footer, intercept: intercept}
	if _, err := tea.NewProgram(m, tea.WithOutput(os.Stdout), tea.WithInput(os.Stdin)).Run(); err != nil {
		return fmt.Errorf("prompt: %w", err)
	}
	switch m.state {
	case back:
		return ErrBack
	case interrupted:
		writeln(Out, Muted("Interrupted."))
		return ErrInterrupted
	}
	return nil
}
