package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/compose"
	"github.com/amirhosseinbanaei/NgiTool/internal/discover"
	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/model"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/update"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

// Check is one doctor line. Later prompts append theirs to checks.
type Check struct {
	ID    string
	Label string
	Run   func(ctx context.Context, e *env) CheckResult
}

// CheckResult is what a check found and, when it is not ok, how to fix it.
type CheckResult struct {
	Status ui.Status
	Detail string
	Fix    string
}

var checks = []Check{
	{ID: "root", Label: "Running as root", Run: checkRoot},
	{ID: "docker", Label: "Docker CLI", Run: checkDocker},
	{ID: "daemon", Label: "Docker daemon", Run: checkDaemon},
	{ID: "compose", Label: "Docker Compose v2", Run: checkCompose},
	{ID: "docker-access", Label: "Docker access", Run: checkDockerAccess},
	{ID: "ss", Label: "ss (socket owners)", Run: checkSS},
	{ID: "front-door", Label: "Front door", Run: checkFrontDoor},
	{ID: "configs", Label: "nginx configs", Run: checkConfigs},
	{ID: "apps", Label: "Linked apps", Run: checkApps},
	{ID: "state", Label: "State directory", Run: checkStateDir},
	{ID: "updates", Label: "Update source", Run: checkUpdates},
}

type doctorLine struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func doctorCmd(e *env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:         "doctor",
		Short:       "root, docker, compose, ss, front door, configs, apps, state, updates",
		Long:        "Runs every check at once and prints ✔ ok, ! warning or ✖ failed, with a fix for each line that is not ok.\nExits 1 when a check failed.",
		Annotations: map[string]string{annGroup: "server", annSynopsis: "doctor [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var results []CheckResult
			run := func() {
				results = runChecks(cmd.Context(), e)
			}
			if asJSON {
				run()
			} else {
				_ = ui.Task("Checking this server", func(*ui.TaskCtl) error { run(); return nil })
			}
			lines := make([]doctorLine, len(checks))
			failed, warned := 0, 0
			for i, ch := range checks {
				r := results[i]
				lines[i] = doctorLine{ID: ch.ID, Label: ch.Label, Status: r.Status.String(), Detail: r.Detail, Fix: r.Fix}
				switch r.Status {
				case ui.StatusFail:
					failed++
				case ui.StatusWarn:
					warned++
				}
			}
			if asJSON {
				if err := printJSON(lines); err != nil {
					return err
				}
			} else {
				ui.Heading("Doctor", "NgiTool "+version.Version)
				lw := 0
				for _, ch := range checks {
					lw = max(lw, ui.Width(ch.Label))
				}
				for i, ch := range checks {
					for _, l := range ui.CheckLine(results[i].Status, ui.Pad(ch.Label, lw), results[i].Detail, results[i].Fix) {
						ui.Plain(l)
					}
				}
				ui.Plain("")
				ui.Plain("  " + summaryLine(len(checks)-failed-warned, warned, failed))
			}
			if failed > 0 {
				return &ExitError{Code: ExitFail}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}

func summaryLine(ok, warn, fail int) string {
	parts := []string{ui.OK(fmt.Sprintf("%d ok", ok))}
	if warn > 0 {
		parts = append(parts, ui.Warn(fmt.Sprintf("%d warning%s", warn, plural(warn))))
	}
	if fail > 0 {
		parts = append(parts, ui.Err(fmt.Sprintf("%d failed", fail)))
	}
	return strings.Join(parts, ui.Muted(" · "))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// runChecks runs every check concurrently; results keep the registry order.
func runChecks(ctx context.Context, e *env) []CheckResult {
	out := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, ch := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = ch.Run(ctx, e)
		}()
	}
	wg.Wait()
	return out
}

func checkRoot(context.Context, *env) CheckResult {
	if uid := os.Geteuid(); uid != 0 {
		return CheckResult{ui.StatusWarn, fmt.Sprintf("uid %d", uid), "run with sudo; without root NgiTool sees only part of the server"}
	}
	return CheckResult{Status: ui.StatusOK, Detail: "uid 0"}
}

func checkDocker(ctx context.Context, _ *env) CheckResult {
	if !execx.Has("docker") {
		return CheckResult{ui.StatusWarn, "not installed", "needed for nginx in containers and compose projects: https://docs.docker.com/engine/install/"}
	}
	res := execx.Run(ctx, "docker", []string{"version", "--format", "{{.Client.Version}}"}, execx.Opts{Timeout: 5 * time.Second})
	return CheckResult{Status: ui.StatusOK, Detail: "client " + firstLine(res.Stdout)}
}

func checkDaemon(ctx context.Context, _ *env) CheckResult {
	if !execx.Has("docker") {
		return CheckResult{ui.StatusWarn, "skipped (no docker CLI)", ""}
	}
	res := execx.Run(ctx, "docker", []string{"info", "--format", "{{.ServerVersion}}"}, execx.Opts{Timeout: 8 * time.Second})
	if res.Code == 0 && strings.TrimSpace(res.Stdout) != "" {
		return CheckResult{Status: ui.StatusOK, Detail: "server " + firstLine(res.Stdout)}
	}
	msg := strings.ToLower(res.Stderr)
	switch {
	case strings.Contains(msg, "permission denied"):
		return CheckResult{ui.StatusFail, "permission denied on the docker socket", "run as root, or add this user to the docker group"}
	case res.Code == 124:
		return CheckResult{ui.StatusFail, "no answer within 8s", "check `systemctl status docker`"}
	}
	return CheckResult{ui.StatusFail, "not reachable", "start it: systemctl start docker"}
}

func checkCompose(ctx context.Context, _ *env) CheckResult {
	if execx.Has("docker") {
		res := execx.Run(ctx, "docker", []string{"compose", "version", "--short"}, execx.Opts{Timeout: 5 * time.Second, Scrub: execx.ComposeScrub})
		if res.Code == 0 {
			return CheckResult{Status: ui.StatusOK, Detail: "v" + strings.TrimPrefix(firstLine(res.Stdout), "v")}
		}
	}
	if execx.Has("docker-compose") {
		return CheckResult{ui.StatusWarn, "only docker-compose v1 found", "install the v2 plugin: apt install docker-compose-plugin (DOCK-05)"}
	}
	return CheckResult{ui.StatusWarn, "not installed", "install the plugin: apt install docker-compose-plugin"}
}

func checkSS(context.Context, *env) CheckResult {
	if execx.Has("ss") {
		return CheckResult{Status: ui.StatusOK, Detail: "found"}
	}
	return CheckResult{ui.StatusWarn, "not installed", "apt install iproute2 — needed to see which process owns :80 and :443"}
}

func checkStateDir(_ context.Context, e *env) CheckResult {
	dir := e.paths.Lib
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return CheckResult{ui.StatusFail, dir + " is not a directory", "move it out of the way"}
		}
		if syscall.Access(dir, 2) != nil {
			return CheckResult{ui.StatusFail, dir + " is not writable", "run as root, or set NGITOOL_ROOT"}
		}
		return CheckResult{Status: ui.StatusOK, Detail: dir}
	}
	parent := filepath.Dir(dir)
	for {
		if _, err := os.Stat(parent); err == nil {
			break
		}
		parent = filepath.Dir(parent)
	}
	if syscall.Access(parent, 2) != nil {
		return CheckResult{ui.StatusFail, dir + " cannot be created", "run as root, or set NGITOOL_ROOT"}
	}
	return CheckResult{Status: ui.StatusOK, Detail: dir + " (created on first change)"}
}

func checkUpdates(ctx context.Context, _ *env) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := update.NewClient(5 * time.Second)
	rs, err := client.Releases(ctx)
	var rl *update.RateLimitError
	var st *update.StatusError
	switch {
	case errors.As(err, &rl):
		return CheckResult{ui.StatusWarn, "GitHub rate limit reached", "set GITHUB_TOKEN, or wait an hour"}
	case errors.As(err, &st):
		return CheckResult{ui.StatusWarn, "releases answered " + st.Status, "releases come from github.com/" + update.Repo + "; offline: " + update.EnvDownloadBase + "=… ngitool update --version vX.Y.Z"}
	case err != nil:
		return CheckResult{ui.StatusWarn, "unreachable", "offline? use a mirror: " + update.EnvDownloadBase + "=… ngitool update --version vX.Y.Z"}
	}
	latest, ok := update.Latest(rs, false)
	if !ok {
		return CheckResult{Status: ui.StatusOK, Detail: "reachable, no releases yet"}
	}
	if version.IsDev() {
		return CheckResult{Status: ui.StatusOK, Detail: "reachable, latest " + latest.Tag + " (this is a dev build)"}
	}
	if version.Compare(latest.Tag, version.Version) > 0 {
		return CheckResult{ui.StatusWarn, "update available: " + latest.Tag, "ngitool update"}
	}
	return CheckResult{Status: ui.StatusOK, Detail: "up to date (" + latest.Tag + ")"}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// checkDockerAccess is how far NgiTool can see containers (DISC-13).
func checkDockerAccess(ctx context.Context, e *env) CheckResult {
	d := discover.DockerAccess(ctx, newScanEnv(e))
	switch d.State {
	case "ok":
		who := "root"
		if os.Geteuid() != 0 {
			who = "docker group"
		}
		return CheckResult{Status: ui.StatusOK, Detail: "full (" + who + ")"}
	case "rootless", "podman":
		return CheckResult{ui.StatusWarn, d.Detail, d.Hint}
	case "missing":
		return CheckResult{ui.StatusWarn, "no docker: host nginx only", d.Hint}
	}
	return CheckResult{ui.StatusFail, d.Detail, d.Hint}
}

// checkFrontDoor: which instance owns :443 (else :80) (DISC-15).
func checkFrontDoor(ctx context.Context, e *env) CheckResult {
	r := e.report(ctx, false, false)
	if in := r.Find(r.FrontDoor); in != nil {
		return CheckResult{Status: ui.StatusOK, Detail: in.Name + " (" + in.ID + ")"}
	}
	for _, o := range r.Ports {
		if (o.Port == 443 || o.Port == 80) && o.Process != "" {
			return CheckResult{ui.StatusWarn, fmt.Sprintf(":%d is owned by %s, not an nginx", o.Port, firstNonEmpty(o.Container, o.Process)), "ngitool scan shows every port owner"}
		}
	}
	return CheckResult{ui.StatusWarn, "nothing owns :80 or :443 (only this machine is scanned)", "start an nginx on :80/:443 — ngitool scan lists what exists"}
}

// checkConfigs runs nginx -t on every instance that can be tested.
func checkConfigs(ctx context.Context, e *env) CheckResult {
	r := e.report(ctx, false, false)
	senv := newScanEnv(e)
	var bad, untested []string
	tested := 0
	for i := range r.Instances {
		in := &r.Instances[i]
		if !in.Caps.Test.OK {
			untested = append(untested, in.Name)
			continue
		}
		res, err := discover.Test(ctx, senv, in)
		if err != nil {
			untested = append(untested, in.Name)
			continue
		}
		tested++
		if !res.OK {
			bad = append(bad, in.Name+": "+firstLine(res.Output))
		}
	}
	switch {
	case len(bad) > 0:
		return CheckResult{ui.StatusFail, strings.Join(bad, "; "), "fix the file nginx names — ngitool inspect <id> shows it"}
	case tested == 0 && len(untested) == 0:
		return CheckResult{ui.StatusWarn, "no nginx found", "ngitool scan"}
	case len(untested) > 0:
		return CheckResult{ui.StatusOK, fmt.Sprintf("%d valid, %d not testable (%s)", tested, len(untested), strings.Join(untested, ", ")), ""}
	}
	return CheckResult{Status: ui.StatusOK, Detail: fmt.Sprintf("%d valid", tested)}
}

// checkApps is DOCK-07 and DOCK-10: a linked app started without its
// override, or whose files are gone.
func checkApps(ctx context.Context, e *env) CheckResult {
	st, err := model.Load(e.paths)
	if err != nil {
		return CheckResult{ui.StatusFail, err.Error(), "fix or move " + e.paths.State}
	}
	if len(st.Apps) == 0 {
		return CheckResult{Status: ui.StatusOK, Detail: "none linked"}
	}
	ctrs, err := appContainers(ctx)
	if err != nil {
		return CheckResult{ui.StatusWarn, fmt.Sprintf("%d linked; container state unknown: %s", len(st.Apps), firstLine(err.Error())), "check the Docker lines above"}
	}
	var detached, missing []string
	for _, a := range st.Apps {
		switch compose.Check(a, ctrs).Status {
		case compose.NetDetached:
			detached = append(detached, a.Name)
		case compose.NetMissing:
			missing = append(missing, a.Name)
		}
	}
	switch {
	case len(detached) > 0:
		return CheckResult{ui.StatusFail, strings.Join(detached, ", ") + " " + compose.DetachedWhy + " (DOCK-07)", "ngitool app fix " + detached[0]}
	case len(missing) > 0:
		return CheckResult{ui.StatusWarn, strings.Join(missing, ", ") + ": compose files are gone (DOCK-10)", "ngitool app show " + missing[0]}
	}
	return CheckResult{Status: ui.StatusOK, Detail: fmt.Sprintf("%d linked, none detached", len(st.Apps))}
}
