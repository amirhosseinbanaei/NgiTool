package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

func poolCmd(e *env) *cobra.Command {
	c := &cobra.Command{
		Use:   "pool",
		Short: "load-balanced pools: members, methods, drain, blue/green, health",
		Long: "ls | show <pool>            list pools / one pool with its members and their health\n" +
			"add <pool> --to … | rm <pool>  create a pool (for --pool on route add) / remove it and its routes\n" +
			"member add|rm|weight|backup     change members\n" +
			"drain | undrain <pool> <member> take a member out (marked down) and back (LB-09)\n" +
			"switch <pool> --to …            blue/green: swap the active set, keep the old one as backup (LB-09)\n" +
			"check [pool] [--mark-down]      request every member directly from the instance's network (LB-08)",
		Annotations: map[string]string{annGroup: "lb", annSynopsis: "pool ls|show|drain|check|…"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	member := &cobra.Command{Use: "member", Short: "add, remove, weigh or back up pool members", Args: noArgs,
		Annotations: map[string]string{annSynopsis: "member add|rm|weight|backup <pool> …"},
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	member.AddCommand(memberAddCmd(e), memberRmCmd(e), memberWeightCmd(e), memberBackupCmd(e))
	c.AddCommand(poolLsCmd(e), poolShowCmd(e), poolAddCmd(e), poolRmCmd(e), member, drainCmd(e, true), drainCmd(e, false), switchCmd(e), checkCmd(e))
	return c
}

// poolChange loads the pool, applies mut to it in a fresh copy of state,
// validates, and runs the transaction on its instance.
func (e *env) poolChange(cmd *cobra.Command, f txFlags, name, summary string, mut func(next *model.State, p *model.Pool) error) (*apply.Result, *model.State, error) {
	ctx := cmd.Context()
	st, err := model.Load(e.paths)
	if err != nil {
		return nil, nil, err
	}
	p := st.Pool(name)
	if p == nil {
		return nil, nil, &UsageError{Msg: "no pool " + name + " — see: ngitool pool ls"}
	}
	rep := e.freshReport(ctx, true)
	in := lookupInstance(rep, p.Instance)
	if in == nil {
		return nil, nil, fmt.Errorf("instance %s was not found by the scan", p.Instance)
	}
	var probe []string
	for _, r := range st.RoutesOf(name) {
		probe = append(probe, r.ID)
	}
	var after *model.State
	res, err := e.change(ctx, rep, in, f, summary, probe, func(next *model.State) error {
		np := next.Pool(name)
		if np == nil {
			return errors.New("pool " + name + " was removed meanwhile")
		}
		if err := mut(next, np); err != nil {
			return err
		}
		after = next
		var routes []string
		for _, r := range next.RoutesOf(name) {
			routes = append(routes, r.ID)
		}
		return validate(next, rep, routes, []string{name})
	})
	return res, after, err
}

func poolArg(st *model.State, args []string, i int, title string) (string, error) {
	if len(args) > i {
		return args[i], nil
	}
	if err := ui.Need("pool", "<pool>"); err != nil {
		return "", err
	}
	if len(st.Pools) == 0 {
		return "", errors.New("no pools yet — a route with two targets makes one: ngitool route add")
	}
	var opts []ui.Option
	for _, p := range st.Pools {
		opts = append(opts, ui.Option{Value: p.Name, Label: p.Name, Hint: model.Describe(&p) + " · " + p.Instance})
	}
	return ui.Select(ui.SelectOpts{Title: title, Options: opts})
}

func memberArg(p *model.Pool, args []string, i int, title string, filter func(model.Member) string) (int, error) {
	if len(args) > i {
		idx := p.Member(args[i])
		if idx < 0 {
			var ls []string
			for _, m := range p.Members {
				ls = append(ls, m.Label())
			}
			return -1, &UsageError{Msg: "pool " + p.Name + " has no member " + args[i] + " (members: " + strings.Join(ls, ", ") + ")"}
		}
		return idx, nil
	}
	if err := ui.Need("member", "<member>"); err != nil {
		return -1, err
	}
	var opts []ui.Option
	for i, m := range p.Members {
		o := ui.Option{Value: strconv.Itoa(i), Label: m.Label(), Hint: m.Kind + " " + strings.Join(m.Params(), " ")}
		if filter != nil {
			o.Disabled = filter(m)
		}
		opts = append(opts, o)
	}
	v, err := ui.Select(ui.SelectOpts{Title: title, Options: opts})
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(v)
}

// ---- ls / show ------------------------------------------------------------

func poolLsCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "ls", Short: "every pool", Args: noArgs,
		Annotations: map[string]string{annSynopsis: "ls [--json]"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			if asJSON {
				out := st.Pools
				if out == nil {
					out = []model.Pool{}
				}
				return printJSON(out)
			}
			if len(st.Pools) == 0 {
				ui.Info("no pools yet")
				ui.Hint("a route with two or more targets makes one: ngitool route add")
				return nil
			}
			rows := [][]string{}
			pools := append([]model.Pool{}, st.Pools...)
			sort.Slice(pools, func(i, j int) bool { return pools[i].Name < pools[j].Name })
			for _, p := range pools {
				var rs []string
				for _, r := range st.RoutesOf(p.Name) {
					rs = append(rs, r.ID)
				}
				rows = append(rows, []string{ui.Bold(p.Name), p.Instance, p.MethodText(), memberCount(&p), dash(strings.Join(rs, ", "))})
			}
			ui.Plain("")
			ui.PrintTable(rows, ui.TableOpts{Indent: 2, Header: []string{"POOL", "INSTANCE", "METHOD", "MEMBERS", "ROUTES"}})
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func memberCount(p *model.Pool) string {
	n, down, backup := len(p.Members), 0, 0
	for _, m := range p.Members {
		if m.Down {
			down++
		} else if m.Backup {
			backup++
		}
	}
	s := strconv.Itoa(n)
	if down > 0 {
		s += ui.Warn(" (" + strconv.Itoa(down) + " down)")
	}
	if backup > 0 {
		s += ui.Muted(" (" + strconv.Itoa(backup) + " backup)")
	}
	return s
}

func poolShowCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use: "show <pool>", Short: "one pool: method, members, health, routes", Args: maxArgs(1),
		Annotations: map[string]string{annSynopsis: "show <pool> [--json]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name, err := poolArg(st, args, 0, "Which pool?")
			if err != nil {
				return err
			}
			p := st.Pool(name)
			if p == nil {
				return &UsageError{Msg: "no pool " + name + " — see: ngitool pool ls"}
			}
			if asJSON {
				return printJSON(p)
			}
			h := apply.LoadHealth(e.paths.Cache)
			var rs []string
			for _, r := range st.RoutesOf(name) {
				rs = append(rs, r.ID)
			}
			pairs := [][2]string{{"Instance", p.Instance}, {"Upstream", p.Upstream() + ui.Muted("  "+p.Scheme)}, {"Method", p.MethodText() + ui.Muted(stickyNote(p.Sticky))}}
			if p.Keepalive > 0 {
				pairs = append(pairs, [2]string{"Keepalive", strconv.Itoa(p.Keepalive)})
			}
			if p.ErrorPage != "" {
				pairs = append(pairs, [2]string{"Error page", p.ErrorPage})
			}
			pairs = append(pairs, [2]string{"Routes", dash(strings.Join(rs, ", "))})
			ui.Plan("Pool "+name, pairs)
			ui.PrintTable(memberRows(p, h), ui.TableOpts{Indent: 2, Header: []string{"", "MEMBER", "KIND", "PARAMS", "LAST CHECK"}})
			if len(p.Previous) > 0 {
				ui.Hint("blue/green: the previous set is kept as backup — confirm with: ngitool pool switch " + name + " --confirm")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func memberRows(p *model.Pool, h *apply.Health) [][]string {
	var rows [][]string
	for _, m := range p.Members {
		dot, last := ui.Muted(ui.SymRing), ui.Muted("–")
		if mh, ok := h.Members[p.Name+" "+m.Label()]; ok {
			if mh.OK {
				dot = ui.OK(ui.SymDot)
			} else {
				dot = ui.Err(ui.SymErr)
			}
			last = firstNonEmpty(strconv.Itoa(mh.Status), "–")
			if mh.Err != "" {
				last = mh.Err
			}
			last += ui.Muted(" · " + mh.Took.Round(time.Millisecond).String() + " · " + mh.At.Local().Format("Jan 2 15:04"))
		}
		label := m.Label()
		switch {
		case m.Down:
			label += " " + ui.Warn("down")
		case m.Backup:
			label += " " + ui.Muted("backup")
		}
		rows = append(rows, []string{dot, label, m.Kind, dash(strings.Join(m.Params(), " ")), last})
	}
	return rows
}

// ---- add / rm -------------------------------------------------------------

func poolAddCmd(e *env) *cobra.Command {
	var f txFlags
	var instance, method, hashKey, sticky, scheme, errorPage string
	var to []string
	var keepalive int
	var consistent bool
	c := &cobra.Command{
		Use: "add <pool>", Short: "create a pool for routes to share (route add --pool)", Args: maxArgs(1),
		Annotations: map[string]string{annSynopsis: "add <pool> --to TARGET… [--method M]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			} else {
				if err := ui.Need("pool name", "<pool>"); err != nil {
					return err
				}
				if name, err = ui.Input(ui.InputOpts{Title: "Pool name", Placeholder: "shop-web", Validate: model.ValidPoolName}); err != nil {
					return err
				}
			}
			if err := model.ValidPoolName(name); err != nil {
				return &UsageError{Msg: err.Error()}
			}
			if st.Pool(name) != nil {
				return problemErr(model.Problem{Code: "LB-13", Msg: "a pool named " + name + " already exists"})
			}
			rep := e.freshReport(ctx, true)
			in, err := e.instanceFor(rep, st, instance, true)
			if err != nil {
				return err
			}
			p := model.Pool{Name: name, Instance: in.ID, Scheme: scheme, Keepalive: keepalive, ErrorPage: errorPage, HashKey: hashKey, Consistent: consistent}
			if len(to) == 0 {
				if err := ui.Need("targets", "--to TARGET"); err != nil {
					return err
				}
				if to, err = pickMembers(rep, in, nil); err != nil {
					return err
				}
			}
			if err := membersFromSpecs(rep, in, to, &p); err != nil {
				return err
			}
			switch {
			case method != "":
				if p.Method, err = model.ParseMethod(method); err != nil {
					return codeErr(err)
				}
			case len(p.Members) > 1 && ui.CanPrompt():
				if err := askMethod(&p); err != nil {
					return err
				}
			default:
				p.Method = model.DefaultMethod
			}
			if sticky != "" {
				if err := model.ApplySticky(&p, sticky); err != nil {
					return codeErr(err)
				}
			}
			if err := e.offerConnect(ctx, rep, in, &p, false); err != nil {
				return err
			}
			_, err = e.change(ctx, rep, in, f, "pool add "+name, nil, func(next *model.State) error {
				next.Pools = append(next.Pools, p)
				return validate(next, rep, nil, []string{name})
			})
			if err == nil {
				ui.Hint("a pool is written once a route uses it: ngitool route add <host> --pool " + name)
			}
			return err
		},
	}
	fl := c.Flags()
	fl.StringVar(&instance, "instance", "", "instance id (default: the front door)")
	fl.StringArrayVar(&to, "to", nil, "a member; repeatable (see route add)")
	fl.StringVar(&method, "method", "", "round_robin, least_conn (default), ip_hash, hash, random_two, random")
	fl.StringVar(&hashKey, "hash-key", "", "key for --method hash")
	fl.BoolVar(&consistent, "consistent", false, "consistent hashing")
	fl.StringVar(&sticky, "sticky", "", "ip, cloudflare or cookie:NAME (LB-07)")
	fl.StringVar(&scheme, "scheme", "", "http (default), https, grpc, grpcs")
	fl.IntVar(&keepalive, "keepalive", 0, "idle upstream connections per worker (LB-06)")
	fl.StringVar(&errorPage, "error-page", "", "page shown when every member is down (LB-11)")
	f.add(c)
	return c
}

func poolRmCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use: "rm <pool>", Short: "remove a pool and every route that uses it", Args: maxArgs(1),
		Long:        "A pool used by routes takes them with it (LB-12): they are listed before anything is asked.",
		Annotations: map[string]string{annSynopsis: "rm <pool> [--dry-run]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name, err := poolArg(st, args, 0, "Remove which pool?")
			if err != nil {
				return err
			}
			p := st.Pool(name)
			if p == nil {
				return &UsageError{Msg: "no pool " + name}
			}
			rep := e.freshReport(ctx, true)
			in := lookupInstance(rep, p.Instance)
			if in == nil {
				return fmt.Errorf("instance %s was not found by the scan", p.Instance)
			}
			var routes []string
			for _, r := range st.RoutesOf(name) {
				routes = append(routes, r.ID)
			}
			rm := cascade{routes: routes, pools: []string{name}}
			printCascade("Remove pool "+name, st, rm)
			if len(routes) > 0 && !f.dryRun {
				ui.Explain("removes pool "+name+" and the "+strconv.Itoa(len(routes))+" routes that use it (LB-12)", strings.Join(routes, ", ")+" stop being served", "ngitool rollback "+in.ID)
			}
			_, err = e.change(ctx, rep, in, f, "pool rm "+name, nil, func(next *model.State) error {
				rm.apply(next)
				return nil
			})
			return err
		},
	}
	f.add(c)
	return c
}

// ---- members ----------------------------------------------------------------

func memberAddCmd(e *env) *cobra.Command {
	var f txFlags
	var connect bool
	c := &cobra.Command{
		Use: "add <pool> [TARGET…]", Short: "add members to a pool", Args: cobra.ArbitraryArgs,
		Annotations: map[string]string{annSynopsis: "add <pool> TARGET… [--connect]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name, err := poolArg(st, args, 0, "Add members to which pool?")
			if err != nil {
				return err
			}
			p := st.Pool(name)
			if p == nil {
				return &UsageError{Msg: "no pool " + name}
			}
			specs := []string{}
			if len(args) > 1 {
				specs = args[1:]
			}
			rep := e.freshReport(cmd.Context(), true)
			in := lookupInstance(rep, p.Instance)
			if in == nil {
				return fmt.Errorf("instance %s was not found by the scan", p.Instance)
			}
			if len(specs) == 0 {
				if err := ui.Need("targets", "TARGET…"); err != nil {
					return err
				}
				if specs, err = pickMembers(rep, in, nil); err != nil {
					return err
				}
			}
			draft := *p
			draft.Members = append([]model.Member{}, p.Members...)
			if err := membersFromSpecs(rep, in, specs, &draft); err != nil {
				return err
			}
			if err := e.offerConnect(cmd.Context(), rep, in, &draft, connect); err != nil {
				return err
			}
			added := draft.Members[len(p.Members):]
			_, _, err = e.poolChange(cmd, f, name, "pool member add "+name, func(_ *model.State, np *model.Pool) error {
				np.Members = append(np.Members, added...)
				np.Scheme = draft.Scheme
				return nil
			})
			return err
		},
	}
	c.Flags().BoolVar(&connect, "connect", false, "connect containers that share no network with the instance (RP-05)")
	f.add(c)
	return c
}

func memberRmCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use: "rm <pool> <member>", Short: "remove a member", Args: maxArgs(2),
		Annotations: map[string]string{annSynopsis: "rm <pool> <member>"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.memberEdit(cmd, f, args, "Remove which member?", "pool member rm", nil, func(p *model.Pool, i int) error {
				if len(p.Members) == 1 {
					return problemErr(model.Problem{Code: "LB-11", Msg: "that is the last member of " + p.Name, Fix: "remove the pool or its routes instead: ngitool pool rm " + p.Name})
				}
				p.Members = append(p.Members[:i], p.Members[i+1:]...)
				return nil
			})
		},
	}
	f.add(c)
	return c
}

func memberWeightCmd(e *env) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use: "weight <pool> <member> <n>", Short: "set a member's weight", Args: maxArgs(3),
		Annotations: map[string]string{annSynopsis: "weight <pool> <member> <n>"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 3 {
				if err := ui.Need("weight", "<pool> <member> <n>"); err != nil {
					return err
				}
			}
			return e.memberEdit(cmd, f, args, "Weigh which member?", "pool member weight", nil, func(p *model.Pool, i int) error {
				w := ""
				if len(args) == 3 {
					w = args[2]
				} else {
					var err error
					if w, err = ui.Input(ui.InputOpts{Title: "Weight of " + p.Members[i].Label(), Default: strconv.Itoa(max(1, p.Members[i].Weight)),
						Validate: func(v string) error { return model.SetParam(&model.Member{}, "weight="+v) }}); err != nil {
						return err
					}
				}
				return model.SetParam(&p.Members[i], "weight="+w)
			})
		},
	}
	f.add(c)
	return c
}

func memberBackupCmd(e *env) *cobra.Command {
	var f txFlags
	var off bool
	c := &cobra.Command{
		Use: "backup <pool> <member>", Short: "make a member a backup (only used when the others fail)", Args: maxArgs(2),
		Annotations: map[string]string{annSynopsis: "backup <pool> <member> [--off]"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.memberEdit(cmd, f, args, "Which member?", "pool member backup", nil, func(p *model.Pool, i int) error {
				p.Members[i].Backup = !off
				return nil
			})
		},
	}
	c.Flags().BoolVar(&off, "off", false, "make it a normal member again")
	f.add(c)
	return c
}

func (e *env) memberEdit(cmd *cobra.Command, f txFlags, args []string, title, summary string, filter func(model.Member) string, mut func(p *model.Pool, i int) error) error {
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	name, err := poolArg(st, args, 0, "Which pool?")
	if err != nil {
		return err
	}
	p := st.Pool(name)
	if p == nil {
		return &UsageError{Msg: "no pool " + name}
	}
	idx, err := memberArg(p, args, 1, title, filter)
	if err != nil {
		return err
	}
	label := p.Members[idx].Label()
	_, _, err = e.poolChange(cmd, f, name, summary+" "+name+" "+label, func(_ *model.State, np *model.Pool) error {
		i := np.Member(label)
		if i < 0 {
			return errors.New(label + " left the pool meanwhile")
		}
		return mut(np, i)
	})
	return err
}

// ---- drain / undrain / switch (LB-09) ---------------------------------------

func drainCmd(e *env, down bool) *cobra.Command {
	var f txFlags
	use, short := "drain <pool> <member>", "take a member out of rotation (marked down) for a deploy (LB-09)"
	if !down {
		use, short = "undrain <pool> <member>", "put a drained member back (LB-09)"
	}
	c := &cobra.Command{
		Use: use, Short: short, Args: maxArgs(2),
		Annotations: map[string]string{annSynopsis: use},
		RunE: func(cmd *cobra.Command, args []string) error {
			filter := func(m model.Member) string {
				if m.Down == down {
					return map[bool]string{true: "already drained", false: "not drained"}[down]
				}
				return ""
			}
			verb := map[bool]string{true: "drain", false: "undrain"}[down]
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name, err := poolArg(st, args, 0, "Which pool?")
			if err != nil {
				return err
			}
			p := st.Pool(name)
			if p == nil {
				return &UsageError{Msg: "no pool " + name}
			}
			idx, err := memberArg(p, args, 1, map[bool]string{true: "Drain which member?", false: "Bring back which member?"}[down], filter)
			if err != nil {
				return err
			}
			label := p.Members[idx].Label()
			if p.Members[idx].Down == down {
				ui.Info(label + " is " + filter(p.Members[idx]))
				return nil
			}
			if down && len(p.Active()) == 1 && !p.Members[idx].Backup {
				ui.Warning("that is the last active member: " + name + " answers 502 until one is back" + errPage(p) + " " + ui.Muted("(LB-11)"))
			}
			_, after, err := e.poolChange(cmd, f, name, "pool "+verb+" "+name+" "+label, func(_ *model.State, np *model.Pool) error {
				np.Members[np.Member(label)].Down = down
				return nil
			})
			if err == nil && after != nil && !f.dryRun {
				ap := after.Pool(name)
				ui.Heading("Members of "+name, "")
				ui.PrintTable(memberRows(ap, apply.LoadHealth(e.paths.Cache)), ui.TableOpts{Indent: 2})
				if down {
					ui.Hint("in-flight requests finish on " + label + "; bring it back with: ngitool pool undrain " + name + " " + label)
				}
			}
			return err
		},
	}
	f.add(c)
	return c
}

func errPage(p *model.Pool) string {
	if p.ErrorPage != "" {
		return " (" + p.ErrorPage + " is shown)"
	}
	return ""
}

func switchCmd(e *env) *cobra.Command {
	var f txFlags
	var to []string
	var confirm, revert bool
	c := &cobra.Command{
		Use:   "switch <pool> --to TARGET…",
		Short: "blue/green: make another set of members active in one apply (LB-09)",
		Long: "The new members take all traffic; the old ones stay as backup members (marked down when the method\n" +
			"cannot have backups) until you confirm. --confirm drops the old set, --revert swaps it back.",
		Annotations: map[string]string{annSynopsis: "switch <pool> --to TARGET… | --confirm | --revert"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			name, err := poolArg(st, args, 0, "Switch which pool?")
			if err != nil {
				return err
			}
			p := st.Pool(name)
			if p == nil {
				return &UsageError{Msg: "no pool " + name}
			}
			switch {
			case confirm || revert:
				if len(p.Previous) == 0 {
					return errors.New("pool " + name + " has no switch waiting to be confirmed")
				}
				what := map[bool]string{true: "revert", false: "confirm"}[revert]
				_, _, err := e.poolChange(cmd, f, name, "pool switch "+name+" --"+what, func(_ *model.State, np *model.Pool) error {
					if revert {
						np.Members = np.Previous
					} else {
						keep := np.Members[:0]
						for _, m := range np.Members {
							if !isPrev(np.Previous, m) {
								keep = append(keep, m)
							}
						}
						np.Members = keep
					}
					np.Previous = nil
					return nil
				})
				return err
			}
			rep := e.freshReport(ctx, true)
			in := lookupInstance(rep, p.Instance)
			if in == nil {
				return fmt.Errorf("instance %s was not found by the scan", p.Instance)
			}
			if len(to) == 0 {
				if err := ui.Need("the new members", "--to TARGET…"); err != nil {
					return err
				}
				if to, err = pickMembers(rep, in, nil); err != nil {
					return err
				}
			}
			draft := model.Pool{Name: name, Scheme: p.Scheme}
			if err := membersFromSpecs(rep, in, to, &draft); err != nil {
				return err
			}
			if err := e.offerConnect(ctx, rep, in, &draft, false); err != nil {
				return err
			}
			canBackup := p.Method == model.RoundRobin || p.Method == model.LeastConn
			_, _, err = e.poolChange(cmd, f, name, "pool switch "+name, func(_ *model.State, np *model.Pool) error {
				prev := append([]model.Member{}, np.Members...)
				np.Previous = prev
				np.Members = append([]model.Member{}, draft.Members...)
				for _, m := range prev {
					if np.Member(m.Label()) >= 0 {
						continue
					}
					if canBackup {
						m.Backup = true
					} else {
						m.Down = true // backup is refused with this method (LB-02)
					}
					np.Members = append(np.Members, m)
				}
				return nil
			})
			if err != nil || f.dryRun {
				return err
			}
			if !ui.CanPrompt() || e.yes {
				ui.Hint("the old members are kept as backup: ngitool pool switch " + name + " --confirm (or --revert)")
				return nil
			}
			keep, err := ui.Confirm("Keep the new set?", "Yes drops the old members; No keeps them as backup for now", false)
			if err != nil || !keep {
				ui.Hint("later: ngitool pool switch " + name + " --confirm (or --revert)")
				return err
			}
			_, _, err = e.poolChange(cmd, f, name, "pool switch "+name+" --confirm", func(_ *model.State, np *model.Pool) error {
				keep := np.Members[:0]
				for _, m := range np.Members {
					if !isPrev(np.Previous, m) {
						keep = append(keep, m)
					}
				}
				np.Members, np.Previous = keep, nil
				return nil
			})
			return err
		},
	}
	c.Flags().StringArrayVar(&to, "to", nil, "a new member; repeatable")
	c.Flags().BoolVar(&confirm, "confirm", false, "drop the previous members")
	c.Flags().BoolVar(&revert, "revert", false, "make the previous members active again")
	f.add(c)
	return c
}

// isPrev: m is one of the previous set and was not picked again.
func isPrev(prev []model.Member, m model.Member) bool {
	for _, p := range prev {
		if p.Label() == m.Label() {
			return m.Backup || m.Down
		}
	}
	return false
}

// ---- check (LB-08, LB-11) -----------------------------------------------------

type memberCheck struct {
	Pool   string        `json:"pool"`
	Member string        `json:"member"`
	Kind   string        `json:"kind"`
	OK     bool          `json:"ok"`
	Status int           `json:"status,omitempty"`
	Took   time.Duration `json:"took"`
	Err    string        `json:"error,omitempty"`
	Via    string        `json:"via"`
}

func checkCmd(e *env) *cobra.Command {
	var f txFlags
	var asJSON, markDown bool
	c := &cobra.Command{
		Use:   "check [pool]",
		Short: "request every member directly from the instance's network",
		Long: "Container members are requested from inside the instance's network with a throwaway\n" +
			"curlimages/curl container (or docker exec … wget in the instance when that image is not available);\n" +
			"host members with a direct request. Any HTTP answer counts as up — active health checks are NGINX Plus\n" +
			"only (LB-08), so this is NgiTool's. --mark-down drains the failing members after a confirm.",
		Annotations: map[string]string{annSynopsis: "check [pool] [--mark-down] [--json]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			var pools []model.Pool
			if len(args) == 1 {
				p := st.Pool(args[0])
				if p == nil {
					return &UsageError{Msg: "no pool " + args[0]}
				}
				pools = []model.Pool{*p}
			} else {
				pools = st.Pools
			}
			if len(pools) == 0 {
				ui.Info("no pools yet")
				return nil
			}
			rep := e.report(ctx, false, false)
			h := apply.LoadHealth(e.paths.Cache)
			var results []memberCheck
			for _, p := range pools {
				in := lookupInstance(rep, p.Instance)
				if in == nil {
					continue
				}
				for _, m := range p.Members {
					var mc memberCheck
					run := func(*ui.TaskCtl) error {
						mc = e.checkMember(ctx, rep, in, p, m)
						if !mc.OK {
							return errors.New(mc.Err)
						}
						return nil
					}
					if asJSON {
						_ = run(nil)
					} else {
						_ = ui.Task(p.Name+" "+m.Label(), run)
					}
					results = append(results, mc)
					h.Members[p.Name+" "+m.Label()] = apply.MemberHealth{OK: mc.OK, Status: mc.Status, Took: mc.Took, Err: mc.Err, At: time.Now()}
				}
			}
			h.Save(e.paths.Cache)
			if asJSON {
				if results == nil {
					results = []memberCheck{}
				}
				return printJSON(results)
			}
			rows := [][]string{}
			var failing []memberCheck
			for _, r := range results {
				status := ui.OK(ui.SymDot + " " + strconv.Itoa(r.Status))
				if !r.OK {
					status = ui.Err(ui.SymErr + " down")
					failing = append(failing, r)
				}
				rows = append(rows, []string{r.Pool, r.Member, status, r.Took.Round(time.Millisecond).String(), dash(r.Err), ui.Muted(r.Via)})
			}
			ui.Plain("")
			ui.PrintTable(rows, ui.TableOpts{Indent: 2, Header: []string{"POOL", "MEMBER", "STATUS", "LATENCY", "LAST ERROR", "VIA"}})
			if len(failing) == 0 || !markDown {
				if len(failing) > 0 {
					ui.Hint("drain them: ngitool pool check --mark-down, or ngitool pool drain <pool> <member>")
				}
				return nil
			}
			ok, err := ui.Sure(e.yes, fmt.Sprintf("Mark %d failing %s down?", len(failing), either(len(failing), "member", "members")), "they get no traffic until: ngitool pool undrain")
			if err != nil {
				return err
			}
			if !ok {
				return errCancelled
			}
			by := map[string][]string{}
			for _, r := range failing {
				by[r.Pool] = append(by[r.Pool], r.Member)
			}
			for pool, ms := range by {
				_, _, err := e.poolChange(cmd, f, pool, "pool check --mark-down "+pool, func(_ *model.State, np *model.Pool) error {
					for _, l := range ms {
						if i := np.Member(l); i >= 0 {
							np.Members[i].Down = true
						}
					}
					if len(np.Active()) == 0 {
						return problemErr(model.Problem{Code: "LB-11", Msg: "every member of " + pool + " failed: marking them all down only turns 502 into 502", Fix: "fix the app, or drain members one by one"})
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&markDown, "mark-down", false, "drain the failing members (asks first)")
	f.add(c)
	return c
}

// curlImage is pulled once for pool check; offline, docker exec + wget in
// the instance (alpine images have it) is used instead.
const curlImage = "curlimages/curl"

func (e *env) checkMember(ctx context.Context, rep *discover.Report, in *discover.Instance, p model.Pool, m model.Member) memberCheck {
	mc := memberCheck{Pool: p.Name, Member: m.Label(), Kind: m.Kind}
	scheme := "http"
	if p.Scheme == "https" || p.Scheme == "grpcs" {
		scheme = "https"
	}
	if m.Kind == model.KindUnix {
		start := time.Now()
		conn, err := net.DialTimeout("unix", m.Ref, 3*time.Second)
		mc.Took, mc.Via = time.Since(start), "unix dial"
		if err != nil {
			mc.Err = err.Error()
			return mc
		}
		_ = conn.Close()
		mc.OK = true
		return mc
	}
	var host string
	switch m.Kind {
	case model.KindContainer, model.KindService:
		host = firstNonEmpty(m.Host, m.Ref)
	case model.KindHostPort:
		host = "127.0.0.1"
		if !model.HostNetwork(in) {
			host = model.Gateway(rep, in)
		}
	default:
		host = m.Ref
	}
	url := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(m.Port)) + "/"
	if model.HostNetwork(in) {
		mc.Via = "direct"
		client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		start := time.Now()
		res, err := client.Get(url)
		mc.Took = time.Since(start)
		if err != nil {
			mc.Err = shortErr(err.Error())
			return mc
		}
		_ = res.Body.Close()
		mc.Status, mc.OK = res.StatusCode, true
		return mc
	}
	network := ""
	if c := rep.Container(in.Container); c != nil && len(c.Networks) > 0 {
		network = c.Networks[0]
		for _, n := range c.Networks {
			if n != "bridge" {
				network = n
				break
			}
		}
	}
	run := newScanEnv(e).Run
	start := time.Now()
	res := run.Run(ctx, "docker", []string{"run", "--rm", "--network", network, curlImage, "-sk", "-o", "/dev/null", "-w", "%{http_code} %{time_total}", "--max-time", "5", url}, execx.Opts{Timeout: 60 * time.Second})
	mc.Took, mc.Via = time.Since(start), "curl on "+network
	if res.Code == 125 || res.Code == 127 {
		// The image could not be pulled (offline): ask the instance itself.
		start = time.Now()
		res = run.Run(ctx, "docker", []string{"exec", in.Container, "wget", "-q", "-S", "-O", "/dev/null", "-T", "5", url}, execx.Opts{Timeout: 15 * time.Second})
		mc.Took, mc.Via = time.Since(start), "wget in "+in.Container
		if code := wgetStatus(res.Stderr); code > 0 {
			mc.Status, mc.OK = code, true
			return mc
		}
		mc.Err = shortErr(execx.Tail(res, 1))
		return mc
	}
	// curl's own timing: the container start is not the member's latency.
	f := strings.Fields(res.Stdout)
	code := 0
	if len(f) == 2 {
		code, _ = strconv.Atoi(f[0])
		if secs, err := strconv.ParseFloat(f[1], 64); err == nil {
			mc.Took = time.Duration(secs * float64(time.Second))
		}
	}
	if code > 0 {
		mc.Status, mc.OK = code, true
		return mc
	}
	mc.Err = curlErr(res.Code)
	return mc
}

func wgetStatus(out string) int {
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.HasPrefix(f[0], "HTTP/") {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

func curlErr(code int) string {
	switch code {
	case 6:
		return "name does not resolve on this network"
	case 7:
		return "connection refused"
	case 28:
		return "timed out"
	case 35, 60:
		return "TLS handshake failed"
	case 52:
		return "empty reply"
	}
	return "curl exit " + strconv.Itoa(code)
}

func shortErr(s string) string {
	if i := strings.LastIndex(s, ": "); i >= 0 && len(s) > 60 {
		return s[i+2:]
	}
	return s
}
