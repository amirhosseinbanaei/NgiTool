// Package certs is everything about TLS certificates NgiTool issues or
// imports: the four kinds the legacy edge CLI had (Let's Encrypt through
// certbot, Cloudflare Origin CA, custom, self-signed), reading and
// validating PEM files, name coverage with one-level wildcards, the
// HTTP-01 preflight that runs before Let's Encrypt is asked, and certbot's
// command lines. Keys are written 0600 and never printed.
package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Kinds.
const (
	LetsEncrypt = "letsencrypt"
	Origin      = "origin"
	Custom      = "custom"
	SelfSigned  = "self-signed"
)

// Kinds is every kind, in the order the wizard offers them.
var Kinds = []string{LetsEncrypt, Origin, Custom, SelfSigned}

// Challenges for Let's Encrypt.
const (
	HTTP01 = "http"
	DNS01  = "dns"
)

var kindLabels = map[string]string{
	LetsEncrypt: "Let's Encrypt",
	Origin:      "Cloudflare Origin CA",
	Custom:      "custom",
	SelfSigned:  "self-signed",
}

// ChallengeOf is how a certificate is validated: Let's Encrypt entries
// written before HTTP-01 existed are DNS-01; other kinds have none.
func ChallengeOf(kind, challenge string) string {
	if kind != LetsEncrypt {
		return ""
	}
	if challenge == "" {
		return DNS01
	}
	return challenge
}

// Label is "Let's Encrypt (HTTP)", "Cloudflare Origin CA", "custom" …
func Label(kind, challenge string) string {
	l, ok := kindLabels[kind]
	if !ok {
		l = kind
	}
	if ch := ChallengeOf(kind, challenge); ch != "" {
		l += " (" + strings.ToUpper(ch) + ")"
	}
	return l
}

// ── names ───────────────────────────────────────────────────────────────────

// NameCovers reports whether a certificate name (exact or "*.domain")
// covers host. Wildcards cover exactly one level (CERT-04).
func NameCovers(name, host string) bool {
	name, host = strings.ToLower(name), strings.ToLower(host)
	if name == host {
		return true
	}
	if base, ok := strings.CutPrefix(name, "*."); ok {
		return strings.HasSuffix(host, "."+base) && !strings.Contains(strings.TrimSuffix(host, "."+base), ".")
	}
	return false
}

// Covers reports whether any of names covers host.
func Covers(names []string, host string) bool {
	for _, n := range names {
		if NameCovers(n, host) {
			return true
		}
	}
	return false
}

// Missing are the hosts names do not cover.
func Missing(names, hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if !Covers(names, h) {
			out = append(out, h)
		}
	}
	return out
}

// DomainOf is the configured domain host belongs to (longest suffix), or "".
func DomainOf(host string, domains []string) string {
	best := ""
	for _, d := range domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

// DepthBelow counts the labels between host and domain:
// api.example.com → 1, a.b.example.com → 2, example.com → 0.
func DepthBelow(host, domain string) int {
	if host == domain {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(host, "."+domain), "."))
}

// GuessDomain is the last two labels, when no domain is configured.
func GuessDomain(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) <= 2 {
		return host
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// WildcardDepthNote explains why *.domain does not cover a host two levels
// down (CERT-04), or "" when it does not apply.
func WildcardDepthNote(host string, domains []string) string {
	d := DomainOf(host, domains)
	if d == "" {
		d = GuessDomain(host)
	}
	if DepthBelow(host, d) <= 1 {
		return ""
	}
	return host + " is " + strconv.Itoa(DepthBelow(host, d)) + " levels below " + d +
		": *." + d + " does not cover it, nor does Cloudflare's free edge certificate (CERT-04)"
}

// ── PEM ─────────────────────────────────────────────────────────────────────

// Info is what NgiTool shows about a certificate.
type Info struct {
	Names      []string  `json:"names"`
	NotBefore  time.Time `json:"notBefore"`
	NotAfter   time.Time `json:"notAfter"`
	Issuer     string    `json:"issuer"`
	SelfSigned bool      `json:"selfSigned,omitempty"`
}

// DaysLeft are whole days until NotAfter (negative once expired).
func (i Info) DaysLeft(now time.Time) int {
	return int(i.NotAfter.Sub(now).Hours() / 24)
}

// Leaf parses the first certificate of a PEM chain.
func Leaf(chain []byte) (*x509.Certificate, error) {
	rest := chain
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil, errors.New("no PEM certificate found")
		}
		if blk.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				return nil, fmt.Errorf("the certificate could not be read: %w", err)
			}
			return c, nil
		}
	}
}

// Inspect reads the leaf of a PEM chain.
func Inspect(chain []byte) (Info, error) {
	c, err := Leaf(chain)
	if err != nil {
		return Info{}, err
	}
	return infoOf(c), nil
}

func infoOf(c *x509.Certificate) Info {
	names := append([]string{}, c.DNSNames...)
	if len(names) == 0 && c.Subject.CommonName != "" {
		names = []string{c.Subject.CommonName}
	}
	issuer := c.Issuer.CommonName
	if len(c.Issuer.Organization) > 0 {
		issuer = c.Issuer.Organization[0]
	}
	return Info{Names: names, NotBefore: c.NotBefore, NotAfter: c.NotAfter, Issuer: issuer,
		SelfSigned: bytes.Equal(c.RawIssuer, c.RawSubject)}
}

// InspectFile reads a PEM chain from disk.
func InspectFile(p string) (Info, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Info{}, errors.New("certificate file missing")
		}
		return Info{}, err
	}
	return Inspect(b)
}

// ParseKey reads an unencrypted PEM private key (PKCS#1, PKCS#8, SEC 1).
func ParseKey(keyPEM []byte) (crypto.Signer, error) {
	rest := keyPEM
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			return nil, errors.New("the private key could not be read (PEM, unencrypted)")
		}
		if !strings.Contains(blk.Type, "PRIVATE KEY") {
			continue
		}
		if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
			if s, ok := k.(crypto.Signer); ok {
				return s, nil
			}
		}
		if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
			return k, nil
		}
		if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
			return k, nil
		}
		return nil, errors.New("the private key could not be read (PEM, unencrypted)")
	}
}

// ValidatePair checks a certificate chain and key before they are stored:
// both parse, the key belongs to the leaf, and it has not expired.
func ValidatePair(chain, keyPEM []byte, now time.Time) (Info, error) {
	c, err := Leaf(chain)
	if err != nil {
		return Info{}, err
	}
	k, err := ParseKey(keyPEM)
	if err != nil {
		return Info{}, err
	}
	if !samePublic(c.PublicKey, k.Public()) {
		return Info{}, errors.New("the private key does not belong to this certificate")
	}
	info := infoOf(c)
	if info.NotAfter.Before(now) {
		return info, fmt.Errorf("the certificate expired on %s", info.NotAfter.Format("2006-01-02"))
	}
	return info, nil
}

func samePublic(a, b crypto.PublicKey) bool {
	type eq interface{ Equal(crypto.PublicKey) bool }
	if e, ok := a.(eq); ok {
		return e.Equal(b)
	}
	return false
}

// ── files ───────────────────────────────────────────────────────────────────

// Files are where a certificate's PEM files are: on this machine, and as
// nginx sees them.
type Files struct {
	Cert, Key     string // host paths
	CertIn, KeyIn string // nginx paths
}

// FilesIn is the layout every kind shares: <dir>/<name>/fullchain.pem and
// privkey.pem; Let's Encrypt keeps its own live/<name>/ inside dir.
func FilesIn(hostDir, nginxDir, name string) Files {
	return Files{
		Cert: filepath.Join(hostDir, name, "fullchain.pem"), Key: filepath.Join(hostDir, name, "privkey.pem"),
		CertIn: nginxDir + "/" + name + "/fullchain.pem", KeyIn: nginxDir + "/" + name + "/privkey.pem",
	}
}

// Store writes a validated pair to <dir>/fullchain.pem and privkey.pem:
// the directory 0700, the key 0600, the chain 0644.
func Store(dir string, chain, keyPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "privkey.pem"), append(bytes.TrimSpace(keyPEM), '\n'), 0o600); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "fullchain.pem"), append(bytes.TrimSpace(chain), '\n'), 0o644)
}

func writeFile(p string, b []byte, mode os.FileMode) error {
	tmp := p + ".ngt-tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// FreeName is base, or base-2, base-3 … when taken says it is used.
func FreeName(base string, taken func(string) bool) string {
	name := base
	for i := 2; taken(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

// ── generating ──────────────────────────────────────────────────────────────

// SelfSignedPair makes a self-signed certificate for names (testing only:
// Cloudflare Full (strict) and every browser reject it, CERT-03).
func SelfSignedPair(names []string, days int, now time.Time) (chain, keyPEM []byte, err error) {
	if len(names) == 0 {
		return nil, nil, errors.New("no names")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              names,
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Duration(days) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return encodePair(der, key)
}

func encodePair(der []byte, key crypto.Signer) ([]byte, []byte, error) {
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}

// KeyAndCSR makes an RSA 2048 key and a CSR for names: what Cloudflare's
// Origin CA signs (request type origin-rsa).
func KeyAndCSR(names []string) (keyPEM, csrPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
	}, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
