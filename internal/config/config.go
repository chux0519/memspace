// Package config loads MemSpace configuration from TOML and environment variables.
package config

import (
	"fmt"
	"github.com/chux0519/memspace/internal/admission"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration structure.
type Config struct {
	Server    ServerConfig     `toml:"server"`
	Database  DatabaseConfig   `toml:"database"`
	Auth      AuthConfig       `toml:"auth"`
	Client    ClientConfig     `toml:"client"`
	Embedding EmbeddingConfig  `toml:"embedding"`
	Context   admission.Policy `toml:"context"`
}

// EmbeddingConfig enables semantic search through an OpenAI-compatible
// embeddings service (e.g. embeddings.cpp's embeddings_server). The server
// calls it, so the URL must be reachable from the server.
type EmbeddingConfig struct {
	// URL of the embeddings endpoint, e.g. http://127.0.0.1:8090/v1/embeddings.
	// Empty disables semantic search.
	URL   string `toml:"url"`
	Model string `toml:"model"`
	// APIKeyEnv names an environment variable holding a bearer key.
	APIKeyEnv string `toml:"api_key_env"`
	// Dimensions requests Matryoshka-truncated vectors (0 = native).
	Dimensions int `toml:"dimensions"`
	// QueryPrefix is prepended to queries ("" = automatic: "query: " for
	// Snowflake arctic-embed models).
	QueryPrefix  string   `toml:"query_prefix"`
	BatchSize    int      `toml:"batch_size"`
	QueryTimeout Duration `toml:"query_timeout"`
	// MinSimilarity is the cosine similarity a meaning-only match needs to
	// be injected into an agent's context; SearchMinSimilarity is the floor
	// below which semantic hits are dropped from search. 0 = the model's
	// historical default (bge-m3 0.60 / 0.45, arctic-embed 0.35 / 0.20).
	MinSimilarity       float64 `toml:"min_similarity"`
	SearchMinSimilarity float64 `toml:"search_min_similarity"`
}

type ServerConfig struct {
	// Address is the listen address, e.g. "127.0.0.1:8765".
	// Default binds to localhost only (security: remote access must be explicit).
	Address string `toml:"address"`
	// PublicURL is how browsers reach the server (e.g. behind a reverse
	// proxy); used for login links printed by `memspace server login-link`.
	// Default: http://<address>.
	PublicURL string `toml:"public_url"`
	// DownloadsDir holds client binaries (memspace-<os>-<arch>[.exe]) served
	// at /downloads/ for setting up new devices. Empty: no downloads.
	DownloadsDir string `toml:"downloads_dir"`
	// UpdateCheck asks GitHub once a day for the latest release, so the web
	// UI can show that a newer server is available: "on" (default) or "off".
	UpdateCheck string `toml:"update_check"`
}

type DatabaseConfig struct {
	// Path is the SQLite database file path.
	Path string `toml:"path"`
}

type AuthConfig struct {
	// OwnerUserID is the single owner for v0.1.
	OwnerUserID string `toml:"owner_user_id"`
}

// ClientConfig controls the per-device client: credentials, spool and sync.
//
// All client state lives under Home, which must be an absolute path: hooks
// run with the agent's project directory as cwd, so relative paths would
// scatter state across repositories.
type ClientConfig struct {
	// Home is the client state directory (default $MEMSPACE_HOME or ~/.memspace).
	Home string `toml:"home"`
	// ServerURL overrides the server URL saved by `memspace client connect`.
	ServerURL string `toml:"server_url"`
	// SpoolMaxEvents caps the number of unsent events kept locally.
	SpoolMaxEvents int `toml:"spool_max_events"`
	// SpoolMaxBytes caps the total size of unsent events kept locally.
	SpoolMaxBytes int64 `toml:"spool_max_bytes"`
	// BatchSize is the maximum number of events per upload request.
	BatchSize int `toml:"batch_size"`
	// BackoffBase and BackoffMax bound the exponential retry delay after a
	// failed sync.
	BackoffBase Duration `toml:"backoff_base"`
	BackoffMax  Duration `toml:"backoff_max"`
	// Inject controls context injection into agent sessions: "on"
	// (default) or "off".
	Inject string `toml:"inject"`
	// AutoUpgrade controls whether the client replaces itself with the
	// server's version when the server is newer: "on" (default) or "off"
	// (doctor and the web UI still show that it is behind).
	AutoUpgrade string `toml:"auto_upgrade"`
}

// Duration is a time.Duration that decodes from a TOML string like "30s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Paths derived from ClientConfig.Home.
func (c ClientConfig) SpoolDir() string        { return filepath.Join(c.Home, "spool") }
func (c ClientConfig) StateDir() string        { return filepath.Join(c.Home, "state") }
func (c ClientConfig) LogDir() string          { return filepath.Join(c.Home, "logs") }
func (c ClientConfig) CredentialsPath() string { return filepath.Join(c.Home, "credentials.json") }

// DefaultHome returns $MEMSPACE_HOME, or ~/.memspace.
func DefaultHome() string {
	if v := os.Getenv("MEMSPACE_HOME"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".memspace"
	}
	return filepath.Join(home, ".memspace")
}

// Default returns a configuration with sensible defaults.
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Address: "127.0.0.1:8765",
		},
		Database: DatabaseConfig{
			Path: "./data/memspace.db",
		},
		Auth: AuthConfig{
			OwnerUserID: "owner",
		},
		Client: ClientConfig{
			Home:           DefaultHome(),
			SpoolMaxEvents: 50000,
			SpoolMaxBytes:  256 << 20,
			BatchSize:      200,
			BackoffBase:    Duration{5 * time.Second},
			BackoffMax:     Duration{10 * time.Minute},
		},
	}
}

// ResolvePath picks the config file: the explicit path, then $MEMSPACE_CONFIG,
// then <home>/config.toml if it exists. Returns "" when there is none.
func ResolvePath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("MEMSPACE_CONFIG"); v != "" {
		return v
	}
	p := filepath.Join(DefaultHome(), "config.toml")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return ""
}

// Load reads configuration from a TOML file at path (see ResolvePath).
// If path is empty or the file does not exist, defaults are returned.
// Environment variables override file values:
//
//	MEMSPACE_ADDRESS        → server.address
//	MEMSPACE_DB_PATH        → database.path
//	MEMSPACE_OWNER_USER_ID  → auth.owner_user_id
//	MEMSPACE_HOME           → client.home
//	MEMSPACE_SERVER_URL     → client.server_url
//	MEMSPACE_BACKOFF_BASE   → client.backoff_base
//	MEMSPACE_INJECT         → client.inject
//	MEMSPACE_AUTO_UPGRADE   → client.auto_upgrade
//	MEMSPACE_UPDATE_CHECK   → server.update_check
//	MEMSPACE_EMBEDDING_URL  → embedding.url
//	MEMSPACE_EMBEDDING_MODEL → embedding.model
func Load(path string) (*Config, error) {
	cfg := Default()

	path = ResolvePath(path)
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			meta, err := toml.DecodeFile(path, cfg)
			if err != nil {
				return nil, fmt.Errorf("config: parse %s: %w", path, err)
			}
			for _, key := range meta.Undecoded() {
				if len(key) > 0 && key[0] == "context" {
					return nil, fmt.Errorf("config: unknown context setting %s", key.String())
				}
			}
		}
	}

	// Environment overrides.
	if v := os.Getenv("MEMSPACE_ADDRESS"); v != "" {
		cfg.Server.Address = v
	}
	if v := os.Getenv("MEMSPACE_DB_PATH"); v != "" {
		cfg.Database.Path = v
	}
	if v := os.Getenv("MEMSPACE_OWNER_USER_ID"); v != "" {
		cfg.Auth.OwnerUserID = v
	}
	if v := os.Getenv("MEMSPACE_HOME"); v != "" {
		cfg.Client.Home = v
	}
	if v := os.Getenv("MEMSPACE_SERVER_URL"); v != "" {
		cfg.Client.ServerURL = v
	}
	if v := os.Getenv("MEMSPACE_EMBEDDING_URL"); v != "" {
		cfg.Embedding.URL = v
	}
	if v := os.Getenv("MEMSPACE_EMBEDDING_MODEL"); v != "" {
		cfg.Embedding.Model = v
	}
	if v := os.Getenv("MEMSPACE_INJECT"); v != "" {
		cfg.Client.Inject = v
	}
	if v := os.Getenv("MEMSPACE_AUTO_UPGRADE"); v != "" {
		cfg.Client.AutoUpgrade = v
	}
	if v := os.Getenv("MEMSPACE_UPDATE_CHECK"); v != "" {
		cfg.Server.UpdateCheck = v
	}
	if v := os.Getenv("MEMSPACE_BACKOFF_BASE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("config: MEMSPACE_BACKOFF_BASE: %w", err)
		}
		cfg.Client.BackoffBase = Duration{d}
	}

	home, err := filepath.Abs(cfg.Client.Home)
	if err != nil {
		return nil, fmt.Errorf("config: client.home: %w", err)
	}
	cfg.Client.Home = home
	if err := cfg.Context.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return cfg, nil
}
