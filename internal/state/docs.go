package state

import "github.com/amirhosseinbanaei/NgiTool/internal/paths"

// Current schema numbers. Bump one and add a Migrations entry for the old
// number whenever a document's shape changes (edge case SYS-07).
const (
	ConfigSchema = 1
	StateSchema  = 1
)

// Config is /etc/ngitool/config.json.
type Config struct {
	Schema      int      `json:"schema"`
	UpdateCheck bool     `json:"updateCheck"`         // background "new version" notice
	Channel     string   `json:"channel"`             // "stable" or "prerelease"
	ScanRoots   []string `json:"scanRoots"`           // where compose projects are looked for
	FrontDoor   string   `json:"frontDoor,omitempty"` // default instance that owns :80/:443
	Pinned      string   `json:"pinned,omitempty"`    // set by `update --version`, cleared by a plain update
}

// DefaultConfig is what a missing config.json means.
func DefaultConfig() Config {
	return Config{
		Schema:      ConfigSchema,
		UpdateCheck: true,
		Channel:     "stable",
		ScanRoots:   []string{"/home", "/root", "/opt", "/srv"},
	}
}

// State is /var/lib/ngitool/state.json: what NgiTool manages. Later prompts
// add instances, routes and pools here.
type State struct {
	Schema int `json:"schema"`
}

// Schema 0 is a file written before "schema" existed; it only needs the stamp.
var (
	ConfigMigrations = map[int]Migration{0: func(map[string]any) error { return nil }}
	StateMigrations  = map[int]Migration{0: func(map[string]any) error { return nil }}
)

func ConfigStore(p paths.Paths) Store {
	return Store{Path: p.Config, Schema: ConfigSchema, Migrations: ConfigMigrations}
}

func StateStore(p paths.Paths) Store {
	return Store{Path: p.State, Schema: StateSchema, Migrations: StateMigrations}
}

// LoadConfig returns the config with defaults for anything missing.
func LoadConfig(p paths.Paths) (Config, error) {
	c := DefaultConfig()
	_, err := ConfigStore(p).Load(&c)
	return c, err
}
