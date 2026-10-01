package model

import (
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
)

// Load reads state.json (an empty state when it does not exist yet).
func Load(p paths.Paths) (*State, error) {
	st := &State{Schema: state.StateSchema}
	if _, err := state.StateStore(p).Load(st); err != nil {
		return nil, err
	}
	return st, nil
}

// Save writes state.json atomically. Callers hold the lock and save only
// after nginx accepted the change (APPLY-01).
func Save(p paths.Paths, st *State) error {
	if st.Instances == nil {
		st.Instances = []Adopted{}
	}
	if st.Pools == nil {
		st.Pools = []Pool{}
	}
	if st.Routes == nil {
		st.Routes = []Route{}
	}
	if st.Apps == nil {
		st.Apps = []compose.App{}
	}
	return state.StateStore(p).Save(st)
}
