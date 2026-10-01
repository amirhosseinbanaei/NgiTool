package compose

import (
	"strconv"
	"strings"
)

// Lifecycle actions.
const (
	Up       = "up"
	Restart  = "restart"
	Recreate = "recreate"
	Rebuild  = "rebuild"
	Pull     = "pull"
	Stop     = "stop"
	Down     = "down"
	Logs     = "logs"
	PS       = "ps"
)

// Action is one lifecycle command with the explanation shown before it
// runs (DOCK-13, DOCK-14): what it does, what it affects, how to undo it.
type Action struct {
	ID      string
	Label   string
	Hint    string
	What    string
	Affects string
	Undo    string
	Options []string // option flags that apply, in the order they are asked
	// Services: the action takes a service list (down is project-wide).
	Services bool
}

// Actions in menu order.
var Actions = []Action{
	{ID: Up, Label: "Start / update", Hint: "up -d: create what is missing, recreate only what changed", Services: true,
		What:    "docker compose up -d: creates missing containers and recreates only those whose config changed. Unchanged containers keep running.",
		Affects: "the chosen services (and the services they depend on, unless --no-deps); a recreated container is down for a few seconds",
		Undo:    "stop or down stops them again; a changed config stays until the files change back",
		Options: []string{"build", "pull", "no-deps", "remove-orphans"}},
	{ID: Restart, Label: "Restart", Hint: "same containers, same config — does not apply changes", Services: true,
		What:    "docker compose restart: restarts the same containers. It does NOT apply changes to compose files, env or images — use recreate for that.",
		Affects: "the chosen services: each is down for a moment",
		Undo:    "nothing to undo: the containers are the same ones"},
	{ID: Recreate, Label: "Recreate", Hint: "up -d --force-recreate: new containers from the current config and env", Services: true,
		What:    "docker compose up -d --force-recreate: replaces the containers with new ones built from the current files and env.",
		Affects: "the chosen services: brief downtime per service; data outside volumes is lost with the old container",
		Undo:    "no way back to the old containers; recreate again after changing the files back",
		Options: []string{"no-deps"}},
	{ID: Rebuild, Label: "Rebuild", Hint: "build, then up -d — after code or Dockerfile changes", Services: true,
		What:    "docker compose build, then up -d: builds new images from the build contexts and replaces the containers that use them. BuildKit secrets and args are passed by compose itself; NgiTool adds nothing.",
		Affects: "services with a build: section; the build can take minutes and uses disk for the new image layers",
		Undo:    "the old image stays on disk untagged until pruned; there is no one-step undo",
		Options: []string{"no-cache", "pull-base", "no-deps"}},
	{ID: Pull, Label: "Pull", Hint: "pull newer images, then up -d", Services: true,
		What:    "docker compose pull, then up -d: downloads newer images for image-only services and recreates the containers whose image changed.",
		Affects: "services without a build: section (those are skipped); recreated containers are down for a few seconds",
		Undo:    "pin the previous tag in the compose file and run up again; the old image may still be on disk"},
	{ID: Stop, Label: "Stop", Hint: "stop the containers, keep them", Services: true,
		What:    "docker compose stop: stops the containers and keeps them, with their writable layers.",
		Affects: "the chosen services: their routes answer 502 until they are started again",
		Undo:    "start them: ngitool app up"},
	{ID: Down, Label: "Down", Hint: "remove the containers and the project network",
		What:    "docker compose down: removes the containers and the project's network. Named volumes are kept unless you choose to delete them.",
		Affects: "every service of the app: its routes answer 502 until it is started again",
		Undo:    "ngitool app up creates them again; deleted volumes cannot be brought back",
		Options: []string{"volumes", "remove-orphans"}},
	{ID: Logs, Label: "Logs", Hint: "follow the output — Ctrl-C returns", Services: true,
		What:    "docker compose logs: shows the services' output, following it until Ctrl-C.",
		Affects: "nothing: read-only",
		Undo:    "nothing to undo",
		Options: []string{"no-follow", "timestamps"}},
	{ID: PS, Label: "Containers", Hint: "state, health, networks, routes", Services: true,
		What:    "docker compose ps: lists the containers with their state.",
		Affects: "nothing: read-only",
		Undo:    "nothing to undo"},
}

// ActionOf returns the action id, or nil.
func ActionOf(id string) *Action {
	for i := range Actions {
		if Actions[i].ID == id {
			return &Actions[i]
		}
	}
	return nil
}

// Option is one action option: its flag and the one line that explains it.
type Option struct {
	Flag   string
	Label  string
	Hint   string
	Danger string // shown as a danger chip
}

// Options by flag name.
var Options = map[string]Option{
	"build":          {Flag: "--build", Label: "Build first", Hint: "build images before starting (up --build)"},
	"pull":           {Flag: "--pull", Label: "Pull images", Hint: "always pull newer images before starting (up --pull always)"},
	"no-deps":        {Flag: "--no-deps", Label: "Only these services", Hint: "do not start or recreate the services they depend on"},
	"remove-orphans": {Flag: "--remove-orphans", Label: "Remove orphans", Hint: "remove containers of services that are no longer in these files", Danger: "removes containers of services no longer in these files; with the wrong -f set that can include real services"},
	"no-cache":       {Flag: "--no-cache", Label: "No cache", Hint: "rebuild every layer: slow, but nothing stale survives"},
	"pull-base":      {Flag: "--pull", Label: "Pull base images", Hint: "fetch newer FROM images: slower, picks up security fixes"},
	"volumes":        {Flag: "--volumes", Label: "Delete volumes", Hint: "also remove the named volumes: the data in them is gone", Danger: "deletes the data in the project's named volumes"},
	"no-follow":      {Flag: "--no-follow", Label: "Don't follow", Hint: "print what is there and return"},
	"timestamps":     {Flag: "--timestamps", Label: "Timestamps", Hint: "prefix each line with its time"},
}

// Opts are the chosen options.
type Opts struct {
	Build, Pull, NoDeps, RemoveOrphans bool
	NoCache, PullBase, Volumes         bool
	NoFollow, Timestamps               bool
	Tail                               int    // logs; 0 = 100
	Since                              string // logs, e.g. 10m
}

// Set turns an option on by its key in Options.
func (o *Opts) Set(key string) {
	switch key {
	case "build":
		o.Build = true
	case "pull":
		o.Pull = true
	case "no-deps":
		o.NoDeps = true
	case "remove-orphans":
		o.RemoveOrphans = true
	case "no-cache":
		o.NoCache = true
	case "pull-base":
		o.PullBase = true
	case "volumes":
		o.Volumes = true
	case "no-follow":
		o.NoFollow = true
	case "timestamps":
		o.Timestamps = true
	}
}

// Steps are the compose argument lists an action runs, in order, after
// the project's global arguments. Services may be empty (all). Every
// command is a plain compose command; NgiTool never adds anything a user
// would not type themselves.
func Steps(action string, o Opts, services []string) [][]string {
	svc := func(a []string) []string { return append(a, services...) }
	upArgs := func(force bool) []string {
		a := []string{"up", "-d"}
		if force {
			a = append(a, "--force-recreate")
		}
		if o.Build {
			a = append(a, "--build")
		}
		if o.Pull {
			a = append(a, "--pull", "always")
		}
		if o.NoDeps {
			a = append(a, "--no-deps")
		}
		if o.RemoveOrphans {
			a = append(a, "--remove-orphans")
		}
		return svc(a)
	}
	switch action {
	case Up:
		return [][]string{upArgs(false)}
	case Restart:
		return [][]string{svc([]string{"restart"})}
	case Recreate:
		return [][]string{upArgs(true)}
	case Rebuild:
		b := []string{"build"}
		if o.NoCache {
			b = append(b, "--no-cache")
		}
		if o.PullBase {
			b = append(b, "--pull")
		}
		u := []string{"up", "-d"}
		if o.NoDeps {
			u = append(u, "--no-deps")
		}
		return [][]string{svc(b), svc(u)}
	case Pull:
		return [][]string{svc([]string{"pull"}), svc([]string{"up", "-d"})}
	case Stop:
		return [][]string{svc([]string{"stop"})}
	case Down:
		a := []string{"down"}
		if o.Volumes {
			a = append(a, "--volumes")
		}
		if o.RemoveOrphans {
			a = append(a, "--remove-orphans")
		}
		return [][]string{a}
	case Logs:
		a := []string{"logs"}
		if !o.NoFollow {
			a = append(a, "--follow")
		}
		tail := o.Tail
		if tail == 0 {
			tail = 100
		}
		a = append(a, "--tail", strconv.Itoa(tail))
		if o.Since != "" {
			a = append(a, "--since", o.Since)
		}
		if o.Timestamps {
			a = append(a, "--timestamps")
		}
		return [][]string{svc(a)}
	case PS:
		return [][]string{svc([]string{"ps", "-a", "--format", "json"})}
	}
	return nil
}

// Argv is the full argument list of one step for bin.
func Argv(b Bin, p Project, step []string) []string { return b.Argv(append(p.Args(), step...)) }

// CommandLine is the step as a person would type it, quoted where needed.
func CommandLine(b Bin, p Project, step []string) string {
	parts := []string{b.Name}
	for _, a := range Argv(b, p, step) {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@+%", r)) {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}
