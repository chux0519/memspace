package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// release serves a release's files: the asset for this platform and
// SHA256SUMS (with sums overridden when set).
func release(t *testing.T, content string, sums string) *httptest.Server {
	t.Helper()
	asset, err := Asset(runtime.GOOS, runtime.GOARCH, FullBuild)
	if err != nil {
		t.Skip(err)
	}
	h := sha256.Sum256([]byte(content))
	if sums == "" {
		sums = hex.EncodeToString(h[:]) + "  " + asset + "\n" + strings.Repeat("0", 64) + "  other\n"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + asset:
			w.Write([]byte(content))
		case "/SHA256SUMS":
			w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fakeExe(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "memspace")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func okVerify(path, version string) error { return nil }

func TestInstall(t *testing.T) {
	srv := release(t, "new binary", "")
	exe := fakeExe(t)
	var verified string
	err := Install(context.Background(), Options{Exe: exe, Version: "v0.2.0", BaseURL: srv.URL,
		Verify: func(path, version string) error { verified = version; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Errorf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old" {
		t.Errorf("prev = %q", b)
	}
	if verified != "v0.2.0" {
		t.Errorf("verified %q", verified)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), ".memspace-upgrade-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestInstallRejects(t *testing.T) {
	cases := map[string]struct {
		sums   string
		verify func(string, string) error
		want   string
	}{
		"checksum":   {sums: strings.Repeat("a", 64) + "  " + mustAsset(t) + "\n", verify: okVerify, want: "checksum mismatch"},
		"unlisted":   {sums: strings.Repeat("a", 64) + "  other\n", verify: okVerify, want: "not listed"},
		"broken run": {verify: func(string, string) error { return errors.New("exec format error") }, want: "does not run"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := release(t, "new binary", c.sums)
			exe := fakeExe(t)
			err := Install(context.Background(), Options{Exe: exe, Version: "v0.2.0", BaseURL: srv.URL, Verify: c.verify})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if b, _ := os.ReadFile(exe); string(b) != "old" {
				t.Errorf("exe replaced: %q", b)
			}
		})
	}
	if err := Install(context.Background(), Options{Exe: fakeExe(t), Version: "v0.2.0-0.20261028072244-1915962822f9"}); err == nil {
		t.Error("installed a pseudo-version")
	}
}

func mustAsset(t *testing.T) string {
	a, err := Asset(runtime.GOOS, runtime.GOARCH, FullBuild)
	if err != nil {
		t.Skip(err)
	}
	return a
}

func TestAuto(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	exe := fakeExe(t)

	// Not newer, or not releases: nothing happens.
	for _, c := range [][2]string{{"v0.2.0", "v0.2.0"}, {"v0.2.0", "v0.1.9"}, {"devel-abc", "v0.2.0"}, {"v0.1.0", "devel-abc"}} {
		if err := Auto(ctx, dir, c[0], c[1], Options{Exe: exe, BaseURL: "http://127.0.0.1:1"}); err != nil {
			t.Errorf("%v: %v", c, err)
		}
	}
	if st := LoadState(dir); st != (State{}) {
		t.Fatalf("state written: %+v", st)
	}

	// A failure is recorded and not retried before RetryAfter.
	if err := Auto(ctx, dir, "v0.1.0", "v0.2.0", Options{Exe: exe, BaseURL: "http://127.0.0.1:1", Verify: okVerify}); err == nil {
		t.Fatal("unreachable download succeeded")
	}
	st := LoadState(dir)
	if st.Target != "v0.2.0" || st.Error == "" {
		t.Fatalf("failure not recorded: %+v", st)
	}
	srv := release(t, "new binary", "")
	if err := Auto(ctx, dir, "v0.1.0", "v0.2.0", Options{Exe: exe, BaseURL: srv.URL, Verify: okVerify}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "old" {
		t.Fatal("retried before RetryAfter")
	}

	// After RetryAfter it tries again and succeeds.
	st.AttemptedAt = time.Now().Add(-RetryAfter - time.Minute)
	SaveState(dir, st)
	if err := Auto(ctx, dir, "v0.1.0", "v0.2.0", Options{Exe: exe, BaseURL: srv.URL, Verify: okVerify}); err != nil {
		t.Fatal(err)
	}
	st = LoadState(dir)
	if st.Error != "" || st.Target != "" || st.UpgradedFrom != "v0.1.0" || st.UpgradedTo != "v0.2.0" {
		t.Errorf("state after upgrade: %+v", st)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Errorf("exe = %q", b)
	}

	// A failure is cleared once the client runs that version (upgraded by hand).
	SaveState(dir, State{Target: "v0.3.0", Error: "boom", AttemptedAt: time.Now()})
	Auto(ctx, dir, "v0.3.0", "v0.3.0", Options{Exe: exe})
	if st := LoadState(dir); st.Error != "" || st.Target != "" {
		t.Errorf("failure not cleared: %+v", st)
	}
}

func TestAsset(t *testing.T) {
	for _, c := range []struct {
		os, arch string
		full     bool
		want     string
	}{
		{"linux", "amd64", false, "memspace-linux-amd64"},
		{"windows", "arm64", false, "memspace-windows-amd64.exe"},
		{"darwin", "arm64", true, "memspace-server-darwin-arm64"},
		{"windows", "amd64", true, ""},
	} {
		got, err := Asset(c.os, c.arch, c.full)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("Asset(%s, %s, %v) = %q, %v", c.os, c.arch, c.full, got, err)
		}
	}
}
