package cli

import (
	"context"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/cloudflare"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// rmOpts are the flags of every removal (rm, reset, cert rm, www rm,
// domain rm): --force allows a cascade off a terminal, --purge deletes
// what becomes unused, --purge-dns only its DNS records.
type rmOpts struct {
	txFlags
	kind, certTo, dir string
	purge, purgeDNS   bool
}

func (o *rmOpts) add(c *cobra.Command) {
	c.Flags().StringVar(&o.certTo, "cert-to", "", "certificates: move the routes using it to this certificate")
	c.Flags().BoolVar(&o.purge, "purge", false, "also delete what nothing uses any more: certificates, static folders, DNS records, app containers")
	c.Flags().BoolVar(&o.purgeDNS, "purge-dns", false, "also delete the Cloudflare DNS records of removed hosts")
	o.txFlags.add(c)
}

func rmCmd(e *env) *cobra.Command {
	o := &rmOpts{}
	var domain, app, cert, www, host string
	c := &cobra.Command{
		Use:   "rm [host[/path]]",
		Short: "remove a domain, host, path, app, certificate or static folder — and what goes with it",
		Long: "Shows the cascade first (cli/src/remove.mjs): a host takes its paths, a domain its hosts (but not those of\n" +
			"a more specific domain), an app the routes to it, a certificate the routes using it unless --cert-to moves\n" +
			"them, a static folder the routes serving it. Then what is left unused — certificates, static folders,\n" +
			"DNS records, app containers — is offered for deletion (pre-ticked: certificates), or deleted with --purge.\n" +
			"Off a terminal a cascade needs --force. Without an argument: a picker.",
		Example:     "ngitool rm api.example.com\nngitool rm example.com/blog --yes\nngitool rm --domain example.com --force --yes --purge\nngitool rm --cert old.example.com --cert-to example.com",
		Annotations: map[string]string{annGroup: "routes", annSynopsis: "rm [host[/path]] [--purge]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for k, v := range map[string]string{model.RmDomain: domain, model.RmApp: app, model.RmCert: cert, model.RmWWW: www, model.RmHost: host} {
				if v != "" {
					o.kind, args = k, []string{v}
				}
			}
			return e.removeCmd(cmd, o, args)
		},
	}
	fl := c.Flags()
	fl.StringVar(&domain, "domain", "", "remove a domain with its hosts")
	fl.StringVar(&host, "host", "", "remove a host with its paths")
	fl.StringVar(&app, "app", "", "unlink an app with the routes to it")
	fl.StringVar(&cert, "cert", "", "remove a certificate")
	fl.StringVar(&www, "www", "", "remove a static folder (www/NAME) with the routes serving it")
	fl.StringVar(&o.dir, "dir", "", "static folders: the edge stack's directory")
	o.add(c)
	return c
}

func resetCmd(e *env) *cobra.Command {
	o := &rmOpts{}
	c := &cobra.Command{
		Use:   "reset",
		Short: "remove everything NgiTool manages: routes, pools, certificates, domains, app links",
		Long: "Adopted instances and edge stacks stay (release them with instance release). Asks you to type \"reset\";\n" +
			"off a terminal it needs --yes --force.",
		Annotations: map[string]string{annGroup: "routes", annSynopsis: "reset [--purge]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.kind = model.RmAll
			return e.removeCmd(cmd, o, nil)
		},
	}
	o.add(c)
	return c
}

// removeCmd is the one removal flow: target, plan, confirm, extras, apply.
func (e *env) removeCmd(cmd *cobra.Command, o *rmOpts, args []string) error {
	ctx := cmd.Context()
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	t, err := e.rmTarget(ctx, st, o, args)
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	stackDir := e.wwwStackDir(st, *t, o.dir)
	exists := func(dir string) bool {
		if stackDir == "" {
			return false
		}
		p, err := edge.SafeWWW(stackDir, dir)
		if err != nil {
			return false
		}
		_, err = os.Stat(p)
		return err == nil
	}
	if t.Kind == model.RmCert && t.MoveTo == "" && !e.force {
		if t, err = certUsers(st, *t); err != nil || t == nil {
			if err == nil {
				err = errCancelled
			}
			return err
		}
	}
	rm, err := model.PlanRemoval(st, *t, exists)
	if err != nil {
		return &UsageError{Msg: err.Error()}
	}
	printRemoval(st, rm)
	switch {
	case t.Kind == model.RmAll:
		if ui.CanPrompt() && !e.yes {
			if err := ui.ConfirmTyped("Type \"reset\" to remove everything above", "reset"); err != nil {
				return err
			}
		} else if !(e.yes && e.force) {
			return &UsageError{Msg: "ngitool reset needs --yes --force off a terminal"}
		}
	case rm.Cascades():
		if !ui.CanPrompt() && !e.force {
			return &UsageError{Msg: "removing " + t.Label() + " also removes what is listed above — add --force"}
		}
		ui.Explain("removes "+t.Label()+" and everything listed above from nginx and from NgiTool",
			strconv.Itoa(len(rm.Routes))+" routes, "+strconv.Itoa(len(rm.Pools))+" pools",
			"ngitool rollback <instance> puts the nginx files back (a snapshot is taken first)")
		if ok, err := ui.Sure(e.yes, "Remove "+t.Label()+" and everything listed?", ""); err != nil || !ok {
			if err == nil {
				err = errCancelled
			}
			return err
		}
	default:
		if ok, err := ui.Sure(e.yes, "Remove "+t.Label()+"?", ""); err != nil || !ok {
			if err == nil {
				err = errCancelled
			}
			return err
		}
	}
	picked, err := pickExtras(o, e.yes, rm, exists)
	if err != nil {
		return err
	}
	if o.dryRun {
		ui.Hint("--dry-run: nothing was changed")
	}
	return e.applyRemoval(cmd, o, st, rm, picked, stackDir)
}

// rmTarget is the target from flags and args, or the pickers.
func (e *env) rmTarget(ctx context.Context, st *model.State, o *rmOpts, args []string) (*model.Target, error) {
	t := &model.Target{Kind: o.kind, MoveTo: o.certTo}
	if t.Kind == model.RmAll {
		return t, nil
	}
	if len(args) == 1 {
		t.Key = args[0]
		switch t.Kind {
		case "":
			h, p, err := model.ParseTarget(args[0])
			if err != nil {
				return nil, &UsageError{Msg: err.Error()}
			}
			switch {
			case p != "":
				t.Kind, t.Key = model.RmPath, model.RouteID(h, p)
			case !hostRouted(st, h) && st.Domain(h) != nil:
				t.Kind, t.Key = model.RmDomain, h
			default:
				t.Kind, t.Key = model.RmHost, h
			}
		case model.RmWWW:
			t.Key = edge.NormalizeWWW(args[0])
		case model.RmHost, model.RmDomain:
			h, err := model.NormalizeHost(args[0])
			if err != nil {
				return nil, &UsageError{Msg: err.Error()}
			}
			t.Key = h
		}
		return t, nil
	}
	if err := ui.Need("what to remove", "a host[/path] argument, or --domain|--app|--cert|--www NAME"); err != nil {
		return nil, err
	}
	if t.Kind == "" {
		count := map[string]int{model.RmDomain: len(st.Domains), model.RmApp: len(st.Apps), model.RmCert: len(st.Certs)}
		hosts := map[string]bool{}
		for _, r := range st.Routes {
			if r.Path == "" {
				hosts[r.Host] = true
			} else {
				count[model.RmPath]++
			}
		}
		count[model.RmHost] = len(hosts)
		count[model.RmWWW] = len(e.wwwRows(st, ""))
		opt := func(kind, label, hint string) ui.Option {
			o := ui.Option{Value: kind, Label: label, Hint: strconv.Itoa(count[kind]) + " · " + hint}
			if count[kind] == 0 {
				o.Disabled = "none"
			}
			return o
		}
		v, err := ui.Select(ui.SelectOpts{Title: "What do you want to remove?", Options: []ui.Option{
			opt(model.RmDomain, "Domain", "with its hosts, paths, certificate and DNS"),
			opt(model.RmHost, "Host", "with its paths"),
			opt(model.RmPath, "Path", "one path route"),
			opt(model.RmApp, "App", "unlink it, with the routes that use it"),
			opt(model.RmCert, "Certificate", "switch or remove the routes using it"),
			opt(model.RmWWW, "Static folder", "a folder in the edge stack's www/"),
			ui.Sep("careful"),
			{Value: model.RmAll, Label: "Everything (reset)", Hint: "every route, domain, certificate and app link"},
		}})
		if err != nil {
			return nil, err
		}
		t.Kind = v
		if v == model.RmAll {
			return t, nil
		}
	}
	var opts []ui.Option
	switch t.Kind {
	case model.RmDomain:
		for _, d := range st.Domains {
			opts = append(opts, ui.Option{Value: d.Name, Label: d.Name, Hint: "cert " + firstNonEmpty(d.Cert, "none")})
		}
	case model.RmHost, model.RmPath:
		for _, r := range st.Routes {
			if (t.Kind == model.RmHost) == (r.Path == "") {
				opts = append(opts, ui.Option{Value: r.ID, Label: r.ID, Hint: model.Describe(st.Pool(r.Pool)) + " · " + r.Instance})
			}
		}
	case model.RmApp:
		for _, a := range st.Apps {
			opts = append(opts, ui.Option{Value: a.Name, Label: a.Name, Hint: a.WorkingDir})
		}
	case model.RmCert:
		for _, c := range st.Certs {
			opts = append(opts, ui.Option{Value: c.Name, Label: c.Name, Hint: certs.Label(c.Kind, c.Challenge) + " · " + strings.Join(c.Names, ", ")})
		}
	case model.RmWWW:
		for _, w := range e.wwwRows(st, "") {
			hint := "not served"
			if len(w.Routes) > 0 {
				hint = "served at " + strings.Join(w.Routes, ", ")
			}
			opts = append(opts, ui.Option{Value: w.Dir, Label: "www/" + w.Dir, Hint: hint})
		}
	}
	if len(opts) == 0 {
		ui.Hint("no " + model.RmKinds[t.Kind] + "s to remove")
		return nil, nil
	}
	v, err := ui.Select(ui.SelectOpts{Title: "Remove which " + model.RmKinds[t.Kind] + "?", Options: opts, Filter: len(opts) > 8})
	if err != nil {
		return nil, err
	}
	t.Key = v
	return t, nil
}

func hostRouted(st *model.State, host string) bool {
	for _, r := range st.Routes {
		if r.Host == host {
			return true
		}
	}
	return false
}

// certUsers asks what happens to the routes of a certificate in use
// (switch to a covering one, or remove them); off a terminal it names the
// flags.
func certUsers(st *model.State, t model.Target) (*model.Target, error) {
	c := st.Cert(t.Instance, t.Key)
	if c == nil {
		return &t, nil
	}
	users := st.CertUsers(c.Instance, c.Name)
	if len(users) == 0 {
		return &t, nil
	}
	var others []string
	for _, o := range st.Certs {
		if o.Name == c.Name || o.Instance != c.Instance {
			continue
		}
		ok := true
		for _, id := range users {
			for _, n := range st.Route(id).Names() {
				ok = ok && certs.Covers(o.Names, n)
			}
		}
		if ok {
			others = append(others, o.Name)
		}
	}
	if !ui.CanPrompt() {
		msg := c.Name + " is used by " + strings.Join(users, ", ") + " — add --cert-to OTHER to move them"
		if len(others) > 0 {
			msg += " (" + strings.Join(others, ", ") + " covers them)"
		}
		return nil, &UsageError{Msg: msg + ", or --force to remove them too"}
	}
	var opts []ui.Option
	for _, n := range others {
		x := st.Cert(c.Instance, n)
		opts = append(opts, ui.Option{Value: "move:" + n, Label: "Switch to " + n, Hint: certs.Label(x.Kind, x.Challenge) + " · " + strings.Join(x.Names, ", ")})
	}
	opts = append(opts, ui.Option{Value: "drop", Label: either(len(users), "Remove that route too", "Remove those routes too")},
		ui.Option{Value: "cancel", Label: "Cancel"})
	v, err := ui.Select(ui.SelectOpts{Title: c.Name + " is used by " + strings.Join(users, ", ") + ". What should happen to " + either(len(users), "it", "them") + "?", Options: opts})
	if err != nil || v == "cancel" {
		return nil, err
	}
	if to, ok := strings.CutPrefix(v, "move:"); ok {
		t.MoveTo = to
	}
	return &t, nil
}

// printRemoval is the plan block of a removal.
func printRemoval(st *model.State, rm *model.Removal) {
	title := "Remove " + rm.Target.Label()
	if rm.Target.Kind == model.RmAll {
		title = "Remove everything NgiTool manages"
	}
	var pairs [][2]string
	row := func(k, v string) { pairs = append(pairs, [2]string{k, v}) }
	if len(rm.Domains) > 0 {
		row("Domains", strings.Join(rm.Domains, ", "))
	}
	for i, id := range rm.Routes {
		r := st.Route(id)
		k := ""
		if i == 0 {
			k = "Routes"
		}
		row(k, id+ui.Muted("  → "+model.Describe(st.Pool(r.Pool))+" · "+r.Instance))
	}
	if len(rm.Pools) > 0 {
		row("Pools", strings.Join(rm.Pools, ", "))
	}
	for p, ms := range rm.Members {
		row("Members", strings.Join(ms, ", ")+ui.Muted("  out of pool "+p))
	}
	for _, c := range rm.Certs {
		x := st.Cert(c.Instance, c.Name)
		row("Certificate", c.Name+ui.Muted("  "+certs.Label(x.Kind, x.Challenge)))
	}
	for _, a := range rm.Apps {
		row("App", a+ui.Muted("  unlinked — the project stays"))
	}
	for _, w := range rm.WWW {
		row("Folder", "www/"+w+ui.Muted("  deleted"))
	}
	var moved []string
	for id := range rm.Moved {
		moved = append(moved, id)
	}
	sort.Strings(moved)
	for _, id := range moved {
		row("Moves", id+ui.Muted(" → certificate ")+rm.Moved[id])
	}
	for d, to := range rm.DomainCert {
		row("Domain cert", d+ui.Muted(" → ")+firstNonEmpty(to, ui.Muted("none")))
	}
	if len(pairs) == 0 {
		row("Nothing else", ui.Muted("only "+rm.Target.Label()))
	}
	ui.Plan(title, pairs)
}

// pickExtras: --purge all, --purge-dns the DNS records, a terminal asks
// (certificates pre-ticked), otherwise none.
func pickExtras(o *rmOpts, yes bool, rm *model.Removal, exists func(string) bool) (map[string]bool, error) {
	var opts []ui.Option
	var pre []string
	for _, c := range rm.Extras.Certs {
		v := "cert:" + c.Instance + "|" + c.Name
		opts = append(opts, ui.Option{Value: v, Label: "certificate " + c.Name, Hint: "nothing else uses it — its files are deleted"})
		pre = append(pre, v)
	}
	for _, w := range rm.Extras.WWW {
		if exists(w) {
			opts = append(opts, ui.Option{Value: "www:" + w, Label: "www/" + w, Hint: "static files — deleted from disk"})
		}
	}
	for _, h := range rm.Extras.DNS {
		opts = append(opts, ui.Option{Value: "dns:" + h, Label: "DNS " + h, Hint: "A/AAAA records in Cloudflare"})
	}
	for _, a := range rm.Extras.Down {
		opts = append(opts, ui.Option{Value: "down:" + a, Label: "stop " + a, Hint: "docker compose down — data volumes are kept"})
	}
	out := map[string]bool{}
	switch {
	case len(opts) == 0:
	case o.purge:
		for _, x := range opts {
			out[x.Value] = true
		}
	case ui.CanPrompt() && !yes:
		vs, err := ui.MultiSelect(ui.MultiOpts{Title: "Also delete what nothing else uses?", Options: opts, Selected: pre})
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			out[v] = true
		}
	case o.purgeDNS:
		for _, x := range opts {
			if strings.HasPrefix(x.Value, "dns:") {
				out[x.Value] = true
			}
		}
	}
	return out, nil
}

func picked(p map[string]bool, prefix string) []string {
	var out []string
	for v := range p {
		if s, ok := strings.CutPrefix(v, prefix); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// applyRemoval makes it so: stop apps, one transaction per instance, then
// certificate files, folders, overrides, DNS records and state.
func (e *env) applyRemoval(cmd *cobra.Command, o *rmOpts, st *model.State, rm *model.Removal, sel map[string]bool, stackDir string) error {
	ctx := cmd.Context()
	if o.dryRun {
		rep := e.freshReport(ctx, true)
		for _, id := range rm.Instances(st) {
			if in := lookupInstance(rep, id); in != nil {
				if _, err := e.change(ctx, rep, in, o.txFlags, "rm "+rm.Target.Label(), nil, func(next *model.State) error {
					rm.ApplyOn(next, id)
					return nil
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Containers first: `down` needs the app's override, which goes next.
	for _, name := range picked(sel, "down:") {
		a := st.App(name)
		if a == nil {
			continue
		}
		b, err := e.composeBin(ctx)
		if err != nil {
			return err
		}
		p := a.Compose()
		_ = ui.Task("Stopping "+name, func(*ui.TaskCtl) error {
			res := appRunner.Run(ctx, b.Name, compose.Argv(b, p, []string{"down"}), p.Opts())
			if res.Code != 0 {
				ui.Warning(execx.Tail(res, 4))
			}
			return nil
		})
	}
	rep := e.freshReport(ctx, true)
	for _, id := range rm.Instances(st) {
		in := lookupInstance(rep, id)
		if in == nil {
			return errors.New(id + " was not found by the scan — run: ngitool scan")
		}
		if _, err := e.change(ctx, rep, in, o.txFlags, "rm "+rm.Target.Label(), nil, func(next *model.State) error {
			rm.ApplyOn(next, id)
			return nil
		}); err != nil {
			return err
		}
	}
	// Files and records, only after nginx accepted the change.
	var extraCerts []model.CertRef
	for _, s := range picked(sel, "cert:") {
		inst, name, _ := strings.Cut(s, "|")
		extraCerts = append(extraCerts, model.CertRef{Instance: inst, Name: name})
	}
	for _, c := range append(append([]model.CertRef{}, rm.Certs...), extraCerts...) {
		x := st.Cert(c.Instance, c.Name)
		in := lookupInstance(rep, c.Instance)
		if x == nil || in == nil {
			continue
		}
		h, err := e.homeOf(ctx, st, in)
		if err == nil {
			err = ui.Task("Deleting certificate "+c.Name, func(*ui.TaskCtl) error { return e.removeCertFiles(ctx, h, *x) })
		}
		if err != nil {
			ui.Warning(err.Error())
		}
	}
	for _, w := range append(append([]string{}, rm.WWW...), picked(sel, "www:")...) {
		if stackDir == "" {
			continue
		}
		if p, err := edge.SafeWWW(stackDir, w); err == nil {
			if _, err := os.Stat(p); err == nil && os.RemoveAll(p) == nil {
				ui.Done("deleted www/" + w)
			}
		}
	}
	for _, name := range rm.Apps {
		if a := st.App(name); a != nil && a.Override != "" {
			_ = os.Remove(a.Override)
		}
		ui.Done(name + " unlinked " + ui.Muted("— the project itself is untouched"))
	}
	if hosts := picked(sel, "dns:"); len(hosts) > 0 {
		e.deleteDNS(ctx, st, hosts)
	}
	// What nothing uses any more but stays, so it can be deleted later.
	var kept []string
	for _, c := range rm.Extras.Certs {
		if !sel["cert:"+c.Instance+"|"+c.Name] {
			kept = append(kept, "certificate "+c.Name+" (ngitool cert rm "+c.Name+")")
		}
	}
	for _, w := range rm.Extras.WWW {
		if !sel["www:"+w] {
			kept = append(kept, "www/"+w+" (ngitool www rm "+w+")")
		}
	}
	for _, h := range rm.Extras.DNS {
		if !sel["dns:"+h] {
			kept = append(kept, "DNS "+h)
		}
	}
	if len(kept) > 0 {
		ui.Hint("kept, unused now: " + strings.Join(kept, ", ") + " — --purge deletes them")
	}
	return e.withLock(ctx, true, func() error {
		cur, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		rm.ApplyState(cur, extraCerts)
		if err := model.Save(e.paths, cur); err != nil {
			return err
		}
		ui.Done(rm.Target.Label() + " removed")
		return nil
	})
}

// deleteDNS removes the A/AAAA records of hosts in Cloudflare.
func (e *env) deleteDNS(ctx context.Context, st *model.State, hosts []string) {
	token := ""
	for _, x := range st.Edges {
		if token = edge.ReadToken(x.Dir); token != "" {
			break
		}
	}
	token = firstNonEmpty(token, os.Getenv("CLOUDFLARE_API_TOKEN"))
	if token == "" {
		ui.Warning("no Cloudflare token — delete the DNS records for " + strings.Join(hosts, ", ") + " yourself")
		return
	}
	cf := cloudflare.New(token)
	for _, h := range hosts {
		z := zoneFor(ctx, st, strings.TrimPrefix(h, "www."), token)
		if z == nil {
			ui.Warning(h + ": no Cloudflare zone found — DNS record left alone")
			continue
		}
		_ = ui.Task("Deleting DNS record "+h, func(t *ui.TaskCtl) error {
			n, err := cf.DeleteRecords(ctx, z.ID, h)
			if err != nil {
				ui.Warning(err.Error())
				return nil
			}
			t.Update("Deleting DNS record " + h + ui.Muted(" ("+strconv.Itoa(n)+" removed)"))
			return nil
		})
	}
}

// wwwStackDir is the edge stack whose www/ a removal touches.
func (e *env) wwwStackDir(st *model.State, t model.Target, dir string) string {
	if dir != "" {
		return dir
	}
	if t.Instance != "" {
		if ed := st.EdgeOf(t.Instance); ed != nil {
			return ed.Dir
		}
	}
	for _, r := range st.Routes {
		if ed := st.EdgeOf(r.Instance); ed != nil && (t.Kind != model.RmHost || r.Host == t.Key) {
			return ed.Dir
		}
	}
	if len(st.Edges) > 0 {
		return st.Edges[0].Dir
	}
	return ""
}

// ── www ─────────────────────────────────────────────────────────────────────

// wwwRows are the folders of a stack's www/ with the routes serving them.
func (e *env) wwwRows(st *model.State, dir string) []edge.WWWDir {
	if dir == "" {
		if len(st.Edges) == 0 {
			return nil
		}
		dir = st.Edges[0].Dir
	}
	ed := st.EdgeAt(dir)
	used := map[string][]string{}
	for _, r := range st.Routes {
		if ed != nil && r.Instance != ed.Instance {
			continue
		}
		if p := st.Pool(r.Pool); p != nil && p.Static() {
			used[p.Members[0].Ref] = append(used[p.Members[0].Ref], r.ID)
		}
	}
	return edge.ListWWW(dir, used)
}

func wwwCmd(e *env) *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:         "www",
		Short:       "static folders in the edge stack's www/",
		Long:        "ls  every folder with the routes serving it\nrm  delete a folder with the routes serving it (asked first)",
		Annotations: map[string]string{annGroup: "edge", annSynopsis: "www ls|rm"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.PersistentFlags().StringVar(&dir, "dir", "", "the edge stack's directory (default: the one NgiTool runs)")
	var asJSON bool
	ls := &cobra.Command{
		Use: "ls", Short: "every folder in www/ with the routes serving it", Args: noArgs,
		Annotations: map[string]string{annSynopsis: "ls [--json]"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			d := dir
			if d == "" {
				ed, _, err := e.stackOf(cmd.Context(), "")
				if err != nil {
					return err
				}
				d = ed.Dir
			}
			rows := e.wwwRows(st, d)
			if asJSON {
				if rows == nil {
					rows = []edge.WWWDir{}
				}
				return printJSON(rows)
			}
			ui.Heading("Static folders", edge.Layout{Dir: d}.WWW()+"/")
			if len(rows) == 0 {
				ui.Hint("none yet — they are made by a route with a static target (route add … --to static:NAME)")
				return nil
			}
			var t [][]string
			for _, w := range rows {
				served := ui.Muted("not served")
				if len(w.Routes) > 0 {
					served = ui.Muted("served at ") + strings.Join(w.Routes, ", ")
				}
				t = append(t, []string{ui.Bold("www/" + w.Dir), served})
			}
			ui.PrintTable(t, ui.TableOpts{Indent: 2})
			return nil
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	o := &rmOpts{}
	rm := &cobra.Command{
		Use: "rm [folder]", Short: "delete a folder with the routes serving it", Args: maxArgs(1),
		Annotations: map[string]string{annSynopsis: "rm <folder> [--purge]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			o.kind, o.dir = model.RmWWW, dir
			return e.removeCmd(cmd, o, args)
		},
	}
	o.add(rm)
	c.AddCommand(ls, rm)
	return c
}

// ── domain ──────────────────────────────────────────────────────────────────

func domainCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:         "domain",
		Short:       "Cloudflare zones NgiTool keeps DNS records in",
		Long:        "ls  every domain with its zone and default certificate\nrm  remove a domain with its hosts (hosts of a more specific domain stay)",
		Annotations: map[string]string{annGroup: "certs", annSynopsis: "domain ls|rm"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var asJSON bool
	ls := &cobra.Command{
		Use: "ls", Short: "every domain with its zone and certificate", Args: noArgs,
		Annotations: map[string]string{annSynopsis: "ls [--json]"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(st.Domains)
			}
			ui.Heading("Domains", strconv.Itoa(len(st.Domains)))
			if len(st.Domains) == 0 {
				ui.Hint("none — a route with a Cloudflare DNS step, or the migration, records them")
				return nil
			}
			var t [][]string
			for _, d := range st.Domains {
				zone := ui.Warn("not in Cloudflare")
				if d.Zone != nil {
					zone = ui.Muted("zone " + d.Zone.ID[:min(8, len(d.Zone.ID))] + "…")
				}
				apex := ui.Muted("apex not served")
				if r := st.Route(d.Name); r != nil {
					apex = model.Describe(st.Pool(r.Pool))
				}
				t = append(t, []string{ui.Bold(d.Name), ui.Muted("cert " + firstNonEmpty(d.Cert, "none")), zone, apex})
			}
			ui.PrintTable(t, ui.TableOpts{Indent: 2})
			return nil
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	o := &rmOpts{}
	rm := &cobra.Command{
		Use: "rm [domain]", Short: "remove a domain with its hosts, paths and (asked) certificate and DNS", Args: maxArgs(1),
		Annotations: map[string]string{annSynopsis: "rm <domain> [--purge]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			o.kind = model.RmDomain
			return e.removeCmd(cmd, o, args)
		},
	}
	o.add(rm)
	c.AddCommand(ls, rm)
	return c
}
