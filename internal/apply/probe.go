package apply

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
)

// Target is one route as the probe requests it.
type Target struct {
	Route string
	Host  string // Host header (and SNI)
	Path  string
	TLS   bool
	Addr  string   // host:port of the instance's own listener
	Kinds []string // member kinds, for the 502 hint
}

// Probe is what one request found.
type Probe struct {
	Route   string        `json:"route"`
	URL     string        `json:"url"`
	Status  int           `json:"status,omitempty"`
	Err     string        `json:"error,omitempty"`
	Took    time.Duration `json:"took"`
	Reached bool          `json:"reached"` // anything but 502, 503, 504
	Skipped string        `json:"skipped,omitempty"`
	At      time.Time     `json:"at"`
}

// Prober requests a route through its instance.
type Prober interface {
	Probe(ctx context.Context, t Target) Probe
}

// HTTPProber is the real one: a Go HTTP request to the instance's own
// listener with the route's Host header; TLS is not verified (the probe
// asks "does the route reach its upstream", not "is the cert valid").
type HTTPProber struct{ Timeout time.Duration }

func (h HTTPProber) Probe(ctx context.Context, t Target) Probe {
	p := Probe{Route: t.Route, At: time.Now()}
	scheme := "http"
	if t.TLS {
		scheme = "https"
	}
	p.URL = scheme + "://" + t.Host + firstNonEmpty(t.Path, "/")
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 8 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, t.Addr)
		},
		TLSClientConfig: &tls.Config{ServerName: t.Host, InsecureSkipVerify: true},
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		p.Err = err.Error()
		return p
	}
	req.Header.Set("User-Agent", "ngitool-probe")
	start := time.Now()
	res, err := client.Do(req)
	p.Took = time.Since(start)
	if err != nil {
		p.Err = err.Error()
		return p
	}
	_ = res.Body.Close()
	p.Status = res.StatusCode
	p.Reached = res.StatusCode != 502 && res.StatusCode != 503 && res.StatusCode != 504
	return p
}

// ProbeAddr is where the instance's own listener answers from this host:
// 127.0.0.1 for host nginx and host-network containers, the published
// port for a container. Without one the probe is skipped, not faked.
func ProbeAddr(in *discover.Instance, port int) (string, string) {
	if model.HostNetwork(in) {
		return "127.0.0.1:" + strconv.Itoa(port), ""
	}
	for _, p := range in.Ports {
		if p.ContainerPort == port && p.Proto == "tcp" && p.HostPort > 0 {
			ip := p.HostIP
			if ip == "" || ip == "0.0.0.0" || ip == "::" {
				ip = "127.0.0.1"
			}
			return net.JoinHostPort(ip, strconv.Itoa(p.HostPort)), ""
		}
	}
	return "", in.Name + " publishes no port for " + strconv.Itoa(port) + " — probe skipped; check from a container on its network"
}

// Causes are the likely reasons for a 502/503/504 per member kind.
func Causes(status int, kinds []string) []string {
	var out []string
	switch status {
	case 504:
		return []string{"the upstream accepted the connection but did not answer in time: raise --read-timeout, or check the app"}
	case 503:
		out = append(out, "every member is down, drained or at max_conns (LB-11)")
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if seen[k] {
			continue
		}
		seen[k] = true
		switch k {
		case model.KindContainer:
			out = append(out, "container member: it is stopped, not on a network the instance shares (RP-05), or listens on another port")
		case model.KindService:
			out = append(out, "compose service: a plain `docker compose up` may have dropped it from the shared network (DOCK-07), or it listens on another port")
		case model.KindHostPort:
			out = append(out, "host port: nothing listens there, it listens on 127.0.0.1 only, or a firewall blocks the docker bridge (RP-06)")
		case model.KindAddress:
			out = append(out, "address: unreachable from the instance, or http/https mismatch (LB-16)")
		case model.KindUnix:
			out = append(out, "unix socket: missing, or nginx workers cannot open it (RP-16)")
		}
	}
	return out
}

// CloudflareHints explain the errors Cloudflare shows in front of an
// origin (RP-21).
var CloudflareHints = map[int]string{
	520: "the origin answered with something Cloudflare could not read: an empty reply, oversized headers (RP-09) or a crash",
	521: "the origin refused the connection: nginx is down or the firewall blocks Cloudflare's ranges on 80/443",
	522: "the connection to the origin timed out: wrong DNS target, a firewall dropping packets, or an overloaded server",
	523: "Cloudflare cannot route to the origin: the DNS record points at an address that does not exist",
	524: "the origin took more than 100 s to answer: the app is slow; move long work to a background job",
	525: "the TLS handshake with the origin failed: SSL mode Full/Strict but nginx has no certificate for that name (RP-18)",
	526: "the origin's certificate is invalid for SSL mode Full (strict): use a Cloudflare Origin CA or a publicly trusted certificate",
	530: "Cloudflare returned 530 with a 1xxx error: usually a DNS or Authenticated Origin Pulls problem",
}

// Health is the last probe of every route and member, kept in the cache
// (not state) for `route ls` and `pool check`.
type Health struct {
	Routes  map[string]Probe        `json:"routes"`
	Members map[string]MemberHealth `json:"members"` // "<pool> <member label>"
}

// MemberHealth is one member's last direct check (LB-08).
type MemberHealth struct {
	OK     bool          `json:"ok"`
	Status int           `json:"status,omitempty"`
	Took   time.Duration `json:"took"`
	Err    string        `json:"error,omitempty"`
	At     time.Time     `json:"at"`
}

// HealthPath is the cache file.
func HealthPath(cacheDir string) string { return filepath.Join(cacheDir, "health.json") }

// LoadHealth reads the cache; a missing or broken file is empty.
func LoadHealth(cacheDir string) *Health {
	h := &Health{Routes: map[string]Probe{}, Members: map[string]MemberHealth{}}
	if b, err := os.ReadFile(HealthPath(cacheDir)); err == nil {
		_ = json.Unmarshal(b, h)
	}
	if h.Routes == nil {
		h.Routes = map[string]Probe{}
	}
	if h.Members == nil {
		h.Members = map[string]MemberHealth{}
	}
	return h
}

// Save writes the cache; failing to is not an error worth stopping for.
func (h *Health) Save(cacheDir string) {
	if b, err := json.Marshal(h); err == nil {
		_ = state.WriteFile(HealthPath(cacheDir), b, state.FileMode)
	}
}

// ProbeTargets are the requests that check routes on an instance.
func ProbeTargets(st *model.State, in *discover.Instance, ids []string, httpPort, httpsPort int) ([]Target, []Probe) {
	var ts []Target
	var skipped []Probe
	for _, id := range ids {
		r := st.Route(id)
		if r == nil || !r.Enabled || r.Instance != in.ID {
			continue
		}
		host := r.Host
		switch model.HostKind(host) {
		case "regex":
			skipped = append(skipped, Probe{Route: id, Skipped: "regex server name: nothing to request"})
			continue
		case "wildcard":
			host = "probe" + strings.TrimPrefix(host, "*")
		}
		port, isTLS := httpPort, false
		if r.TLS != nil {
			port, isTLS = httpsPort, true
		}
		addr, why := ProbeAddr(in, port)
		if why != "" {
			skipped = append(skipped, Probe{Route: id, Skipped: why})
			continue
		}
		t := Target{Route: id, Host: host, Path: r.Path + "/", TLS: isTLS, Addr: addr}
		if r.Path == "" {
			t.Path = "/"
		}
		if p := st.Pool(r.Pool); p != nil {
			for _, m := range p.Members {
				t.Kinds = append(t.Kinds, m.Kind)
			}
		}
		ts = append(ts, t)
	}
	return ts, skipped
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
