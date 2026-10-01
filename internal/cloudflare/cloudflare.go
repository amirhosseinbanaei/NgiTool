// Package cloudflare is the Cloudflare API v4 as NgiTool needs it: token
// verify, zone lookup by longest suffix, A records proxied or DNS-only,
// Origin CA certificates, the public IP, and the real-IP ranges plus the
// Authenticated Origin Pulls CA for the edge stack (cf-sync).
//
// The token is the one certbot uses (secrets/cloudflare.ini). It is sent
// only to the API base and never printed, logged or stored by this package.
// Permissions: Zone → DNS → Edit (DNS records, DNS-01), plus
// Zone → SSL and Certificates → Edit for Origin CA certificates.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Endpoints. Tests point them at an httptest server.
var (
	APIBase  = "https://api.cloudflare.com/client/v4"
	TraceURL = "https://1.1.1.1/cdn-cgi/trace"
	IPsV4URL = "https://www.cloudflare.com/ips-v4"
	IPsV6URL = "https://www.cloudflare.com/ips-v6"
	AOPCAURL = "https://developers.cloudflare.com/ssl/static/authenticated_origin_pull_ca.pem"
)

// Comment is put on every DNS record NgiTool writes.
const Comment = "managed by NgiTool"

// Error is a Cloudflare refusal or an unreachable API.
type Error struct{ Msg string }

func (e *Error) Error() string { return "Cloudflare: " + e.Msg }

// Client talks to the API with one token.
type Client struct {
	Token string
	Base  string
	HTTP  *http.Client
}

// New is a client for token with the default endpoints.
func New(token string) *Client {
	return &Client{Token: token, Base: APIBase, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, route string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+route, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Msg: "could not reach the API: " + reason(err)}
	}
	defer res.Body.Close()
	var env envelope
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	_ = json.Unmarshal(data, &env)
	if res.StatusCode >= 300 || !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			m := e.Message
			if e.Code != 0 {
				m += fmt.Sprintf(" (%d)", e.Code)
			}
			msgs = append(msgs, m)
		}
		if len(msgs) == 0 {
			msgs = []string{fmt.Sprintf("HTTP %d", res.StatusCode)}
		}
		return &Error{Msg: strings.Join(msgs, "; ")}
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func reason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return "timeout"
		}
		return ue.Err.Error()
	}
	return err.Error()
}

// Verify checks that the token is active.
func (c *Client) Verify(ctx context.Context) error {
	var r struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/tokens/verify", nil, &r); err != nil {
		return err
	}
	if r.Status != "active" {
		if r.Status == "" {
			r.Status = "not active"
		}
		return &Error{Msg: "the token is " + r.Status}
	}
	return nil
}

// Zone is a Cloudflare zone.
type Zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FindZone is the zone host lives in: a.b.example.com, then b.example.com,
// then example.com. nil when the account has none of them.
func (c *Client) FindZone(ctx context.Context, host string) (*Zone, error) {
	labels := strings.Split(host, ".")
	for i := 0; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zs []Zone
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zs); err != nil {
			return nil, err
		}
		if len(zs) > 0 {
			return &zs[0], nil
		}
	}
	return nil, nil
}

// Record is a DNS record.
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// Records are the A, AAAA and CNAME records of name.
func (c *Client) Records(ctx context.Context, zoneID, name string) ([]Record, error) {
	var all []Record
	if err := c.do(ctx, http.MethodGet, "/zones/"+zoneID+"/dns_records?name="+url.QueryEscape(name)+"&per_page=100", nil, &all); err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range all {
		if r.Type == "A" || r.Type == "AAAA" || r.Type == "CNAME" {
			out = append(out, r)
		}
	}
	return out, nil
}

// Upsert results.
const (
	Created   = "created"
	Updated   = "updated"
	Unchanged = "unchanged"
)

// UpsertA points name at ip with the proxy on or off. It refuses to
// replace a CNAME.
func (c *Client) UpsertA(ctx context.Context, zoneID, name, ip string, proxied bool) (string, error) {
	rs, err := c.Records(ctx, zoneID, name)
	if err != nil {
		return "", err
	}
	var a *Record
	for i := range rs {
		switch rs[i].Type {
		case "CNAME":
			return "", &Error{Msg: name + " is a CNAME to " + rs[i].Content + " — change it in the dashboard"}
		case "A":
			if a == nil {
				a = &rs[i]
			}
		}
	}
	body := Record{Type: "A", Name: name, Content: ip, Proxied: proxied, TTL: 1, Comment: Comment}
	if a == nil {
		return Created, c.do(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", body, nil)
	}
	if a.Content == ip && a.Proxied == proxied {
		return Unchanged, nil
	}
	return Updated, c.do(ctx, http.MethodPatch, "/zones/"+zoneID+"/dns_records/"+a.ID, body, nil)
}

// DeleteRecords removes the A and AAAA records of name; the number gone.
func (c *Client) DeleteRecords(ctx context.Context, zoneID, name string) (int, error) {
	rs, err := c.Records(ctx, zoneID, name)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rs {
		if r.Type != "A" && r.Type != "AAAA" {
			continue
		}
		if err := c.do(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+r.ID, nil, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// OriginValidityDays is 15 years, Cloudflare's longest.
const OriginValidityDays = 5475

// CreateOriginCert has Cloudflare's Origin CA sign csr for hostnames and
// returns the certificate PEM.
func (c *Client) CreateOriginCert(ctx context.Context, hostnames []string, csr string) (string, error) {
	var r struct {
		Certificate string `json:"certificate"`
	}
	body := map[string]any{"hostnames": hostnames, "csr": csr, "request_type": "origin-rsa", "requested_validity": OriginValidityDays}
	if err := c.do(ctx, http.MethodPost, "/certificates", body, &r); err != nil {
		return "", err
	}
	if r.Certificate == "" {
		return "", &Error{Msg: "no certificate in the answer"}
	}
	return r.Certificate, nil
}

var ipRE = regexp.MustCompile(`(?m)^ip=([0-9.]+)$`)

// PublicIP is this server's public IPv4 as Cloudflare sees it, "" when
// unknown.
func PublicIP(ctx context.Context, client *http.Client) string {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second}
	}
	body, err := get(ctx, client, TraceURL)
	if err != nil {
		return ""
	}
	if m := ipRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func get(ctx context.Context, client *http.Client, u string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", &Error{Msg: u + ": " + reason(err)}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", &Error{Msg: fmt.Sprintf("%s: HTTP %d", u, res.StatusCode)}
	}
	return string(b), nil
}

// Sync is what cf-sync fetches: the ranges real_ip trusts, and the AOP CA
// ("" when it could not be fetched; it is needed only for AOP).
type Sync struct {
	Ranges []string
	AOPCA  string
}

// FetchSync downloads Cloudflare's IPv4 and IPv6 ranges and the AOP CA.
func FetchSync(ctx context.Context, client *http.Client) (Sync, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	var s Sync
	for _, u := range []string{IPsV4URL, IPsV6URL} {
		body, err := get(ctx, client, u)
		if err != nil {
			return s, err
		}
		for _, f := range strings.Fields(body) {
			if strings.Contains(f, "/") {
				s.Ranges = append(s.Ranges, f)
			}
		}
	}
	if len(s.Ranges) < 10 {
		return s, &Error{Msg: "unexpected answer from cloudflare.com/ips"}
	}
	if ca, err := get(ctx, client, AOPCAURL); err == nil && strings.Contains(ca, "BEGIN CERTIFICATE") {
		s.AOPCA = ca
	}
	return s, nil
}

// RealIPConf is conf.d/cloudflare-realip.conf for ranges.
func RealIPConf(ranges []string, day time.Time) string {
	var b strings.Builder
	b.WriteString("# Written by ngitool edge cf-sync on " + day.UTC().Format("2006-01-02") + " — do not edit by hand.\n")
	b.WriteString("# Restores the real visitor IP from Cloudflare's CF-Connecting-IP header.\n")
	for _, r := range ranges {
		b.WriteString("set_real_ip_from " + r + ";\n")
	}
	b.WriteString("real_ip_header CF-Connecting-IP;\n")
	return b.String()
}
