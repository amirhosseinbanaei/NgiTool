package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssetPerArch(t *testing.T) {
	cases := map[string]string{
		"amd64": "ngitool_0.2.0_linux_amd64.tar.gz",
		"arm64": "ngitool_0.2.0_linux_arm64.tar.gz",
		"arm":   "ngitool_0.2.0_linux_armv7.tar.gz",
	}
	for arch, want := range cases {
		got, err := AssetName("v0.2.0", "linux", arch)
		if err != nil || got != want {
			t.Errorf("%s: got %q %v, want %q", arch, got, err, want)
		}
	}
	for _, bad := range [][2]string{{"linux", "386"}, {"darwin", "arm64"}, {"linux", "riscv64"}} {
		if _, err := AssetName("v0.2.0", bad[0], bad[1]); err == nil {
			t.Errorf("%s/%s accepted", bad[0], bad[1])
		}
	}
}

func TestLatestAndFind(t *testing.T) {
	rs := []Release{{Tag: "v0.3.0-rc.1", Prerelease: true}, {Tag: "v0.2.0"}, {Tag: "v0.1.0"}}
	if r, _ := Latest(rs, false); r.Tag != "v0.2.0" {
		t.Errorf("stable latest = %s", r.Tag)
	}
	if r, _ := Latest(rs, true); r.Tag != "v0.3.0-rc.1" {
		t.Errorf("prerelease latest = %s", r.Tag)
	}
	if r, ok := Find(rs, "0.1.0"); !ok || r.Tag != "v0.1.0" {
		t.Errorf("find = %v %v", r, ok)
	}
}

func tarball(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// mirror serves a releases list plus <tag>/<asset> and <tag>/checksums.txt.
func mirror(t *testing.T, tamper bool) (*httptest.Server, *Client) {
	archive := tarball(t, "ngitool", []byte("#!/bin/sh\necho v0.2.0\n"))
	asset := "ngitool_0.2.0_linux_amd64.tar.gz"
	checksum := sum(archive)
	if tamper {
		checksum = strings.Repeat("0", 64)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"v0.1.0"},{"tag_name":"v0.2.0","body":"## Changes\n\n- feat: first\n- fix: second\n"},{"tag_name":"v0.9.0","draft":true}]`)
	})
	mux.HandleFunc("/v0.2.0/"+asset, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("/v0.2.0/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n%s  other.tar.gz\n", checksum, asset, strings.Repeat("a", 64))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &Client{ReleasesURL: srv.URL + "/releases", DownloadBase: srv.URL, HTTP: srv.Client(), Token: "secret"}
}

func TestReleasesDropDraftsAndSort(t *testing.T) {
	_, c := mirror(t, false)
	rs, err := c.Releases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Tag != "v0.2.0" {
		t.Fatalf("releases = %+v", rs)
	}
	if n := Notes(rs[0].Body, 2); len(n) != 2 || n[1] != "- feat: first" {
		t.Errorf("notes = %q", n)
	}
}

func TestDownloadVerifies(t *testing.T) {
	_, c := mirror(t, false)
	bin, err := c.Download(context.Background(), "v0.2.0", "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bin), "echo v0.2.0") {
		t.Fatalf("binary = %q", bin)
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	_, c := mirror(t, true)
	_, err := c.Download(context.Background(), "v0.2.0", "linux", "amd64")
	var ce *ChecksumError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("want ChecksumError, got %v", err)
	}
}

func TestVerifyUnlisted(t *testing.T) {
	if err := Verify([]byte("x"), "missing.tar.gz", map[string]string{}); err == nil {
		t.Fatal("unlisted asset verified")
	}
}

func TestTokenOnlyToGitHubAPI(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{ReleasesURL: srv.URL, HTTP: srv.Client(), Token: "secret"}
	if _, err := c.Releases(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatal("token sent to a non-GitHub host")
	}
}

func TestRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := &Client{ReleasesURL: srv.URL, HTTP: srv.Client()}
	_, err := c.Releases(context.Background())
	var rl *RateLimitError
	if !errors.As(err, &rl) || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("want RateLimitError, got %v", err)
	}
}

func TestInstallAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ngitool")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A failing check leaves everything as it was.
	if err := Install(target, []byte("broken"), func(string) error { return errors.New("does not start") }); err == nil {
		t.Fatal("failed check installed anyway")
	}
	assertFile(t, target, "old")
	if _, err := os.Stat(target + ".prev"); err == nil {
		t.Fatal(".prev created by a failed install")
	}

	if err := Install(target, []byte("new"), func(p string) error {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o755 {
			return errors.New("not executable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "new")
	assertFile(t, target+".prev", "old")

	if err := Rollback(target); err != nil {
		t.Fatal(err)
	}
	assertFile(t, target, "old")
	assertFile(t, target+".prev", "new")
	if err := Rollback(target); err != nil { // rolling back twice undoes the rollback
		t.Fatal(err)
	}
	assertFile(t, target, "new")

	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left-over files: %v", entries)
	}
}

func TestRollbackWithoutPrev(t *testing.T) {
	target := filepath.Join(t.TempDir(), "ngitool")
	_ = os.WriteFile(target, []byte("x"), 0o755)
	if err := Rollback(target); err == nil || !strings.Contains(err.Error(), "nothing to roll back") {
		t.Fatalf("got %v", err)
	}
}

func TestCheckWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o555)
	defer os.Chmod(dir, 0o755)
	err := CheckWritable(filepath.Join(dir, "ngitool"), []string{"update"})
	var nw *NotWritableError
	if !errors.As(err, &nw) || !strings.Contains(err.Error(), "sudo ") {
		t.Fatalf("got %v", err)
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s = %q (%v), want %q", filepath.Base(path), b, err, want)
	}
}
