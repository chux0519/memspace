// Package upgrade replaces the running memspace binary with a release from
// GitHub. The server decides the target: a client upgrades to the server's
// version when the server is newer, so the two stay compatible. Failures are
// recorded so doctor, the web UI and the next session can tell the user.
package upgrade

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/chux0519/memspace/internal/model"
)

// RetryAfter is how long a failed automatic upgrade waits before the next
// attempt at the same version.
const RetryAfter = 6 * time.Hour

// maxAsset bounds a download.
const maxAsset = 256 << 20

// serverAssets lists the platforms with a published full (server) binary.
var serverAssets = map[string]bool{"linux-amd64": true, "linux-arm64": true, "darwin-arm64": true}

// Asset is the release file for a platform: the full binary for builds that
// include the server, else the client.
func Asset(goos, goarch string, full bool) (string, error) {
	if goos == "windows" {
		goarch = "amd64" // Windows on ARM runs the amd64 build
	}
	platform := goos + "-" + goarch
	if full {
		if !serverAssets[platform] {
			return "", fmt.Errorf("no server build is published for %s; build from source", platform)
		}
		return "memspace-server-" + platform, nil
	}
	ext := ""
	if goos == "windows" {
		ext = ".exe"
	}
	return "memspace-" + platform + ext, nil
}

// Newer reports whether target is a release newer than current, which must
// also be a release: development builds are never replaced.
func Newer(current, target string) bool {
	return model.IsRelease(current) && model.IsRelease(target) && semver.Compare(target, current) > 0
}

// Options configures Install.
type Options struct {
	Exe     string // binary to replace (default: the running executable)
	Version string // release tag to install
	// BaseURL holds the release's files (default: the GitHub release).
	BaseURL string
	HTTP    *http.Client
	// Verify checks a downloaded binary before it replaces Exe (default:
	// runs it with --version).
	Verify func(path, version string) error
}

// Executable is the running binary's path with symlinks resolved.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Install downloads opts.Version, checks it against the release's SHA256SUMS,
// runs it, and atomically replaces opts.Exe. The replaced binary is kept as
// Exe + ".prev". Running processes keep the old binary until they restart.
func Install(ctx context.Context, opts Options) error {
	if !model.IsRelease(opts.Version) {
		return fmt.Errorf("%q is not a release version", opts.Version)
	}
	exe := opts.Exe
	if exe == "" {
		var err error
		if exe, err = Executable(); err != nil {
			return err
		}
	}
	asset, err := Asset(runtime.GOOS, runtime.GOARCH, FullBuild)
	if err != nil {
		return err
	}
	base := opts.BaseURL
	if base == "" {
		base = "https://github.com/" + model.ReleaseRepo + "/releases/download/" + opts.Version
	}
	hc := opts.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	verify := opts.Verify
	if verify == nil {
		verify = runVersion
	}

	dir := filepath.Dir(exe)
	cleanup(exe)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe" // Windows runs only files with an executable extension
	}
	tmp, err := os.CreateTemp(dir, ".memspace-upgrade-*"+suffix)
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	sum, err := download(ctx, hc, base+"/"+asset, tmp)
	tmp.Close()
	if err != nil {
		return err
	}
	want, err := checksum(ctx, hc, base+"/SHA256SUMS", asset)
	if err != nil {
		return err
	}
	if sum != want {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s)", asset, sum, want)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	if err := verify(tmpPath, opts.Version); err != nil {
		return fmt.Errorf("downloaded binary does not run: %w", err)
	}
	return replace(exe, tmpPath)
}

func download(ctx context.Context, hc *http.Client, url string, w io.Writer) (string, error) {
	body, err := get(ctx, hc, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(body, maxAsset+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxAsset {
		return "", fmt.Errorf("download %s: larger than %d bytes", url, maxAsset)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checksum(ctx context.Context, hc *http.Client, url, asset string) (string, error) {
	body, err := get(ctx, hc, url)
	if err != nil {
		return "", err
	}
	defer body.Close()
	sc := bufio.NewScanner(io.LimitReader(body, 1<<20))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed in %s", asset, url)
}

func get(ctx context.Context, hc *http.Client, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Report the cause without the redirect target: GitHub redirects
		// to a signed URL of a thousand characters.
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	return resp.Body, nil
}

func runVersion(path, version string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return err
	}
	if got, want := strings.TrimSpace(string(out)), "memspace version "+version; got != want {
		return fmt.Errorf("it reports %q, want %q", got, want)
	}
	return nil
}

// replace moves next into exe's place. On Unix a rename over the running
// binary is atomic and safe. Windows cannot overwrite or delete a running
// executable but can rename it, so exe moves aside to .prev first.
func replace(exe, next string) error {
	prev := exe + ".prev"
	if runtime.GOOS != "windows" {
		os.Remove(prev)
		if err := os.Link(exe, prev); err != nil {
			copyFile(exe, prev) // best effort: the backup is a convenience
		}
		return os.Rename(next, exe)
	}
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		// An older binary still running from .prev: move it aside too;
		// cleanup removes it once it has exited.
		if err := os.Rename(prev, fmt.Sprintf("%s.%d.old", exe, time.Now().UnixNano())); err != nil {
			return fmt.Errorf("move aside %s: %w", prev, err)
		}
	}
	if err := os.Rename(exe, prev); err != nil {
		return fmt.Errorf("move aside %s: %w", exe, err)
	}
	if err := os.Rename(next, exe); err != nil {
		os.Rename(prev, exe)
		return err
	}
	return nil
}

// cleanup removes binaries moved aside by earlier Windows upgrades whose
// processes have exited, and temporary files of interrupted upgrades.
func cleanup(exe string) {
	dir := filepath.Dir(exe)
	old, _ := filepath.Glob(filepath.Join(dir, filepath.Base(exe)+".*.old"))
	for _, p := range old {
		os.Remove(p) // fails while that binary still runs
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, ".memspace-upgrade-*"))
	for _, p := range tmps {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(p)
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// State records automatic upgrades in the client state directory.
type State struct {
	// Target and Error describe the last failed attempt; both are cleared
	// once the client runs the target version or newer.
	Target      string    `json:"target,omitempty"`
	Error       string    `json:"error,omitempty"`
	AttemptedAt time.Time `json:"attempted_at,omitzero"`
	// From/To/At describe the last successful upgrade.
	UpgradedFrom string    `json:"upgraded_from,omitempty"`
	UpgradedTo   string    `json:"upgraded_to,omitempty"`
	UpgradedAt   time.Time `json:"upgraded_at,omitzero"`
}

const stateFile = "upgrade.json"

// LoadState reads the state; a missing or unreadable file is empty.
func LoadState(dir string) State {
	var st State
	if b, err := os.ReadFile(filepath.Join(dir, stateFile)); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

// SaveState writes the state atomically.
func SaveState(dir string, st State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp, err := os.CreateTemp(dir, ".upgrade-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), filepath.Join(dir, stateFile)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Settle clears a recorded failure once the client runs the failed target
// or a newer version (upgraded by hand, say). It reports whether st changed.
func (st *State) Settle(current string) bool {
	if st.Target == "" || !model.IsRelease(current) || semver.Compare(current, st.Target) < 0 {
		return false
	}
	st.Target, st.Error, st.AttemptedAt = "", "", time.Time{}
	return true
}

// Auto upgrades the client to the server's version when the server is newer,
// at most once per RetryAfter for a version that failed. It records the
// outcome in dir and returns the error of an attempt it made.
func Auto(ctx context.Context, dir, current, server string, opts Options) error {
	st := LoadState(dir)
	changed := st.Settle(current)
	if !Newer(current, server) {
		if changed {
			SaveState(dir, st)
		}
		return nil
	}
	if st.Target == server && st.Error != "" && time.Since(st.AttemptedAt) < RetryAfter {
		if changed {
			SaveState(dir, st)
		}
		return nil
	}
	unlock, err := lock(dir)
	if err != nil {
		return nil // another process is upgrading
	}
	defer unlock()

	opts.Version = server
	st.Target, st.AttemptedAt = server, time.Now().UTC()
	if err := Install(ctx, opts); err != nil {
		st.Error = err.Error()
		SaveState(dir, st)
		return err
	}
	st = State{UpgradedFrom: current, UpgradedTo: server, UpgradedAt: time.Now().UTC()}
	return SaveState(dir, st)
}

// lock keeps two processes from upgrading at once. A lock older than the
// download timeout is abandoned.
func lock(dir string) (func(), error) {
	p := filepath.Join(dir, "upgrade.lock")
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > 15*time.Minute {
			os.Remove(p)
			continue
		}
		return nil, err
	}
	return nil, errors.New("upgrade lock busy")
}
