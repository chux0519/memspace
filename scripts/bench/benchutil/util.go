// Package benchutil contains measurement helpers, never product behavior.
package benchutil

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

func JSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}
func NewDir(path string) error {
	if path == "" {
		return fmt.Errorf("output directory required")
	}
	if _, e := os.Stat(path); !os.IsNotExist(e) {
		return fmt.Errorf("output must not exist: %s", path)
	}
	return os.MkdirAll(path, 0700)
}
func SHA(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func FileSHA(path string) string {
	b, e := os.ReadFile(path)
	if e != nil {
		return "unavailable"
	}
	return SHA(b)
}
func Dist(v []float64) map[string]any {
	out := map[string]any{"n": len(v)}
	if len(v) == 0 {
		return out
	}
	v = append([]float64{}, v...)
	sort.Float64s(v)
	for _, p := range []int{50, 95, 99, 100} {
		x := float64(len(v)-1) * float64(p) / 100
		i := int(x)
		val := v[i]
		if i+1 < len(v) {
			val += (v[i+1] - v[i]) * (x - float64(i))
		}
		key := fmt.Sprintf("p%d", p)
		if p == 100 {
			key = "max"
		}
		out[key] = val
	}
	sum := 0.
	for _, x := range v {
		sum += x
	}
	out["mean"] = sum / float64(len(v))
	return out
}
func Counts(ctx context.Context, d *sql.DB) map[string]int64 {
	m := map[string]int64{}
	for _, t := range []string{"events", "sessions", "projects", "search_docs", "embedding_chunks"} {
		var n int64
		if d.QueryRowContext(ctx, "SELECT count(*) FROM "+t).Scan(&n) == nil {
			m[t] = n
		}
	}
	return m
}
func Manifest() map[string]any {
	cmd := func(args ...string) string {
		b, e := exec.Command("git", args...).Output()
		if e != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(b))
	}
	exe, _ := os.Executable()
	load, _ := os.ReadFile("/proc/loadavg")
	cpu, _ := os.ReadFile("/proc/cpuinfo")
	model := ""
	for _, l := range strings.Split(string(cpu), "\n") {
		if strings.HasPrefix(l, "model name") {
			model = strings.TrimSpace(strings.SplitN(l, ":", 2)[1])
			break
		}
	}
	return map[string]any{"at": time.Now().UTC(), "git_revision": cmd("rev-parse", "HEAD"), "git_status": cmd("status", "--short"), "binary_sha256": FileSHA(exe), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpu_count": runtime.NumCPU(), "cpu_model": model, "loadavg": strings.TrimSpace(string(load))}
}
func RequireCopy(path string) error {
	a, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	home, _ := os.UserHomeDir()
	prod := filepath.Join(home, ".local/share/memspace/memspace.db")
	real, _ := filepath.EvalSymlinks(a)
	pr, _ := filepath.EvalSymlinks(prod)
	pinfo, _ := os.Stat(prod)
	ainfo, _ := os.Stat(a)
	if a == prod || real != "" && real == pr || pinfo != nil && ainfo != nil && os.SameFile(pinfo, ainfo) {
		return fmt.Errorf("refusing production database; make sqlite3 .backup first")
	}
	if _, e = os.Stat(a); e != nil {
		return e
	}
	return nil
}
