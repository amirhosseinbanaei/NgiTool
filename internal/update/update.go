// Package update replaces the ngitool binary with a release from GitHub:
// pick the release, download the archive for this OS/arch, verify its
// SHA-256 against checksums.txt, swap the binary atomically, keep the old
// one as <binary>.prev for rollback.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/amirhosseinbanaei/NgiTool/internal/version"
)

const (
	Repo                = "amirhosseinbanaei/NgiTool"
	DefaultReleasesURL  = "https://api.github.com/repos/" + Repo + "/releases?per_page=100"
	DefaultDownloadBase = "https://github.com/" + Repo + "/releases/download"

	EnvReleasesURL  = "NGITOOL_RELEASES_URL"
	EnvDownloadBase = "NGITOOL_DOWNLOAD_BASE"
	EnvToken        = "GITHUB_TOKEN"

	BinaryName   = "ngitool"
	ChecksumFile = "checksums.txt"
	maxDownload  = 64 << 20
)

// Release is the part of GitHub's release object NgiTool reads.
type Release struct {
	Tag        string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Published  string `json:"published_at"`
	URL        string `json:"html_url"`
}

// Client talks to the releases API and the download host.
type Client struct {
	ReleasesURL  string
	DownloadBase string
	Token        string
	HTTP         *http.Client
}

// NewClient reads NGITOOL_RELEASES_URL, NGITOOL_DOWNLOAD_BASE and GITHUB_TOKEN.
func NewClient(timeout time.Duration) *Client {
	c := &Client{
		ReleasesURL:  DefaultReleasesURL,
		DownloadBase: DefaultDownloadBase,
		Token:        os.Getenv(EnvToken),
		HTTP:         &http.Client{Timeout: timeout},
	}
	if v := os.Getenv(EnvReleasesURL); v != "" {
		c.ReleasesURL = v
	}
	if v := os.Getenv(EnvDownloadBase); v != "" {
		c.DownloadBase = strings.TrimRight(v, "/")
	}
	return c
}

// RateLimitError is GitHub's 60-requests-per-hour limit (edge case SYS-02).
type RateLimitError struct {
	Reset         time.Time
	Authenticated bool
}

func (e *RateLimitError) Error() string {
	msg := "GitHub API rate limit reached"
	if !e.Authenticated {
		msg += " (60 requests/hour without a token); set GITHUB_TOKEN to raise it"
	}
	if !e.Reset.IsZero() {
		msg += fmt.Sprintf(", or retry after %s", e.Reset.Local().Format("15:04"))
	}
	return msg
}

// UnreachableError wraps a network failure with the offline way out.
type UnreachableError struct {
	URL string
	Err error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cannot reach %s: %v — offline? point %s at a mirror and pass --version vX.Y.Z", host(e.URL), e.Err, EnvDownloadBase)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// StatusError is a non-200 answer from the API or the download host.
type StatusError struct {
	URL    string
	Status string
	Code   int
}

func (e *StatusError) Error() string {
	if e.Code == http.StatusNotFound {
		return fmt.Sprintf("%s answered 404 Not Found — no such release or repository (is %s public?)", e.URL, Repo)
	}
	return fmt.Sprintf("%s answered %s", e.URL, e.Status)
}

// ChecksumError is a download whose SHA-256 does not match (edge case SYS-04).
type ChecksumError struct {
	Asset     string
	Want, Got string
}

func (e *ChecksumError) Error() string {
	if e.Want == "" {
		return fmt.Sprintf("%s is not listed in %s — refusing to install an unverified binary", e.Asset, ChecksumFile)
	}
	return fmt.Sprintf("checksum mismatch for %s: expected %s, got %s — the download is corrupt or was tampered with; nothing was changed", e.Asset, short(e.Want), short(e.Got))
}

// NotWritableError is a binary directory we cannot replace files in (SYS-03).
type NotWritableError struct {
	Dir     string
	Command string
}

func (e *NotWritableError) Error() string {
	return fmt.Sprintf("cannot write to %s — run: %s", e.Dir, e.Command)
}

// Releases lists releases, newest first by version, drafts dropped.
func (c *Client) Releases(ctx context.Context) ([]Release, error) {
	body, err := c.get(ctx, c.ReleasesURL, true)
	if err != nil {
		return nil, err
	}
	var rs []Release
	if err := json.Unmarshal(body, &rs); err != nil {
		// A single release object (the /releases/latest shape) is fine too.
		var one Release
		if err2 := json.Unmarshal(body, &one); err2 != nil || one.Tag == "" {
			return nil, fmt.Errorf("unexpected releases response from %s: %w", host(c.ReleasesURL), err)
		}
		rs = []Release{one}
	}
	out := rs[:0]
	for _, r := range rs {
		if !r.Draft && r.Tag != "" {
			if _, err := version.Parse(r.Tag); err == nil {
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return version.Compare(out[i].Tag, out[j].Tag) > 0 })
	return out, nil
}

// Latest is the newest release, including prereleases only when asked.
func Latest(rs []Release, prerelease bool) (Release, bool) {
	for _, r := range rs {
		if prerelease || !(r.Prerelease || version.IsPrerelease(r.Tag)) {
			return r, true
		}
	}
	return Release{}, false
}

// Find is the release with this tag ("v" optional).
func Find(rs []Release, tag string) (Release, bool) {
	for _, r := range rs {
		if strings.TrimPrefix(r.Tag, "v") == strings.TrimPrefix(tag, "v") {
			return r, true
		}
	}
	return Release{}, false
}

// Tag normalises "1.2.3" to "v1.2.3".
func Tag(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// ArchName is the release's name for an OS/arch: amd64, arm64, armv7.
func ArchName(goos, goarch string) (string, error) {
	if goos != "linux" {
		return "", fmt.Errorf("NgiTool releases are built for linux only (this is %s/%s)", goos, goarch)
	}
	switch goarch {
	case "amd64", "arm64":
		return goarch, nil
	case "arm":
		return "armv7", nil
	}
	return "", fmt.Errorf("no NgiTool release for linux/%s (built: amd64, arm64, armv7)", goarch)
}

// AssetName is ngitool_<version>_linux_<arch>.tar.gz, the GoReleaser name.
func AssetName(tag, goos, goarch string) (string, error) {
	arch, err := ArchName(goos, goarch)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", BinaryName, strings.TrimPrefix(tag, "v"), goos, arch), nil
}

// AssetURL is <download base>/<tag>/<asset>, GitHub's layout and the mirror's.
func (c *Client) AssetURL(tag, asset string) string {
	return c.DownloadBase + "/" + url.PathEscape(tag) + "/" + url.PathEscape(asset)
}

// Download fetches the archive for tag and this OS/arch, verifies it against
// checksums.txt and returns the ngitool binary inside it.
func (c *Client) Download(ctx context.Context, tag, goos, goarch string) ([]byte, error) {
	asset, err := AssetName(tag, goos, goarch)
	if err != nil {
		return nil, err
	}
	sums, err := c.get(ctx, c.AssetURL(tag, ChecksumFile), false)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", ChecksumFile, err)
	}
	archive, err := c.get(ctx, c.AssetURL(tag, asset), false)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	if err := Verify(archive, asset, ParseChecksums(sums)); err != nil {
		return nil, err
	}
	return ExtractBinary(archive, BinaryName)
}

// ParseChecksums reads sha256sum output: "<hex>  <name>" per line.
func ParseChecksums(b []byte) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// Verify checks data's SHA-256 against the listed sum for asset.
func Verify(data []byte, asset string, sums map[string]string) error {
	want := sums[asset]
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if want == "" || want != got {
		return &ChecksumError{Asset: asset, Want: want, Got: got}
	}
	return nil
}

// ExtractBinary returns the file called name from a .tar.gz.
func ExtractBinary(archive []byte, name string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("archive is not gzip: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("archive has no %s binary", name)
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

// CheckWritable fails with the command to run when target's directory
// cannot take a new file.
func CheckWritable(target string, args []string) error {
	dir := filepath.Dir(target)
	if err := syscall.Access(dir, 2 /* W_OK */); err != nil {
		cmd := "sudo " + strings.Join(append([]string{target}, args...), " ")
		return &NotWritableError{Dir: dir, Command: cmd}
	}
	return nil
}

// Install writes bin next to target, runs check on it (e.g. "does it
// start?"), keeps the current binary as target.prev and renames the new one
// over target. The rename is atomic: running processes keep the old inode.
func Install(target string, bin []byte, check func(path string) error) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".ngitool.new-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if check != nil {
		if err := check(tmpName); err != nil {
			return err
		}
	}
	if _, err := os.Stat(target); err == nil {
		if err := keepPrev(target); err != nil {
			return fmt.Errorf("keep the current binary as %s.prev: %w", filepath.Base(target), err)
		}
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

// Rollback swaps target and target.prev, so a second rollback undoes the first.
func Rollback(target string) error {
	prev := target + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous binary at %s — nothing to roll back to", prev)
	}
	swap := target + ".swap"
	_ = os.Remove(swap)
	if err := os.Link(target, swap); err != nil {
		if err := copyFile(target, swap); err != nil {
			return err
		}
	}
	if err := os.Rename(prev, target); err != nil {
		_ = os.Remove(swap)
		return err
	}
	if err := os.Rename(swap, prev); err != nil {
		return err
	}
	syncDir(filepath.Dir(target))
	return nil
}

func keepPrev(target string) error {
	prev := target + ".prev"
	_ = os.Remove(prev)
	if err := os.Link(target, prev); err == nil {
		return nil
	}
	return copyFile(target, prev)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, st.Mode().Perm())
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Notes returns the first n non-empty lines of release notes.
func Notes(body string, n int) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
		if len(out) == n {
			break
		}
	}
	return out
}

func (c *Client) get(ctx context.Context, u string, api bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ngitool/"+version.Version)
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		// The token only ever goes to GitHub's API, never to a mirror.
		if c.Token != "" && host(u) == "api.github.com" {
			req.Header.Set("Authorization", "Bearer "+c.Token)
		}
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &UnreachableError{URL: u, Err: err}
	}
	defer res.Body.Close()
	if (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests) &&
		res.Header.Get("X-RateLimit-Remaining") == "0" {
		e := &RateLimitError{Authenticated: req.Header.Get("Authorization") != ""}
		var reset int64
		if _, err := fmt.Sscan(res.Header.Get("X-RateLimit-Reset"), &reset); err == nil && reset > 0 {
			e.Reset = time.Unix(reset, 0)
		}
		return nil, e
	}
	if res.StatusCode != http.StatusOK {
		return nil, &StatusError{URL: u, Status: res.Status, Code: res.StatusCode}
	}
	return io.ReadAll(io.LimitReader(res.Body, maxDownload))
}

func host(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12] + "…"
	}
	return sum
}
