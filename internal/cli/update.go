package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/amirhosseinbanaei/NgiTool/internal/execx"
	"github.com/amirhosseinbanaei/NgiTool/internal/paths"
	"github.com/amirhosseinbanaei/NgiTool/internal/state"
	"github.com/amirhosseinbanaei/NgiTool/internal/ui"
	"github.com/amirhosseinbanaei/NgiTool/internal/update"
	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

type updateFlags struct {
	check      bool
	version    string
	rollback   bool
	prerelease bool
	json       bool
}

func updateCmd(e *env) *cobra.Command {
	f := &updateFlags{}
	c := &cobra.Command{
		Use:   "update",
		Short: "check, install, pin or roll back a release",
		Long: `Downloads the release for this machine from GitHub, verifies its SHA-256
against the release's checksums.txt, and swaps the binary atomically. The
binary it replaces is kept next to it as ngitool.prev.

Exit codes for --check: 0 up to date, 10 an update exists.

Environment:
  GITHUB_TOKEN            raises GitHub's API limit above 60 requests/hour
  NGITOOL_DOWNLOAD_BASE   download host for offline mirrors (<base>/<tag>/<asset>)
  NGITOOL_RELEASES_URL    releases JSON to read instead of GitHub's API`,
		Example: `ngitool update --check
ngitool update --yes
ngitool update --version v0.1.0     # pin or downgrade
ngitool update --rollback           # back to the binary before the last update`,
		Annotations: map[string]string{annGroup: "tool", annSynopsis: "update [--check]", annUsage: "update [--check | --version vX.Y.Z | --rollback] [--prerelease] [--json]"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.rollback {
				return runRollback(cmd.Context(), e)
			}
			return runUpdate(cmd.Context(), e, f)
		},
	}
	fl := c.Flags()
	fl.BoolVar(&f.check, "check", false, "only report whether an update exists (exit 10 when it does)")
	fl.StringVar(&f.version, "version", "", "install exactly this `vX.Y.Z` (pin or downgrade)")
	fl.BoolVar(&f.rollback, "rollback", false, "restore the binary kept from before the last update")
	fl.BoolVar(&f.prerelease, "prerelease", false, "include pre-releases")
	fl.BoolVar(&f.json, "json", false, "with --check: machine-readable output")
	c.MarkFlagsMutuallyExclusive("check", "rollback")
	c.MarkFlagsMutuallyExclusive("version", "rollback")
	return c
}

type checkReport struct {
	Current         string   `json:"current"`
	Latest          string   `json:"latest"`
	UpdateAvailable bool     `json:"updateAvailable"`
	Notes           []string `json:"notes,omitempty"`
}

func runUpdate(ctx context.Context, e *env, f *updateFlags) error {
	cfg, err := state.LoadConfig(e.paths)
	if err != nil {
		return err
	}
	pre := f.prerelease || cfg.Channel == "prerelease"
	client := update.NewClient(60 * time.Second)
	current := version.Version

	var wantTag string
	if f.version != "" {
		if _, err := version.Parse(f.version); err != nil {
			return &UsageError{Msg: err.Error(), Cmd: "ngitool update"}
		}
		wantTag = update.Tag(f.version)
	}

	// Find the target release. With --version the list is only needed for
	// the notes, so an unreachable API is a warning (offline mirrors).
	var releases []update.Release
	fetch := func(t *ui.TaskCtl) error {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		rs, err := client.Releases(ctx)
		releases = rs
		return err
	}
	var target update.Release
	if f.json {
		err = fetch(nil)
	} else {
		err = ui.Task("Checking releases", fetch)
	}
	switch {
	case err != nil && wantTag == "":
		return err
	case err != nil:
		ui.Warning("could not read the release list (" + err.Error() + "); installing " + wantTag + " without release notes")
		target = update.Release{Tag: wantTag}
	case wantTag != "":
		r, ok := update.Find(releases, wantTag)
		if !ok {
			return fmt.Errorf("there is no release %s — available: %s", wantTag, tagList(releases, 6))
		}
		target = r
	default:
		r, ok := update.Latest(releases, pre)
		if !ok {
			return errors.New("no releases published yet")
		}
		target = r
	}

	cmp := version.Compare(target.Tag, current)
	notes := update.Notes(target.Body, 6)

	if f.check {
		rep := checkReport{Current: current, Latest: target.Tag, UpdateAvailable: cmp > 0, Notes: notes}
		if f.json {
			if err := printJSON(rep); err != nil {
				return err
			}
		} else {
			printCheck(rep)
		}
		if rep.UpdateAvailable {
			return &ExitError{Code: ExitUpdate}
		}
		return nil
	}

	if wantTag == "" && cmp <= 0 {
		ui.Done("NgiTool " + current + " is up to date " + ui.Muted("(latest: "+target.Tag+")"))
		return nil
	}
	if cmp == 0 {
		ui.Done("NgiTool " + current + " is already installed")
		return nil
	}

	exe, err := paths.Executable()
	if err != nil {
		return err
	}
	if err := update.CheckWritable(exe, os.Args[1:]); err != nil {
		return err
	}
	asset, err := update.AssetName(target.Tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	change := current + " " + ui.SymArrow + " " + ui.Accent(target.Tag)
	if cmp < 0 {
		change += "  " + ui.Warn("downgrade")
	}
	ui.Plan("Update NgiTool", [][2]string{
		{"version", change},
		{"binary", exe},
		{"keeps", exe + ".prev " + ui.Muted("(undo: ngitool update --rollback)")},
		{"download", client.AssetURL(target.Tag, asset)},
		{"verified by", update.ChecksumFile + " (SHA-256)"},
	})
	if len(notes) > 0 {
		for _, n := range notes {
			ui.Hint(n)
		}
		fmt.Fprintln(ui.Out)
	}
	if cmp < 0 {
		ui.Explain(
			"replaces "+current+" with the older "+target.Tag,
			"only the ngitool binary; config and state stay as they are",
			"ngitool update (latest) or ngitool update --rollback")
		fmt.Fprintln(ui.Out)
	}
	ok, err := ui.Sure(e.yes, "Install "+target.Tag+"?", "")
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}

	return e.withLock(ctx, false, func() error {
		var bin []byte
		if err := ui.Task("Downloading "+asset, func(t *ui.TaskCtl) error {
			var derr error
			bin, derr = client.Download(ctx, target.Tag, runtime.GOOS, runtime.GOARCH)
			if derr == nil {
				t.Update(fmt.Sprintf("Downloaded and verified %s (%.1f MB)", asset, float64(len(bin))/1e6))
			}
			return derr
		}); err != nil {
			return err
		}
		if err := ui.Task("Installing "+target.Tag, func(t *ui.TaskCtl) error {
			return update.Install(exe, bin, func(p string) error { return checkBinary(ctx, p, target.Tag) })
		}); err != nil {
			return err
		}
		setPin(e, wantTag, cmp < 0 || wantTag != "")
		ui.Done("NgiTool " + target.Tag + " installed " + ui.Muted("("+current+" kept as "+filepath.Base(exe)+".prev)"))
		return nil
	})
}

// checkBinary runs the new binary before it replaces the old one: it has to
// start on this machine and report the version it was downloaded as.
func checkBinary(ctx context.Context, path, tag string) error {
	res, err := execx.Must(ctx, path, []string{"version", "--json"}, execx.Opts{
		Timeout: 15 * time.Second,
		Env:     []string{"NGITOOL_NO_UPDATE_CHECK=1"},
	})
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run on this machine: %w", err)
	}
	var info version.Info
	if err := json.Unmarshal([]byte(res.Stdout), &info); err != nil {
		return fmt.Errorf("the downloaded binary printed unexpected version output: %w", err)
	}
	if version.Compare(info.Version, tag) != 0 {
		return fmt.Errorf("the downloaded binary says it is %s, expected %s", info.Version, tag)
	}
	return nil
}

// setPin records a --version install so the background check stays quiet;
// a plain update clears it. Best effort: a read-only /etc is only a warning.
func setPin(e *env, tag string, pin bool) {
	cfg, err := state.LoadConfig(e.paths)
	if err != nil {
		return
	}
	want := ""
	if pin {
		want = tag
	}
	if cfg.Pinned == want {
		return
	}
	cfg.Pinned = want
	if err := state.ConfigStore(e.paths).Save(cfg); err != nil {
		ui.Warning("could not record the pin in " + e.paths.Config + ": " + err.Error())
	}
}

func runRollback(ctx context.Context, e *env) error {
	exe, err := paths.Executable()
	if err != nil {
		return err
	}
	prev := exe + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary at %s — nothing to roll back to", prev)
	}
	if err := update.CheckWritable(exe, os.Args[1:]); err != nil {
		return err
	}
	prevVersion := binaryVersion(ctx, prev)
	ui.Plan("Roll back NgiTool", [][2]string{
		{"version", version.Version + " " + ui.SymArrow + " " + ui.Accent(prevVersion)},
		{"binary", exe},
		{"keeps", prev + ui.Muted(" (rolling back again undoes this)")},
	})
	ok, err := ui.Sure(e.yes, "Roll back to "+prevVersion+"?", "")
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	return e.withLock(ctx, false, func() error {
		if err := update.Rollback(exe); err != nil {
			return err
		}
		ui.Done("Rolled back to " + prevVersion + " " + ui.Muted("("+version.Version+" kept as "+filepath.Base(prev)+")"))
		return nil
	})
}

func binaryVersion(ctx context.Context, path string) string {
	res := execx.Run(ctx, path, []string{"version", "--json"}, execx.Opts{Timeout: 10 * time.Second, Env: []string{"NGITOOL_NO_UPDATE_CHECK=1"}})
	var info version.Info
	if res.Code != 0 || json.Unmarshal([]byte(res.Stdout), &info) != nil {
		return "the previous version"
	}
	return info.Version
}

func printCheck(r checkReport) {
	if r.UpdateAvailable {
		ui.Plan("Update available", [][2]string{{"current", r.Current}, {"latest", ui.Accent(r.Latest)}})
		for _, n := range r.Notes {
			ui.Hint(n)
		}
		if len(r.Notes) > 0 {
			fmt.Fprintln(ui.Out)
		}
		ui.Plain("  Install it: " + ui.Key("ngitool update"))
		return
	}
	ui.Done("NgiTool " + r.Current + " is up to date " + ui.Muted("(latest: "+r.Latest+")"))
}

func tagList(rs []update.Release, n int) string {
	var tags []string
	for i, r := range rs {
		if i == n {
			tags = append(tags, "…")
			break
		}
		tags = append(tags, r.Tag)
	}
	if len(tags) == 0 {
		return "none"
	}
	return strings.Join(tags, ", ")
}
