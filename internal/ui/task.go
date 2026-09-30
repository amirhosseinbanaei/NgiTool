package ui

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// ShowSecondsAfter is when a running task starts showing its elapsed time.
const ShowSecondsAfter = 3 * time.Second

// TaskCtl lets a running task change its label.
type TaskCtl struct {
	mu    sync.Mutex
	label string
}

// Update replaces the label shown next to the spinner and in the final line.
func (t *TaskCtl) Update(label string) {
	t.mu.Lock()
	t.label = label
	t.mu.Unlock()
}

func (t *TaskCtl) get() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.label
}

func elapsed(start time.Time) string { return seconds(time.Since(start)) }

func seconds(d time.Duration) string {
	if d < ShowSecondsAfter {
		return ""
	}
	return Muted(fmt.Sprintf(" %ds", int(d.Round(time.Second).Seconds())))
}

// Task runs fn behind a spinner that shows elapsed seconds after 3 s, then
// collapses to one ✔ or ✖ line that keeps the label. Off a terminal it
// prints only that final line.
func Task(label string, fn func(*TaskCtl) error) error {
	ctl := &TaskCtl{label: label}
	start := time.Now()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	if outTTY {
		cursorHide()
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(80 * time.Millisecond)
			defer t.Stop()
			for i := 0; ; i++ {
				clearLine()
				fmt.Fprint(Out, Accent(frames[i%len(frames)])+" "+Truncate(ctl.get(), Columns()-8)+elapsed(start))
				select {
				case <-stop:
					return
				case <-t.C:
				}
			}
		}()
	}
	err := fn(ctl)
	close(stop)
	wg.Wait()
	if outTTY {
		clearLine()
		cursorShow()
	}
	if err != nil {
		writeln(Out, Err(SymErr)+" "+ctl.get())
	} else {
		writeln(Out, OK(SymOK)+" "+ctl.get()+elapsed(start))
	}
	return err
}

// StepFunc is one step of a Steps run.
type StepFunc struct {
	Label string
	Run   func(*TaskCtl) error
}

type stepState int

const (
	stepPending stepState = iota
	stepRunning
	stepDone
	stepFailed
	stepSkipped
)

// Steps runs a multi-step operation, showing every step as done, running or
// pending. It stops at the first failure; the rest are marked skipped.
func Steps(title string, steps []StepFunc) error {
	if title != "" {
		Heading(title, "")
	}
	states := make([]stepState, len(steps))
	ctls := make([]*TaskCtl, len(steps))
	for i, s := range steps {
		ctls[i] = &TaskCtl{label: s.Label}
	}
	starts := make([]time.Time, len(steps))
	took := make([]time.Duration, len(steps))
	var mu sync.Mutex
	frame := 0
	drawn := 0

	render := func() []string {
		lines := make([]string, len(steps))
		for i := range steps {
			label := ctls[i].get()
			switch states[i] {
			case stepDone:
				lines[i] = "  " + OK(SymOK) + " " + label + seconds(took[i])
			case stepFailed:
				lines[i] = "  " + Err(SymErr) + " " + label
			case stepRunning:
				lines[i] = "  " + Accent(frames[frame%len(frames)]) + " " + Bold(label) + elapsed(starts[i])
			case stepSkipped:
				lines[i] = "  " + Muted(SymDash+" "+label+" (skipped)")
			default:
				lines[i] = "  " + Muted(SymRing+" "+label)
			}
			lines[i] = Truncate(lines[i], Columns()-1)
		}
		return lines
	}
	redraw := func() {
		mu.Lock()
		defer mu.Unlock()
		cursorUp(drawn)
		clearDown()
		lines := render()
		fmt.Fprintln(Out, strings.Join(lines, "\n"))
		drawn = len(lines)
		frame++
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	if outTTY {
		cursorHide()
		redraw()
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(80 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					redraw()
				}
			}
		}()
	}

	var err error
	for i, s := range steps {
		mu.Lock()
		states[i], starts[i] = stepRunning, time.Now()
		mu.Unlock()
		err = s.Run(ctls[i])
		mu.Lock()
		took[i] = time.Since(starts[i])
		if err != nil {
			states[i] = stepFailed
			for j := i + 1; j < len(steps); j++ {
				states[j] = stepSkipped
			}
		} else {
			states[i] = stepDone
		}
		mu.Unlock()
		if !outTTY {
			// Off a terminal: one line per finished step, nothing redrawn.
			lines := render()
			writeln(Out, lines[i])
			if err != nil {
				for _, l := range lines[i+1:] {
					writeln(Out, l)
				}
			}
		}
		if err != nil {
			break
		}
	}
	if outTTY {
		close(stop)
		wg.Wait()
		redraw()
		cursorShow()
	}
	return err
}
