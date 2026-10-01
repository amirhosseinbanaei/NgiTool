package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Migration upgrades a document from schema N to N+1 in place.
type Migration func(doc map[string]any) error

// Store is one JSON file with a top-level "schema" integer. Migrations[n]
// turns schema n into n+1; a file without "schema" is schema 0.
type Store struct {
	Path       string
	Schema     int
	Migrations map[int]Migration
}

// NewerSchemaError means the file was written by a newer NgiTool.
type NewerSchemaError struct {
	Path       string
	File, Want int
}

func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("%s has schema %d but this ngitool knows schema %d; it was written by a newer version — run: ngitool update", e.Path, e.File, e.Want)
}

// Load reads the file into v, running any migrations in memory first. It
// returns the schema the file had on disk (-1 when the file does not exist,
// leaving v untouched so callers can pre-fill defaults).
func (s Store) Load(v any) (from int, err error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	b = trimBOM(b)
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("%s is not valid JSON: %w", s.Path, err)
	}
	from = schemaOf(doc)
	if from > s.Schema {
		return from, &NewerSchemaError{Path: s.Path, File: from, Want: s.Schema}
	}
	for n := from; n < s.Schema; n++ {
		if m := s.Migrations[n]; m != nil {
			if err := m(doc); err != nil {
				return from, fmt.Errorf("migrate %s from schema %d to %d: %w", s.Path, n, n+1, err)
			}
		}
		doc["schema"] = n + 1
	}
	b, err = json.Marshal(doc)
	if err != nil {
		return from, err
	}
	return from, json.Unmarshal(b, v)
}

// Save writes v atomically with the current schema number.
func (s Store) Save(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	doc := map[string]any{}
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("state documents must be JSON objects: %w", err)
	}
	doc["schema"] = s.Schema
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(s.Path, append(out, '\n'), FileMode)
}

// Migrate loads the file and, when it was on an older schema, keeps a copy
// of the original next to it (<file>.schema-N.bak) and saves the upgraded
// version. Call it while holding the lock.
func (s Store) Migrate(v any) (migrated bool, err error) {
	from, err := s.Load(v)
	if err != nil || from < 0 || from == s.Schema {
		return false, err
	}
	orig, err := os.ReadFile(s.Path)
	if err != nil {
		return false, err
	}
	if err := WriteFile(fmt.Sprintf("%s.schema-%d.bak", s.Path, from), orig, FileMode); err != nil {
		return false, err
	}
	return true, s.Save(v)
}

func schemaOf(doc map[string]any) int {
	if f, ok := doc["schema"].(float64); ok {
		return int(f)
	}
	return 0
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
