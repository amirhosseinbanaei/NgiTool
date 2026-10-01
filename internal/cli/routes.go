package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/cloudflare"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

func routeCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:         "route",
		Short:       "send a host or path to containers, services or ports",
		Long:        "add      the wizard: instance, hostname, targets, method, certificate, http mode\nls       every route, grouped by instance and host\nrm       remove a route (shows what goes with it first)\nenable   put a disabled route back\ndisable  stop serving a route but keep it in state\nedit     re-run the wizard with the route's answers filled in",
		Annotations: map[string]string{annGroup: "routes", annSynopsis: "route add|ls|edit|rm|…"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(routeAddCmd(e, false), routeLsCmd(e), routeRmCmd(e), routeToggleCmd(e, true), routeToggleCmd(e, false), routeAddCmd(e, true))
	return c
}

// routeOpts are route add/edit's flags: one per wizard step.
type routeOpts struct {
	txFlags
	instance, pool, method, hashKey, sticky, cert, http, bodySize   string
	connectTimeout, readTimeout, sendTimeout, hostHeader, errorPage string
	upstreamSNI, upstreamCA, scheme                                 string
	to, headers                                                     []string
	consistent, httpOnly, www, noWWW, noWebsocket, noBuffering      bool
	strip, noStrip, upstreamVerify, connect, advanced               bool
	keepalive                                                       int
	dns                                                             string
	issue                                                           certAddOpts // --issue and its flags
}

func routeAddCmd(e *env, edit bool) *cobra.Command {
	o := &routeOpts{}
	use, short, syn := "add [host[/path]]", "route a hostname (or a path of it) to one or more targets", "add [host[/path]] [--to TARGET]…"
	if edit {
		use, short, syn = "edit <host[/path]>", "change a route: the wizard again, filled in", "edit <route> [flags of add]"
	}
	c := &cobra.Command{
		Use:   use,
		Short: short,
		Long: "On a terminal every flag left out is asked: the instance (the front door by default), the hostname,\n" +
			"a checklist of targets (containers, compose services, host ports), the method for two or more,\n" +
			"a certificate already on the instance, and the http mode. Off a terminal the hostname and --to are needed.\n\n" +
			"Targets: container:NAME:PORT · service:PROJECT/SERVICE:PORT · port:PORT · HOST:PORT · https://HOST · unix:/path,\n" +
			"each optionally followed by ,weight=N ,backup ,down ,max_fails=N ,fail_timeout=T ,max_conns=N.",
		Example: "ngitool route add api.example.com --to container:api:3000\n" +
			"ngitool route add shop.example.com --to service:shop/web:3000 --to service:shop/web2:3000 --method least_conn\n" +
			"ngitool route add example.com/blog --to port:8081 --strip --http-only --yes",
		Annotations: map[string]string{annSynopsis: syn},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return e.routeAdd(cmd, o, target, edit)
		},
	}
	fl := c.Flags()
	fl.StringVar(&o.instance, "instance", "", "instance id (default: the front door)")
	fl.StringArrayVar(&o.to, "to", nil, "a target; repeat for a pool")
	fl.StringVar(&o.pool, "pool", "", "use an existing pool instead of --to")
	fl.StringVar(&o.method, "method", "", "round_robin, least_conn, ip_hash, hash, random_two, random (two or more targets)")
	fl.StringVar(&o.hashKey, "hash-key", "", "key for --method hash, e.g. $request_uri")
	fl.BoolVar(&o.consistent, "consistent", false, "consistent hashing for --method hash")
	fl.StringVar(&o.sticky, "sticky", "", "sticky preset: ip, cloudflare, cookie:NAME (LB-07)")
	fl.IntVar(&o.keepalive, "keepalive", 0, "idle upstream connections kept per worker (LB-06)")
	fl.StringVar(&o.errorPage, "error-page", "", "page shown when every member is down (LB-11)")
	fl.StringVar(&o.cert, "cert", "", "a certificate already on the instance, by name or path")
	fl.StringVar(&o.issue.kind, "issue", "", "issue a new certificate: letsencrypt, origin, custom or self-signed")
	fl.StringVar(&o.issue.challenge, "challenge", "", "--issue letsencrypt: http (HTTP-01) or dns (DNS-01 through Cloudflare)")
	fl.BoolVar(&o.issue.wildcard, "wildcard", false, "--issue: also *.<host>")
	fl.StringVar(&o.issue.certFile, "cert-file", "", "--issue custom: the full chain PEM")
	fl.StringVar(&o.issue.keyFile, "key-file", "", "--issue custom: the private key PEM")
	fl.BoolVar(&o.issue.staging, "staging", false, "--issue letsencrypt: the staging CA")
	fl.BoolVar(&o.issue.force, "force-preflight", false, "--issue letsencrypt: ask Let's Encrypt although the HTTP-01 preflight failed")
	fl.StringVar(&o.dns, "dns", "", "Cloudflare DNS for the host: proxied, dns-only or skip (needs the edge stack's token)")
	fl.BoolVar(&o.httpOnly, "http-only", false, "no certificate: serve plain HTTP")
	fl.StringVar(&o.http, "http", "", "port 80 with a certificate: redirect (default) or serve (RP-17)")
	fl.BoolVar(&o.www, "www", false, "also serve www.<host> (RP-01)")
	fl.BoolVar(&o.noWWW, "no-www", false, "do not serve www.<host>")
	fl.BoolVar(&o.strip, "strip", false, "path routes: strip the path before proxying (RP-23)")
	fl.BoolVar(&o.noStrip, "no-strip", false, "path routes: keep the path")
	fl.BoolVar(&o.noWebsocket, "no-websocket", false, "do not pass WebSocket upgrades (RP-08)")
	fl.BoolVar(&o.noBuffering, "no-buffering", false, "turn proxy buffering off, for SSE and streaming (RP-08)")
	fl.StringVar(&o.bodySize, "body-size", "", "client_max_body_size, e.g. 100m (RP-10)")
	fl.StringVar(&o.connectTimeout, "connect-timeout", "", "proxy_connect_timeout (default 5s)")
	fl.StringVar(&o.readTimeout, "read-timeout", "", "proxy_read_timeout (default 120s)")
	fl.StringVar(&o.sendTimeout, "send-timeout", "", "proxy_send_timeout (default 120s)")
	fl.StringArrayVar(&o.headers, "header", nil, "extra request header NAME=VALUE; repeatable")
	fl.StringVar(&o.hostHeader, "host-header", "", "Host sent upstream: $host (default), $proxy_host or a name (RP-24)")
	fl.BoolVar(&o.upstreamVerify, "upstream-verify", false, "https targets: verify their certificate (RP-11)")
	fl.StringVar(&o.upstreamSNI, "upstream-sni", "", "https targets: the name to send and verify (RP-11)")
	fl.StringVar(&o.upstreamCA, "upstream-ca", "", "https targets: CA file to verify with")
	fl.StringVar(&o.scheme, "scheme", "", "upstream scheme when no target names it: http, https, grpc, grpcs (RP-14)")
	fl.BoolVar(&o.connect, "connect", false, "connect containers that share no network with the instance (RP-05)")
	fl.BoolVar(&o.advanced, "advanced", false, "ask the advanced questions too")
	o.txFlags.add(c)
	return c
}

// draft is everything the wizard collected.
type draft struct {
	route           model.Route
	pool            model.Pool
	issued          *model.Cert // a certificate issued by this wizard run
	home            *certHome
	dnsZone         *model.Zone // the DNS step's zone, token and IP
	dnsToken, dnsIP string
	dnsDone         bool
	reuse           bool   // pool already exists (--pool)
	oldID           string // edit: the route being replaced
	oldPool         string
}

func (e *env) routeAdd(cmd *cobra.Command, o *routeOpts, target string, edit bool) error {
	ctx := cmd.Context()
	rep := e.freshReport(ctx, true)
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	d := &draft{route: model.Route{Enabled: true, Options: model.DefaultOptions()}}
	var old *model.Route
	if edit {
		if target == "" {
			if err := ui.Need("route", "<route>"); err != nil {
				return err
			}
			if target, err = pickRoute(st, "Which route?"); err != nil {
				return err
			}
		}
		if old = st.Route(target); old == nil {
			return &UsageError{Msg: "no route " + target + " — see: ngitool route ls", Cmd: "ngitool route edit"}
		}
		d.route, d.oldID, d.oldPool = *old, old.ID, old.Pool
		if p := st.Pool(old.Pool); p != nil {
			d.pool = *p
			d.pool.Members = append([]model.Member{}, p.Members...)
		}
		if o.instance == "" {
			o.instance = old.Instance
		}
	}

	// a. instance
	in, err := e.wizardInstance(cmd, rep, st, o.instance)
	if err != nil {
		return err
	}
	d.route.Instance = in.ID

	// b. hostname
	if target == "" {
		if err := ui.Need("hostname", "a host[/path] argument"); err != nil {
			return err
		}
		if target, err = askHostname(rep, st, in); err != nil {
			return err
		}
	}
	host, path, err := model.ParseTarget(target)
	if err != nil {
		return codeErr(err)
	}
	d.route.Host, d.route.Path, d.route.ID = host, path, model.RouteID(host, path)
	if !edit || d.oldID != d.route.ID {
		if r := st.Route(d.route.ID); r != nil {
			return problemErr(model.Problem{Code: "RP-03", Msg: d.route.ID + " is already routed on " + r.Instance, Fix: "change it instead: ngitool route edit " + r.ID})
		}
	}

	// c–e. targets, ports, method
	if err := e.wizardTargets(cmd, o, rep, st, in, d, edit); err != nil {
		return err
	}

	// DNS (Cloudflare), then f–g. certificate and http mode
	if err := e.wizardDNS(cmd, o, st, in, d); err != nil {
		return err
	}
	if err := e.wizardTLS(cmd, o, st, in, d); err != nil {
		return err
	}

	// h. advanced
	if err := wizardOptions(cmd, o, d); err != nil {
		return err
	}

	// RP-05: offer to connect containers that share no network.
	if err := e.offerConnect(ctx, rep, in, &d.pool, o.connect); err != nil {
		return err
	}

	// A new static folder gets a placeholder page (EDGE-04).
	if d.pool.Static() && !o.dryRun {
		if ed := edgeStackFor(st, in); ed != nil && !strings.HasPrefix(d.pool.Members[0].Ref, "/") {
			made, err := edge.EnsureStatic(ed.Dir, d.pool.Members[0].Ref, d.route.Host)
			if err != nil {
				return err
			}
			if made {
				ui.Done("www/" + d.pool.Members[0].Ref + ui.Muted(" created with a placeholder page — replace it with your build"))
			}
		}
	}

	// i. plan, then the transaction
	printRoutePlan(d, in, edit)
	summary := "route add " + d.route.ID
	if edit {
		summary = "route edit " + d.route.ID
	}
	_, err = e.change(ctx, rep, in, o.txFlags, summary, []string{d.route.ID}, func(next *model.State) error {
		if d.issued != nil {
			next.RemoveCert(d.issued.Instance, d.issued.Name)
			next.Certs = append(next.Certs, *d.issued)
		}
		return d.applyTo(next, rep, edit)
	})
	if err != nil || o.dryRun {
		if d.issued != nil {
			// commitOrCleanup: the route did not happen, the certificate goes
			if rerr := e.removeCertFiles(ctx, d.home, *d.issued); rerr != nil {
				ui.Warning("the new certificate's files stay: " + rerr.Error())
			}
		}
		return err
	}
	e.applyDNS(ctx, d, in)
	return nil
}

// applyTo puts the drafted route (and pool) into a fresh copy of state.
func (d *draft) applyTo(next *model.State, rep *discover.Report, edit bool) error {
	r, p := d.route, d.pool
	if edit {
		next.RemoveRoute(d.oldID)
	}
	if !d.reuse {
		if edit && d.oldPool != "" && len(next.RoutesOf(d.oldPool)) == 0 && p.Name == d.oldPool {
			next.RemovePool(d.oldPool)
		} else if p.Name == "" || next.Pool(p.Name) != nil {
			p.Name = next.PoolName(r.Host, r.Path)
		}
		p.Instance = r.Instance
		next.Pools = append(next.Pools, p)
	}
	r.Pool = p.Name
	if r.Created == "" {
		r.Created = model.Now(time.Now())
	}
	next.Routes = append(next.Routes, r)
	if edit && d.oldPool != "" && d.oldPool != p.Name && len(next.RoutesOf(d.oldPool)) == 0 {
		next.RemovePool(d.oldPool) // left unused by the edit (LB-12)
	}
	return validate(next, rep, []string{r.ID}, []string{p.Name})
}

// wizardInstance is step a: the front door by default; writable and
// adopted instances otherwise, with "adopt one…" last.
func (e *env) wizardInstance(cmd *cobra.Command, rep *discover.Report, st *model.State, flag string) (*discover.Instance, error) {
	if flag != "" || !ui.CanPrompt() {
		return e.instanceFor(rep, st, flag, true)
	}
	var opts []ui.Option
	def := ""
	adoptable := 0
	for i := range rep.Instances {
		in := &rep.Instances[i]
		if st.Adopted(in.ID) == nil && in.Caps.Write.OK {
			adoptable++
		}
		if st.Adopted(in.ID) == nil || !in.Caps.Write.OK {
			continue
		}
		o := instanceOption(in)
		o.Hint = in.Kind + " · " + dashPlain(in.Version) + " · " + strconv.Itoa(len(st.RoutesOn(in.ID))) + " routes"
		opts = append(opts, o)
		if in.FrontDoor || def == "" {
			def = in.ID // the front door by default
		}
	}
	if len(opts) == 0 && adoptable == 0 {
		return nil, errors.New("no nginx NgiTool may write to — see why: ngitool instances")
	}
	if len(opts) == 1 && adoptable == 0 {
		ui.Plain(ui.OK(ui.SymOK) + " " + ui.Bold("Instance") + ui.Muted(" › ") + ui.Accent(opts[0].Label))
		return rep.Find(opts[0].Value), nil
	}
	if adoptable > 0 {
		opts = append(opts, ui.Option{Value: "\x00adopt", Label: "Adopt one…", Hint: strconv.Itoa(adoptable) + " more writable nginx: let NgiTool write there too"})
	}
	id, err := ui.Select(ui.SelectOpts{Title: "Which nginx serves it?", Options: opts, Default: def})
	if err != nil {
		return nil, err
	}
	if id != "\x00adopt" {
		return rep.Find(id), nil
	}
	in, err := pickAdoptable(rep, st)
	if err != nil {
		return nil, err
	}
	if err := e.adopt(cmd, rep, st, in, txFlags{}); err != nil {
		return nil, err
	}
	rep = e.freshReport(cmd.Context(), true)
	return rep.Find(in.ID), nil
}

// askHostname is step b, the only typed field: validated as it is typed,
// with the parent domains already served as suggestions.
func askHostname(rep *discover.Report, st *model.State, in *discover.Instance) (string, error) {
	seen := map[string]bool{}
	var sugg []string
	for _, x := range rep.Instances {
		if x.Summary == nil {
			continue
		}
		for _, s := range x.Summary.Servers {
			for _, n := range s.Names {
				if n.Kind != "exact" || !strings.Contains(n.Name, ".") {
					continue
				}
				parts := strings.Split(n.Name, ".")
				parent := strings.Join(parts[max(0, len(parts)-2):], ".")
				for _, v := range []string{parent, n.Name} {
					if !seen[v] {
						seen[v] = true
						sugg = append(sugg, v)
					}
				}
			}
		}
	}
	sort.Strings(sugg)
	note := "a host or host/path, e.g. api.example.com or example.com/blog"
	if len(sugg) > 0 {
		var parents []string
		for _, s := range sugg {
			if strings.Count(s, ".") == 1 {
				parents = append(parents, s)
			}
		}
		if len(parents) > 0 {
			note += " · served already: " + strings.Join(parents[:min(4, len(parents))], ", ")
		}
	}
	return ui.Input(ui.InputOpts{Title: "Hostname", Note: note, Placeholder: "app.example.com", Suggestions: sugg,
		Validate: func(v string) error {
			h, p, err := model.ParseTarget(v)
			if err != nil {
				return err
			}
			if r := st.Route(model.RouteID(h, p)); r != nil {
				return fmt.Errorf("%s is already routed (RP-03) — use: ngitool route edit %s", r.ID, r.ID)
			}
			for _, r := range st.Routes {
				if r.Host == h && r.Instance != in.ID {
					return fmt.Errorf("%s is routed on %s already (RP-04)", h, r.Instance)
				}
			}
			return nil
		}})
}

// wizardTargets is steps c–e.
func (e *env) wizardTargets(cmd *cobra.Command, o *routeOpts, rep *discover.Report, st *model.State, in *discover.Instance, d *draft, edit bool) error {
	fl := cmd.Flags()
	if o.pool != "" {
		p := st.Pool(o.pool)
		if p == nil {
			return &UsageError{Msg: "no pool " + o.pool + " — see: ngitool pool ls"}
		}
		if p.Instance != in.ID {
			return problemErr(model.Problem{Code: "LB-12", Msg: "pool " + p.Name + " lives on " + p.Instance, Fix: "a pool serves routes of its own instance"})
		}
		d.pool, d.reuse = *p, true
		return nil
	}
	specs := o.to
	if len(specs) == 0 {
		if edit && !ui.CanPrompt() {
			specs = nil // keep the members
		} else {
			if err := ui.Need("targets", "--to TARGET (repeat for a pool)"); err != nil {
				return err
			}
			var pre []string
			for _, m := range d.pool.Members {
				pre = append(pre, specOf(m))
			}
			var err error
			if specs, err = pickMembers(st, rep, in, pre); err != nil {
				return err
			}
		}
	}
	for i, sp := range specs {
		if sp == staticNew {
			// legacy: a domain's placeholder folder is named after the host
			specs[i] = "static:" + d.route.Host + strings.ReplaceAll(d.route.Path, "/", "-")
		}
	}
	if specs != nil {
		keep := map[string]model.Member{}
		for _, m := range d.pool.Members {
			keep[m.Label()] = m
		}
		d.pool.Members = nil
		if o.scheme != "" {
			d.pool.Scheme = o.scheme
		} else if !edit {
			d.pool.Scheme = ""
		}
		if err := membersFromSpecs(st, rep, in, specs, &d.pool); err != nil {
			return err
		}
		for i, m := range d.pool.Members {
			if k, ok := keep[m.Label()]; ok && i < len(specs) && !strings.Contains(specs[i], ",") {
				d.pool.Members[i] = k // edit: members that stay keep their weight and flags
			}
		}
	}
	if d.pool.Scheme == "" {
		d.pool.Scheme = "http"
	}
	if o.scheme != "" {
		d.pool.Scheme = o.scheme
	}
	p := &d.pool
	if p.Static() {
		p.Method = model.RoundRobin
		return nil
	}
	switch {
	case fl.Changed("method"):
		m, err := model.ParseMethod(o.method)
		if err != nil {
			return codeErr(err)
		}
		p.Method = m
	case len(p.Members) > 1 && ui.CanPrompt() && (!edit || len(o.to) > 0 || p.Method == ""):
		if err := askMethod(p); err != nil {
			return err
		}
	case p.Method == "":
		p.Method = model.RoundRobin
		if len(p.Members) > 1 {
			p.Method = model.DefaultMethod
		}
	}
	if o.hashKey != "" {
		p.HashKey = o.hashKey
	}
	if fl.Changed("consistent") {
		p.Consistent = o.consistent
	}
	if o.sticky != "" {
		if err := model.ApplySticky(p, o.sticky); err != nil {
			return codeErr(err)
		}
	}
	if fl.Changed("keepalive") {
		p.Keepalive = o.keepalive
	}
	if o.errorPage != "" {
		p.ErrorPage = o.errorPage
	}
	return nil
}

// specOf is a member as a --to spec.
func specOf(m model.Member) string {
	switch m.Kind {
	case model.KindContainer:
		return "container:" + m.Ref + ":" + strconv.Itoa(m.Port)
	case model.KindService:
		if m.App != "" {
			_, svc, _ := strings.Cut(m.Ref, "/")
			return "app:" + m.App + "/" + svc + ":" + strconv.Itoa(m.Port)
		}
		return "service:" + m.Ref + ":" + strconv.Itoa(m.Port)
	case model.KindHostPort:
		return "port:" + strconv.Itoa(m.Port)
	case model.KindUnix:
		return "unix:" + m.Ref
	}
	return m.Label()
}

// wizardTLS is steps f and g, plus apex + www (RP-01). A certificate can
// be one found on the instance, one NgiTool issued, or a new one: issued
// right here, before the transaction, so nginx -t sees its files.
func (e *env) wizardTLS(cmd *cobra.Command, o *routeOpts, st *model.State, in *discover.Instance, d *draft) error {
	fl := cmd.Flags()
	r := &d.route
	names := r.Names()
	if fl.Changed("www") || fl.Changed("no-www") {
		r.WWW = o.www && !o.noWWW
		names = r.Names()
	}
	cands := e.certCands(st, in)
	switch {
	case o.httpOnly:
		r.TLS = nil
	case o.issue.kind != "":
		if err := e.newCertForRoute(cmd, o, st, in, d); err != nil {
			return err
		}
	case o.cert != "":
		var found *certCand
		for i := range cands {
			if cands[i].Name == o.cert || cands[i].Cert == o.cert {
				found = &cands[i]
			}
		}
		if found == nil {
			return problemErr(model.Problem{Code: "RP-18", Msg: "no certificate " + o.cert + " on " + in.Name, Fix: "pick one of: " + certList(cands) + " — or --issue letsencrypt|origin|custom|self-signed, or --http-only"})
		}
		r.TLS = &found.TLS
	case cmd.Name() == "edit" && !ui.CanPrompt():
	default:
		cover := covering(cands, names)
		switch {
		case len(cover) == 1 && !ui.CanPrompt():
			r.TLS = &cover[0].TLS
		case !ui.CanPrompt():
			r.TLS = nil
			if len(cover) > 1 {
				ui.Info("more than one certificate covers " + r.Host + ": serving HTTP only — pass --cert NAME")
			}
		default:
			var opts []ui.Option
			for _, c := range cands {
				op := ui.Option{Value: c.Cert, Label: c.Name, Hint: certHint(c)}
				if len(c.Names) > 0 && len(covering([]certCand{c}, names)) == 0 {
					op.Disabled = "does not cover " + strings.Join(names, ", ")
				}
				opts = append(opts, op)
			}
			opts = append(opts,
				ui.Option{Value: "\x00issue", Label: "Issue a new certificate…", Hint: "Let's Encrypt, Cloudflare Origin CA, self-signed — or import one"},
				ui.Option{Value: "", Label: "HTTP only", Hint: "no certificate on this route"})
			def := ""
			switch {
			case r.TLS != nil:
				def = r.TLS.Cert
			case len(cover) > 0:
				def = cover[0].Cert
			case len(cands) == 0:
				def = "\x00issue"
			}
			v, err := ui.Select(ui.SelectOpts{Title: "Certificate", Options: opts, Default: def})
			if err != nil {
				return err
			}
			r.TLS = nil
			if v == "\x00issue" {
				if err := e.newCertForRoute(cmd, o, st, in, d); err != nil {
					return err
				}
				break
			}
			for i := range cands {
				if cands[i].Cert == v && v != "" {
					r.TLS = &cands[i].TLS
				}
			}
		}
	}
	// RP-01: an apex host can serve www too.
	if r.Path == "" && strings.Count(r.Host, ".") == 1 && !fl.Changed("www") && !fl.Changed("no-www") && ui.CanPrompt() && d.issued == nil &&
		(r.TLS == nil || model.Covers(r.TLS.Names, "www."+r.Host)) {
		yes, err := ui.Confirm("Also serve www."+r.Host+"?", "both names on one server (RP-01)", r.WWW)
		if err != nil {
			return err
		}
		r.WWW = yes
	}
	switch {
	case r.TLS == nil:
		r.HTTP = model.HTTPServe
	case o.http != "":
		if o.http != model.HTTPRedirect && o.http != model.HTTPServe {
			return &UsageError{Msg: "--http is redirect or serve"}
		}
		r.HTTP = o.http
	case ui.CanPrompt():
		v, err := ui.Select(ui.SelectOpts{Title: "Plain HTTP on port 80", Default: firstNonEmpty(r.HTTP, model.HTTPRedirect), Options: []ui.Option{
			{Value: model.HTTPRedirect, Label: "Redirect to HTTPS", Hint: "with HSTS — the usual choice"},
			{Value: model.HTTPServe, Label: "Serve it as well", Hint: "no HSTS; browsers that saw HSTS before keep upgrading (RP-17)"},
		}})
		if err != nil {
			return err
		}
		r.HTTP = v
	case r.HTTP == "":
		r.HTTP = model.HTTPRedirect
	}
	if r.HTTP == model.HTTPServe && r.TLS != nil && r.DNS == model.DNSProxied {
		// cli/src/flows.mjs:318-322
		ui.Warning(r.Host + " is proxied: Cloudflare's \"Always Use HTTPS\" still redirects http:// before it reaches this server — turn it off to use HTTP")
		ui.Hint("browsers that saw HTTPS-only (HSTS) before keep upgrading until they next load https:// — which now tells them to stop")
	}
	return nil
}

// certCands are the certificates a route on in can use: the ones NgiTool
// issued for it, then the ones found in its config and cert directories.
func (e *env) certCands(st *model.State, in *discover.Instance) []certCand {
	var out []certCand
	seen := map[string]bool{}
	for _, c := range st.Certs {
		if c.Instance != in.ID {
			continue
		}
		cc := certCand{TLS: *c.TLS(), Source: certs.Label(c.Kind, c.Challenge)}
		if info, err := certs.InspectFile(c.HostCert); err == nil {
			cc.Names, cc.Expires = info.Names, info.NotAfter
		}
		seen[c.Cert] = true
		out = append(out, cc)
	}
	for _, c := range findCerts(in) {
		if !seen[c.Cert] {
			out = append(out, c)
		}
	}
	return out
}

// newCertForRoute asks for (or reads) a new certificate's kind, issues it
// after a confirm, and puts it on the route; the draft remembers it so a
// failed transaction deletes its files again.
func (e *env) newCertForRoute(cmd *cobra.Command, o *routeOpts, st *model.State, in *discover.Instance, d *draft) error {
	ctx := cmd.Context()
	h, err := e.homeOf(ctx, st, in)
	if err != nil {
		return err
	}
	names := d.route.Names()
	if o.issue.wildcard {
		names = append(names, "*."+d.route.Host)
	}
	if d.route.Path != "" {
		names = []string{d.route.Host}
	}
	rq, err := e.certRequest(ctx, st, h, names, d.route.DNS, &o.issue)
	if err != nil {
		return err
	}
	ui.Plan("New certificate", [][2]string{{"Name", rq.Name}, {"Kind", certs.Label(rq.Kind, rq.Challenge)}, {"Names", strings.Join(rq.Names, ", ")}})
	if ok, err := ui.Sure(e.yes, "Issue it now?", "the route is written after it exists; if that fails, the certificate is deleted again"); err != nil || !ok {
		if err == nil {
			err = errCancelled
		}
		return err
	}
	// HTTP-01 is validated against DNS: the record must exist first.
	if rq.Kind == certs.LetsEncrypt && rq.Challenge == certs.HTTP01 && d.dnsZone != nil {
		e.applyDNS(ctx, d, in)
		d.dnsDone = true
	}
	c, err := e.issue(ctx, st, h, rq)
	if err != nil {
		return err
	}
	d.issued, d.home = c, h
	d.route.TLS = c.TLS()
	return nil
}

// wizardDNS is the Cloudflare step: only when the instance's edge stack
// has a token, only for whole-host routes. proxied, dns-only or skip.
func (e *env) wizardDNS(cmd *cobra.Command, o *routeOpts, st *model.State, in *discover.Instance, d *draft) error {
	ctx := cmd.Context()
	r := &d.route
	if r.Path != "" {
		return nil
	}
	ed := edgeStackFor(st, in)
	token := ""
	if ed != nil {
		token = edge.ReadToken(ed.Dir)
	}
	if token == "" {
		if o.dns != "" && o.dns != model.DNSSkip {
			return &UsageError{Msg: "--dns needs the edge stack's Cloudflare token (ngitool edge init)"}
		}
		return nil
	}
	mode := o.dns
	switch mode {
	case "", model.DNSProxied, model.DNSOnly, model.DNSSkip:
	default:
		return &UsageError{Msg: "--dns is proxied, dns-only or skip"}
	}
	ip := edge.ReadEnv(ed.Dir)["SERVER_IP"]
	zone := zoneFor(ctx, st, r.Host, token)
	if mode == "" {
		switch {
		case !ui.CanPrompt():
			mode = firstNonEmpty(r.DNS, model.DNSSkip)
		case zone == nil:
			ui.Warning(r.Host + " is not in this Cloudflare account — DNS records are not set (add the record yourself)")
			mode = model.DNSSkip
		case ip == "":
			ui.Warning("SERVER_IP is not set in " + edge.Layout{Dir: ed.Dir}.Env() + " — add the DNS record for " + r.Host + " yourself (or run ngitool edge init)")
			mode = model.DNSSkip
		default:
			v, err := ui.Select(ui.SelectOpts{Title: "DNS for " + r.Host, Note: "an A record → " + ip + " in zone " + zone.Name, Default: firstNonEmpty(r.DNS, model.DNSProxied), Options: []ui.Option{
				{Value: model.DNSProxied, Label: "Proxied (orange cloud)", Hint: "Cloudflare in front: caching, DDoS protection, hides this server's IP"},
				{Value: model.DNSOnly, Label: "DNS only", Hint: "visitors connect straight to this server"},
				{Value: model.DNSSkip, Label: "Leave DNS alone", Hint: "you manage the record yourself"},
			}})
			if err != nil {
				return err
			}
			mode = v
		}
	}
	if mode != model.DNSSkip && (zone == nil || ip == "") {
		return errors.New("--dns " + mode + ": " + r.Host + " needs a Cloudflare zone in this account and SERVER_IP in .env")
	}
	r.DNS = mode
	if mode != model.DNSSkip {
		d.dnsZone, d.dnsToken, d.dnsIP = zone, token, ip
	}
	if domain := certs.DomainOf(r.Host, st.DomainNames()); domain != "" {
		if note := certs.WildcardDepthNote(r.Host, st.DomainNames()); note != "" {
			ui.Warning(note)
		}
	}
	return nil
}

// applyDNS points the route's host (and www) at SERVER_IP.
func (e *env) applyDNS(ctx context.Context, d *draft, in *discover.Instance) {
	if d.dnsZone == nil || d.dnsDone {
		return
	}
	cf := cloudflare.New(d.dnsToken)
	for _, h := range d.route.Names() {
		_ = ui.Task("DNS "+h+" → "+d.dnsIP+" ("+d.route.DNS+")", func(t *ui.TaskCtl) error {
			res, err := cf.UpsertA(ctx, d.dnsZone.ID, h, d.dnsIP, d.route.DNS == model.DNSProxied)
			if err != nil {
				ui.Warning("DNS not changed: " + err.Error())
				return nil
			}
			t.Update("DNS " + h + " → " + d.dnsIP + " (" + d.route.DNS + ") " + ui.Muted(res))
			return nil
		})
	}
	d.dnsDone = true
}

func certHint(c certCand) string {
	var parts []string
	if len(c.Names) > 0 {
		parts = append(parts, strings.Join(c.Names[:min(3, len(c.Names))], ", "))
	}
	if !c.Expires.IsZero() {
		days := int(time.Until(c.Expires).Hours() / 24)
		parts = append(parts, strconv.Itoa(days)+" days left")
	}
	return strings.Join(parts, " · ")
}

func certList(cs []certCand) string {
	if len(cs) == 0 {
		return "(none found)"
	}
	var n []string
	for _, c := range cs {
		n = append(n, c.Name)
	}
	return strings.Join(n, ", ")
}

// wizardOptions is step h.
func wizardOptions(cmd *cobra.Command, o *routeOpts, d *draft) error {
	fl := cmd.Flags()
	op := &d.route.Options
	set := func(name string, v *string, src string) error {
		if fl.Changed(name) {
			*v = src
		}
		return nil
	}
	_ = set("body-size", &op.BodySize, o.bodySize)
	_ = set("connect-timeout", &op.ConnectTimeout, o.connectTimeout)
	_ = set("read-timeout", &op.ReadTimeout, o.readTimeout)
	_ = set("send-timeout", &op.SendTimeout, o.sendTimeout)
	_ = set("host-header", &op.HostHeader, o.hostHeader)
	if fl.Changed("no-websocket") {
		op.WebSocket = !o.noWebsocket
	}
	if fl.Changed("no-buffering") {
		op.Buffering = !o.noBuffering
	}
	if fl.Changed("strip") || fl.Changed("no-strip") {
		op.StripPrefix = o.strip && !o.noStrip
	}
	for _, h := range o.headers {
		hd, err := model.ParseHeader(h)
		if err != nil {
			return err
		}
		op.Headers = append(op.Headers, hd)
	}
	if o.upstreamVerify || o.upstreamSNI != "" || o.upstreamCA != "" {
		op.UpstreamTLS = &model.UpstreamTLS{Verify: o.upstreamVerify, ServerName: o.upstreamSNI, CA: o.upstreamCA}
	}
	if !ui.CanPrompt() {
		return nil
	}
	if d.route.Path != "" && !fl.Changed("strip") && !fl.Changed("no-strip") {
		v, err := ui.Select(ui.SelectOpts{Title: "Path " + d.route.Path, Default: map[bool]string{true: "strip", false: "keep"}[op.StripPrefix], Options: []ui.Option{
			{Value: "keep", Label: "Keep it", Hint: "the app sees " + d.route.Path + "/… (it must be built for that base path)"},
			{Value: "strip", Label: "Strip it", Hint: "the app sees /…; links it writes may miss " + d.route.Path},
		}})
		if err != nil {
			return err
		}
		op.StripPrefix = v == "strip"
	}
	adv := o.advanced
	if !adv {
		var err error
		if adv, err = ui.Confirm("Advanced options?", "websocket, buffering, body size, timeouts, upstream TLS", false); err != nil {
			return err
		}
	}
	if !adv {
		return nil
	}
	var err error
	if op.WebSocket, err = ui.Confirm("Pass WebSocket upgrades?", "RP-08", op.WebSocket); err != nil {
		return err
	}
	stream, err := ui.Confirm("Streaming or server-sent events?", "turns proxy buffering off (RP-08)", !op.Buffering)
	if err != nil {
		return err
	}
	op.Buffering = !stream
	ask := func(title, def string, v *string, check func(string) error) error {
		s, err := ui.Input(ui.InputOpts{Title: title, Default: *v, Placeholder: def, Validate: func(x string) error { return check(x) }})
		if err == nil {
			*v = s
		}
		return err
	}
	for _, q := range []struct {
		t, def string
		v      *string
		check  func(string) error
	}{
		{"Largest upload (client_max_body_size)", "nginx default 1m", &op.BodySize, model.ValidSize},
		{"Connect timeout", "5s", &op.ConnectTimeout, model.ValidTime},
		{"Read timeout", "120s", &op.ReadTimeout, model.ValidTime},
		{"Send timeout", "120s", &op.SendTimeout, model.ValidTime},
	} {
		if err := ask(q.t, q.def, q.v, q.check); err != nil {
			return err
		}
	}
	if d.pool.Scheme == "https" || d.pool.Scheme == "grpcs" {
		u := op.UpstreamTLS
		if u == nil {
			u = &model.UpstreamTLS{}
		}
		if u.Verify, err = ui.Confirm("Verify the upstream's certificate?", "RP-11", u.Verify); err != nil {
			return err
		}
		if err := ask("Upstream name (SNI)", "the target's host name", &u.ServerName, func(string) error { return nil }); err != nil {
			return err
		}
		op.UpstreamTLS = u
	}
	return nil
}

// offerConnect is RP-05: a container that shares no network with the
// instance joins it. A service of a linked app joins through the app's
// override (permanent, survives recreates — cli/src/flows.mjs
// attachService); anything else can be connected now with docker network
// connect, which lasts until the container is recreated.
func (e *env) offerConnect(ctx context.Context, rep *discover.Report, in *discover.Instance, p *model.Pool, yes bool) error {
	tmp := &model.State{Pools: []model.Pool{*p}}
	tmp.Pools[0].Instance = in.ID
	if tmp.Pools[0].Name == "" {
		tmp.Pools[0].Name = "draft"
	}
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, pr := range model.CheckPool(tmp, rep, &tmp.Pools[0]) {
		if pr.Connect == nil {
			continue
		}
		if c := rep.Container(pr.Connect.Container); c != nil && c.Project != "" && st.AppOfProject(c.Project) != nil {
			key := c.Project + "/" + c.Service
			if done[key] {
				continue // another replica of the same service
			}
			done[key] = true
			if err := e.attachMember(ctx, rep, st.AppOfProject(c.Project).Name, c.Project, c.Service, pr.Connect.Network, p, yes); err != nil {
				return err
			}
			continue
		}
		join := yes
		if !join && ui.CanPrompt() {
			var err error
			if join, err = ui.Confirm("Connect "+pr.Connect.Container+" to the "+pr.Connect.Network+" network now?",
				"lasts until the container is recreated — link its project (ngitool app link) to make it permanent (RP-05)", true); err != nil {
				return err
			}
		}
		if !join {
			ui.Warning(pr.Connect.Container + " is not on " + pr.Connect.Network + " — nginx cannot reach it until it is " + ui.Muted("(RP-05)"))
			continue
		}
		err := ui.Task("Connecting "+pr.Connect.Container+" to "+pr.Connect.Network, func(*ui.TaskCtl) error {
			res := newScanEnv(e).Run.Run(ctx, "docker", []string{"network", "connect", pr.Connect.Network, pr.Connect.Container}, execx.Opts{Timeout: 20 * time.Second})
			if res.Code != 0 {
				return errors.New(execx.Tail(res, 3))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if c := rep.Container(pr.Connect.Container); c != nil {
			c.Networks = append(c.Networks, pr.Connect.Network)
		}
	}
	return nil
}

// attachMember joins an app's service to the instance's network through
// the override, then points the pool's members of it at the alias.
func (e *env) attachMember(ctx context.Context, rep *discover.Report, app, project, service, network string, p *model.Pool, yes bool) error {
	how, now := "", -1
	if !ui.CanPrompt() {
		if !yes {
			ui.Warning(project + "/" + service + " is not on " + network + " — nginx cannot reach it until it is " + ui.Muted("(RP-05)"))
			ui.Hint(ui.SymArrow + " pass --connect to attach it through the app's override, or: ngitool app attach " + app + " " + service)
			return nil
		}
		how, now = "override", 1
	}
	at, recreated, err := e.attachService(ctx, app, service, network, how, now, false)
	if err != nil {
		return err
	}
	for i := range p.Members {
		m := &p.Members[i]
		if m.Kind == model.KindService && m.Ref == project+"/"+service {
			m.App, m.Host = app, at.Alias
		}
	}
	if recreated {
		for i := range rep.Containers {
			if c := &rep.Containers[i]; c.Project == project && c.Service == service {
				c.Networks = appendOnce(c.Networks, network)
			}
		}
	}
	return nil
}

func printRoutePlan(d *draft, in *discover.Instance, edit bool) {
	r, p := d.route, d.pool
	title := "Add route"
	if edit {
		title = "Change route"
	}
	pairs := [][2]string{
		{"Route", ui.Bold(strings.Join(r.Names(), " ")) + r.Path},
		{"Instance", in.Name + ui.Muted("  "+in.ID)},
	}
	switch {
	case p.Static():
		pairs = append(pairs, [2]string{"Target", "files in " + p.Members[0].Ref + ui.Muted("  static (EDGE-04)")})
	case len(p.Members) == 1:
		pairs = append(pairs, [2]string{"Target", p.Members[0].Label() + ui.Muted("  "+p.Members[0].Kind)})
	default:
		var ms []string
		for _, m := range p.Members {
			ms = append(ms, m.Label())
		}
		pairs = append(pairs, [2]string{"Pool", strings.Join(ms, ", ")}, [2]string{"Method", p.MethodText() + ui.Muted(stickyNote(p.Sticky))})
	}
	if p.Scheme != "http" {
		pairs = append(pairs, [2]string{"Upstream", p.Scheme})
	}
	if r.TLS != nil {
		pairs = append(pairs, [2]string{"TLS", r.TLS.Name + ui.Muted("  "+r.TLS.Cert)}, [2]string{"Port 80", r.HTTP})
	} else {
		pairs = append(pairs, [2]string{"TLS", ui.Muted("none — HTTP only")})
	}
	if r.DNS != "" && r.DNS != model.DNSSkip {
		pairs = append(pairs, [2]string{"DNS", r.DNS + ui.Muted("  Cloudflare A record, after the route is in place")})
	}
	if r.Path != "" && !p.Static() {
		pairs = append(pairs, [2]string{"Path", map[bool]string{true: "stripped", false: "kept"}[r.Options.StripPrefix]})
	}
	var opts []string
	o := r.Options
	if !o.WebSocket {
		opts = append(opts, "no websocket")
	}
	if !o.Buffering {
		opts = append(opts, "buffering off")
	}
	for _, kv := range [][2]string{{"body", o.BodySize}, {"connect", o.ConnectTimeout}, {"read", o.ReadTimeout}, {"send", o.SendTimeout}, {"host", o.HostHeader}} {
		if kv[1] != "" {
			opts = append(opts, kv[0]+" "+kv[1])
		}
	}
	if len(opts) > 0 {
		pairs = append(pairs, [2]string{"Options", strings.Join(opts, " · ")})
	}
	ui.Plan(title, pairs)
}

func stickyNote(s string) string {
	if s == "" {
		return ""
	}
	return "  sticky: " + s
}

func pickRoute(st *model.State, title string) (string, error) {
	ids := routeIDs(st)
	if len(ids) == 0 {
		return "", errors.New("no routes yet — add one: ngitool route add")
	}
	var opts []ui.Option
	for _, id := range ids {
		r := st.Route(id)
		opts = append(opts, ui.Option{Value: id, Label: id, Hint: model.Describe(st.Pool(r.Pool)) + " · " + r.Instance})
	}
	return ui.Select(ui.SelectOpts{Title: title, Options: opts, Filter: len(opts) > 8})
}

// ---- route ls ---------------------------------------------------------

type routeJSON struct {
	ID       string        `json:"id"`
	Instance string        `json:"instance"`
	Host     string        `json:"host"`
	Path     string        `json:"path,omitempty"`
	Enabled  bool          `json:"enabled"`
	TLS      string        `json:"tls,omitempty"`
	HTTP     string        `json:"http"`
	Pool     string        `json:"pool"`
	Method   string        `json:"method"`
	Members  []string      `json:"members"`
	Health   *apply.Probe  `json:"health,omitempty"`
	Options  model.Options `json:"options"`
}

func routeLsCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "ls",
		Short:       "every route, grouped by instance and host",
		Annotations: map[string]string{annSynopsis: "ls [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			h := apply.LoadHealth(e.paths.Cache)
			if asJSON {
				out := []routeJSON{}
				for _, id := range routeIDs(st) {
					r := st.Route(id)
					p := st.Pool(r.Pool)
					j := routeJSON{ID: r.ID, Instance: r.Instance, Host: r.Host, Path: r.Path, Enabled: r.Enabled, HTTP: r.HTTP, Pool: r.Pool, Options: r.Options, Members: []string{}}
					if r.TLS != nil {
						j.TLS = r.TLS.Name
					}
					if p != nil {
						j.Method = p.Method
						for _, m := range p.Members {
							j.Members = append(j.Members, m.Label())
						}
					}
					if pr, ok := h.Routes[r.ID]; ok {
						j.Health = &pr
					}
					out = append(out, j)
				}
				return printJSON(out)
			}
			if len(st.Routes) == 0 {
				ui.Info("no routes yet")
				ui.Hint("add one: ngitool route add")
				return nil
			}
			var insts []string
			seen := map[string]bool{}
			for _, r := range st.Routes {
				if !seen[r.Instance] {
					seen[r.Instance] = true
					insts = append(insts, r.Instance)
				}
			}
			sort.Strings(insts)
			for _, id := range insts {
				ui.Plain("")
				for _, l := range ui.Tree(routeTree(st, h, id)) {
					ui.Plain(l)
				}
			}
			ui.Plain("")
			ui.Plain("  " + ui.Muted(ui.OK(ui.SymDot)+" reached its upstream  "+ui.Err(ui.SymErr)+" 502/503/504  "+ui.SymRing+" not probed yet"))
			if len(st.Apps) > 0 {
				// DOCK-07: an app behind these routes started without its override.
				if ctrs, err := appContainers(cmd.Context()); err == nil {
					var views []appJSON
					for _, a := range st.Apps {
						if v := appView(st, a, ctrs); len(v.Routes) > 0 {
							views = append(views, v)
						}
					}
					warnDetached(views)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func routeTree(st *model.State, h *apply.Health, instance string) ui.Node {
	root := ui.Node{Label: ui.Bold(instance)}
	if a := st.Adopted(instance); a != nil {
		root.Hint = a.Layout + " layout · " + a.Root
	}
	byHost := map[string][]model.Route{}
	var hosts []string
	for _, r := range st.RoutesOn(instance) {
		if byHost[r.Host] == nil {
			hosts = append(hosts, r.Host)
		}
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	for _, host := range hosts {
		rs := byHost[host]
		lead := rs[0]
		hn := ui.Node{Label: host}
		if lead.TLS != nil {
			hn.Hint = "https · " + lead.TLS.Name + " · port 80 " + lead.HTTP
		} else {
			hn.Hint = "http only"
		}
		if lead.WWW {
			hn.Label += ui.Muted(" + www")
		}
		for _, r := range rs {
			p := firstNonEmpty(r.Path, "/")
			label := healthDot(h, r) + " " + p + "  " + ui.Muted("→ ") + model.Describe(st.Pool(r.Pool))
			hint := ""
			if !r.Enabled {
				label = ui.Muted(ui.SymRing + " " + p + "  → " + model.Describe(st.Pool(r.Pool)))
				hint = "disabled"
			} else if pr, ok := h.Routes[r.ID]; ok && pr.Status > 0 {
				hint = strconv.Itoa(pr.Status) + " · " + pr.At.Local().Format("Jan 2 15:04")
			}
			hn.Children = append(hn.Children, ui.Node{Label: label, Hint: hint})
		}
		root.Children = append(root.Children, hn)
	}
	return root
}

func healthDot(h *apply.Health, r model.Route) string {
	pr, ok := h.Routes[r.ID]
	switch {
	case !ok || pr.Skipped != "":
		return ui.Muted(ui.SymRing)
	case pr.Reached:
		return ui.OK(ui.SymDot)
	}
	return ui.Err(ui.SymErr) // not a dot: plain output must tell them apart
}

// ---- route rm / enable / disable ------------------------------------------

func routeRmCmd(e *env) *cobra.Command {
	var f txFlags
	var keepPool bool
	c := &cobra.Command{
		Use:         "rm <route>",
		Short:       "remove a route; its pool goes too when nothing else uses it",
		Long:        "Shows the cascade first, as the Node CLI did: the route, its pool when no other route uses it (LB-12),\nand the files that disappear. --keep-pool keeps the pool.",
		Annotations: map[string]string{annSynopsis: "rm <route> [--keep-pool] [--dry-run]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else {
				if err := ui.Need("route", "<route>"); err != nil {
					return err
				}
				if id, err = pickRoute(st, "Remove which route?"); err != nil {
					return err
				}
			}
			r := st.Route(id)
			if r == nil {
				return &UsageError{Msg: "no route " + id + " — see: ngitool route ls", Cmd: "ngitool route rm"}
			}
			rep := e.freshReport(ctx, true)
			in := lookupInstance(rep, r.Instance)
			if in == nil {
				return fmt.Errorf("instance %s was not found by the scan — is it still there?", r.Instance)
			}
			plan := cascadePlan(st, []string{id}, keepPool)
			printCascade("Remove route "+id, st, plan)
			if !f.dryRun {
				ui.Explain(id+" stops being served by "+in.Name, strings.Join(plan.summary(), "; "), "ngitool rollback "+in.ID+" (the snapshot is taken before)")
			}
			_, err = e.change(ctx, rep, in, f, "route rm "+id, nil, func(next *model.State) error {
				plan.apply(next)
				return nil
			})
			return err
		},
	}
	c.Flags().BoolVar(&keepPool, "keep-pool", false, "keep the pool even when no route uses it")
	f.add(c)
	return c
}

// cascade is what goes when routes are removed (cli/src/remove.mjs
// planRemoval): the routes, and the pools they leave unused (LB-12).
type cascade struct {
	routes []string
	pools  []string
}

func cascadePlan(st *model.State, routes []string, keepPools bool) cascade {
	rm := cascade{routes: routes}
	gone := map[string]bool{}
	for _, id := range routes {
		gone[id] = true
	}
	if keepPools {
		return rm
	}
	seen := map[string]bool{}
	for _, id := range routes {
		r := st.Route(id)
		if r == nil || seen[r.Pool] {
			continue
		}
		seen[r.Pool] = true
		used := false
		for _, o := range st.RoutesOf(r.Pool) {
			if !gone[o.ID] {
				used = true
			}
		}
		if !used {
			rm.pools = append(rm.pools, r.Pool)
		}
	}
	return rm
}

func (rm cascade) apply(next *model.State) {
	for _, id := range rm.routes {
		next.RemoveRoute(id)
	}
	for _, p := range rm.pools {
		next.RemovePool(p)
	}
}

func (rm cascade) summary() []string {
	out := []string{strconv.Itoa(len(rm.routes)) + " " + either(len(rm.routes), "route", "routes")}
	if len(rm.pools) > 0 {
		out = append(out, "pool "+strings.Join(rm.pools, ", ")+" (no other route uses it)")
	}
	return out
}

func printCascade(title string, st *model.State, rm cascade) {
	var pairs [][2]string
	for _, id := range rm.routes {
		r := st.Route(id)
		pairs = append(pairs, [2]string{"Route", id + ui.Muted("  → "+model.Describe(st.Pool(r.Pool)))})
	}
	for _, p := range rm.pools {
		pairs = append(pairs, [2]string{"Pool", p + ui.Muted("  no other route uses it (LB-12)")})
	}
	ui.Plan(title, pairs)
}

func routeToggleCmd(e *env, on bool) *cobra.Command {
	var f txFlags
	use, short := "disable <route>", "stop serving a route but keep it in state (RP-22)"
	if on {
		use, short = "enable <route>", "serve a disabled route again (RP-22)"
	}
	c := &cobra.Command{
		Use:         use,
		Short:       short,
		Annotations: map[string]string{annSynopsis: strings.Replace(use, "<route>", "<route> [--dry-run]", 1)},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			} else {
				if err := ui.Need("route", "<route>"); err != nil {
					return err
				}
				if id, err = pickRoute(st, "Which route?"); err != nil {
					return err
				}
			}
			r := st.Route(id)
			if r == nil {
				return &UsageError{Msg: "no route " + id + " — see: ngitool route ls"}
			}
			if r.Enabled == on {
				ui.Info(id + " is already " + map[bool]string{true: "enabled", false: "disabled"}[on])
				return nil
			}
			rep := e.freshReport(ctx, true)
			in := lookupInstance(rep, r.Instance)
			if in == nil {
				return fmt.Errorf("instance %s was not found by the scan", r.Instance)
			}
			verb := map[bool]string{true: "enable", false: "disable"}[on]
			var probe []string
			if on {
				probe = []string{id}
			}
			_, err = e.change(ctx, rep, in, f, "route "+verb+" "+id, probe, func(next *model.State) error {
				x := next.Route(id)
				x.Enabled = on
				x.Disabled = ""
				if !on {
					x.Disabled = model.Now(time.Now())
				}
				if on {
					return validate(next, rep, []string{id}, []string{x.Pool})
				}
				return nil
			})
			return err
		},
	}
	f.add(c)
	return c
}
