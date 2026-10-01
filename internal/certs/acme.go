package certs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Check is the preflight result for one name: OK true (reached this
// server), false (would fail, with the reason), or unknown when the test
// file could not even be written.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Unknown bool   `json:"unknown,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// PreflightTimeout bounds each request, as the legacy CLI did.
const PreflightTimeout = 10 * time.Second

// Preflight answers, before Let's Encrypt is asked: can this server answer
// an HTTP-01 challenge for each name? It puts a file where certbot would
// (<acmeDir>/.well-known/acme-challenge/<token>) and fetches
// http://<name>/.well-known/acme-challenge/<token> exactly as Let's
// Encrypt will, following redirects. That catches DNS pointing elsewhere,
// a closed port 80, and Cloudflare's "Always Use HTTPS" — without spending
// rate limits (CERT-01). client may be nil (a default one is used).
func Preflight(ctx context.Context, acmeDir string, names []string, client *http.Client) []Check {
	if client == nil {
		client = &http.Client{Timeout: PreflightTimeout}
	}
	dir := filepath.Join(acmeDir, ".well-known", "acme-challenge")
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	token := "ngitool-check-" + hex.EncodeToString(b)
	file := filepath.Join(dir, token)
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = os.WriteFile(file, []byte(token), 0o644)
	}
	if err != nil {
		out := make([]Check, len(names))
		for i, n := range names {
			out[i] = Check{Name: n, Unknown: true, Reason: "could not write the test file (" + errText(err) + ")"}
		}
		return out
	}
	defer os.Remove(file)
	out := make([]Check, len(names))
	var wg sync.WaitGroup
	for i, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = fetchToken(ctx, client, n, token)
		}()
	}
	wg.Wait()
	return out
}

func fetchToken(ctx context.Context, client *http.Client, name, token string) Check {
	u := "http://" + name + "/.well-known/acme-challenge/" + token
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Check{Name: name, Reason: err.Error()}
	}
	res, err := client.Do(req)
	if err != nil {
		return Check{Name: name, Reason: netReason(err) + " — DNS or port 80 does not reach this server"}
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == token {
		return Check{Name: name, OK: true}
	}
	via := ""
	if final := res.Request.URL.String(); final != u {
		via = " (redirected to " + final + ")"
	}
	return Check{Name: name, Reason: "HTTP " + strconv.Itoa(res.StatusCode) + via + " — the request did not reach this nginx"}
}

// netReason is the short cause of a failed request: no such host,
// connection refused, timeout …
func netReason(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	var ue *url.Error
	switch {
	case errors.As(err, &dns):
		if dns.IsNotFound {
			return "ENOTFOUND (no DNS record)"
		}
		return "DNS lookup failed"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ue) && ue.Timeout():
		return "timeout"
	case errors.As(err, &op):
		if strings.Contains(op.Err.Error(), "refused") {
			return "ECONNREFUSED"
		}
		return op.Err.Error()
	}
	return err.Error()
}

func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// Failed are the checks that would fail.
func Failed(cs []Check) []Check {
	var out []Check
	for _, c := range cs {
		if !c.OK && !c.Unknown {
			out = append(out, c)
		}
	}
	return out
}

// PreflightFix is what to do when the preflight fails.
const PreflightFix = "point the DNS A record at this server, open port 80, and for Cloudflare-proxied hosts turn " +
	"\"Always Use HTTPS\" off — or use the DNS challenge (--challenge dns) (CERT-01)"

// ── certbot ─────────────────────────────────────────────────────────────────

// Issue is one Let's Encrypt request.
type Issue struct {
	Name        string   // --cert-name
	Names       []string // -d …
	Challenge   string   // http or dns
	Email       string
	Webroot     string // HTTP-01: the webroot as certbot sees it
	Credentials string // DNS-01: the cloudflare.ini as certbot sees it
	Propagation string // seconds
	Staging     bool   // the integration test never asks the real CA
	// Authenticator overrides HTTP-01's: "nginx" for host certbot with the
	// nginx plugin (CERT-05).
	Authenticator string
}

// ErrWildcardHTTP is HTTP-01 asked for a wildcard.
var ErrWildcardHTTP = errors.New("the HTTP challenge cannot issue wildcards — use the DNS challenge (--challenge dns)")

// CertbotArgs are certbot's arguments for an issue (certonly).
func (i Issue) CertbotArgs() ([]string, error) {
	if i.Email == "" {
		return nil, errors.New("no ACME email — set ACME_EMAIL (ngitool edge init) or pass --email")
	}
	var how []string
	switch i.Challenge {
	case HTTP01, "":
		for _, n := range i.Names {
			if strings.HasPrefix(n, "*.") {
				return nil, ErrWildcardHTTP
			}
		}
		switch i.Authenticator {
		case "nginx":
			how = []string{"--nginx"}
		default:
			how = []string{"--webroot", "--webroot-path", i.Webroot}
		}
	case DNS01:
		how = []string{"--dns-cloudflare", "--dns-cloudflare-credentials", i.Credentials,
			"--dns-cloudflare-propagation-seconds", firstNonEmpty(i.Propagation, "30")}
	default:
		return nil, fmt.Errorf("unknown challenge %q (http or dns)", i.Challenge)
	}
	args := append([]string{"certonly"}, how...)
	args = append(args, "--cert-name", i.Name)
	for _, n := range i.Names {
		args = append(args, "-d", n)
	}
	args = append(args, "--email", i.Email, "--agree-tos", "--no-eff-email", "--non-interactive", "--keep-until-expiring")
	if i.Staging {
		args = append(args, "--staging")
	}
	return args, nil
}

// RenewArgs: everything due, or one certificate; force renews regardless.
func RenewArgs(name string, force bool) []string {
	a := []string{"renew"}
	if name != "" {
		a = append(a, "--cert-name", name)
	}
	if force {
		a = append(a, "--force-renewal")
	}
	return a
}

// DeleteArgs removes a certificate from certbot's store.
func DeleteArgs(name string) []string {
	return []string{"delete", "--non-interactive", "--cert-name", name}
}

var certbotNoise = regexp.MustCompile(`^(Saving debug log|Ask for help|See the logfile|Some challenges have failed\.?$)`)

// ExplainCertbot is certbot's own error lines without its boilerplate,
// at most 12 of them.
func ExplainCertbot(stdout, stderr string) string {
	var keep []string
	for _, l := range strings.Split(stderr+"\n"+stdout, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !certbotNoise.MatchString(l) {
			keep = append(keep, l)
		}
	}
	if len(keep) > 12 {
		keep = keep[len(keep)-12:]
	}
	return strings.Join(keep, "\n")
}

var renewLineRE = regexp.MustCompile(`(?i)renew|skipped|success|fail|not due|error`)

// RenewSummary are the lines of certbot renew worth showing, per
// certificate (CERT-02), the last 10.
func RenewSummary(stdout, stderr string) []string {
	var out []string
	for _, l := range strings.Split(stdout+"\n"+stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" && renewLineRE.MatchString(l) {
			out = append(out, l)
		}
	}
	if len(out) > 10 {
		out = out[len(out)-10:]
	}
	return out
}

var renewFailRE = regexp.MustCompile(`(?m)/live/([^/\s]+)/fullchain\.pem \(failure\)`)

// RenewFailures are the certificate names certbot renew reported as
// failed.
func RenewFailures(out string) []string {
	var names []string
	for _, m := range renewFailRE.FindAllStringSubmatch(out, -1) {
		names = append(names, m[1])
	}
	sort.Strings(names)
	return names
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
