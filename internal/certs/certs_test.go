package certs

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// legacy: "wildcards cover exactly one level" (cli/src/targets.mjs:290-300)
func TestWildcardsCoverExactlyOneLevel(t *testing.T) {
	if !NameCovers("*.example.com", "api.example.com") {
		t.Error("*.example.com covers api.example.com")
	}
	if NameCovers("*.example.com", "example.com") {
		t.Error("*.example.com does not cover the apex")
	}
	if NameCovers("*.example.com", "a.b.example.com") {
		t.Error("*.example.com does not cover two levels down (CERT-04)")
	}
	if !Covers([]string{"example.com", "*.example.com"}, "example.com") {
		t.Error("the apex name covers the apex")
	}
	if !NameCovers("API.Example.com", "api.example.com") {
		t.Error("names compare case-insensitively")
	}
	if got := Missing([]string{"*.example.com"}, []string{"a.example.com", "a.b.example.com"}); !reflect.DeepEqual(got, []string{"a.b.example.com"}) {
		t.Errorf("Missing: %v", got)
	}
}

// legacy: "domainOf picks the longest configured suffix"
func TestDomainOfAndDepth(t *testing.T) {
	if d := DomainOf("a.shop.example.com", []string{"example.com", "shop.example.com"}); d != "shop.example.com" {
		t.Errorf("DomainOf: %s", d)
	}
	if d := DomainOf("other.org", []string{"example.com"}); d != "" {
		t.Errorf("DomainOf other: %q", d)
	}
	if n := DepthBelow("a.b.example.com", "example.com"); n != 2 {
		t.Errorf("DepthBelow: %d", n)
	}
	if n := DepthBelow("example.com", "example.com"); n != 0 {
		t.Errorf("DepthBelow apex: %d", n)
	}
	if note := WildcardDepthNote("a.b.example.com", []string{"example.com"}); !strings.Contains(note, "CERT-04") {
		t.Errorf("note: %q", note)
	}
	if note := WildcardDepthNote("api.example.com", nil); note != "" {
		t.Errorf("one level needs no note: %q", note)
	}
}

// legacy: "letsencrypt entries without a challenge are DNS-01; labels say which"
func TestChallengeAndLabel(t *testing.T) {
	if ChallengeOf(LetsEncrypt, "") != DNS01 || ChallengeOf(LetsEncrypt, "http") != HTTP01 || ChallengeOf(Origin, "") != "" {
		t.Error("ChallengeOf")
	}
	if l := Label(LetsEncrypt, "http"); l != "Let's Encrypt (HTTP)" {
		t.Errorf("Label: %q", l)
	}
	if l := Label(Custom, ""); l != "custom" {
		t.Errorf("Label custom: %q", l)
	}
}

func TestPEMValidation(t *testing.T) {
	now := time.Now()
	chain, key, err := SelfSignedPair([]string{"example.com", "www.example.com"}, 90, now)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ValidatePair(chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(info.Names, []string{"example.com", "www.example.com"}) || !info.SelfSigned || info.DaysLeft(now) < 88 {
		t.Errorf("info: %+v", info)
	}
	_, other, _ := SelfSignedPair([]string{"example.com"}, 90, now)
	if _, err := ValidatePair(chain, other, now); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Errorf("mismatched key: %v", err)
	}
	if _, err := ValidatePair(chain, []byte("garbage"), now); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("bad key: %v", err)
	}
	if _, err := ValidatePair([]byte("nope"), key, now); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("bad cert: %v", err)
	}
	if _, err := ValidatePair(chain, key, now.Add(100*24*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	k, csr, err := KeyAndCSR([]string{"example.com"})
	if err != nil || !strings.Contains(string(csr), "CERTIFICATE REQUEST") || !strings.Contains(string(k), "PRIVATE KEY") {
		t.Errorf("CSR: %v", err)
	}
}

func TestStoreKeepsTheKeyPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "example.com")
	chain, key, _ := SelfSignedPair([]string{"example.com"}, 30, time.Now())
	if err := Store(dir, chain, key); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"privkey.pem": 0o600, "fullchain.pem": 0o644} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v", name, st.Mode().Perm(), err)
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir %v", st.Mode().Perm())
	}
	info, err := InspectFile(filepath.Join(dir, "fullchain.pem"))
	if err != nil || info.Names[0] != "example.com" {
		t.Errorf("inspect: %+v %v", info, err)
	}
	if _, err := InspectFile(filepath.Join(dir, "nope.pem")); err == nil || err.Error() != "certificate file missing" {
		t.Errorf("missing: %v", err)
	}
	if n := FreeName("example.com", func(s string) bool { return s == "example.com" || s == "example.com-2" }); n != "example.com-3" {
		t.Errorf("FreeName: %s", n)
	}
}

// dialTo sends every request to addr whatever its host: the preflight
// fetches http://<name>/… exactly as Let's Encrypt would.
func dialTo(addr string) *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}}
}

// CERT-01: the HTTP-01 preflight, against an httptest server playing nginx.
func TestPreflightHTTP01(t *testing.T) {
	acme := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "ok.example.com":
			http.ServeFile(w, r, filepath.Join(acme, strings.TrimPrefix(r.URL.Path, "/")))
		case "redirect.example.com":
			if r.URL.Path == "/elsewhere" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			http.Redirect(w, r, "http://redirect.example.com/elsewhere", http.StatusMovedPermanently)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	got := Preflight(context.Background(), acme, []string{"ok.example.com", "other.example.com", "redirect.example.com"}, dialTo(srv.Listener.Addr().String()))
	if !got[0].OK {
		t.Errorf("ok: %+v", got[0])
	}
	if got[1].OK || !strings.Contains(got[1].Reason, "HTTP 404") || !strings.Contains(got[1].Reason, "did not reach this nginx") {
		t.Errorf("404: %+v", got[1])
	}
	if got[2].OK || !strings.Contains(got[2].Reason, "redirected to http://redirect.example.com/elsewhere") {
		t.Errorf("redirect: %+v", got[2])
	}
	if len(Failed(got)) != 2 {
		t.Errorf("Failed: %+v", Failed(got))
	}
	// The test file is gone afterwards.
	if ents, _ := os.ReadDir(filepath.Join(acme, ".well-known", "acme-challenge")); len(ents) != 0 {
		t.Errorf("token left behind: %v", ents)
	}
	// Nothing listening: DNS or port 80 does not reach this server.
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close()
	got = Preflight(context.Background(), acme, []string{"down.example.com"}, dialTo(addr))
	if got[0].OK || !strings.Contains(got[0].Reason, "DNS or port 80 does not reach this server") {
		t.Errorf("refused: %+v", got[0])
	}
	// A webroot that cannot be written: unknown, not failed.
	got = Preflight(context.Background(), "/proc/ngitool-nope", []string{"x.example.com"}, nil)
	if !got[0].Unknown || len(Failed(got)) != 0 {
		t.Errorf("unwritable: %+v", got)
	}
}

func TestCertbotArguments(t *testing.T) {
	http01 := Issue{Name: "example.com", Names: []string{"example.com", "www.example.com"}, Challenge: HTTP01, Email: "a@example.com", Webroot: "/var/acme"}
	args, err := http01.CertbotArgs()
	want := "certonly --webroot --webroot-path /var/acme --cert-name example.com -d example.com -d www.example.com --email a@example.com --agree-tos --no-eff-email --non-interactive --keep-until-expiring"
	if err != nil || strings.Join(args, " ") != want {
		t.Errorf("http-01:\n%s\n%v", strings.Join(args, " "), err)
	}
	wild := Issue{Name: "example.com", Names: []string{"example.com", "*.example.com"}, Challenge: HTTP01, Email: "a@example.com"}
	if _, err := wild.CertbotArgs(); err != ErrWildcardHTTP {
		t.Errorf("wildcards need DNS-01: %v", err)
	}
	wild.Challenge, wild.Credentials, wild.Staging = DNS01, "/secrets/cloudflare.ini", true
	args, _ = wild.CertbotArgs()
	if s := strings.Join(args, " "); !strings.Contains(s, "--dns-cloudflare --dns-cloudflare-credentials /secrets/cloudflare.ini --dns-cloudflare-propagation-seconds 30") || !strings.HasSuffix(s, "--staging") {
		t.Errorf("dns-01: %s", s)
	}
	host := Issue{Name: "x", Names: []string{"x.example.com"}, Email: "a@example.com", Authenticator: "nginx"}
	if args, _ := host.CertbotArgs(); args[1] != "--nginx" {
		t.Errorf("host nginx plugin: %v", args)
	}
	if _, err := (Issue{Names: []string{"x"}}).CertbotArgs(); err == nil {
		t.Error("no email")
	}
	if s := strings.Join(RenewArgs("example.com", true), " "); s != "renew --cert-name example.com --force-renewal" {
		t.Errorf("renew: %s", s)
	}
	if s := strings.Join(DeleteArgs("example.com"), " "); s != "delete --non-interactive --cert-name example.com" {
		t.Errorf("delete: %s", s)
	}
	out := "Saving debug log to /var/log/letsencrypt/letsencrypt.log\nSome challenges have failed.\nexample.com: Invalid response from http://example.com/.well-known/acme-challenge/x: 404\nAsk for help or search for solutions at https://community.letsencrypt.org."
	if s := ExplainCertbot("", out); s != "example.com: Invalid response from http://example.com/.well-known/acme-challenge/x: 404" {
		t.Errorf("explain: %q", s)
	}
	renew := "Failed to renew certificate example.com with error: x\nAll renewals failed. The following certificates could not be renewed:\n  /etc/letsencrypt/live/example.com/fullchain.pem (failure)\n  /etc/letsencrypt/live/b.example.com/fullchain.pem (failure)"
	if got := RenewFailures(renew); !reflect.DeepEqual(got, []string{"b.example.com", "example.com"}) {
		t.Errorf("failures: %v", got)
	}
	if len(RenewSummary(renew, "")) == 0 {
		t.Error("summary")
	}
}
