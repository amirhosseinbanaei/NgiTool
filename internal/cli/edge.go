package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/apply"
	"github.com/amirhosseinbanaei/NgiTool/internal/cloudflare"
	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// edgeRunner runs docker for the edge stack's commands; tests replace it.
var edgeRunner execx.Runner = execx.System

func edgeCmd(e *env) *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "edge",
		Short: "NgiTool's own nginx + certbot stack: create it, run it, keep it current",
		Long: "init            write the stack (default " + edge.DefaultDir + ") and ask for the network, Cloudflare token, ACME email, public IP\n" +
			"up|down|restart run it with docker compose (explicit -p)\n" +
			"status          services, network, Cloudflare and ACME settings, certificates, routes\n" +
			"logs [host]     follow nginx's access log, status codes coloured, optionally one host\n" +
			"cf-sync         refresh Cloudflare's real-IP ranges and the origin-pull CA\n" +
			"upgrade-assets  show and apply what this NgiTool would change in the stack's own files",
		Annotations: map[string]string{annGroup: "edge", annSynopsis: "edge init|up|status|logs|…"},
		Args:        noArgs,
		RunE:        func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.PersistentFlags().StringVar(&dir, "dir", "", "the stack's directory (default: the one NgiTool runs)")
	c.AddCommand(edgeInitCmd(e), edgeUpCmd(e, &dir), edgeSimpleCmd(e, &dir, "down"), edgeSimpleCmd(e, &dir, "restart"),
		edgeStatusCmd(e, &dir), edgeLogsCmd(e, &dir), edgeCFSyncCmd(e, &dir), edgeUpgradeCmd(e, &dir))
	return c
}

// stackOf is the stack NgiTool runs in dir (or the only one), with its
// compose binary.
func (e *env) stackOf(ctx context.Context, dir string) (*model.Edge, edge.Stack, error) {
	st, err := model.Load(e.paths)
	if err != nil {
		return nil, edge.Stack{}, err
	}
	var ed *model.Edge
	switch {
	case dir != "":
		abs, _ := filepath.Abs(dir)
		if ed = st.EdgeAt(abs); ed == nil {
			if err := edge.Check(abs); err != nil {
				return nil, edge.Stack{}, err
			}
			// A stack NgiTool did not create (a legacy one before migration):
			// its project is the label of its containers, else the default.
			ed = &model.Edge{Dir: abs, Project: projectOfDir(ctx, abs), Instance: model.EdgeInstanceID(abs)}
		}
	case len(st.Edges) == 1:
		ed = &st.Edges[0]
	case len(st.Edges) == 0:
		return nil, edge.Stack{}, errors.New("no edge stack yet\n" + ui.SymArrow + " create one: ngitool edge init")
	default:
		if err := ui.Need("stack", "--dir DIR"); err != nil {
			return nil, edge.Stack{}, err
		}
		var opts []ui.Option
		for _, x := range st.Edges {
			opts = append(opts, ui.Option{Value: x.Dir, Label: x.Dir, Hint: "project " + x.Project})
		}
		v, err := ui.Select(ui.SelectOpts{Title: "Which edge stack?", Options: opts})
		if err != nil {
			return nil, edge.Stack{}, err
		}
		ed = st.EdgeAt(v)
	}
	b, err := e.composeBin(ctx)
	if err != nil {
		return nil, edge.Stack{}, err
	}
	return ed, edge.Stack{Dir: ed.Dir, Project: ed.Project, Run: edgeRunner, Bin: b}, nil
}

// projectOfDir is the compose project of containers running from dir.
func projectOfDir(ctx context.Context, dir string) string {
	ctrs, _ := compose.Containers(ctx, edgeRunner)
	for _, c := range ctrs {
		if c.WorkDir == dir {
			return c.Project
		}
	}
	return edge.DefaultProject
}

// ── init ────────────────────────────────────────────────────────────────────

type initOpts struct {
	network, subnet, email, ip, tokenFile, project string
	httpPort, httpsPort                            int
	noToken, start, noStart                        bool
}

func edgeInitCmd(e *env) *cobra.Command {
	o := &initOpts{}
	c := &cobra.Command{
		Use:   "init [dir]",
		Short: "create the edge stack: compose.yaml, conf/, .env, the token file",
		Long: "Writes the stack from the files inside this binary (existing files are kept; upgrade-assets shows the\n" +
			"difference), then asks what the legacy `edge init` asked: the Docker network nginx shares with your\n" +
			"apps, the Cloudflare API token (masked, verified, skippable), the ACME email and the public IP. Writes\n" +
			".env and secrets/cloudflare.ini (0600 in a 0700 dir), fetches Cloudflare's IP ranges and offers to\n" +
			"start the stack. The token is read from --token-file or $CLOUDFLARE_API_TOKEN off a terminal; it is\n" +
			"never printed or stored anywhere else.",
		Example:     "ngitool edge init\nngitool edge init /opt/ngitool/edge --network edge --email you@example.com --no-token --start --yes",
		Annotations: map[string]string{annSynopsis: "init [dir] [--network N] [--email E] [--token-file F|--no-token] [--start]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) == 1 {
				dir = args[0]
			}
			return e.edgeInit(cmd, o, dir)
		},
	}
	fl := c.Flags()
	fl.StringVar(&o.network, "network", "", "Docker network nginx shares with the apps (created when missing)")
	fl.StringVar(&o.subnet, "subnet", "", "subnet for a new network (default "+edge.DefaultSubnet+" for \"edge\")")
	fl.StringVar(&o.email, "email", "", "email Let's Encrypt sends expiry notices to")
	fl.StringVar(&o.ip, "server-ip", "", "public IPv4 DNS records point at (default: detected)")
	fl.StringVar(&o.tokenFile, "token-file", "", "file holding the Cloudflare API token ($CLOUDFLARE_API_TOKEN works too)")
	fl.BoolVar(&o.noToken, "no-token", false, "no Cloudflare token: DNS records, DNS-01 and Origin CA stay off")
	fl.StringVar(&o.project, "project-name", "", "compose project name (default $COMPOSE_PROJECT_NAME, else \"edge\")")
	fl.IntVar(&o.httpPort, "http-port", 0, "host port for HTTP (default 80)")
	fl.IntVar(&o.httpsPort, "https-port", 0, "host port for HTTPS (default 443)")
	fl.BoolVar(&o.start, "start", false, "start the stack when done")
	fl.BoolVar(&o.noStart, "no-start", false, "do not offer to start it")
	return c
}

var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func (e *env) edgeInit(cmd *cobra.Command, o *initOpts, dir string) error {
	ctx := cmd.Context()
	b, err := e.composeBin(ctx)
	if err != nil {
		return err
	}
	if dir == "" {
		dir = edge.DefaultDir
		if ui.CanPrompt() {
			if dir, err = ui.Input(ui.InputOpts{Title: "Where should the stack live?", Default: edge.DefaultDir, Placeholder: edge.DefaultDir,
				Note: "a directory of its own; /opt is the usual place for a self-contained stack",
				Validate: func(v string) error {
					if !filepath.IsAbs(v) {
						return errors.New("an absolute path")
					}
					return nil
				}}); err != nil {
				return err
			}
		}
	}
	dir, _ = filepath.Abs(dir)
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	project := firstNonEmpty(o.project, os.Getenv("COMPOSE_PROJECT_NAME"), edge.DefaultProject)
	if ed := st.EdgeAt(dir); ed != nil && o.project == "" {
		project = ed.Project
	}
	ui.Heading("Edge stack", dir)
	env := edge.ReadEnv(dir)

	// The network nginx shares with the apps.
	network := o.network
	if network == "" {
		network = env["EDGE_NETWORK"]
		if ui.CanPrompt() {
			nets := edge.Networks(ctx, edgeRunner)
			var opts []ui.Option
			hasEdge := false
			for _, n := range nets {
				hint := n.Subnet + " · " + strconv.Itoa(len(n.Containers)) + " " + either(len(n.Containers), "container", "containers")
				if n.Compose != "" {
					hint += " · created by compose project " + n.Compose
				}
				opts = append(opts, ui.Option{Value: n.Name, Label: n.Name, Hint: hint})
				hasEdge = hasEdge || n.Name == "edge"
			}
			create := ui.Option{Value: "\x00new", Label: "＋ Create \"edge\"", Hint: edge.DefaultSubnet + ", interface br-edge"}
			if hasEdge {
				create.Disabled = "already exists"
			}
			opts = append(opts, create)
			v, err := ui.Select(ui.SelectOpts{Title: "Docker network nginx shares with your apps", Options: opts, Default: network})
			if err != nil {
				return err
			}
			network = v
			if v == "\x00new" {
				network = "edge"
			}
		}
	}
	if edge.NetworkInfo(ctx, edgeRunner, network) == nil {
		subnet := o.subnet
		if subnet == "" && network == "edge" {
			subnet = edge.DefaultSubnet
		}
		if err := ui.Task("Creating Docker network "+network, func(*ui.TaskCtl) error {
			return edge.CreateNetwork(ctx, edgeRunner, network, subnet)
		}); err != nil {
			return err
		}
	}
	net := edge.NetworkInfo(ctx, edgeRunner, network)

	// Cloudflare token: masked, verified, skippable. Never printed.
	token, err := e.askToken(ctx, dir, o)
	if err != nil {
		return err
	}

	// ACME email.
	email := o.email
	if email == "" {
		email = env["ACME_EMAIL"]
		if ui.CanPrompt() {
			if email, err = ui.Input(ui.InputOpts{Title: "Email for Let's Encrypt", Default: env["ACME_EMAIL"], Placeholder: firstNonEmpty(env["ACME_EMAIL"], "you@example.com"),
				Note: "expiry warnings go here", Validate: func(v string) error {
					if !emailRE.MatchString(v) {
						return errors.New("not an email address")
					}
					return nil
				}}); err != nil {
				return err
			}
		}
	}
	if email != "" && !emailRE.MatchString(email) {
		return &UsageError{Msg: "--email: not an email address", Cmd: "ngitool edge init"}
	}

	// Public IP for DNS records.
	ip := o.ip
	if ip == "" {
		ip = env["SERVER_IP"]
		if ip == "" {
			_ = ui.Task("Detecting this server's public IP", func(t *ui.TaskCtl) error {
				ip = cloudflare.PublicIP(ctx, nil)
				if ip == "" {
					t.Update("Public IP not detected")
				}
				return nil
			})
		}
		if ui.CanPrompt() {
			if ip, err = ui.Input(ui.InputOpts{Title: "Public IP for DNS records", Default: ip, Placeholder: firstNonEmpty(ip, "203.0.113.10"),
				Note: "Cloudflare A records point here; leave empty to keep DNS records off", Validate: func(v string) error {
					if v != "" && !isIPv4(v) {
						return errors.New("an IPv4 address")
					}
					return nil
				}}); err != nil {
				return err
			}
		}
	}
	if ip != "" && !isIPv4(ip) {
		return &UsageError{Msg: "--server-ip: an IPv4 address", Cmd: "ngitool edge init"}
	}

	// Write the stack.
	var wr edge.WriteResult
	if err := ui.Task("Writing the stack", func(t *ui.TaskCtl) error {
		var werr error
		wr, werr = edge.Write(dir)
		t.Update(fmt.Sprintf("Wrote the stack %s", ui.Muted(fmt.Sprintf("(%d written, %d already there, %d kept as they are)", len(wr.Written), len(wr.Same), len(wr.Kept)))))
		return werr
	}); err != nil {
		return err
	}
	if len(wr.Kept) > 0 {
		ui.Hint(strconv.Itoa(len(wr.Kept)) + " files differ from this NgiTool's: see them with ngitool edge upgrade-assets --dir " + dir)
	}
	values := [][2]string{{"ACME_EMAIL", email}, {"EDGE_NETWORK", network}, {"SERVER_IP", ip}}
	if o.httpPort > 0 {
		values = append(values, [2]string{"HTTP_PORT", strconv.Itoa(o.httpPort)})
	}
	if o.httpsPort > 0 {
		values = append(values, [2]string{"HTTPS_PORT", strconv.Itoa(o.httpsPort)})
	}
	if project != edge.DefaultProject {
		// Plain `docker compose` in the directory then finds the same project.
		values = append(values, [2]string{"COMPOSE_PROJECT_NAME", project})
	}
	if err := edge.WriteEnv(dir, values); err != nil {
		return err
	}
	if token != "\x00keep" {
		if err := edge.WriteToken(dir, token); err != nil {
			return err
		}
	}
	if !e.writeCFSyncDirect(ctx, dir) {
		ui.Hint("kept the Cloudflare IP list shipped with NgiTool — refresh it later: ngitool edge cf-sync")
	}
	err = e.withLock(ctx, true, func() error {
		st, err := model.Load(e.paths)
		if err != nil {
			return err
		}
		if ed := st.EdgeAt(dir); ed != nil {
			ed.Project = project
		} else {
			st.Edges = append(st.Edges, model.Edge{Dir: dir, Project: project, Instance: model.EdgeInstanceID(dir), Added: model.Now(time.Now())})
		}
		return model.Save(e.paths, st)
	})
	if err != nil {
		return err
	}
	env = edge.ReadEnv(dir)
	pairs := [][2]string{{"Directory", dir}, {"Project", project}}
	if net != nil {
		pairs = append(pairs, [2]string{"Network", net.Name + ui.Muted("  "+net.Subnet+" · host apps listen on "+net.Gateway+" · interface "+net.Bridge)})
	} else {
		pairs = append(pairs, [2]string{"Network", network})
	}
	switch {
	case edge.ReadToken(dir) != "":
		pairs = append(pairs, [2]string{"Cloudflare", ui.OK("token verified")})
	default:
		pairs = append(pairs, [2]string{"Cloudflare", ui.Warn("no token — DNS records, Let's Encrypt DNS-01 and Origin CA are off (HTTP-01 still works)")})
	}
	pairs = append(pairs, [2]string{"Public IP", firstNonEmpty(env["SERVER_IP"], ui.Warn("unknown — DNS records are off"))},
		[2]string{"Ports", env["HTTP_PORT"] + ", " + env["HTTPS_PORT"]})
	ui.Plan("Ready", pairs)
	start := o.start
	if !start && !o.noStart && ui.CanPrompt() {
		if start, err = ui.Confirm("Start the edge stack now?", "docker compose up -d, then NgiTool adopts its nginx", true); err != nil {
			return err
		}
	}
	if start {
		stack := edge.Stack{Dir: dir, Project: project, Run: edgeRunner, Bin: b}
		ed := model.Edge{Dir: dir, Project: project, Instance: model.EdgeInstanceID(dir)}
		return e.edgeUp(cmd, &ed, stack, false)
	}
	ui.Hint("start it: ngitool edge up --dir " + dir)
	ui.Hint("then: ngitool route add app.example.com --instance " + model.EdgeInstanceID(dir))
	return nil
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && strings.Count(s, ".") == 3
}

// askToken returns the token to write, "\x00keep" to keep the file as it
// is, or "" for none. It is verified with Cloudflare before it is kept.
func (e *env) askToken(ctx context.Context, dir string, o *initOpts) (string, error) {
	if o.noToken {
		return "", nil
	}
	given := os.Getenv("CLOUDFLARE_API_TOKEN")
	if o.tokenFile != "" {
		b, err := os.ReadFile(o.tokenFile)
		if err != nil {
			return "", err
		}
		given = strings.TrimSpace(string(b))
		if m := regexp.MustCompile(`dns_cloudflare_api_token\s*=\s*(\S+)`).FindStringSubmatch(given); m != nil {
			given = m[1]
		}
	}
	if given != "" {
		if err := ui.Task("Verifying the Cloudflare token", func(*ui.TaskCtl) error { return cloudflare.New(given).Verify(ctx) }); err != nil {
			return "", err
		}
		return given, nil
	}
	if edge.ReadToken(dir) != "" {
		if !ui.CanPrompt() {
			return "\x00keep", nil
		}
		keep, err := ui.Confirm("Keep the Cloudflare token already in secrets/cloudflare.ini?", "", true)
		if err != nil || keep {
			return "\x00keep", err
		}
	}
	for ui.CanPrompt() {
		t, err := ui.Input(ui.InputOpts{Title: "Cloudflare API token", Secret: true,
			Note: "Zone → DNS → Edit (+ SSL and Certificates → Edit for Origin CA) · Enter to skip"})
		if err != nil {
			return "", err
		}
		if t == "" {
			return "", nil
		}
		if err := ui.Task("Verifying the token", func(*ui.TaskCtl) error { return cloudflare.New(t).Verify(ctx) }); err != nil {
			ui.Fail(err.Error())
			continue
		}
		return t, nil
	}
	return "", nil
}

// writeCFSyncDirect fetches Cloudflare's lists into a stack that is not
// running yet (nothing to test or reload). Later refreshes go through the
// transaction (edge cf-sync).
func (e *env) writeCFSyncDirect(ctx context.Context, dir string) bool {
	var s cloudflare.Sync
	err := ui.Task("Fetching Cloudflare IP ranges", func(t *ui.TaskCtl) error {
		var ferr error
		s, ferr = cloudflare.FetchSync(ctx, nil)
		if ferr == nil {
			t.Update("Fetched Cloudflare IP ranges " + ui.Muted("("+strconv.Itoa(len(s.Ranges))+" ranges)"))
		}
		return ferr
	})
	if err != nil {
		ui.Warning("could not fetch Cloudflare's IP ranges: " + err.Error())
		return false
	}
	l := edge.Layout{Dir: dir}
	_ = os.WriteFile(l.RealIP(), []byte(cloudflare.RealIPConf(s.Ranges, time.Now())), 0o644)
	if s.AOPCA != "" {
		_ = os.MkdirAll(filepath.Dir(l.AOPCA()), 0o755)
		_ = os.WriteFile(l.AOPCA(), []byte(s.AOPCA), 0o644)
	}
	return true
}

// ── up / down / restart ─────────────────────────────────────────────────────

func edgeUpCmd(e *env, dir *string) *cobra.Command {
	var create bool
	c := &cobra.Command{
		Use:   "up",
		Short: "start (or update) the edge stack",
		Long: "Checks what the legacy `edge up` checked — the network exists, secrets/cloudflare.ini is a file, data/acme\n" +
			"exists — and that :80/:443 are free (EDGE-01) and no copy of the stack runs under another project name\n" +
			"(EDGE-03). Then docker compose up -d with an explicit -p, and NgiTool adopts the stack's nginx.",
		Annotations: map[string]string{annSynopsis: "up [--create-network]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ed, stack, err := e.stackOf(cmd.Context(), *dir)
			if err != nil {
				return err
			}
			return e.edgeUp(cmd, ed, stack, create)
		},
	}
	c.Flags().BoolVar(&create, "create-network", false, "create the network when it is missing (EDGE-02)")
	return c
}

func (e *env) edgeUp(cmd *cobra.Command, ed *model.Edge, stack edge.Stack, create bool) error {
	ctx := cmd.Context()
	env := edge.ReadEnv(stack.Dir)
	ctrs, _ := compose.Containers(ctx, edgeRunner)
	if dups := stack.Duplicates(ctrs); len(dups) > 0 {
		return problemErr(model.Problem{Code: "EDGE-03", Msg: "this stack already runs as compose project " + strings.Join(dups, ", ") + " — two copies would fight over the ports and certificates",
			Fix: "stop it: docker compose -p " + dups[0] + " --project-directory " + stack.Dir + " down — then ngitool edge up"})
	}
	running := stack.Running(ctx)
	if !running {
		rep := e.freshReport(ctx, true)
		held := map[int][2]string{}
		for _, p := range rep.Listeners {
			who := firstNonEmpty(p.Container, p.Process, "pid "+strconv.Itoa(p.PID))
			if p.Container != "" {
				who = "container " + p.Container
			}
			held[p.Port] = [2]string{who, p.Container}
		}
		own := ""
		if st, err := stack.Status(ctx); err == nil {
			own = st["nginx"].Container
		}
		if cs := edge.Conflicts(env, own, held); len(cs) > 0 {
			var ws []string
			for _, c := range cs {
				ws = append(ws, ":"+strconv.Itoa(c.Port)+" is held by "+c.Owner)
			}
			return problemErr(model.Problem{Code: "EDGE-01", Msg: strings.Join(ws, "; ") + " — the edge stack cannot bind it",
				Fix: "put the stack behind it: set HTTP_PORT/HTTPS_PORT in " + stack.Layout().Env() + " (e.g. 8080/8443) and route the front door to it;\n" +
					"  or in front of it: stop that server first, then ngitool edge up"})
		}
	}
	for _, p := range stack.Preconditions(ctx, env["EDGE_NETWORK"]) {
		switch {
		case p.OK && p.Fixed:
			ui.Done(p.Name + ui.Muted(" created"))
		case p.OK:
		case strings.HasPrefix(p.Name, "network "):
			ok := create
			if !ok && ui.CanPrompt() {
				var err error
				if ok, err = ui.Confirm("The network "+env["EDGE_NETWORK"]+" is missing. Create it?", "EDGE-02 · apps attached to it need `ngitool app fix` afterwards", true); err != nil {
					return err
				}
			}
			if !ok {
				return problemErr(model.Problem{Code: "EDGE-02", Msg: p.Name + " " + p.Why, Fix: p.Fix})
			}
			subnet := ""
			if env["EDGE_NETWORK"] == "edge" {
				subnet = edge.DefaultSubnet
			}
			if err := ui.Task("Creating Docker network "+env["EDGE_NETWORK"], func(*ui.TaskCtl) error {
				return edge.CreateNetwork(ctx, edgeRunner, env["EDGE_NETWORK"], subnet)
			}); err != nil {
				return err
			}
			e.reconnectHint(env["EDGE_NETWORK"])
		default:
			return fmt.Errorf("%s %s\n%s %s", p.Name, p.Why, ui.SymArrow, p.Fix)
		}
	}
	res := execx.Result{}
	err := ui.Task("Starting nginx and certbot", func(*ui.TaskCtl) error {
		res = stack.Exec(ctx, "up", "-d", "--quiet-pull", "--remove-orphans")
		if res.Code != 0 {
			return errors.New("docker compose up failed")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("docker compose up failed\n%s", execx.Tail(res, 8))
	}
	return e.adoptEdge(cmd, ed)
}

// reconnectHint: a recreated network is empty; apps attached through the
// override come back with `app fix`, plain containers need a connect.
func (e *env) reconnectHint(network string) {
	st, err := model.Load(e.paths)
	if err != nil {
		return
	}
	var apps []string
	for _, a := range st.Apps {
		for _, at := range a.Attached {
			if at.Network == network {
				apps = append(apps, a.Name)
				break
			}
		}
	}
	if len(apps) > 0 {
		ui.Hint(ui.SymArrow + " apps on it: ngitool app fix " + strings.Join(apps, " ") + " (EDGE-02)")
	}
	for _, p := range st.Pools {
		for _, m := range p.Members {
			if m.Kind == model.KindContainer {
				ui.Hint(ui.SymArrow + " docker network connect " + network + " " + m.Ref)
			}
		}
	}
}

// adoptEdge registers the stack's nginx as an adopted instance (no
// questions: NgiTool runs this stack).
func (e *env) adoptEdge(cmd *cobra.Command, ed *model.Edge) error {
	ctx := cmd.Context()
	rep := e.freshReport(ctx, true)
	in := rep.Find(ed.Instance)
	if in == nil {
		ui.Warning("the stack started but the scan did not find its nginx (" + ed.Instance + ") — run: ngitool scan")
		return nil
	}
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	if st.EdgeAt(ed.Dir) == nil {
		if err := e.withLock(ctx, true, func() error {
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			st.Edges = append(st.Edges, model.Edge{Dir: ed.Dir, Project: ed.Project, Instance: ed.Instance, Added: model.Now(time.Now())})
			return model.Save(e.paths, st)
		}); err != nil {
			return err
		}
	}
	if st.Adopted(in.ID) != nil {
		ui.Done("Edge stack running " + ui.Muted("("+in.Name+", "+firstNonEmpty(in.Version, "nginx")+")"))
		return nil
	}
	yes := e.yes
	e.yes = true
	defer func() { e.yes = yes }()
	st, _ = model.Load(e.paths)
	return e.adopt(cmd, rep, st, in, txFlags{})
}

func edgeSimpleCmd(e *env, dir *string, action string) *cobra.Command {
	short := map[string]string{"down": "stop the edge stack (every site goes offline)", "restart": "restart nginx and certbot"}[action]
	return &cobra.Command{
		Use:         action,
		Short:       short,
		Annotations: map[string]string{annSynopsis: action},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			_, stack, err := e.stackOf(ctx, *dir)
			if err != nil {
				return err
			}
			step := []string{action}
			label := "Restarting the edge stack"
			if action == "down" {
				label = "Stopping the edge stack"
				ui.Explain("docker compose down: removes the nginx and certbot containers",
					"every site this stack serves goes offline; files, certificates and routes stay",
					"ngitool edge up")
				ui.Plain("  " + ui.Muted("Will run: ") + ui.Key(stack.CommandLine(step...)))
				ok, err := ui.Sure(e.yes, "Stop the edge stack?", "")
				if err != nil {
					return err
				}
				if !ok {
					return errCancelled
				}
			}
			var res execx.Result
			if err := ui.Task(label, func(*ui.TaskCtl) error {
				if res = stack.Exec(ctx, step...); res.Code != 0 {
					return errors.New("failed")
				}
				return nil
			}); err != nil {
				return fmt.Errorf("docker compose %s failed\n%s", action, execx.Tail(res, 8))
			}
			return nil
		},
	}
}

// ── status ──────────────────────────────────────────────────────────────────

type edgeStatusJSON struct {
	Dir       string                  `json:"dir"`
	Project   string                  `json:"project"`
	Instance  string                  `json:"instance"`
	Adopted   bool                    `json:"adopted"`
	Services  map[string]edge.Service `json:"services"`
	Network   *edge.Network           `json:"network"`
	NetworkOK bool                    `json:"networkOk"`
	Token     bool                    `json:"cloudflareToken"`
	ServerIP  string                  `json:"serverIp,omitempty"`
	Email     bool                    `json:"acmeEmail"`
	Ports     [2]string               `json:"ports"`
	Certs     int                     `json:"certificates"`
	Routes    int                     `json:"routes"`
	Outdated  []string                `json:"outdatedAssets,omitempty"`
}

func edgeStatusCmd(e *env, dir *string) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "status",
		Short:       "services, network, Cloudflare and ACME settings, certificates, routes",
		Annotations: map[string]string{annSynopsis: "status [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			ed, stack, err := e.stackOf(ctx, *dir)
			if err != nil {
				return err
			}
			st, err := model.Load(e.paths)
			if err != nil {
				return err
			}
			env := edge.ReadEnv(stack.Dir)
			svcs, _ := stack.Status(ctx)
			nw := edge.NetworkInfo(ctx, edgeRunner, env["EDGE_NETWORK"])
			s := edgeStatusJSON{Dir: stack.Dir, Project: stack.Project, Instance: ed.Instance, Adopted: st.Adopted(ed.Instance) != nil,
				Services: svcs, Network: nw, NetworkOK: nw != nil, Token: edge.ReadToken(stack.Dir) != "", ServerIP: env["SERVER_IP"],
				Email: env["ACME_EMAIL"] != "", Ports: [2]string{env["HTTP_PORT"], env["HTTPS_PORT"]}, Routes: len(st.RoutesOn(ed.Instance))}
			for _, c := range st.Certs {
				if c.Instance == ed.Instance {
					s.Certs++
				}
			}
			for _, d := range edge.Outdated(stack.Dir) {
				s.Outdated = append(s.Outdated, d.Path)
			}
			if asJSON {
				return printJSON(s)
			}
			ui.Heading("Edge stack", stack.Dir+ui.Muted("  project "+stack.Project))
			svc := func(name string) string {
				x, ok := svcs[name]
				switch {
				case !ok:
					return ui.Muted(ui.SymRing+" "+name) + " " + ui.Muted("not created")
				case x.State == "running" && (x.Health == "" || x.Health == "healthy"):
					return ui.OK(ui.SymDot) + " " + name + " " + ui.Muted(firstNonEmpty(x.Health, x.State))
				case x.State == "running":
					return ui.Warn(ui.SymDot) + " " + name + " " + ui.Muted(x.Health)
				}
				return ui.Err(ui.SymDot) + " " + name + " " + ui.Muted(x.State)
			}
			ui.Plain("    " + svc("nginx") + "   " + svc("certbot"))
			if nw != nil {
				ui.Plain("    " + ui.OK(ui.SymDot) + " network " + ui.Bold(nw.Name) + " " + ui.Muted(nw.Subnet+" · gateway "+nw.Gateway))
			} else {
				ui.Plain("    " + ui.Err(ui.SymDot) + " network " + env["EDGE_NETWORK"] + " " + ui.Err("missing — ngitool edge up --create-network (EDGE-02)"))
			}
			var notes []string
			if !s.Token {
				notes = append(notes, "no Cloudflare token (DNS records, Let's Encrypt DNS-01, Origin CA off)")
			}
			if s.ServerIP == "" {
				notes = append(notes, "SERVER_IP not set (DNS records off)")
			}
			if !s.Email {
				notes = append(notes, "ACME_EMAIL not set")
			}
			if len(notes) > 0 {
				ui.Plain("    " + ui.Warn(ui.SymWarn+" "+strings.Join(notes, " · ")))
			}
			if !s.Adopted {
				ui.Plain("    " + ui.Warn(ui.SymWarn+" its nginx is not adopted yet — ngitool edge up adopts it"))
			}
			if len(s.Outdated) > 0 {
				ui.Plain("    " + ui.Muted(ui.SymInfo+" "+strconv.Itoa(len(s.Outdated))+" stack files differ from this NgiTool's — ngitool edge upgrade-assets"))
			}
			ui.Plain("    " + ui.Muted(fmt.Sprintf("%d certificates · %d routes · ports %s/%s", s.Certs, s.Routes, s.Ports[0], s.Ports[1])))
			if s.Certs > 0 {
				printCertTable(st, ed.Instance)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

// ── logs ────────────────────────────────────────────────────────────────────

func edgeLogsCmd(e *env, dir *string) *cobra.Command {
	var noFollow bool
	var tail int
	c := &cobra.Command{
		Use:         "logs [host]",
		Short:       "follow nginx's access log, status codes coloured",
		Long:        "One stream for every site ($host tells them apart); with a host only its lines. Ctrl-C stops following.",
		Annotations: map[string]string{annSynopsis: "logs [host] [--no-follow] [--tail N]"},
		Args:        maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			_, stack, err := e.stackOf(ctx, *dir)
			if err != nil {
				return err
			}
			host := ""
			if len(args) == 1 {
				host = strings.ToLower(args[0])
			}
			step := []string{"logs", "--tail", strconv.Itoa(tail), "--no-log-prefix"}
			if !noFollow {
				step = append(step, "-f")
			}
			step = append(step, "nginx")
			holdInterrupt.Store(true)
			defer holdInterrupt.Store(false)
			o := stack.Opts()
			o.Timeout = 0
			o.Out = &logFilter{host: host}
			res := stack.Run.Run(ctx, stack.Bin.Name, stack.Argv(step...), o)
			o.Out.(*logFilter).flush()
			if res.Code != 0 && res.Code != 130 && res.Code != 2 && ctx.Err() == nil {
				return errors.New("docker compose logs failed\n" + execx.Tail(res, 3))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&noFollow, "no-follow", false, "print and stop")
	c.Flags().IntVar(&tail, "tail", 200, "lines of history")
	return c
}

// logFilter keeps the lines of one host and colours their status code.
type logFilter struct {
	host string
	buf  []byte
}

func (l *logFilter) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := strings.IndexByte(string(l.buf), '\n')
		if i < 0 {
			break
		}
		l.line(string(l.buf[:i]))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *logFilter) flush() {
	if len(l.buf) > 0 {
		l.line(string(l.buf))
		l.buf = nil
	}
}

func (l *logFilter) line(s string) {
	if l.host != "" && !strings.Contains(s, " "+l.host+" ") {
		return
	}
	fmt.Fprintln(ui.Out, ColorLogLine(s))
}

var statusRE = regexp.MustCompile(`" (\d{3}) `)

// ColorLogLine colours the status code of an access-log line (the edge
// stack's log_format) as cli/src/main.mjs did: 5xx Err, 4xx Warn, 3xx
// Accent, 2xx OK.
func ColorLogLine(line string) string {
	loc := statusRE.FindStringSubmatchIndex(line)
	if loc == nil {
		return line
	}
	code := line[loc[2]:loc[3]]
	var col func(string) string
	switch code[0] {
	case '5':
		col = ui.Err
	case '4':
		col = ui.Warn
	case '3':
		col = ui.Accent
	default:
		col = ui.OK
	}
	return line[:loc[2]] + col(code) + line[loc[3]:]
}

// ── cf-sync ─────────────────────────────────────────────────────────────────

func edgeCFSyncCmd(e *env, dir *string) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:         "cf-sync",
		Short:       "refresh Cloudflare's real-IP ranges and the origin-pull CA",
		Long:        "Writes conf/conf.d/cloudflare-realip.conf and conf/certs/cloudflare-origin-pull-ca.pem through the\nusual transaction (snapshot, nginx -t, reload) when the stack runs and is adopted.",
		Annotations: map[string]string{annSynopsis: "cf-sync [--dry-run]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			ed, stack, err := e.stackOf(ctx, *dir)
			if err != nil {
				return err
			}
			var s cloudflare.Sync
			if err := ui.Task("Fetching Cloudflare IP ranges and the origin-pull CA", func(t *ui.TaskCtl) error {
				var ferr error
				s, ferr = cloudflare.FetchSync(ctx, nil)
				if ferr == nil {
					t.Update(fmt.Sprintf("Fetched %d Cloudflare IP ranges%s", len(s.Ranges), map[bool]string{true: " and the origin-pull CA", false: ""}[s.AOPCA != ""]))
				}
				return ferr
			}); err != nil {
				return err
			}
			l := stack.Layout()
			raw := []apply.IncludeEdit{{File: l.RealIP(), Content: cloudflare.RealIPConf(s.Ranges, time.Now())}}
			if s.AOPCA != "" {
				raw = append(raw, apply.IncludeEdit{File: l.AOPCA(), Content: s.AOPCA})
			}
			return e.stackFiles(cmd, ed, raw, f, "edge cf-sync")
		},
	}
	f.add(c)
	return c
}

// stackFiles writes the stack's own files: through the transaction when
// its nginx is adopted, directly otherwise (nothing runs that could
// reject them yet).
func (e *env) stackFiles(cmd *cobra.Command, ed *model.Edge, raw []apply.IncludeEdit, f txFlags, summary string) error {
	ctx := cmd.Context()
	st, err := model.Load(e.paths)
	if err != nil {
		return err
	}
	rep := e.freshReport(ctx, true)
	in := rep.Find(ed.Instance)
	if in != nil && st.Adopted(in.ID) != nil {
		_, err := e.change(ctx, rep, in, f, summary, nil, func(*model.State) error { return nil }, func(c *apply.Change) { c.Raw = raw })
		return err
	}
	for _, r := range raw {
		old, _ := os.ReadFile(r.File)
		if string(old) == r.Content {
			continue
		}
		ui.Plain(indentBlock(ui.Diff(ui.UnifiedDiff(r.File, r.File, string(old), r.Content))))
	}
	if f.dryRun {
		ui.Hint("--dry-run: nothing was written")
		return nil
	}
	for _, r := range raw {
		if err := os.MkdirAll(filepath.Dir(r.File), 0o755); err != nil {
			return err
		}
		if err := state.WriteFile(r.File, []byte(r.Content), 0o644); err != nil {
			return err
		}
	}
	ui.Done("Written " + ui.Muted("(the stack's nginx is not adopted or not running: nothing to test or reload)"))
	return nil
}

func indentBlock(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}

// ── upgrade-assets ──────────────────────────────────────────────────────────

func edgeUpgradeCmd(e *env, dir *string) *cobra.Command {
	var f txFlags
	c := &cobra.Command{
		Use:   "upgrade-assets",
		Short: "show and apply what this NgiTool would change in the stack's own files",
		Long: "Compares compose.yaml, conf/nginx.conf, start.sh, conf.d/ and snippets/ with the files inside this binary.\n" +
			"Never touches conf/sites, conf/locations, snippets/ssl, data/, secrets/ or www/. nginx files go through the\n" +
			"transaction; a changed compose.yaml takes effect with ngitool edge up.",
		Annotations: map[string]string{annSynopsis: "upgrade-assets [--dry-run]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			ed, stack, err := e.stackOf(ctx, *dir)
			if err != nil {
				return err
			}
			diffs := edge.Outdated(stack.Dir)
			if len(diffs) == 0 {
				ui.Info("the stack's files match this NgiTool " + ui.Muted("("+stack.Dir+")"))
				return nil
			}
			var conf []apply.IncludeEdit
			var other []edge.AssetDiff
			for _, d := range diffs {
				p := filepath.Join(stack.Dir, filepath.FromSlash(d.Path))
				if strings.HasPrefix(d.Path, "conf/") {
					conf = append(conf, apply.IncludeEdit{File: p, Content: d.New})
				} else {
					other = append(other, d)
				}
			}
			for _, d := range other {
				p := filepath.Join(stack.Dir, d.Path)
				ui.Plain(indentBlock(ui.Diff(ui.UnifiedDiff(p, p, d.Old, d.New))))
			}
			if len(other) > 0 && !f.dryRun {
				ok, err := ui.Sure(e.yes, "Replace "+either(len(other), "this file", "these files")+"?", "the stack itself is not restarted: ngitool edge up applies compose.yaml")
				if err != nil {
					return err
				}
				if !ok {
					return errCancelled
				}
				for _, d := range other {
					if err := state.WriteFile(filepath.Join(stack.Dir, d.Path), []byte(d.New), 0o644); err != nil {
						return err
					}
				}
				ui.Done("Updated " + strconv.Itoa(len(other)) + " " + either(len(other), "file", "files") + ui.Muted(" — apply with: ngitool edge up"))
			}
			if len(conf) == 0 {
				return nil
			}
			sort.Slice(conf, func(i, j int) bool { return conf[i].File < conf[j].File })
			return e.stackFiles(cmd, ed, conf, f, "edge upgrade-assets")
		},
	}
	f.add(c)
	return c
}

// edgeStackFor is the edge stack an instance belongs to (an edge-kind
// instance whose working dir is the stack), or nil.
func edgeStackFor(st *model.State, in *discover.Instance) *model.Edge {
	if in == nil || in.Kind != discover.KindEdge {
		return nil
	}
	if ed := st.EdgeOf(in.ID); ed != nil {
		return ed
	}
	if in.WorkingDir == "" {
		return nil
	}
	return &model.Edge{Dir: in.WorkingDir, Project: firstNonEmpty(in.Project, edge.DefaultProject), Instance: in.ID}
}
