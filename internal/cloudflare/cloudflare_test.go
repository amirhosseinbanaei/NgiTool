package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a tiny Cloudflare: zones, DNS records, origin certificates.
type fakeAPI struct {
	mu      sync.Mutex
	token   string
	zones   map[string]string // name → id
	records []Record
	calls   []string
	nextID  int
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	reply := func(code int, result any, errs ...string) {
		w.WriteHeader(code)
		env := map[string]any{"success": len(errs) == 0, "result": result, "errors": []map[string]any{}}
		for _, e := range errs {
			env["errors"] = append(env["errors"].([]map[string]any), map[string]any{"code": 9109, "message": e})
		}
		_ = json.NewEncoder(w).Encode(env)
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		reply(http.StatusForbidden, nil, "Invalid access token")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	switch {
	case path == "/user/tokens/verify":
		reply(200, map[string]string{"status": "active"})
	case path == "/zones":
		name := r.URL.Query().Get("name")
		if id, ok := f.zones[name]; ok {
			reply(200, []Zone{{ID: id, Name: name}})
		} else {
			reply(200, []Zone{})
		}
	case strings.HasSuffix(path, "/dns_records") && r.Method == http.MethodGet:
		var out []Record
		for _, rec := range f.records {
			if rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		reply(200, out)
	case strings.HasSuffix(path, "/dns_records") && r.Method == http.MethodPost:
		var rec Record
		_ = json.NewDecoder(r.Body).Decode(&rec)
		f.nextID++
		rec.ID = "r" + string(rune('0'+f.nextID))
		f.records = append(f.records, rec)
		reply(200, rec)
	case strings.Contains(path, "/dns_records/"):
		id := path[strings.LastIndex(path, "/")+1:]
		for i, rec := range f.records {
			if rec.ID != id {
				continue
			}
			if r.Method == http.MethodDelete {
				f.records = append(f.records[:i], f.records[i+1:]...)
				reply(200, map[string]string{"id": id})
				return
			}
			var upd Record
			_ = json.NewDecoder(r.Body).Decode(&upd)
			upd.ID = id
			f.records[i] = upd
			reply(200, upd)
			return
		}
		reply(404, nil, "record not found")
	case path == "/certificates":
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if body["request_type"] != "origin-rsa" || body["requested_validity"] != float64(OriginValidityDays) {
			reply(400, nil, "bad request")
			return
		}
		reply(200, map[string]string{"certificate": "-----BEGIN CERTIFICATE-----\nMII...\n-----END CERTIFICATE-----\n"})
	default:
		reply(404, nil, "no route "+path)
	}
}

func client(t *testing.T) (*Client, *fakeAPI) {
	f := &fakeAPI{token: "tok-123", zones: map[string]string{"example.com": "zone1", "shop.example.com": "zone2"}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("tok-123")
	c.Base = srv.URL + "/client/v4"
	return c, f
}

func TestVerifyAndZoneByLongestSuffix(t *testing.T) {
	c, _ := client(t)
	ctx := context.Background()
	if err := c.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	bad := *c
	bad.Token = "wrong"
	if err := bad.Verify(ctx); err == nil || !strings.Contains(err.Error(), "Invalid access token (9109)") {
		t.Errorf("bad token: %v", err)
	}
	z, err := c.FindZone(ctx, "a.b.shop.example.com")
	if err != nil || z == nil || z.ID != "zone2" {
		t.Errorf("longest suffix: %+v %v", z, err)
	}
	z, _ = c.FindZone(ctx, "api.example.com")
	if z == nil || z.ID != "zone1" {
		t.Errorf("zone: %+v", z)
	}
	if z, _ := c.FindZone(ctx, "other.org"); z != nil {
		t.Errorf("no zone: %+v", z)
	}
}

func TestUpsertAndDeleteRecords(t *testing.T) {
	c, f := client(t)
	ctx := context.Background()
	res, err := c.UpsertA(ctx, "zone1", "api.example.com", "203.0.113.10", true)
	if err != nil || res != Created {
		t.Fatalf("create: %s %v", res, err)
	}
	if f.records[0].Comment != Comment || !f.records[0].Proxied || f.records[0].TTL != 1 {
		t.Errorf("record: %+v", f.records[0])
	}
	if res, _ := c.UpsertA(ctx, "zone1", "api.example.com", "203.0.113.10", true); res != Unchanged {
		t.Errorf("unchanged: %s", res)
	}
	if res, _ := c.UpsertA(ctx, "zone1", "api.example.com", "203.0.113.10", false); res != Updated || f.records[0].Proxied {
		t.Errorf("dns-only: %s %+v", res, f.records[0])
	}
	f.records = append(f.records, Record{ID: "c1", Type: "CNAME", Name: "www.example.com", Content: "example.com"})
	if _, err := c.UpsertA(ctx, "zone1", "www.example.com", "203.0.113.10", true); err == nil || !strings.Contains(err.Error(), "is a CNAME to example.com") {
		t.Errorf("CNAME refused: %v", err)
	}
	n, err := c.DeleteRecords(ctx, "zone1", "api.example.com")
	if err != nil || n != 1 {
		t.Errorf("delete: %d %v", n, err)
	}
	if n, _ := c.DeleteRecords(ctx, "zone1", "www.example.com"); n != 0 {
		t.Errorf("CNAMEs are never deleted: %d", n)
	}
}

func TestOriginCertAndSync(t *testing.T) {
	c, _ := client(t)
	pem, err := c.CreateOriginCert(context.Background(), []string{"example.com", "*.example.com"}, "CSR")
	if err != nil || !strings.Contains(pem, "BEGIN CERTIFICATE") {
		t.Errorf("origin: %v", err)
	}
	pub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ips-v4":
			_, _ = io.WriteString(w, strings.Repeat("198.51.100.0/24\n", 8))
		case "/ips-v6":
			_, _ = io.WriteString(w, "2001:db8::/32\n2001:db8:1::/48\n")
		case "/aop.pem":
			_, _ = io.WriteString(w, "-----BEGIN CERTIFICATE-----\nAOP\n-----END CERTIFICATE-----\n")
		case "/cdn-cgi/trace":
			_, _ = io.WriteString(w, "fl=1\nip=203.0.113.10\nts=1\n")
		}
	}))
	defer pub.Close()
	old := [4]string{IPsV4URL, IPsV6URL, AOPCAURL, TraceURL}
	defer func() { IPsV4URL, IPsV6URL, AOPCAURL, TraceURL = old[0], old[1], old[2], old[3] }()
	IPsV4URL, IPsV6URL, AOPCAURL, TraceURL = pub.URL+"/ips-v4", pub.URL+"/ips-v6", pub.URL+"/aop.pem", pub.URL+"/cdn-cgi/trace"
	s, err := FetchSync(context.Background(), nil)
	if err != nil || len(s.Ranges) != 10 || !strings.Contains(s.AOPCA, "AOP") {
		t.Fatalf("sync: %+v %v", s, err)
	}
	conf := RealIPConf(s.Ranges, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(conf, "on 2026-01-02") || !strings.Contains(conf, "set_real_ip_from 2001:db8::/32;") || !strings.HasSuffix(conf, "real_ip_header CF-Connecting-IP;\n") {
		t.Errorf("realip:\n%s", conf)
	}
	if ip := PublicIP(context.Background(), nil); ip != "203.0.113.10" {
		t.Errorf("public ip: %q", ip)
	}
	IPsV6URL = pub.URL + "/nothing"
	if _, err := FetchSync(context.Background(), nil); err == nil {
		t.Error("too few ranges must fail")
	}
}
