package discover

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Scanner runs one scan, step by step, so the CLI can show progress.
type Scanner struct {
	env Env
	rep *Report

	procs          map[int]*proc
	selfNS         string
	foreignMasters []*proc
	kubelet        bool
	ingress        bool

	ctrs      []*ctr
	ctrByName map[string]*ctr
	listeners []listener

	inst   []*Instance
	builds map[string]*build
	mu     sync.Mutex
}

// New starts a scan of the machine env describes.
func New(env Env) *Scanner {
	return &Scanner{
		env:       env,
		rep:       &Report{Schema: Schema, ScannedAt: env.Now(), Root: env.Euid == 0, Findings: []Finding{}, Ports: []PortOwner{}, Instances: []Instance{}},
		ctrByName: map[string]*ctr{},
		builds:    map[string]*build{},
	}
}

// StepDef is one scan step: what it is called and what it counts.
type StepDef struct {
	Name string
	Unit string // "master", "container" … singular
	Run  func(context.Context) int
}

// Steps are the scan in order. Each returns how many things it found.
func (s *Scanner) Steps() []StepDef {
	return []StepDef{
		{"Host processes", "host instance", s.scanHost},
		{"Docker containers", "nginx container", s.scanDocker},
		{"Compose projects", "defined service", s.scanCompose},
		{"Port owners", "listening socket", s.scanPorts},
		{"Parsing configs", "config", s.parseAll},
	}
}

// RunStep runs one step and records its count and time.
func (s *Scanner) RunStep(ctx context.Context, d StepDef) Step {
	start := time.Now()
	n := d.Run(ctx)
	st := Step{Name: d.Name, Count: n, Unit: d.Unit, Took: time.Since(start)}
	s.rep.Steps = append(s.rep.Steps, st)
	return st
}

// Label is "Docker containers · 1 nginx container · 120ms".
func (st Step) Label() string {
	unit := st.Unit
	if st.Count != 1 {
		unit += "s"
	}
	return st.Name + " · " + strconv.Itoa(st.Count) + " " + unit + " · " + shortDuration(st.Took)
}

func shortDuration(d time.Duration) string {
	if d < time.Second {
		return strconv.Itoa(int(d.Milliseconds())) + "ms"
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}

// Scan runs every step and returns the report.
func Scan(ctx context.Context, env Env) *Report {
	s := New(env)
	for _, d := range s.Steps() {
		s.RunStep(ctx, d)
	}
	return s.Report()
}

// add registers an instance, keeping ids unique.
func (s *Scanner) add(in *Instance, b *build) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := in.ID
	for n := 2; s.builds[id] != nil; n++ {
		id = in.ID + "#" + strconv.Itoa(n)
	}
	in.ID = id
	s.inst = append(s.inst, in)
	s.builds[id] = b
	return id
}

func (s *Scanner) fail(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.rep.ReadFailures {
		if f == msg {
			return
		}
	}
	s.rep.ReadFailures = append(s.rep.ReadFailures, msg)
}

// parseAll reads every instance's config (four at a time), then decides
// capabilities, reachability, port owners and findings.
func (s *Scanner) parseAll(ctx context.Context) int {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, in := range s.inst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.loadConfig(ctx, in, s.builds[in.ID])
		}()
	}
	wg.Wait()
	n := 0
	for _, in := range s.inst {
		s.capabilities(in, s.builds[in.ID])
		s.reachability(in)
		if len(in.Files) > 0 {
			n++
		}
	}
	s.portOwners()
	s.findings()
	return n
}

var stateRank = map[string]int{StateRunning: 0, StateStopped: 1, StateDefined: 2}

// Report is the finished scan: front door first, then running, stopped,
// defined.
func (s *Scanner) Report() *Report {
	sort.SliceStable(s.inst, func(i, j int) bool {
		a, b := s.inst[i], s.inst[j]
		if a.FrontDoor != b.FrontDoor {
			return a.FrontDoor
		}
		if stateRank[a.State] != stateRank[b.State] {
			return stateRank[a.State] < stateRank[b.State]
		}
		return a.ID < b.ID
	})
	s.rep.Instances = s.rep.Instances[:0]
	for _, in := range s.inst {
		s.rep.Instances = append(s.rep.Instances, *in)
	}
	return s.rep
}
