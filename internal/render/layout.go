// Package render turns the state of one instance into nginx files, from
// templates embedded in the binary. Every file it produces starts with the
// NgiTool marker and a hash of its body (APPLY-05), and lands in the layout
// of its instance kind:
//
//	host   /etc/nginx/ngitool/{upstreams,servers,locations}/ + <hook dir>/ngitool.conf
//	edge   conf/sites/<host>.conf, conf/locations/<host>/, conf/conf.d/ngitool.conf
//	mount  <mounted conf dir>/ngitool/{upstreams,servers,locations}/ + <mounted conf dir>/ngitool.conf
//
// It only renders; internal/apply writes.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/nginxconf"
)

// Marker is the first line of every file NgiTool writes. Only files that
// start with it are ever rewritten or deleted (CONF-09).
const Marker = "# Managed by NgiTool — generated from state; edit with ngitool, changes here are overwritten"

// HashPrefix starts the second line: "# ngitool: <id> sha256:<hex>".
const HashPrefix = "# ngitool: "

// Layout is where one instance's files go, as nginx sees the paths.
type Layout struct {
	Kind      string
	Entry     string // the file the hook includes (conf.d/ngitool.conf, or the include line's target)
	Upstreams string // dir
	Servers   string // dir
	Locations string // dir; one subdir per host
	Snippet   string // the shared proxy snippet
	// Owned are the dirs NgiTool may delete managed files from.
	Owned []string
}

// Plan computes the layout an instance gets when it is adopted: the root
// dir and, when no include of conf.d/*.conf exists, the line to add.
func Plan(in *discover.Instance) (model.Adopted, error) {
	a := model.Adopted{ID: in.ID, Kind: in.Kind}
	if in.Summary == nil || in.Summary.Hook == nil {
		return a, fmt.Errorf("%s has no http {} block to hook into", in.Name)
	}
	h := in.Summary.Hook
	main := in.Conf
	if main == "" {
		main = "/etc/nginx/nginx.conf"
	}
	switch {
	case in.Kind == discover.KindEdge:
		a.Layout = model.LayoutEdge
		a.Root = path.Dir(firstNonEmpty(h.Dir, path.Dir(main)+"/conf.d"))
	case in.Kind == discover.KindHost:
		a.Layout = model.LayoutHost
		a.Root = path.Join(path.Dir(main), "ngitool")
	default:
		a.Layout = model.LayoutMount
		if h.NeedsLine {
			a.Root = path.Join(path.Dir(main), "ngitool")
		} else {
			a.Root = path.Join(h.Dir, "ngitool")
		}
	}
	hp, err := HostPath(in, a.Root, "")
	if err != nil {
		return a, err
	}
	a.HostRoot = hp
	return a, nil
}

// LayoutOf is the layout of an adopted instance.
func LayoutOf(in *discover.Instance, a *model.Adopted) Layout {
	h := &nginxconf.Hook{NeedsLine: true}
	if in.Summary != nil && in.Summary.Hook != nil {
		h = in.Summary.Hook
	}
	entry := path.Join(a.Root, "ngitool.conf")
	if !h.NeedsLine && a.Include == nil {
		entry = path.Join(h.Dir, "ngitool.conf")
	}
	if a.Layout == model.LayoutEdge {
		l := Layout{
			Kind:      a.Layout,
			Entry:     path.Join(a.Root, "conf.d", "ngitool.conf"),
			Upstreams: path.Join(a.Root, "ngitool", "upstreams"),
			Servers:   path.Join(a.Root, "sites"),
			Locations: path.Join(a.Root, "locations"),
			Snippet:   path.Join(a.Root, "snippets", "ngitool-proxy.conf"),
		}
		l.Owned = []string{path.Join(a.Root, "conf.d"), l.Upstreams, l.Servers, l.Locations, path.Join(a.Root, "snippets")}
		return l
	}
	l := Layout{
		Kind:      a.Layout,
		Entry:     entry,
		Upstreams: path.Join(a.Root, "upstreams"),
		Servers:   path.Join(a.Root, "servers"),
		Locations: path.Join(a.Root, "locations"),
		Snippet:   path.Join(a.Root, "proxy.conf"),
	}
	l.Owned = []string{path.Dir(entry), a.Root, l.Upstreams, l.Servers, l.Locations}
	return l
}

// IncludeLine is the line adopt adds inside http {} when there is no
// conf.d/*.conf include: the only edit to a hand-written file.
func IncludeLine(l Layout) string {
	return "    include " + l.Entry + ";  # NgiTool (ngitool instance release removes this line)"
}

// HostPath is where an nginx path really is on this machine: the same
// path for a host instance (under chroot in tests), through the bind
// mounts for a container (CONF-07).
func HostPath(in *discover.Instance, p, chroot string) (string, error) {
	if in.Kind == discover.KindHost {
		return filepath.Join(chroot, p), nil
	}
	var ms []nginxconf.Mount
	for _, m := range in.Mounts {
		if m.Type == "bind" || m.Type == "volume" {
			ms = append(ms, nginxconf.Mount{Dest: m.Dest, Source: m.Source})
		}
	}
	return nginxconf.FileSource{Container: true, Mounts: ms}.HostPath(p)
}

// Stamp puts the marker and the hash line on top of body.
func Stamp(id, body string) string {
	return Marker + "\n" + HashPrefix + id + " sha256:" + Sum(body) + "\n" + body
}

// Sum is the hex sha256 of a body.
func Sum(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

var hashRE = regexp.MustCompile(`^# ngitool: (\S+) sha256:([0-9a-f]{64})$`)

// Parsed is a managed file split into its header and body.
type Parsed struct {
	Managed bool   // starts with the NgiTool marker
	ID      string // from the hash line
	Want    string // hash in the header
	Body    string
	Edited  bool // the body no longer matches its hash (APPLY-05)
}

// Parse reads a file's header. Files without the marker are hand-written.
func Parse(content string) Parsed {
	first, rest, _ := strings.Cut(content, "\n")
	if strings.TrimRight(first, "\r") != Marker {
		return Parsed{Body: content}
	}
	p := Parsed{Managed: true}
	second, body, _ := strings.Cut(rest, "\n")
	m := hashRE.FindStringSubmatch(strings.TrimRight(second, "\r"))
	if m == nil {
		p.Body, p.Edited = rest, true
		return p
	}
	p.ID, p.Want, p.Body = m[1], m[2], body
	p.Edited = Sum(body) != p.Want
	return p
}

// Slug turns a path into a file name: /api/v1 → api-v1.
func Slug(p string) string { return strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "-") }

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
