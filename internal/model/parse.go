package model

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Rules ported from the Node CLI (cli/src/targets.mjs): HOST_RE, PATH_RE.
var (
	hostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+([a-z]{2,}|xn--[a-z0-9-]+)$`)
	pathRE = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)
	poolRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)
	sizeRE = regexp.MustCompile(`^[0-9]+[kKmMgG]?$`)
	timeRE = regexp.MustCompile(`^[0-9]+(ms|s|m|h)?$`)
	hdrRE  = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

// NormalizeHost lower-cases a hostname and turns IDN labels into punycode
// (RP-19). Wildcards (*.example.com) and regex names (~…) are kept as they
// are and reported by Kind.
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSpace(h)
	if strings.HasPrefix(h, "~") {
		if _, err := regexp.Compile(strings.TrimPrefix(h, "~")); err != nil {
			return "", fmt.Errorf("invalid regex server name: %v (RP-19)", err)
		}
		return h, nil
	}
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "" {
		return "", fmt.Errorf("a hostname is required, e.g. api.example.com")
	}
	wild := strings.HasPrefix(h, "*.")
	labels := strings.Split(strings.TrimPrefix(h, "*."), ".")
	for i, l := range labels {
		if !isASCII(l) {
			p, err := punycode(l)
			if err != nil {
				return "", fmt.Errorf("%s: %v (RP-19)", l, err)
			}
			labels[i] = "xn--" + p
		}
	}
	out := strings.Join(labels, ".")
	if !hostRE.MatchString(out) || len(out) > 253 {
		return "", fmt.Errorf("%q is not a valid hostname (e.g. api.example.com) (RP-19)", h)
	}
	for _, l := range labels {
		if len(l) > 63 {
			return "", fmt.Errorf("label %q is longer than 63 characters (RP-19)", l)
		}
	}
	if wild {
		out = "*." + out
	}
	return out, nil
}

// HostKind is exact, wildcard or regex (RP-19).
func HostKind(h string) string {
	switch {
	case strings.HasPrefix(h, "~"):
		return "regex"
	case strings.HasPrefix(h, "*."):
		return "wildcard"
	}
	return "exact"
}

// NormalizePath turns "api/", "/api" and "api" into "/api"; "" and "/"
// mean the whole host.
func NormalizePath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	p = "/" + p
	if !pathRE.MatchString(p) {
		return "", fmt.Errorf("invalid path %s: letters, digits and . _ ~ - only, e.g. /admin or /api/v1", p)
	}
	return p, nil
}

// ParseTarget splits "https://Example.com/Admin/" into host and path.
func ParseTarget(s string) (host, path string, err error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	h, p, _ := strings.Cut(t, "/")
	if host, err = NormalizeHost(h); err != nil {
		return "", "", err
	}
	if path, err = NormalizePath(p); err != nil {
		return "", "", err
	}
	return host, path, nil
}

// ValidPoolName checks a pool name: lower-case letters, digits, _ and -.
func ValidPoolName(n string) error {
	if !poolRE.MatchString(n) {
		return fmt.Errorf("pool names are lower-case letters, digits, _ and - (at most 48), e.g. shop-web")
	}
	return nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// punycode encodes one label (RFC 3492), without the xn-- prefix.
func punycode(label string) (string, error) {
	const (
		base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
		initialBias, initialN        = 72, 128
	)
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	adapt := func(delta, numPoints int, first bool) int {
		if first {
			delta /= damp
		} else {
			delta /= 2
		}
		delta += delta / numPoints
		k := 0
		for delta > ((base-tmin)*tmax)/2 {
			delta /= base - tmin
			k += base
		}
		return k + (base-tmin+1)*delta/(delta+skew)
	}
	runes := []rune(label)
	var out []byte
	for _, r := range runes {
		if r < 0x80 {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	h := b
	if b > 0 {
		out = append(out, '-')
	}
	n, delta, bias := initialN, 0, initialBias
	for h < len(runes) {
		m := int(^uint(0) >> 1)
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) == n {
				q := delta
				for k := base; ; k += base {
					t := k - bias
					if t < tmin {
						t = tmin
					} else if t > tmax {
						t = tmax
					}
					if q < t {
						break
					}
					out = append(out, digit(t+(q-t)%(base-t)))
					q = (q - t) / (base - t)
				}
				out = append(out, digit(q))
				bias = adapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return string(out), nil
}

// PlusOnly are NGINX Plus directives and parameters, with the open-source
// alternative (LB-15, LB-08, LB-07). NgiTool never renders them.
var PlusOnly = map[string]string{
	"sticky":       "sticky is NGINX Plus only — use a sticky preset instead: --sticky cookie:<name> (hash $cookie_<name> consistent) or --sticky cloudflare (LB-07)",
	"slow_start":   "slow_start is NGINX Plus only — bring a member back with `ngitool pool undrain` after it is warm (LB-15)",
	"queue":        "queue is NGINX Plus only — use max_conns with a backup member, or proxy_next_upstream (LB-15)",
	"ntlm":         "ntlm is NGINX Plus only — NTLM needs a pinned connection that open-source nginx cannot keep; use ip_hash and keepalive (LB-15)",
	"least_time":   "least_time is NGINX Plus only — use least_conn or random two least_conn (LB-15)",
	"health_check": "active health checks are NGINX Plus only — NgiTool uses passive max_fails/fail_timeout plus `ngitool pool check` (LB-08)",
	"drain":        "the drain parameter is NGINX Plus only — use `ngitool pool drain`, which marks the member down (LB-15)",
	"route":        "the route parameter belongs to Plus sticky sessions — use a sticky preset instead (LB-15)",
	"service":      "the service parameter (DNS SRV) is NGINX Plus only — list the members instead (LB-15)",
}

// ParseMethod reads a method name as typed: "round_robin", "least_conn",
// "ip_hash", "hash", "random", "random_two" (also "rr", "least-conn",
// "random two least_conn").
func ParseMethod(s string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(s))
	k = strings.NewReplacer("-", "_", " ", "_").Replace(k)
	switch k {
	case "", "rr", "round_robin", "roundrobin":
		return RoundRobin, nil
	case "least_conn", "leastconn", "lc":
		return LeastConn, nil
	case "ip_hash", "iphash":
		return IPHash, nil
	case "hash", "consistent_hash":
		return Hash, nil
	case "random":
		return Random, nil
	case "random_two", "random_two_least_conn", "p2c":
		return RandomTwo, nil
	}
	if why, ok := PlusOnly[k]; ok {
		return "", Problem{Code: "LB-15", Msg: why}
	}
	return "", fmt.Errorf("unknown method %q — one of round_robin, least_conn, ip_hash, hash, random_two, random (LB-01)", s)
}

// Sticky presets (LB-07). ApplySticky sets the pool's method from one.
func ApplySticky(p *Pool, preset string) error {
	switch {
	case preset == "" || preset == "none":
		p.Sticky = ""
		return nil
	case preset == "ip":
		p.Method, p.HashKey, p.Consistent = IPHash, "", false
	case preset == "cloudflare":
		p.Method, p.HashKey, p.Consistent = Hash, "$http_cf_connecting_ip", true
	case strings.HasPrefix(preset, "cookie:"):
		name := strings.TrimPrefix(preset, "cookie:")
		if !regexp.MustCompile(`^[A-Za-z0-9_]+$`).MatchString(name) {
			return fmt.Errorf("cookie names for sticky sessions are letters, digits and _ (got %q) (LB-07)", name)
		}
		p.Method, p.HashKey, p.Consistent = Hash, "$cookie_"+name, true
	default:
		return fmt.Errorf("unknown sticky preset %q — ip, cloudflare or cookie:<name> (LB-07)", preset)
	}
	p.Sticky = preset
	return nil
}

// ParseMember reads a member spec as given to --to:
//
//	container:NAME[:PORT]        a container, by name
//	service:PROJECT/SERVICE[:PORT]  a compose service
//	port:PORT  (or host:PORT)    a process on this host
//	unix:/path/to.sock           a unix socket
//	HOST:PORT, addr:HOST:PORT, http(s)://HOST[:PORT], grpc(s)://…  an address
//
// followed by comma-separated parameters: weight=N, backup, down,
// max_fails=N, fail_timeout=T, max_conns=N. It returns the scheme the
// spec named ("" when none).
func ParseMember(spec string) (Member, string, error) {
	head, params, _ := strings.Cut(strings.TrimSpace(spec), ",")
	var m Member
	scheme := ""
	kind, rest, _ := strings.Cut(head, ":")
	switch kind {
	case "container":
		m.Kind = KindContainer
		name, port, err := namePort(rest)
		if err != nil {
			return m, "", err
		}
		m.Ref, m.Host, m.Port = name, name, port
	case "service":
		m.Kind = KindService
		name, port, err := namePort(rest)
		if err != nil {
			return m, "", err
		}
		if strings.Count(name, "/") != 1 {
			return m, "", fmt.Errorf("service members are PROJECT/SERVICE[:PORT], e.g. service:shop/web:3000")
		}
		m.Ref, m.Port = name, port
	case "port", "host":
		m.Kind = KindHostPort
		p, err := ParsePort(rest)
		if err != nil {
			return m, "", err
		}
		m.Port = p
	case "unix":
		m.Kind = KindUnix
		if !strings.HasPrefix(rest, "/") {
			return m, "", fmt.Errorf("unix members are unix:/absolute/path.sock (RP-16)")
		}
		m.Ref = rest
	case "tcp", "udp":
		return m, "", Problem{Code: "RP-14", Msg: "TCP/UDP stream proxying is out of scope: NgiTool reports stream {} blocks but never writes them",
			Fix: "proxy it with a hand-written stream {} block; NgiTool leaves it alone"}
	case "http", "https", "grpc", "grpcs":
		scheme = kind
		rest = strings.TrimPrefix(rest, "//")
		rest = strings.TrimSuffix(rest, "/")
		if strings.Contains(rest, "/") {
			return m, "", fmt.Errorf("a member is a host and port, not a URL with a path: %s", spec)
		}
		def := 80
		if scheme == "https" || scheme == "grpcs" {
			def = 443
		}
		h, p, err := addrPort(rest, def)
		if err != nil {
			return m, "", err
		}
		m.Kind, m.Ref, m.Port = KindAddress, h, p
	case "addr":
		h, p, err := addrPort(rest, 0)
		if err != nil {
			return m, "", err
		}
		m.Kind, m.Ref, m.Port = KindAddress, h, p
	default:
		h, p, err := addrPort(head, 0)
		if err != nil {
			return m, "", fmt.Errorf("unknown member %q — container:NAME:PORT, service:PROJECT/SERVICE:PORT, port:PORT, unix:/path or HOST:PORT", spec)
		}
		m.Kind, m.Ref, m.Port = KindAddress, h, p
	}
	if params != "" {
		for _, kv := range strings.Split(params, ",") {
			if err := SetParam(&m, kv); err != nil {
				return m, "", err
			}
		}
	}
	return m, scheme, nil
}

// SetParam applies one member parameter: weight=N, backup, down,
// max_fails=N, fail_timeout=T, max_conns=N. Plus-only ones are refused
// (LB-15).
func SetParam(m *Member, kv string) error {
	k, v, hasV := strings.Cut(strings.TrimSpace(kv), "=")
	if why, ok := PlusOnly[k]; ok {
		return Problem{Code: "LB-15", Msg: why}
	}
	num := func() (int, error) {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s needs a whole number, got %q", k, v)
		}
		return n, nil
	}
	switch {
	case k == "backup" && !hasV:
		m.Backup = true
	case k == "down" && !hasV:
		m.Down = true
	case k == "weight":
		n, err := num()
		if err != nil || n < 1 {
			return fmt.Errorf("weight is a whole number of at least 1")
		}
		m.Weight = n
	case k == "max_fails":
		n, err := num()
		if err != nil {
			return err
		}
		m.MaxFails = &n
	case k == "fail_timeout":
		if !timeRE.MatchString(v) {
			return fmt.Errorf("fail_timeout is a time like 10s or 1m")
		}
		m.FailTimeout = v
	case k == "max_conns":
		n, err := num()
		if err != nil {
			return err
		}
		m.MaxConns = n
	default:
		return fmt.Errorf("unknown member parameter %q — weight=N, backup, down, max_fails=N, fail_timeout=T, max_conns=N", kv)
	}
	return nil
}

// ParsePort reads a TCP port.
func ParsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("a port is a number from 1 to 65535, got %q", s)
	}
	return n, nil
}

func namePort(s string) (string, int, error) {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		p, err := ParsePort(s[i+1:])
		if err != nil {
			return "", 0, err
		}
		s = s[:i]
		if s == "" {
			return "", 0, fmt.Errorf("a name is required before :PORT")
		}
		return s, p, nil
	}
	if s == "" {
		return "", 0, fmt.Errorf("a name is required")
	}
	return s, 0, nil
}

func addrPort(s string, def int) (string, int, error) {
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := ParsePort(p)
		if err != nil {
			return "", 0, err
		}
		return strings.ToLower(h), n, nil
	}
	s = strings.Trim(s, "[]")
	if s == "" || def == 0 {
		return "", 0, fmt.Errorf("an address needs a port: HOST:PORT (IPv6: [2001:db8::1]:8080)")
	}
	return strings.ToLower(s), def, nil
}

// ValidSize checks client_max_body_size values (RP-10): 0, 10m, 1g …
func ValidSize(s string) error {
	if s != "" && !sizeRE.MatchString(s) {
		return fmt.Errorf("a size is a number with k, m or g, e.g. 100m (RP-10)")
	}
	return nil
}

// ValidTime checks timeouts: 5s, 120s, 2m.
func ValidTime(s string) error {
	if s != "" && !timeRE.MatchString(s) {
		return fmt.Errorf("a time is a number with ms, s, m or h, e.g. 60s")
	}
	return nil
}

// ParseHeader reads NAME=VALUE.
func ParseHeader(s string) (Header, error) {
	k, v, ok := strings.Cut(s, "=")
	k = strings.TrimSpace(k)
	if !ok || !hdrRE.MatchString(k) {
		return Header{}, fmt.Errorf("headers are NAME=VALUE, e.g. X-Env=prod")
	}
	if strings.ContainsAny(v, "\r\n") {
		return Header{}, fmt.Errorf("a header value cannot span lines")
	}
	return Header{Name: k, Value: v}, nil
}
