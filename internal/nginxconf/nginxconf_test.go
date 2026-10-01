package nginxconf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseOK(t *testing.T, src string) []Directive {
	t.Helper()
	ds, _, errs := Parse("t.conf", []byte(src))
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	return ds
}

func TestLexerEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want [][]string // name + args of each top-level directive
	}{
		{"double quotes and escapes", `add_header X "a \"b\" \\ c";`, [][]string{{"add_header", "X", `a "b" \ c`}}},
		{"single quotes", `return 200 'it''s';`, [][]string{{"return", "200", "it", "s"}}},
		{"escaped tab and newline", `return 200 "a\tb\n";`, [][]string{{"return", "200", "a\tb\n"}}},
		{"${var} keeps its brace", `log_format x '${request_time}s' ${a}b;`, [][]string{{"log_format", "x", "${request_time}s", "${a}b"}}},
		{"close brace inside a bare word", `set $a b}c;`, [][]string{{"set", "$a", "b}c"}}},
		{"comment only at token start", "a b#c; # real comment\nd;", [][]string{{"a", "b#c"}, {"d"}}},
		{"backslash in a bare word", `rewrite ^/a\ b /c;`, [][]string{{"rewrite", `^/a\ b`, "/c"}}},
		{"quoted regex with braces", `server_name "~^\d{1,3}\.\d{1,3}$";`, [][]string{{"server_name", `~^\d{1,3}\.\d{1,3}$`}}},
		{"CRLF", "a 1;\r\nb 2;\r\n", [][]string{{"a", "1"}, {"b", "2"}}},
		{"BOM", "\xEF\xBB\xBFa 1;", [][]string{{"a", "1"}}},
		{"non-UTF-8 passes through", "a \xff\xfe;", [][]string{{"a", "\xff\xfe"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := parseOK(t, c.src)
			var got [][]string
			for _, d := range ds {
				got = append(got, append([]string{d.Name}, d.Args...))
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestParseFileInfo(t *testing.T) {
	_, info, _ := Parse("x", []byte("\xEF\xBB\xBFa \xff;\r\n"))
	if !info.BOM || !info.CRLF || !info.NonUTF8 {
		t.Fatalf("info %+v", info)
	}
}

func TestManagedHeader(t *testing.T) {
	for src, want := range map[string]string{
		"# Managed by NgiTool — do not edit\nserver {}\n": "ngitool",
		"# Managed by edge — certificate x\n":             "edge",
		"# hand-written\nserver {}\n":                     "",
	} {
		if _, info, _ := Parse("x", []byte(src)); info.Managed != want {
			t.Errorf("%q: %q", src, info.Managed)
		}
	}
}

func TestParseLinesAndBlocks(t *testing.T) {
	ds := parseOK(t, "http {\n  server {\n    listen 80;\n  }\n}\n")
	srv := ds[0].Block[0]
	if srv.Name != "server" || srv.Line != 2 || srv.Block[0].Line != 3 {
		t.Fatalf("lines: %+v", srv)
	}
}

func TestOpaqueLuaBlocks(t *testing.T) {
	ds, _, errs := Parse("openresty.conf", mustRead(t, "testdata/openresty.conf"))
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	s := Summarize(ds)
	if len(s.Servers) != 1 || len(s.Servers[0].Locations) != 2 {
		t.Fatalf("servers: %+v", s.Servers)
	}
	var lua []Directive
	var walk func([]Directive)
	walk = func(ds []Directive) {
		for _, d := range ds {
			if opaque(d.Name) {
				lua = append(lua, d)
			}
			walk(d.Block)
		}
	}
	walk(ds)
	if len(lua) != 3 {
		t.Fatalf("want 3 lua blocks, got %d", len(lua))
	}
	if !strings.Contains(lua[1].Opaque, `ngx.say("{", s, "}")`) {
		t.Errorf("content_by_lua_block body: %q", lua[1].Opaque)
	}
	api := s.Servers[0].Locations[1]
	if api.Target == nil || api.Target.Host != "127.0.0.1" || api.Target.Port != 9000 {
		t.Errorf("proxy after a one-line lua block: %+v", api.Target)
	}
	// The directive after the lua blocks keeps a correct line number.
	if api.Target.Pos.Line != 26 {
		t.Errorf("line after lua: %d", api.Target.Pos.Line)
	}
}

// Round trip: parse → Format → parse gives the same tree (CONF-04).
func TestRoundTrip(t *testing.T) {
	files, _ := filepath.Glob("testdata/*.conf")
	main, dump := SplitDump(string(mustRead(t, "testdata/edge-dump.txt")))
	if main == "" {
		t.Fatal("no dump")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "invalid.conf") {
			continue
		}
		check(t, f, mustRead(t, f))
	}
	for p, text := range dump {
		check(t, p, []byte(text))
	}
}

func check(t *testing.T, name string, src []byte) {
	t.Helper()
	a, _, errs := Parse(name, src)
	if len(errs) > 0 {
		t.Errorf("%s: %v", name, errs)
		return
	}
	b, _, errs := Parse(name, []byte(Format(a)))
	if len(errs) > 0 {
		t.Errorf("%s reformatted: %v\n%s", name, errs, Format(a))
		return
	}
	if !reflect.DeepEqual(strip(a), strip(b)) {
		t.Errorf("%s does not round-trip:\n%s", name, Format(a))
	}
}

// strip drops positions, which formatting changes.
func strip(ds []Directive) []Directive {
	out := make([]Directive, len(ds))
	for i, d := range ds {
		d.Line = 0
		d.Block = strip(d.Block)
		out[i] = d
	}
	return out
}

func TestSplitDump(t *testing.T) {
	out := "# configuration file /etc/nginx/nginx.conf:\nhttp {\n    include conf.d/*.conf;\n}\n\n" +
		"# configuration file /etc/nginx/conf.d/b.conf:\nserver { listen 81; }\n\n" +
		"# configuration file /etc/nginx/conf.d/a.conf:\nserver { listen 80; }\n\n"
	main, files := SplitDump(out)
	if main != "/etc/nginx/nginx.conf" || len(files) != 3 {
		t.Fatalf("main %q files %v", main, files)
	}
	if files["/etc/nginx/conf.d/a.conf"] != "server { listen 80; }\n" {
		t.Fatalf("content %q", files["/etc/nginx/conf.d/a.conf"])
	}
	// Includes are resolved inside the dump, globs in byte order.
	c := Load(DumpSource(files), main)
	s := Summarize(c.Tree)
	if len(c.Errors) > 0 || len(s.Servers) != 2 || s.Servers[0].Listens[0].Port != 80 {
		t.Fatalf("errors %v servers %+v", c.Errors, s.Servers)
	}
	if c.Source != "dump" {
		t.Errorf("source %s", c.Source)
	}
}

func TestIncludeGlobOrder(t *testing.T) {
	files := map[string]string{
		"/c/main.conf":     "http { include sites/*.conf; include sites/z.conf; }\n",
		"/c/sites/b.conf":  "server { server_name b; }\n",
		"/c/sites/B.conf":  "server { server_name B; }\n",
		"/c/sites/a.conf":  "server { server_name a; }\n",
		"/c/sites/10.conf": "server { server_name ten; }\n",
		"/c/sites/2.conf":  "server { server_name two; }\n",
		"/c/sites/z.conf":  "server { server_name z; }\n",
	}
	c := Load(DumpSource(files), "/c/main.conf")
	var names []string
	for _, s := range Summarize(c.Tree).Servers {
		names = append(names, s.NameList())
	}
	want := []string{"ten", "two", "B", "a", "b", "z", "z"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order %v want %v", names, want)
	}
}

func TestIncludeMissingAndLoop(t *testing.T) {
	files := map[string]string{
		"/m.conf":    "include /missing.conf;\ninclude /loop.conf;\n",
		"/loop.conf": "include /loop.conf;\n",
	}
	c := Load(DumpSource(files), "/m.conf")
	var msgs []string
	for _, e := range c.Errors {
		msgs = append(msgs, e.Error())
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "include /missing.conf: no such file in /m.conf:1") || !strings.Contains(all, "too deep") {
		t.Fatalf("errors:\n%s", all)
	}
}

func TestDebianLayoutWithSymlinks(t *testing.T) {
	root, _ := filepath.Abs("testdata/debian")
	c := Load(FileSource{Root: root}, "/etc/nginx/nginx.conf")
	// modules-enabled/*.conf matches nothing: not an error, as in nginx.
	if len(c.Errors) > 0 {
		t.Fatalf("errors: %v", c.Errors)
	}
	s := Summarize(c.Tree)
	if len(s.Servers) != 3 {
		t.Fatalf("servers: %d", len(s.Servers))
	}
	app := s.Servers[0]
	if app.Pos.File != "/etc/nginx/sites-enabled/app.example.com" {
		t.Errorf("server is shown under the enabled path: %s", app.Pos.File)
	}
	links := map[string]string{}
	for _, f := range c.Files {
		links[f.Path] = f.Link
	}
	if links["/etc/nginx/sites-enabled/app.example.com"] != "/etc/nginx/sites-available/app.example.com" ||
		links["/etc/nginx/sites-enabled/default"] != "/etc/nginx/sites-available/default" {
		t.Errorf("links: %v", links)
	}
	if _, ok := links["/etc/nginx/sites-available/old.example.com"]; ok {
		t.Error("a site that is only available must not be read")
	}
	if s.Hook == nil || s.Hook.Existing != "/etc/nginx/conf.d/*.conf" || s.Hook.Pos.Line != 17 {
		t.Errorf("hook: %+v", s.Hook)
	}
	if !app.Listens[0].SSL || !app.Listens[0].HTTP2 || !app.Listens[1].IPv6 || app.SSLCert != "/etc/ssl/certs/app.pem" {
		t.Errorf("listens: %+v cert %s", app.Listens, app.SSLCert)
	}
	loc := app.Locations
	if loc[0].Target.Upstream != "api_pool" || loc[1].Alias != "/srv/app/static/" ||
		loc[2].Modifier != "~" || loc[2].Target.Unix != "/run/php/php8.2-fpm.sock" {
		t.Errorf("locations: %+v", loc)
	}
	if !s.Servers[1].Redirects || s.Servers[1].Proxies {
		t.Errorf("redirect server: %+v", s.Servers[1])
	}
	if len(s.Upstreams) != 1 || s.Upstreams[0].Servers[1].Backup != true {
		t.Errorf("upstreams: %+v", s.Upstreams)
	}
	def := s.Servers[2]
	if !def.Listens[0].Default || !def.Static {
		t.Errorf("default: %+v", def)
	}
}

func TestSummaryOfEdgeDump(t *testing.T) {
	main, files := SplitDump(string(mustRead(t, "testdata/edge-dump.txt")))
	if main != "/etc/nginx/edge/nginx.conf" {
		t.Fatalf("main %s", main)
	}
	c := Load(DumpSource(files), main)
	if len(c.Errors) > 0 {
		t.Fatalf("errors %v", c.Errors)
	}
	s := Summarize(c.Tree)
	if len(s.Servers) != 13 || len(s.Upstreams) != 0 {
		t.Fatalf("servers %d upstreams %d", len(s.Servers), len(s.Upstreams))
	}
	if s.Resolver == nil || s.Resolver.Addrs[0] != "127.0.0.11" || s.Resolver.Valid != "10s" {
		t.Errorf("resolver %+v", s.Resolver)
	}
	if s.Hook == nil || s.Hook.Existing != "/etc/nginx/edge/conf.d/*.conf" {
		t.Errorf("hook %+v", s.Hook)
	}
	// The quoted regex server_name from 00-default.conf survives.
	re := s.Servers[1].Names[0]
	if re.Kind != NameRegex || re.Name != `~^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}$` {
		t.Errorf("regex name %+v", re)
	}
	if !s.Servers[2].Returns || !s.Servers[2].Listens[0].Default || !s.Servers[2].Listens[0].SSL {
		t.Errorf("reject server %+v", s.Servers[2])
	}
	// set $upstream …; proxy_pass http://$upstream; is resolved (CONF-05).
	var panel *Server
	for i := range s.Servers {
		if s.Servers[i].NameList() == "panel.example.com" {
			panel = &s.Servers[i]
			break
		}
	}
	if panel == nil {
		t.Fatal("panel server missing")
	}
	tg := panel.Locations[len(panel.Locations)-1].Target
	if tg == nil || !tg.Variable || tg.Resolved != "http://app1-panel:8080" || tg.Host != "app1-panel" || tg.Port != 8080 {
		t.Fatalf("target %+v", tg)
	}
	if !panel.Listens[0].HTTP2 || panel.SSLCert != "/etc/letsencrypt/live/panel.example.com/fullchain.pem" {
		t.Errorf("panel listens %+v cert %s", panel.Listens, panel.SSLCert)
	}
	if panel.Pos.File != "/etc/nginx/edge/sites/panel.example.com.conf" {
		t.Errorf("file %s", panel.Pos.File)
	}
}

func TestUpstreamEveryParameter(t *testing.T) {
	ds := parseOK(t, string(mustRead(t, "testdata/upstream-full.conf")))
	s := Summarize(ds)
	if len(s.Upstreams) != 4 {
		t.Fatalf("upstreams %d", len(s.Upstreams))
	}
	b := s.Upstreams[0]
	if b.Method != "least_conn" || b.Zone != "backend 64k" || b.Keepalive != 32 || len(b.Servers) != 7 {
		t.Fatalf("backend %+v", b)
	}
	first := b.Servers[0]
	if first.Host != "app1.example.com" || first.Port != 8080 || first.Weight != 5 || !first.Resolve || len(first.Params) != 5 {
		t.Errorf("first %+v", first)
	}
	if !b.Servers[2].Backup || !b.Servers[3].Down || b.Servers[4].Unix != "/run/app.sock" || b.Servers[5].Host != "2001:db8::22" {
		t.Errorf("servers %+v", b.Servers)
	}
	if b.Servers[6].Port != 80 || !b.Servers[6].Resolve {
		t.Errorf("service= server %+v", b.Servers[6])
	}
	if s.Upstreams[1].Method != "hash $request_uri consistent" || s.Upstreams[2].Method != "random two least_conn" || s.Upstreams[3].Method != "ip_hash" {
		t.Errorf("methods %s / %s / %s", s.Upstreams[1].Method, s.Upstreams[2].Method, s.Upstreams[3].Method)
	}
	if s.Resolver == nil || len(s.Resolver.Addrs) != 2 || s.Resolver.IPv6 != "off" {
		t.Errorf("resolver %+v", s.Resolver)
	}
	srv := s.Servers[0]
	kinds := []string{srv.Names[0].Kind, srv.Names[1].Kind, srv.Names[2].Kind}
	if !reflect.DeepEqual(kinds, []string{NameExact, NameWildcard, NameWildcard}) {
		t.Errorf("name kinds %v", kinds)
	}
	l := srv.Locations
	if l[0].Target.Upstream != "backend" {
		t.Errorf("upstream link %+v", l[0].Target)
	}
	if !l[1].Target.Variable || l[1].Target.Resolved != "" || !reflect.DeepEqual(l[1].Target.Candidates, []string{"http://backend", "http://hashed"}) {
		t.Errorf("map target %+v", l[1].Target)
	}
	if l[2].Target.Directive != "grpc_pass" || l[2].Target.Upstream != "hashed" {
		t.Errorf("grpc %+v", l[2].Target)
	}
	if l[3].Modifier != "=" || l[3].Target.Upstream != "" || l[3].Target.Port != 8080 {
		t.Errorf("explicit port is not the upstream %+v", l[3])
	}
}

func TestStreamIsReported(t *testing.T) {
	s := Summarize(parseOK(t, string(mustRead(t, "testdata/stream.conf"))))
	if s.Stream == nil || s.Stream.Line != 9 {
		t.Fatalf("stream %+v", s.Stream)
	}
	if len(s.Upstreams) != 0 || len(s.Servers) != 1 {
		t.Errorf("stream servers/upstreams leaked into http: %+v", s)
	}
	if !s.Servers[0].Returns {
		t.Error("return 200 server")
	}
}

func TestInvalidConfigIsParsedWherePossible(t *testing.T) {
	ds, _, errs := Parse("invalid.conf", mustRead(t, "testdata/invalid.conf"))
	if len(errs) == 0 {
		t.Fatal("want errors")
	}
	s := Summarize(ds)
	if len(s.Servers) == 0 {
		t.Fatal("nothing parsed")
	}
	if !strings.Contains(errs[len(errs)-1].Error(), "unexpected end of file") {
		t.Errorf("errors: %v", errs)
	}
}

func TestHookNeedsLine(t *testing.T) {
	s := Summarize(parseOK(t, "events {}\n\nhttp {\n  include /etc/nginx/mime.types;\n  server { listen 80; }\n}\n"))
	if s.Hook == nil || !s.Hook.NeedsLine || s.Hook.Pos.Line != 3 || !strings.HasPrefix(s.Hook.String(), "needs one include line in t.conf:3") {
		t.Fatalf("hook %+v", s.Hook)
	}
}

func TestListenKeys(t *testing.T) {
	ds := parseOK(t, "http { server { listen 80; listen [::]:443 ssl; listen 127.0.0.1:8080; listen *:81; listen unix:/run/n.sock; } }")
	var got []string
	for _, l := range Summarize(ds).Servers[0].Listens {
		got = append(got, l.Key()+"|"+l.String())
	}
	want := []string{"*:80|80", "[::]:443|[::]:443 ssl", "127.0.0.1:8080|127.0.0.1:8080", "*:81|81", "unix:/run/n.sock|unix:/run/n.sock"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestImplicitListenAndNoHTTP(t *testing.T) {
	s := Summarize(parseOK(t, "http { server { server_name a; } }"))
	if !s.Servers[0].Listens[0].Implicit || s.Servers[0].Listens[0].Port != 80 {
		t.Fatalf("%+v", s.Servers[0].Listens)
	}
	if Summarize(parseOK(t, "events {}")).Hook != nil {
		t.Error("no http, no hook")
	}
}

func TestFileSourceMountsAndImage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte("http { include conf.d/*.conf; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "conf.d"), 0o700)
	os.WriteFile(filepath.Join(dir, "conf.d", "a.conf"), []byte("server { listen 8080; }\n"), 0o600)
	src := FileSource{Container: true, Mounts: []Mount{{Dest: "/etc/nginx", Source: dir}}}
	c := Load(src, "/etc/nginx/nginx.conf")
	if len(c.Errors) > 0 || len(Summarize(c.Tree).Servers) != 1 {
		t.Fatalf("errors %v", c.Errors)
	}
	// Not under a mount, container stopped: inside the image (CONF-06).
	c = Load(FileSource{Container: true}, "/etc/nginx/nginx.conf")
	if len(c.Errors) != 1 || !strings.Contains(c.Errors[0].Msg, "inside the image") {
		t.Fatalf("errors %v", c.Errors)
	}
}
