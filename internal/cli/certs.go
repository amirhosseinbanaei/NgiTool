package cli

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/render"
)

// certCand is a certificate that already exists on an instance: found in
// its config or certificate directories, or issued by NgiTool (cert add).
type certCand struct {
	model.TLS
	Expires time.Time
	Source  string // where it was found
}

// certDirs are directories (as nginx sees them) holding one subdir per
// certificate with fullchain.pem and privkey.pem: Let's Encrypt, and the
// edge stack's data/certs mount.
var certDirs = []string{"/etc/letsencrypt/live", "/etc/edge-certs"}

// findCerts lists every certificate an instance can use: ssl_certificate
// lines the parser found, then the certificate directories.
func findCerts(in *discover.Instance) []certCand {
	var out []certCand
	seen := map[string]bool{}
	add := func(name, cert, key, src string) {
		if cert == "" || key == "" || seen[cert] || strings.Contains(cert, "$") {
			return
		}
		seen[cert] = true
		c := certCand{TLS: model.TLS{Name: name, Cert: cert, Key: key}, Source: src}
		if hp, err := render.HostPath(in, cert, ""); err == nil {
			c.Names, c.Expires = readCert(hp)
		}
		out = append(out, c)
	}
	if in.Summary != nil {
		for _, s := range in.Summary.Servers {
			if s.SSLCert != "" {
				add(certName(s.SSLCert), s.SSLCert, s.SSLKey, "ssl_certificate in "+s.Pos.String())
			}
		}
	}
	dirs := append([]string{}, certDirs...)
	for _, m := range in.Mounts {
		if strings.HasSuffix(m.Source, "/data/certs") || strings.HasSuffix(m.Source, "/data/letsencrypt") {
			d := m.Dest
			if strings.HasSuffix(m.Source, "/data/letsencrypt") {
				d = path.Join(d, "live")
			}
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		hd, err := render.HostPath(in, d, "")
		if err != nil {
			continue
		}
		ents, err := os.ReadDir(hd)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				continue
			}
			cert, key := path.Join(d, e.Name(), "fullchain.pem"), path.Join(d, e.Name(), "privkey.pem")
			if _, err := os.Stat(path.Join(hd, e.Name(), "fullchain.pem")); err != nil {
				continue
			}
			add(e.Name(), cert, key, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func certName(p string) string {
	if path.Base(p) == "fullchain.pem" || path.Base(p) == "cert.pem" {
		return path.Base(path.Dir(p))
	}
	return strings.TrimSuffix(path.Base(p), path.Ext(p))
}

// readCert reads the DNS names and expiry of the first certificate in a
// PEM file.
func readCert(p string) ([]string, time.Time) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, time.Time{}
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, time.Time{}
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, time.Time{}
	}
	names := append([]string{}, c.DNSNames...)
	if len(names) == 0 && c.Subject.CommonName != "" {
		names = []string{c.Subject.CommonName}
	}
	return names, c.NotAfter
}

// covering are the candidates whose names cover every name of the route.
func covering(cs []certCand, names []string) []certCand {
	var out []certCand
	for _, c := range cs {
		ok := len(c.Names) > 0
		for _, n := range names {
			ok = ok && model.Covers(c.Names, n)
		}
		if ok {
			out = append(out, c)
		}
	}
	return out
}
