// Package discover finds every nginx on this machine — host masters, plain
// containers, compose services (running, stopped or only defined) and
// NgiTool's own edge stack — reads each one's config, and reports what
// NgiTool may do to it and why. It only reads: nothing is written,
// reloaded, started or stopped.
package discover

import (
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/driver"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// Schema of the cached report (/var/cache/ngitool/scan.json).
const Schema = 1

// Instance kinds.
const (
	KindHost      = "host"      // nginx installed on the host
	KindContainer = "container" // a plain `docker run` container
	KindCompose   = "compose"   // a service of a compose project
	KindEdge      = "edge"      // NgiTool's own edge stack
)

// Instance states.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateDefined = "defined" // in a compose file, never created (DISC-09)
)

// managedBy values; other managers are "other:<name>".
const (
	ManagedNgiTool = "ngitool"
	ManagedEdge    = "edge"
	ManagedNone    = "none"
)

// Capability is one thing NgiTool may do. Reason says why not when OK is
// false; Note adds what someone might misread when it is true (a :ro mount
// that does not block writing from the host, a single-file mount).
type Capability struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

func yes(note string) Capability  { return Capability{OK: true, Note: note} }
func no(reason string) Capability { return Capability{Reason: reason} }

// Capabilities are read, test, reload and write.
type Capabilities struct {
	Read   Capability `json:"read"`
	Test   Capability `json:"test"`
	Reload Capability `json:"reload"`
	Write  Capability `json:"write"`
}

// Mount is a container mount (CONF-07).
type Mount struct {
	Type   string `json:"type"` // bind, volume, tmpfs
	Name   string `json:"name,omitempty"`
	Source string `json:"source,omitempty"` // on the host
	Dest   string `json:"dest"`             // in the container
	RW     bool   `json:"rw"`
	File   bool   `json:"file,omitempty"` // a single file, not a directory
}

// Published is a port a container publishes on the host.
type Published struct {
	HostIP        string `json:"hostIp"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"`
}

// Public reports whether the port is reachable on every address.
func (p Published) Public() bool { return p.HostIP == "" || p.HostIP == "0.0.0.0" || p.HostIP == "::" }

// Reach is whether a proxy target can be reached from its instance.
type Reach struct {
	Status string `json:"status"` // ok, warn, err, unknown
	Why    string `json:"why,omitempty"`
}

// Reach statuses.
const (
	ReachOK      = "ok"
	ReachWarn    = "warn"
	ReachErr     = "err"
	ReachUnknown = "unknown"
)

// Instance is one nginx.
type Instance struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	State     string `json:"state"`
	Variant   string `json:"variant,omitempty"` // nginx, openresty, angie, tengine
	Version   string `json:"version,omitempty"` // "nginx/1.27.3"
	Owner     string `json:"owner,omitempty"`
	FrontDoor bool   `json:"frontDoor,omitempty"`
	ManagedBy string `json:"managedBy"`

	// Host masters.
	PID     int    `json:"pid,omitempty"`
	Exe     string `json:"exe,omitempty"`
	Unit    string `json:"unit,omitempty"`
	Prefix  string `json:"prefix,omitempty"`
	Globals string `json:"globals,omitempty"`
	Package string `json:"package,omitempty"`
	User    string `json:"user,omitempty"` // the master's user when not root

	// Containers and compose services.
	Container   string      `json:"container,omitempty"`
	Image       string      `json:"image,omitempty"`
	Bin         string      `json:"bin,omitempty"` // nginx binary inside the container
	Project     string      `json:"project,omitempty"`
	Service     string      `json:"service,omitempty"`
	WorkingDir  string      `json:"workingDir,omitempty"`
	ComposeFile []string    `json:"composeFiles,omitempty"`
	NetworkMode string      `json:"networkMode,omitempty"`
	Networks    []string    `json:"networks,omitempty"`
	Ports       []Published `json:"ports,omitempty"`
	Mounts      []Mount     `json:"mounts,omitempty"`
	Templates   bool        `json:"templates,omitempty"`  // /etc/nginx/templates mounted (CONF-08)
	Unresolved  bool        `json:"unresolved,omitempty"` // compose config failed; read from raw YAML

	// Config.
	Conf         string               `json:"conf,omitempty"`   // main file as nginx sees it
	Source       string               `json:"source,omitempty"` // dump or files
	Files        []nginxconf.FileInfo `json:"files,omitempty"`
	ConfigErrors []string             `json:"configErrors,omitempty"`
	Valid        *bool                `json:"valid,omitempty"` // nil: not tested
	TestOutput   string               `json:"testOutput,omitempty"`
	StockMain    bool                 `json:"stockMain,omitempty"` // main file assumed from the image (only conf.d mounted)
	Summary      *nginxconf.Summary   `json:"summary,omitempty"`
	Reach        map[string]Reach     `json:"reach,omitempty"` // by target file:line and address

	Caps    Capabilities   `json:"capabilities"`
	Methods driver.Methods `json:"methods"`
	Notes   []string       `json:"notes,omitempty"`
}

// Counts is "N servers · M upstreams".
func (in Instance) Counts() (servers, upstreams int) {
	if in.Summary == nil {
		return 0, 0
	}
	return len(in.Summary.Servers), len(in.Summary.Upstreams)
}

// Severity of a finding.
const (
	SevError = "error"
	SevWarn  = "warn"
	SevInfo  = "info"
)

// Finding is something worth knowing across instances, with a one-line fix.
type Finding struct {
	Severity  string   `json:"severity"`
	Code      string   `json:"code"` // edge-case ID, e.g. RP-03
	Message   string   `json:"message"`
	Fix       string   `json:"fix,omitempty"`
	Instances []string `json:"instances,omitempty"`
}

// PortOwner is who listens on a host port (DISC-15).
type PortOwner struct {
	Addr      string `json:"addr"`
	Port      int    `json:"port"`
	PID       int    `json:"pid,omitempty"`
	Process   string `json:"process,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Container string `json:"container,omitempty"`
}

// DockerStatus is how far Docker can be used (DISC-13).
type DockerStatus struct {
	State   string `json:"state"` // ok, rootless, missing, down, denied, podman, slow
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// Usable reports whether containers can be listed.
func (d DockerStatus) Usable() bool { return d.State == "ok" || d.State == "rootless" }

// Step is one finished scan step.
type Step struct {
	Name  string        `json:"name"`
	Count int           `json:"count"`
	Unit  string        `json:"unit"`
	Took  time.Duration `json:"took"`
}

// Report is a whole scan.
type Report struct {
	Schema       int          `json:"schema"`
	ScannedAt    time.Time    `json:"scannedAt"`
	Root         bool         `json:"root"`
	Instances    []Instance   `json:"instances"`
	Findings     []Finding    `json:"findings"`
	Ports        []PortOwner  `json:"ports"`
	FrontDoor    string       `json:"frontDoor,omitempty"`
	Docker       DockerStatus `json:"docker"`
	Host         string       `json:"host"` // what host discovery found, or why nothing
	PortsStatus  string       `json:"portsStatus,omitempty"`
	ComposeFiles int          `json:"composeFiles"`
	ReadFailures []string     `json:"readFailures,omitempty"` // DISC-14
	Steps        []Step       `json:"steps"`
	// Containers and Listeners are every container and listening socket,
	// not only nginx ones: route targets are picked from them (prompt 3).
	Containers []Container `json:"containers,omitempty"`
	Listeners  []PortOwner `json:"listeners,omitempty"`
	Resolvers  []string    `json:"resolvers,omitempty"` // the host's nameservers (/etc/resolv.conf)
}

// Container is any container on this machine, as a route target sees it.
// Container IPs are never kept (DOCK-16): nginx reaches containers by name.
type Container struct {
	Name        string              `json:"name"`
	Image       string              `json:"image"`
	State       string              `json:"state"`
	Running     bool                `json:"running"`
	Project     string              `json:"project,omitempty"`
	Service     string              `json:"service,omitempty"`
	NetworkMode string              `json:"networkMode,omitempty"`
	Networks    []string            `json:"networks,omitempty"`
	DNS         map[string][]string `json:"dns,omitempty"`      // network → names that resolve to it
	Gateways    map[string]string   `json:"gateways,omitempty"` // network → bridge gateway (the host, RP-06)
	Exposed     []int               `json:"exposed,omitempty"`  // container ports: EXPOSE and publishes
	Ports       []Published         `json:"ports,omitempty"`
	Instance    string              `json:"instance,omitempty"` // set when it is an nginx instance
}

// Container returns the container named name, or nil.
func (r *Report) Container(name string) *Container {
	for i := range r.Containers {
		if r.Containers[i].Name == name {
			return &r.Containers[i]
		}
	}
	return nil
}

// Find returns the instance with id, or nil.
func (r *Report) Find(id string) *Instance {
	for i := range r.Instances {
		if r.Instances[i].ID == id {
			return &r.Instances[i]
		}
	}
	return nil
}
