package ui

import "fmt"

// MissingError is a value that has to come from a flag because there is no
// terminal to ask on. The CLI turns it into exit code 2.
type MissingError struct {
	What string // "shell", "confirmation"
	Flag string // "--yes", "bash|zsh|fish"
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("missing %s: pass %s (no terminal to ask on)", e.What, e.Flag)
}

// Need is the non-interactive rule: when a value was not given as a flag,
// it can only be asked for on a terminal; otherwise fail naming the flag.
func Need(what, flag string) error {
	if CanPrompt() {
		return nil
	}
	return &MissingError{What: what, Flag: flag}
}

// Sure asks for confirmation unless --yes was given. Off a terminal without
// --yes it fails instead of assuming yes.
func Sure(yes bool, question, note string) (bool, error) {
	if yes {
		return true, nil
	}
	if err := Need("confirmation", "--yes"); err != nil {
		return false, err
	}
	return Confirm(question, note, true)
}

// SureDanger is the typed confirm for irreversible actions. --yes alone is
// not enough to skip it; it takes --yes plus --force.
func SureDanger(yes, force bool, question, name string) error {
	if yes && force {
		return nil
	}
	if err := Need("confirmation for an irreversible action", "--yes --force"); err != nil {
		return err
	}
	return ConfirmTyped(question, name)
}
