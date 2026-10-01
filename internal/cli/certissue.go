package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/certs"
	"github.com/amirhosseinbanaei/NgiTool/internal/cloudflare"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/edge"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
)

// certRunner runs certbot on the host (CERT-05); tests replace it.
var certRunner execx.Runner = execx.System

// preflightClient is the HTTP client of the HTTP-01 preflight; tests
// point it at an httptest server.
var preflightClient = func() *http.Client { return nil }

// issueReq is one certificate to issue or import.
type issueReq struct {
	Kind, Challenge string
	Name            string
	Names           []string
	CertFile        string
	KeyFile         string
	Force           bool // ask Let's Encrypt although the preflight failed
	Staging         bool
	Authenticator   string // host certbot: webroot or nginx
	Webroot         string
	Email           string
}

// certHome is where an instance's certificates live.
type certHome struct {
	ed              *model.Edge // the edge stack, or nil
	host, nginx     string      // custom, self-signed, origin: <dir>/<name>/
	leHost, leNginx string      // Let's Encrypt live/ dir
	hostCertbot     bool        // Let's Encrypt through certbot on this machine
	noLetsEncrypt   string      // why Let's Encrypt is not offered here
	stack           edge.Stack
	stackOK         bool
	adopted         *model.Adopted
	instance        *discover.Instance
	tokenDir        string // the stack whose secrets/cloudflare.ini is used
}

// homeOf works out where certificates of in go.
func (e *env) homeOf(ctx context.Context, st *model.State, in *discover.Instance) (*certHome, error) {
	a := st.Adopted(in.ID)
	if a == nil {
		return nil, fmt.Errorf("%s is not adopted yet\n%s run: ngitool instance adopt %s", in.Name, ui.SymArrow, in.ID)
	}
	h := &certHome{adopted: a, instance: in}
	if ed := edgeStackFor(st, in); ed != nil {
		l := edge.Layout{Dir: ed.Dir}
		h.ed = ed
		h.host, h.nginx = l.Certs(), edge.CertsIn
		h.leHost, h.leNginx = filepath.Join(l.LE(), "live"), edge.LEIn+"/live"
		h.tokenDir = ed.Dir
		if b, err := e.composeBin(ctx); err == nil {
			h.stack, h.stackOK = edge.Stack{Dir: ed.Dir, Project: ed.Project, Run: edgeRunner, Bin: b}, true
		}
		return h, nil
	}
	h.host, h.nginx = filepath.Join(a.HostRoot, "certs"), path.Join(a.Root, "certs")
	for _, x := range st.Edges {
		if edge.ReadToken(x.Dir) != "" {
			h.tokenDir = x.Dir
			break
		}
	}
	switch in.Kind {
	case discover.KindHost:
		h.leHost, h.leNginx = "/etc/letsencrypt/live", "/etc/letsencrypt/live"
		if execx.Has("certbot") {
			h.hostCertbot = true
		} else {
			h.noLetsEncrypt = "certbot is not installed on this server (CERT-05): apt install certbot python3-certbot-nginx — or use HTTP only for now"
		}
	default:
		h.noLetsEncrypt = "Let's Encrypt runs certbot from the edge stack or on the host; " + in.Name + " is a container (CERT-05) — use a custom or Origin CA certificate, or route the host on the edge stack"
	}
	return h, nil
}

// token is the Cloudflare token for this instance's DNS and Origin CA:
// its stack's secrets/cloudflare.ini, another stack's, or
// $CLOUDFLARE_API_TOKEN. Never printed.
func (h *certHome) token() string {
	if h.tokenDir != "" {
		if t := edge.ReadToken(h.tokenDir); t != "" {
			return t
		}
	}
	return os.Getenv("CLOUDFLARE_API_TOKEN")
}

// zoneFor is the Cloudflare zone of host: a domain's recorded zone, else
// a lookup by longest suffix.
func zoneFor(ctx context.Context, st *model.State, host, token string) *model.Zone {
	if d := st.Domain(certs.DomainOf(host, st.DomainNames())); d != nil && d.Zone != nil {
		return d.Zone
	}
	if token == "" {
		return nil
	}
	z, err := cloudflare.New(token).FindZone(ctx, host)
	if err != nil || z == nil {
		return nil
	}
	return &model.Zone{ID: z.ID, Name: z.Name}
}

// freeCertName is a name free in state and on disk.
func (h *certHome) freeCertName(st *model.State, base string) string {
	return certs.FreeName(base, func(n string) bool {
		if st.Cert(h.instance.ID, n) != nil {
			return true
		}
		for _, d := range []string{h.host, h.leHost} {
			if d == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return true
			}
		}
		return false
	})
}

// files is where a kind's files go.
func (h *certHome) files(kind, name string) certs.Files {
	if kind == certs.LetsEncrypt {
		return certs.FilesIn(h.leHost, h.leNginx, name)
	}
	return certs.FilesIn(h.host, h.nginx, name)
}

// issue produces the files of a certificate and returns its state entry
// (not saved: the caller saves it with the change that uses it).
func (e *env) issue(ctx context.Context, st *model.State, h *certHome, rq issueReq) (*model.Cert, error) {
	label := strings.Join(rq.Names, ", ")
	now := time.Now()
	f := h.files(rq.Kind, rq.Name)
	c := &model.Cert{Name: rq.Name, Instance: h.instance.ID, Kind: rq.Kind, Names: rq.Names,
		Cert: f.CertIn, Key: f.KeyIn, HostCert: f.Cert, HostKey: f.Key, Added: model.Now(now)}
	store := func(chain, key []byte) error {
		info, err := certs.ValidatePair(chain, key, now)
		if err != nil {
			return err
		}
		if miss := certs.Missing(info.Names, rq.Names); len(miss) > 0 && rq.Kind == certs.Custom {
			ui.Warning("the certificate is for " + strings.Join(info.Names, ", ") + " — it does not cover " + strings.Join(miss, ", "))
		}
		c.Names = info.Names
		return certs.Store(filepath.Dir(f.Cert), chain, key)
	}
	switch rq.Kind {
	case certs.SelfSigned:
		return c, ui.Task("Creating a self-signed certificate for "+label, func(*ui.TaskCtl) error {
			chain, key, err := certs.SelfSignedPair(rq.Names, 90, now)
			if err != nil {
				return err
			}
			return store(chain, key)
		})
	case certs.Custom:
		if rq.CertFile == "" || rq.KeyFile == "" {
			return nil, &UsageError{Msg: "a custom certificate needs --cert-file and --key-file (PEM: the full chain and the unencrypted key)"}
		}
		chain, err := os.ReadFile(rq.CertFile)
		if err != nil {
			return nil, err
		}
		key, err := os.ReadFile(rq.KeyFile)
		if err != nil {
			return nil, err
		}
		return c, ui.Task("Checking and storing the certificate", func(*ui.TaskCtl) error { return store(chain, key) })
	case certs.Origin:
		tok := h.token()
		if tok == "" {
			return nil, errors.New("a Cloudflare Origin CA certificate needs a Cloudflare token\n" + ui.SymArrow + " ngitool edge init (or $CLOUDFLARE_API_TOKEN), with Zone → SSL and Certificates → Edit")
		}
		return c, ui.Task("Creating a Cloudflare Origin CA certificate for "+label, func(*ui.TaskCtl) error {
			key, csr, err := certs.KeyAndCSR(rq.Names)
			if err != nil {
				return err
			}
			chain, err := cloudflare.New(tok).CreateOriginCert(ctx, rq.Names, string(csr))
			if err != nil {
				return err
			}
			return store([]byte(chain), key)
		})
	case certs.LetsEncrypt:
		return e.letsEncrypt(ctx, st, h, rq, c)
	}
	return nil, &UsageError{Msg: "unknown certificate kind " + rq.Kind + " — letsencrypt, origin, custom or self-signed"}
}

func (e *env) letsEncrypt(ctx context.Context, st *model.State, h *certHome, rq issueReq, c *model.Cert) (*model.Cert, error) {
	if h.noLetsEncrypt != "" {
		return nil, errors.New(h.noLetsEncrypt)
	}
	c.Challenge = certs.ChallengeOf(certs.LetsEncrypt, rq.Challenge)
	email := rq.Email
	if h.ed != nil {
		email = firstNonEmpty(email, edge.ReadEnv(h.ed.Dir)["ACME_EMAIL"])
	}
	is := certs.Issue{Name: rq.Name, Names: rq.Names, Challenge: c.Challenge, Email: email, Staging: rq.Staging}
	if c.Challenge == certs.HTTP01 {
		for _, n := range rq.Names {
			if strings.HasPrefix(n, "*.") {
				return nil, certs.ErrWildcardHTTP
			}
		}
		if err := e.preflight(ctx, h, rq); err != nil {
			return nil, err
		}
	}
	label := strings.Join(rq.Names, ", ")
	if h.ed != nil {
		env := edge.ReadEnv(h.ed.Dir)
		is.Webroot, is.Credentials, is.Propagation = edge.ACMEIn, edge.TokenIn, env["CF_PROPAGATION_SECONDS"]
		if c.Challenge == certs.DNS01 && edge.ReadToken(h.ed.Dir) == "" {
			return nil, errors.New("the DNS challenge needs a Cloudflare token — ngitool edge init, or --challenge http")
		}
		args, err := is.CertbotArgs()
		if err != nil {
			return nil, err
		}
		c.Certbot = "stack"
		what := map[string]string{certs.HTTP01: "HTTP challenge", certs.DNS01: "DNS challenge, ~1 min"}[c.Challenge]
		var res execx.Result
		err = ui.Task("Requesting a Let's Encrypt certificate for "+label+" ("+what+")", func(*ui.TaskCtl) error {
			res = h.stack.Exec(ctx, append([]string{"run", "--rm", "--no-deps", "--entrypoint", "certbot", "certbot"}, args...)...)
			if res.Code != 0 {
				return errors.New("certbot failed")
			}
			return nil
		})
		if err != nil {
			return nil, errors.New("certbot certonly failed (CERT-01)\n" + certs.ExplainCertbot(res.Stdout, res.Stderr))
		}
		return c, nil
	}
	// Host certbot (CERT-05): webroot when one was given, else nginx's plugin.
	is.Authenticator = firstNonEmpty(rq.Authenticator, "nginx")
	if rq.Webroot != "" {
		is.Authenticator, is.Webroot = "webroot", rq.Webroot
	}
	if c.Challenge == certs.DNS01 {
		return nil, errors.New("the DNS challenge runs from the edge stack's certbot (certbot/dns-cloudflare); on host nginx use --challenge http")
	}
	args, err := is.CertbotArgs()
	if err != nil {
		return nil, err
	}
	c.Certbot, c.Authenticator, c.Webroot = "host", is.Authenticator, is.Webroot
	var res execx.Result
	err = ui.Task("Requesting a Let's Encrypt certificate for "+label+" (host certbot, "+is.Authenticator+")", func(*ui.TaskCtl) error {
		res = certRunner.Run(ctx, "certbot", args, execx.Opts{Timeout: 5 * time.Minute})
		if res.Code != 0 {
			return errors.New("certbot failed")
		}
		return nil
	})
	if err != nil {
		return nil, errors.New("certbot certonly failed (CERT-01)\n" + certs.ExplainCertbot(res.Stdout, res.Stderr))
	}
	return c, nil
}

// preflight checks every name answers an HTTP-01 challenge here before
// Let's Encrypt is asked (CERT-01).
func (e *env) preflight(ctx context.Context, h *certHome, rq issueReq) error {
	acme := rq.Webroot
	if h.ed != nil {
		if !h.stack.Running(ctx) {
			return errors.New("the HTTP challenge needs the edge stack running — run: ngitool edge up")
		}
		if res := h.stack.Exec(ctx, "exec", "-T", "nginx", "test", "-d", edge.ACMEIn); res.Code != 0 {
			return errors.New("nginx was started before data/acme existed — run: ngitool edge up (it recreates nginx with the challenge folder)")
		}
		acme = edge.Layout{Dir: h.ed.Dir}.ACME()
	}
	if acme == "" {
		ui.Info("no webroot to test from: certbot's nginx plugin answers the challenge itself — preflight skipped")
		return nil
	}
	var results []certs.Check
	_ = ui.Task("Checking that "+strings.Join(rq.Names, ", ")+" reach this server on port 80", func(*ui.TaskCtl) error {
		results = certs.Preflight(ctx, acme, rq.Names, preflightClient())
		if len(certs.Failed(results)) > 0 {
			return errors.New("")
		}
		return nil
	})
	for _, r := range results {
		if r.Unknown {
			ui.Warning(r.Name + ": " + r.Reason + " — skipping the check")
		}
	}
	bad := certs.Failed(results)
	if len(bad) == 0 {
		return nil
	}
	for _, r := range bad {
		ui.Warning(r.Name + ": " + r.Reason)
	}
	ui.Hint(certs.PreflightFix)
	goOn := rq.Force
	if !goOn && ui.CanPrompt() {
		var err error
		if goOn, err = ui.Confirm("Ask Let's Encrypt anyway?", "failed attempts count toward its rate limit (5 per hour)", false); err != nil {
			return err
		}
	}
	if !goOn {
		return errors.New("the HTTP challenge would fail — nothing was changed (add --force to try anyway) (CERT-01)")
	}
	return nil
}

// removeCertFiles deletes a certificate's files: certbot delete for Let's
// Encrypt, the directory otherwise (only under the instance's cert dir).
func (e *env) removeCertFiles(ctx context.Context, h *certHome, c model.Cert) error {
	if c.Kind == certs.LetsEncrypt {
		args := certs.DeleteArgs(c.Name)
		var res execx.Result
		switch {
		case c.Certbot == "host":
			res = certRunner.Run(ctx, "certbot", args, execx.Opts{Timeout: time.Minute})
		case h.stackOK:
			res = h.stack.Exec(ctx, append([]string{"run", "--rm", "--no-deps", "--entrypoint", "certbot", "certbot"}, args...)...)
		default:
			return errors.New("no certbot to delete " + c.Name + " with")
		}
		if res.Code != 0 {
			return errors.New("certbot delete failed\n" + execx.Tail(res, 4))
		}
		return nil
	}
	dir := filepath.Dir(c.HostCert)
	if h.host == "" || filepath.Dir(dir) != filepath.Clean(h.host) {
		return fmt.Errorf("refusing to delete %s: not in %s", dir, h.host)
	}
	return os.RemoveAll(dir)
}
