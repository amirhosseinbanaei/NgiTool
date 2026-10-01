package render

import (
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
)

// Facts are what rendering needs to know about the instance beyond state.
type Facts struct {
	Instance *discover.Instance
	Adopted  *model.Adopted
	Layout   Layout
	// Resolve: nginx ≥ 1.27.3 takes `server name resolve` in upstream
	// blocks (LB-03). Older ones get the fallback.
	Resolve bool
	// HTTP2Directive: `http2 on;` exists (nginx ≥ 1.25.1); older versions
	// put http2 on the listen.
	HTTP2Directive bool
	Resolver       string // "127.0.0.11" in Docker, the host's nameserver otherwise
	Gateway        string // the host as a containerised instance reaches it (RP-06)
	HostNet        bool
	// UpgradeVar is the $connection_upgrade-style variable the routes use;
	// OwnMap means NgiTool renders the map itself (LB-06, RP-08).
	UpgradeVar string
	OwnMap     bool
	HTTPPort   int
	HTTPSPort  int
	IPv6       bool     // existing servers listen on [::] too (RP-15)
	EdgeExtras []string // edge stack snippets every site includes
	EdgeACME   string   // the edge stack's ACME challenge snippet, for :80 servers
	DefaultSSL bool     // no default server on the TLS port yet: add one that rejects unknown SNI (RP-18)
}

// MinResolve is the first nginx with `resolve` in open-source upstreams.
var MinResolve = [3]int{1, 27, 3}

var verRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// ParseVersion reads "nginx/1.27.3" (any variant) into numbers.
func ParseVersion(v string) ([3]int, bool) {
	m := verRE.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// AtLeast compares versions.
func AtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// SupportsResolve reports whether an nginx build takes `resolve` in an
// upstream: nginx and openresty by their core version, Angie always.
func SupportsResolve(version string) bool {
	if strings.HasPrefix(strings.ToLower(version), "angie/") {
		return true
	}
	v, ok := ParseVersion(version)
	return ok && AtLeast(v, MinResolve)
}

// GatherFacts reads the facts of an adopted instance from a scan.
func GatherFacts(rep *discover.Report, in *discover.Instance, a *model.Adopted) Facts {
	f := Facts{Instance: in, Adopted: a, Layout: LayoutOf(in, a), HTTPPort: 80, HTTPSPort: 443}
	f.Resolve = SupportsResolve(in.Version)
	if v, ok := ParseVersion(in.Version); ok {
		f.HTTP2Directive = AtLeast(v, [3]int{1, 25, 1})
	}
	f.HostNet = model.HostNetwork(in)
	if f.HostNet {
		f.Resolver = strings.Join(rep.Resolvers, " ")
		if f.Resolver == "" {
			f.Resolver = "127.0.0.53"
		}
	} else {
		f.Resolver = "127.0.0.11"
		f.Gateway = model.Gateway(rep, in)
	}
	if strings.Contains(in.Image, "unprivileged") || (in.Kind == discover.KindHost && in.User != "" && in.User != "root") {
		f.HTTPPort, f.HTTPSPort = 8080, 8443 // DISC-06
	}
	own := map[string]bool{}
	for _, fi := range in.Files {
		if fi.Managed == "ngitool" {
			own[fi.Path] = true
		}
	}
	f.UpgradeVar, f.OwnMap = "connection_upgrade", true
	if in.Summary != nil {
		for _, m := range in.Summary.Maps {
			if own[m.Pos.File] || m.Var != "connection_upgrade" {
				continue
			}
			// Reuse it when it gives "" without an Upgrade header, as
			// conf/conf.d/websocket.conf does; one that says "close" would
			// stop keepalive, so NgiTool brings its own variable.
			if m.Value("") == "" {
				f.UpgradeVar, f.OwnMap = "connection_upgrade", false
			} else {
				f.UpgradeVar = "ngt_connection_upgrade"
			}
		}
		f.DefaultSSL = true
		for _, srv := range in.Summary.Servers {
			if own[srv.Pos.File] {
				continue
			}
			for _, l := range srv.Listens {
				if l.IPv6 {
					f.IPv6 = true
				}
				if l.Port == f.HTTPSPort && l.Default {
					f.DefaultSSL = false
				}
			}
		}
	}
	if a.Layout == model.LayoutEdge {
		// Only snippets that exist: a missing non-glob include fails nginx -t.
		exists := func(p string) bool {
			hp, err := HostPath(in, p, "")
			if err != nil {
				return false
			}
			_, err = os.Stat(hp)
			return err == nil
		}
		if p := a.Root + "/snippets/security-headers.conf"; exists(p) {
			f.EdgeExtras = append(f.EdgeExtras, p)
		}
		if p := a.Root + "/snippets/acme-challenge.conf"; exists(p) {
			f.EdgeACME = p
		}
	}
	return f
}
