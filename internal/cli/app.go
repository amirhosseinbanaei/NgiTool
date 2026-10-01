// Package cli wires NgiTool's commands: one file per command group, one
// registry (menu.go) that the help text and the root menu are built from.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// Exit codes.
const (
	ExitOK          = 0
	ExitFail        = 1
	ExitUsage       = 2
	ExitUpdate      = 10 // `update --check`: a newer release exists
	ExitInterrupted = 130
)

// env is what every command shares: resolved paths and the global flags.
type env struct {
	paths   paths.Paths
	yes     bool
	force   bool
	noColor bool
	root    *cobra.Command
	memo    scanMemo         // one scan shared by everything in this run
	fixed   *discover.Report // tests: every scan returns this
	bin     *compose.Bin     // docker compose, found once per run
}

// ExitError ends the run with Code; Msg is printed unless empty.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// UsageError is a mistake in how the command was called (exit 2).
type UsageError struct {
	Msg string
	Cmd string // "ngitool update", for the help hint
}

func (e *UsageError) Error() string { return e.Msg }

// errCancelled is Esc or "No" at a confirmation: nothing was changed.
var errCancelled = errors.New("cancelled — nothing changed")

// Main runs ngitool with args and returns the exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	// Ctrl-C outside a prompt (prompts read it as a key): restore the cursor
	// a spinner may have hidden, then exit 130.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			if holdInterrupt.Load() {
				continue // `app logs` follows: Ctrl-C stops the child and the menu comes back
			}
			ui.RestoreTerminal()
			fmt.Fprintln(os.Stderr, ui.Muted("Interrupted."))
			os.Exit(ExitInterrupted)
		}
	}()

	e := &env{paths: paths.Get()}
	root := newRoot(e)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	code := report(err, cmd)
	if code == ExitOK || code == ExitUpdate {
		afterCommand(e, cmd)
	}
	return code
}

// newRoot builds a fresh command tree (tests build one per case).
func newRoot(e *env) *cobra.Command {
	root := &cobra.Command{
		Use:           "ngitool",
		Short:         "every nginx on this server, from one tool",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          noArgs,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			ui.Init(e.noColor)
			if f := cmd.Flags().Lookup("json"); f != nil && f.Changed {
				ui.SetColor(false)
			}
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !ui.CanPrompt() {
				return cmd.Help()
			}
			return runMenu(e)
		},
	}
	e.root = root
	cobra.EnableCommandSorting = false
	root.CompletionOptions.DisableDefaultCmd = true
	pf := root.PersistentFlags()
	pf.BoolVarP(&e.yes, "yes", "y", false, "don't ask for confirmation")
	pf.BoolVarP(&e.force, "force", "f", false, "with --yes: skip typed confirms too")
	pf.BoolVar(&e.noColor, "no-color", false, "plain output")
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return &UsageError{Msg: err.Error(), Cmd: c.CommandPath()}
	})
	installHelp(root)
	for _, def := range commands {
		root.AddCommand(def(e))
	}
	return root
}

// commands is every command, in help order within each group. Later
// prompts add theirs here and their group to menu.go.
var commands = []func(*env) *cobra.Command{
	scanCmd,
	instancesCmd,
	inspectCmd,
	instanceCmd,
	routeCmd,
	poolCmd,
	appCmd,
	diffCmd,
	applyCmd,
	rollbackCmd,
	testCmd,
	reloadCmd,
	doctorCmd,
	versionCmd,
	updateCmd,
	completionCmd,
	uninstallCmd,
	updateCheckCmd,
}

// noArgs is cobra.NoArgs as a usage error.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return &UsageError{Msg: fmt.Sprintf("unknown command %q", args[0]), Cmd: cmd.CommandPath()}
	}
	return nil
}

// maxArgs is cobra.MaximumNArgs as a usage error.
func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return &UsageError{Msg: fmt.Sprintf("too many arguments: %s", strings.Join(args[n:], " ")), Cmd: cmd.CommandPath()}
		}
		return nil
	}
}

// report prints err the house way and returns the exit code.
func report(err error, cmd *cobra.Command) int {
	if err == nil {
		return ExitOK
	}
	var exit *ExitError
	var usage *UsageError
	var missing *ui.MissingError
	switch {
	case errors.Is(err, ui.ErrInterrupted):
		return ExitInterrupted
	case errors.Is(err, ui.ErrBack), errors.Is(err, errCancelled):
		fmt.Fprintln(ui.Errw, ui.Muted(errCancelled.Error()))
		return ExitFail
	case errors.As(err, &exit):
		if exit.Msg != "" {
			ui.Fail(exit.Msg)
		}
		return exit.Code
	case errors.As(err, &usage), errors.As(err, &missing), strings.HasPrefix(err.Error(), "unknown command"):
		ui.Fail(err.Error())
		path := "ngitool"
		if usage != nil && usage.Cmd != "" {
			path = usage.Cmd
		} else if cmd != nil {
			path = cmd.CommandPath()
		}
		help := "ngitool help"
		if rest := strings.TrimPrefix(path, "ngitool"); rest != "" {
			help += rest
		}
		fmt.Fprintln(ui.Errw, ui.Muted("  run: "+help))
		return ExitUsage
	}
	lines := strings.Split(strings.TrimRight(err.Error(), "\n"), "\n")
	ui.Fail(lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintln(ui.Errw, ui.Muted("  "+l))
	}
	return ExitFail
}
