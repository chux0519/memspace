package syncer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Credentials identify this device to the server. Written by
// `memspace client connect`, mode 0600.
type Credentials struct {
	ServerURL   string    `json:"server_url"`
	DeviceID    string    `json:"device_id"`
	DeviceName  string    `json:"device_name"`
	Token       string    `json:"token"`
	ConnectedAt time.Time `json:"connected_at"`
	// MCPToken is the device's read-only token for the MCP endpoint, handed
	// to agents at runtime (never written into their configuration).
	MCPToken string `json:"mcp_token,omitempty"`
}

// ErrNotConnected means `memspace client connect` has not been run.
var ErrNotConnected = errors.New("device not connected (run 'memspace client connect <server-url>')")

// LoadCredentials reads the credentials file.
func LoadCredentials(path string) (*Credentials, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Token == "" || c.ServerURL == "" {
		return nil, ErrNotConnected
	}
	return &c, nil
}

// SaveCredentials writes the credentials file with owner-only permissions.
func SaveCredentials(path string, c *Credentials) error {
	return writeJSONAtomic(path, c, 0o600)
}

func writeJSONAtomic(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp." + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// State is the persisted outcome of recent sync attempts, used for
// exponential backoff and for `memspace client status`.
type State struct {
	ConsecutiveFailures int       `json:"consecutive_failures"`
	NextAttemptAt       time.Time `json:"next_attempt_at"`
	LastAttemptAt       time.Time `json:"last_attempt_at"`
	LastSuccessAt       time.Time `json:"last_success_at"`
	LastError           string    `json:"last_error,omitempty"`
	TotalUploaded       int64     `json:"total_uploaded"`
	TotalDuplicates     int64     `json:"total_duplicates"`
	TotalRejected       int64     `json:"total_rejected"`
}

// LoadState reads the sync state; a missing file is a zero state.
func LoadState(stateDir string) State {
	var s State
	readJSON(filepath.Join(stateDir, "sync.json"), &s)
	return s
}

func saveState(stateDir string, s State) error {
	return writeJSONAtomic(filepath.Join(stateDir, "sync.json"), s, 0o600)
}

// Offline reports whether the last sync attempt failed.
func (s State) Offline() bool { return s.ConsecutiveFailures > 0 }

// backoff returns the delay before the next attempt after n consecutive
// failures: base·2^(n-1), capped at max, with ±20% jitter so many devices
// do not retry in lockstep.
func backoff(n int, base, max time.Duration) time.Duration {
	if n <= 0 {
		return 0
	}
	d := base
	for i := 1; i < n && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	jitter := 0.8 + 0.4*rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// resolveCache maps local identities to server IDs so steady-state uploads
// need one request per batch. Only touched while holding the sync lock.
type resolveCache struct {
	Projects map[string]string        `json:"projects"` // hint key → project_id
	Sessions map[string]resolvedEntry `json:"sessions"` // agent|native id → ids
}

type resolvedEntry struct {
	SessionID string    `json:"session_id"`
	ProjectID string    `json:"project_id"`
	UsedAt    time.Time `json:"used_at"`
}

const cacheTTL = 30 * 24 * time.Hour

func loadCache(stateDir, serverURL string) *resolveCache {
	c := &resolveCache{}
	readJSON(cachePath(stateDir, serverURL), c)
	if c.Projects == nil {
		c.Projects = map[string]string{}
	}
	if c.Sessions == nil {
		c.Sessions = map[string]resolvedEntry{}
	}
	return c
}

// The cache is keyed by server so reconnecting to a different server never
// reuses foreign IDs.
func cachePath(stateDir, serverURL string) string {
	safe := strings.NewReplacer("://", "_", "/", "_", ":", "_").Replace(serverURL)
	return filepath.Join(stateDir, "resolve-"+safe+".json")
}

func (c *resolveCache) save(stateDir, serverURL string) error {
	for k, e := range c.Sessions {
		if time.Since(e.UsedAt) > cacheTTL {
			delete(c.Sessions, k)
		}
	}
	return writeJSONAtomic(cachePath(stateDir, serverURL), c, 0o600)
}

// lock is a cross-platform exclusive lock file. A holder refreshes its mtime
// while working; a lock not refreshed for lockStale is considered abandoned.
type lock struct{ path string }

const lockStale = 2 * time.Minute

// ErrLocked means another sync is already running.
var ErrLocked = errors.New("another sync is running")

func acquireLock(stateDir string) (*lock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(stateDir, "sync.lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return &lock{path: p}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if !lockHeld(stateDir) {
			os.Remove(p) // abandoned by a crashed sync
			continue
		}
		return nil, ErrLocked
	}
	return nil, ErrLocked
}

// lockHeld reports whether a live sync holds the lock.
func lockHeld(stateDir string) bool {
	fi, err := os.Stat(filepath.Join(stateDir, "sync.lock"))
	return err == nil && time.Since(fi.ModTime()) < lockStale
}

func (l *lock) refresh() {
	now := time.Now()
	os.Chtimes(l.path, now, now)
}

func (l *lock) release() { os.Remove(l.path) }
