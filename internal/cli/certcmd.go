package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

func certCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "cert",
		Short: "certificates: Let's Encrypt, Cloudflare Origin CA, custom, self-signed",
		Long: "ls     every certificate NgiTool issued or imported, with days left\n" +
			"add    issue one (Let's Encrypt HTTP-01 or DNS-01, Origin CA, self-signed) or import one (custom)\n" +
			"renew  certbot renew for Let's Encrypt certificates, then reload\n" +
			"rm     remove one: move its routes to another certificate, or remove them too\n" +
			"aop    Authenticated Origin Pulls: accept HTTPS only from Cloudflare",
		Annotations: map[string]string{annGroup: "certs", annSynopsis: "cert ls|add|renew|rm|aop"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(certLsCmd(e), certAddCmd(e), certRenewCmd(e), certRmCmd(e), certAOPCmd(e))
	return c
}

// ── ls ──────────────────────────────────────────────────────────────────────

type certRow struct {
	model.Cert
	Label    string    `json:"label"`
	Expires  time.Time `json:"expires,omitempty"`
	DaysLeft *int      `json:"daysLeft,omitempty"`
	Problem  string    `json:"problem,omitempty"`
	Routes   []string  `json:"routes"`
}

func certRows(st *model.State, instance string) []certRow {
	now := time.Now()
	var out []certRow
	for _, c := range st.Certs {
		if instance != "" && c.Instance != instance {
			continue
		}
		r := certRow{Cert: c, Label: certs.Label(c.Kind, c.Challenge), Routes: st.CertUsers(c.Instance, c.Name)}
		if r.Routes == nil {
			r.Routes = []string{}
		}
		info, err := certs.InspectFile(c.HostCert)
		if err != nil {
			r.Problem = err.Error()
		} else {
			r.Expires = info.NotAfter
			d := info.DaysLeft(now)
			r.DaysLeft = &d
			if len(info.Names) > 0 {
				r.Names = info.Names
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// printCertTable is the certificate list (CERT-02: days left in colour).
func printCertTable(st *model.State, instance string) {
	rows := certRows(st, instance)
	var t [][]string
	now := time.Now()
	for _, r := range rows {
		mark := ui.OK(ui.SymOK)
		left := ui.DaysLeft(r.Expires, now)
		switch {
		case r.Problem != "":
			mark, left = ui.Err(ui.SymErr), ui.Err(r.Problem)
		case *r.DaysLeft < 14:
			mark = ui.Err(ui.SymWarn)
		case *r.DaysLeft < 30:
			mark = ui.Warn(ui.SymWarn)
		}
		names := strings.Join(r.Names, ", ")
		if r.AOP {
			names += " · AOP on"
		}
		if len(r.Routes) == 0 {
			names += " · unused"
		}
		t = append(t, []string{mark, ui.Bold(r.Name), r.Label, left, ui.Muted(names)})
	}
	ui.PrintTable(t, ui.TableOpts{Indent: 2})
}

func certLsCmd(e *env) *cobra.Command {
	var asJSON, all bool
	var instance string
	c := &cobra.Command{
		Use:         "ls",
		Short:       "every certificate with its kind, days left and names",
		Annotations: map[string]string{annSynopsis: "ls [--instance ID] [--found] [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			rows := certRows(st, instance)
			var found []certCand
			if all {
				rep := e.report(cmd.Context(), false, false)
				for i := range rep.Instances {
					in := &rep.Instances[i]
					if (instance != "" && in.ID != instance) || st.Adopted(in.ID) == nil {
						continue
					}
					for _, f := range findCerts(in) {
						if !registered(st, in.ID, f.Cert) {
							found = append(found, f)
						}
					}
				}
			}
			if asJSON {
				if rows == nil {
					rows = []certRow{}
				}
				return printJSON(map[string]any{"certificates": rows, "found": found})
			}
			ui.Heading("Certificates", fmt.Sprint(len(rows)))
			if len(rows) == 0 {
				ui.Hint("none yet — ngitool cert add, or the certificate step of ngitool route add")
			} else {
				printCertTable(st, instance)
			}
			if len(found) > 0 {
				ui.Heading("Found on the instances", "usable by routes, not managed by NgiTool")
				var t [][]string
				for _, f := range found {
					t = append(t, []string{ui.Muted(ui.SymRing), f.Name, ui.DaysLeft(f.Expires, time.Now()), ui.Muted(strings.Join(f.Names, ", ") + " · " + f.Cert)})
				}
				ui.PrintTable(t, ui.TableOpts{Indent: 2})
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	c.Flags().BoolVar(&all, "found", false, "also list certificates found on adopted instances")
	c.Flags().StringVar(&instance, "instance", "", "only this instance")
	return c
}

func registered(st *model.State, instance, certPath string) bool {
	for _, c := range st.Certs {
		if c.Instance == instance && c.Cert == certPath {
			return true
		}
	}
	return false
}

// ── add ─────────────────────────────────────────────────────────────────────

type certAddOpts struct {
	instance, kind, challenge, name, certFile, keyFile, authenticator, webroot, email string
	wildcard, staging, force                                                          bool
}

func (o *certAddOpts) add(c *cobra.Command) {
	fl := c.Flags()
	fl.StringVar(&o.kind, "kind", "", "letsencrypt, origin, custom or self-signed")
	fl.StringVar(&o.challenge, "challenge", "", "Let's Encrypt: http (HTTP-01 on port 80) or dns (DNS-01 through Cloudflare)")
	fl.BoolVar(&o.wildcard, "wildcard", false, "also *.<name> (DNS-01, Origin CA, self-signed)")
	fl.StringVar(&o.certFile, "cert-file", "", "custom: the full chain PEM")
	fl.StringVar(&o.keyFile, "key-file", "", "custom: the private key PEM (unencrypted)")
	fl.BoolVar(&o.staging, "staging", false, "Let's Encrypt staging CA (test certificates, no rate limits)")
	fl.StringVar(&o.authenticator, "authenticator", "", "host certbot: nginx (default) or webroot (CERT-05)")
	fl.StringVar(&o.webroot, "webroot", "", "host certbot: the webroot nginx serves /.well-known/acme-challenge/ from")
	fl.StringVar(&o.email, "email", "", "ACME email (default: the edge stack's ACME_EMAIL)")
}

func certAddCmd(e *env) *cobra.Command {
	o := &certAddOpts{}
	c := &cobra.Command{
		Use:   "add [name…]",
		Short: "issue or import a certificate for one or more names",
		Long: "Let's Encrypt runs certbot in the edge stack's certbot container (HTTP-01 through data/acme, or DNS-01\n" +
			"through Cloudflare); HTTP-01 is checked first by fetching a test file from every name, so a wrong DNS\n" +
			"record or a closed port 80 never spends Let's Encrypt's rate limit (CERT-01). On host nginx, certbot on\n" +
			"the host is used (CERT-05). Self-signed and custom files go to data/certs/<name>/ with the key 0600.",
		Example: "ngitool cert add example.com --kind letsencrypt --challenge http\n" +
			"ngitool cert add example.com --wildcard --kind letsencrypt --challenge dns\n" +
			"ngitool cert add test.example.com --kind self-signed --yes\n" +
			"ngitool cert add shop.example.com --kind custom --cert-file fullchain.pem --key-file privkey.pem",
		Annotations: map[string]string{annSynopsis: "add [name…] [--kind K] [--challenge http|dns] [--wildcard]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			rep := e.freshReport(ctx, true)
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			in, err := e.instanceFor(rep, st, o.instance, true)
			if err != nil {
				return err
			}
			h, err := e.homeOf(ctx, st, in)
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				if err := ui.Need("names", "a name argument"); err != nil {
					return err
				}
				v, err := ui.Input(ui.InputOpts{Title: "Names on the certificate", Placeholder: "example.com www.example.com",
					Note: "one or more hostnames, separated by spaces", Validate: func(v string) error {
						if len(strings.Fields(v)) == 0 {
							return errors.New("at least one name")
						}
						for _, n := range strings.Fields(v) {
							if _, err := model.NormalizeHost(strings.TrimPrefix(n, "*.")); err != nil {
								return err
							}
						}
						return nil
					}})
				if err != nil {
					return err
				}
				names = strings.Fields(v)
			}
			for i, n := range names {
				h, err := model.NormalizeHost(strings.TrimPrefix(n, "*."))
				if err != nil {
					return &UsageError{Msg: err.Error()}
				}
				if strings.HasPrefix(n, "*.") {
					h = "*." + h
				}
				names[i] = h
			}
			if o.wildcard {
				names = append(names, "*."+strings.TrimPrefix(names[0], "*."))
			}
			rq, err := e.certRequest(ctx, st, h, names, "", o)
			if err != nil {
				return err
			}
			ui.Plan("New certificate", [][2]string{
				{"Name", rq.Name}, {"Kind", certs.Label(rq.Kind, rq.Challenge)}, {"Names", strings.Join(rq.Names, ", ")},
				{"Instance", in.Name + ui.Muted("  "+in.ID)}, {"Files", h.files(rq.Kind, rq.Name).Cert},
			})
			if ok, err := ui.Sure(e.yes, "Create it?", ""); err != nil || !ok {
				if err == nil {
					err = errCancelled
				}
				return err
			}
			c, err := e.issue(ctx, st, h, rq)
			if err != nil {
				return err
			}
			if err := e.saveCert(ctx, *c); err != nil {
				return err
			}
			ui.Done("certificate " + ui.Bold(c.Name) + " ready " + ui.Muted("("+strings.Join(c.Names, ", ")+")"))
			ui.Hint("use it: ngitool route add <host> --cert " + c.Name)
			return nil
		},
	}
	c.Flags().StringVar(&o.instance, "instance", "", "instance id (default: the front door)")
	c.Flags().StringVar(&o.name, "name", "", "certificate name (default: the first name)")
	c.Flags().BoolVar(&o.force, "force-preflight", false, "ask Let's Encrypt although the HTTP-01 preflight failed")
	o.add(c)
	return c
}

// saveCert records a certificate in state (it changes no nginx file).
func (e *env) saveCert(ctx context.Context, c model.Cert) error {
	return e.withLock(ctx, true, func() error {
		st, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		st.RemoveCert(c.Instance, c.Name)
		st.Certs = append(st.Certs, c)
		return model.Save(e.paths, st)
	})
}

// certRequest turns flags (or answers) into an issue request: the kind
// with its disabled reasons, the challenge, the name (cli/src/flows.mjs
// pickCert). dns is the route's DNS mode when known.
func (e *env) certRequest(ctx context.Context, st *model.State, h *certHome, names []string, dns string, o *certAddOpts) (issueReq, error) {
	host := strings.TrimPrefix(names[0], "*.")
	rq := issueReq{Names: names, CertFile: o.certFile, KeyFile: o.keyFile, Staging: o.staging, Force: o.force,
		Authenticator: o.authenticator, Webroot: o.webroot, Email: o.email}
	token := h.token()
	zone := (*model.Zone)(nil)
	if token != "" {
		zone = zoneFor(ctx, st, host, token)
	}
	noToken := ""
	if token == "" {
		noToken = "needs a Cloudflare token (ngitool edge init)"
	}
	noZone := ""
	if zone == nil {
		noZone = certs.GuessDomain(host) + " must be a zone in your Cloudflare account"
	}
	kind := o.kind
	ch := o.challenge
	if kind == "" {
		if err := ui.Need("certificate kind", "--kind letsencrypt|origin|custom|self-signed"); err != nil {
			return rq, err
		}
		le := ui.Option{Value: "le:http", Label: "Let's Encrypt · HTTP", Hint: strings.Join(names, " + ") + " via certbot on port 80 — free, renews itself, no Cloudflare needed"}
		if dns == model.DNSProxied {
			le.Hint += " (proxied: needs \"Always Use HTTPS\" off for the first request)"
		}
		led := ui.Option{Value: "le:dns", Label: "Let's Encrypt · DNS", Hint: strings.Join(names, " + ") + " via certbot + Cloudflare DNS — wildcards, works behind the proxy"}
		if h.noLetsEncrypt != "" {
			le.Disabled, led.Disabled = h.noLetsEncrypt, h.noLetsEncrypt
		} else if h.ed == nil {
			led.Disabled = "DNS-01 runs from the edge stack's certbot"
		} else if noToken != "" {
			led.Disabled = noToken
		} else if noZone != "" {
			led.Disabled = noZone
		}
		for _, n := range names {
			if strings.HasPrefix(n, "*.") && le.Disabled == "" {
				le.Disabled = "HTTP-01 cannot issue wildcards"
			}
		}
		orig := ui.Option{Value: certs.Origin, Label: "Cloudflare Origin CA", Hint: "15-year certificate, trusted only by Cloudflare"}
		switch {
		case dns == model.DNSOnly:
			orig.Disabled = "browsers don't trust it — it needs the orange cloud (CERT-03)"
		case noToken != "":
			orig.Disabled = noToken
		}
		opts := []ui.Option{le, led, orig,
			{Value: certs.Custom, Label: "Custom certificate", Hint: "import a full chain + private key you already have"},
			{Value: certs.SelfSigned, Label: "Self-signed", Hint: "testing only — browsers and Cloudflare Full (strict) reject it (CERT-03)"}}
		v, err := ui.Select(ui.SelectOpts{Title: "Certificate for " + strings.Join(names, ", "), Options: opts})
		if err != nil {
			return rq, err
		}
		switch v {
		case "le:http":
			kind, ch = certs.LetsEncrypt, certs.HTTP01
		case "le:dns":
			kind, ch = certs.LetsEncrypt, certs.DNS01
		default:
			kind = v
		}
	}
	switch kind {
	case certs.LetsEncrypt:
		if ch == "" {
			// DNS-01 when Cloudflare can do it, HTTP-01 when it cannot.
			ch = certs.HTTP01
			if token != "" && zone != nil && h.ed != nil {
				ch = certs.DNS01
			}
		}
		if ch != certs.HTTP01 && ch != certs.DNS01 {
			return rq, &UsageError{Msg: "--challenge must be http or dns"}
		}
		if ch == certs.DNS01 && (token == "" || zone == nil) {
			return rq, &UsageError{Msg: "--challenge dns needs a Cloudflare token and the domain in that account — use --challenge http"}
		}
	case certs.Origin, certs.SelfSigned:
	case certs.Custom:
		if (rq.CertFile == "" || rq.KeyFile == "") && ui.CanPrompt() {
			exists := func(v string) error {
				if _, err := os.Stat(v); err != nil {
					return errors.New("no such file")
				}
				return nil
			}
			var err error
			if rq.CertFile, err = ui.Input(ui.InputOpts{Title: "Certificate (full chain) file", Placeholder: "/path/fullchain.pem", Validate: exists}); err != nil {
				return rq, err
			}
			if rq.KeyFile, err = ui.Input(ui.InputOpts{Title: "Private key file", Placeholder: "/path/privkey.pem", Validate: exists}); err != nil {
				return rq, err
			}
		}
	default:
		return rq, &UsageError{Msg: "--kind must be letsencrypt, origin, custom or self-signed"}
	}
	if (kind == certs.Origin || kind == certs.SelfSigned) && dns == model.DNSOnly {
		ui.Warning(strings.Join(names, ", ") + " is not proxied: browsers reject a " + certs.Label(kind, "") + " certificate (CERT-03)")
	}
	rq.Kind, rq.Challenge = kind, ch
	rq.Name = firstNonEmpty(o.name, h.freeCertName(st, host))
	return rq, nil
}

// ── renew ───────────────────────────────────────────────────────────────────

func certRenewCmd(e *env) *cobra.Command {
	var force bool
	var instance string
	c := &cobra.Command{
		Use:   "renew [name]",
		Short: "renew Let's Encrypt certificates that are due (or one, --force: regardless)",
		Long: "certbot renew in the edge stack's certbot container (which also renews every 12h on its own), or\n" +
			"certbot on the host for host instances; then nginx -t and reload. Failures are named per certificate (CERT-02).",
		Annotations: map[string]string{annSynopsis: "renew [name] [--force]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
				c := st.Cert(instance, name)
				if c == nil {
					return &UsageError{Msg: "no certificate " + name + " — see: ngitool cert ls"}
				}
				if c.Kind != certs.LetsEncrypt {
					return &UsageError{Msg: name + " is not a Let's Encrypt certificate — see: ngitool cert ls"}
				}
				instance = c.Instance
			}
			rep := e.freshReport(ctx, true)
			insts := map[string]bool{}
			for _, c := range st.Certs {
				if c.Kind == certs.LetsEncrypt && (instance == "" || c.Instance == instance) {
					insts[c.Instance] = true
				}
			}
			if len(insts) == 0 {
				ui.Info("no Let's Encrypt certificates to renew")
				return nil
			}
			var failed []string
			for id := range insts {
				in := rep.Find(id)
				if in == nil {
					ui.Warning(id + " was not found by the scan — skipped")
					continue
				}
				h, err := e.homeOf(ctx, st, in)
				if err != nil {
					return err
				}
				what := "Let's Encrypt certificates that are due"
				switch {
				case name != "":
					what = name
				case force:
					what = "every Let's Encrypt certificate"
				}
				if force {
					what += " (forced)"
				}
				var res execx.Result
				_ = ui.Task("Renewing "+what+ui.Muted(" on "+in.Name), func(*ui.TaskCtl) error {
					args := certs.RenewArgs(name, force)
					if h.ed != nil {
						res = h.stack.Exec(ctx, append([]string{"run", "--rm", "--no-deps", "--entrypoint", "certbot", "certbot"}, args...)...)
					} else {
						res = certRunner.Run(ctx, "certbot", args, execx.Opts{Timeout: 10 * time.Minute})
					}
					if res.Code != 0 {
						return errors.New("")
					}
					return nil
				})
				for _, l := range certs.RenewSummary(res.Stdout, res.Stderr) {
					ui.Hint(l)
				}
				if res.Code != 0 {
					bad := certs.RenewFailures(res.Stdout + "\n" + res.Stderr)
					if len(bad) == 0 {
						bad = []string{"(certbot did not say which)"}
					}
					failed = append(failed, bad...)
					continue
				}
				if in.Caps.Reload.OK {
					if err := e.testAndReload(ctx, in); err != nil {
						return err
					}
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("renewal failed for %s (CERT-02)\n%s see why: ngitool certbot certificates", strings.Join(failed, ", "), ui.SymArrow)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "renew even when not due (--force-renewal)")
	c.Flags().StringVar(&instance, "instance", "", "only this instance")
	return c
}

// testAndReload is `ngitool reload`: nginx -t, then reload.
func (e *env) testAndReload(ctx context.Context, in *discover.Instance) error {
	drv := discover.Driver(newScanEnv(e), in)
	res, err := drv.Test(ctx)
	if err != nil {
		return err
	}
	if !res.OK {
		return errors.New("nginx -t fails, so nothing was reloaded (CONF-03)\n" + res.Output)
	}
	return ui.Task("Reloading "+in.Name, func(*ui.TaskCtl) error { return drv.Reload(ctx) })
}

// ── rm ──────────────────────────────────────────────────────────────────────

func certRmCmd(e *env) *cobra.Command {
	o := &rmOpts{}
	c := &cobra.Command{
		Use:         "rm [name]",
		Short:       "remove a certificate: move its routes to another one (--cert-to), or remove them too",
		Annotations: map[string]string{annSynopsis: "rm <name> [--cert-to OTHER] [--purge]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.kind = model.RmCert
			return e.removeCmd(cmd, o, args)
		},
	}
	o.add(c)
	return c
}

// ── aop ─────────────────────────────────────────────────────────────────────

func certAOPCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:   "aop <name> on|off",
		Short: "Authenticated Origin Pulls: only Cloudflare may connect to the hosts using a certificate",
		Long: "Every route using the certificate includes the edge stack's snippets/cloudflare-aop.conf (mTLS with\n" +
			"Cloudflare's origin-pull CA). Turn Authenticated Origin Pulls ON in Cloudflare first (SSL/TLS → Origin\n" +
			"Server), or every request fails with 400.",
		Annotations: map[string]string{annSynopsis: "aop <name> on|off"},
		Args:        maxArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) != 2 || (args[1] != "on" && args[1] != "off") {
				return &UsageError{Msg: "usage: ngitool cert aop <name> on|off"}
			}
			on := args[1] == "on"
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			c := st.Cert("", args[0])
			if c == nil {
				return &UsageError{Msg: "no certificate " + args[0] + " — see: ngitool cert ls"}
			}
			rep := e.freshReport(ctx, true)
			in := rep.Find(c.Instance)
			if in == nil {
				return errors.New(c.Instance + " was not found by the scan")
			}
			ed := edgeStackFor(st, in)
			if ed == nil {
				return errors.New("Authenticated Origin Pulls are set up by the edge stack (its snippets/cloudflare-aop.conf); " + in.Name + " is not one")
			}
			l := edge.Layout{Dir: ed.Dir}
			if _, err := os.Stat(l.AOPCA()); err != nil && on {
				ui.Info("the origin-pull CA is missing: run ngitool edge cf-sync first")
				return errors.New("no " + l.AOPCA())
			}
			if on {
				ui.Warning("Authenticated Origin Pulls must be ON in Cloudflare (SSL/TLS → Origin Server) first, or every request fails")
			}
			name := c.Name
			_, err = e.change(ctx, rep, in, f, "cert aop "+name+" "+args[1], st.CertUsers(in.ID, name), func(next *model.State) error {
				x := next.Cert(in.ID, name)
				if x == nil {
					return errors.New("certificate " + name + " is gone")
				}
				x.AOP = on
				for i := range next.Routes {
					r := &next.Routes[i]
					if r.Instance == in.ID && r.TLS != nil && r.TLS.Name == name {
						r.TLS.AOP = on
					}
				}
				return nil
			})
			if err == nil {
				ui.Done("Authenticated Origin Pulls " + args[1] + " for hosts using " + name)
			}
			return err
		},
	}
	f.add(c)
	return c
}

// ── certbot ─────────────────────────────────────────────────────────────────

func certbotCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:                "certbot [--dir DIR] [args…]",
		Short:              "run certbot in the edge stack's certbot container (default: certbot certificates)",
		Long:               "Everything after `certbot` goes to certbot untouched, e.g. ngitool certbot certonly --staging …",
		Annotations:        map[string]string{annGroup: "certs", annSynopsis: "certbot [args…]"},
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) >= 2 && args[0] == "--dir" {
				dir, args = args[1], args[2:]
			}
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			if len(args) == 0 {
				args = []string{"certificates"}
			}
			ctx := cmd.Context()
			_, stack, err := e.stackOf(ctx, dir)
			if err != nil {
				return err
			}
			ui.Step(ui.Muted("certbot " + strings.Join(args, " ")))
			o := stack.Opts()
			o.Inherit, o.Timeout = true, 0
			res := stack.Run.Run(ctx, stack.Bin.Name, stack.Argv(append([]string{"run", "--rm", "--no-deps", "--entrypoint", "certbot", "certbot"}, args...)...), o)
			if res.Code != 0 {
				return &ExitError{Code: res.Code}
			}
			return nil
		},
	}
}
