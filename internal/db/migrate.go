// Package db — migration runner.
package db

import (
	"database/sql"
	_ "embed"
	"fmt"
)

//go:embed migrations/001_init.sql
var migration001 string

//go:embed migrations/002_identity_indexes.sql
var migration002 string

//go:embed migrations/003_auth_hardening.sql
var migration003 string

//go:embed migrations/004_search.sql
var migration004 string

//go:embed migrations/005_jobs_summaries.sql
var migration005 string

//go:embed migrations/006_mcp_tokens.sql
var migration006 string

//go:embed migrations/007_embeddings.sql
var migration007 string

//go:embed migrations/008_memories.sql
var migration008 string

//go:embed migrations/009_ui_sessions.sql
var migration009 string

//go:embed migrations/010_admin.sql
var migration010 string

//go:embed migrations/011_device_requests.sql
var migration011 string

//go:embed migrations/012_drop_empty_summaries.sql
var migration012 string

//go:embed migrations/013_device_upgrades.sql
var migration013 string

//go:embed migrations/014_english_stemming.sql
var migration014 string

// migrate applies all schema migrations that have not yet been executed.
// Uses a simple schema_migrations table to track applied migrations.
func migrate(db *sql.DB) error {
	// Bootstrap tracking table first.
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`)
	if err != nil {
		return fmt.Errorf("migrate: create tracking table: %w", err)
	}

	migrations := []struct {
		version string
		sql     string
	}{
		{"001_init", migration001},
		{"002_identity_indexes", migration002},
		{"003_auth_hardening", migration003},
		{"004_search", migration004},
		{"005_jobs_summaries", migration005},
		{"006_mcp_tokens", migration006},
		{"007_embeddings", migration007},
		{"008_memories", migration008},
		{"009_ui_sessions", migration009},
		{"010_admin", migration010},
		{"011_device_requests", migration011},
		{"012_drop_empty_summaries", migration012},
		{"013_device_upgrades", migration013},
		{"014_english_stemming", migration014},
	}

	for _, m := range migrations {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&exists); err != nil {
			return fmt.Errorf("migrate: check %s: %w", m.version, err)
		}
		if exists > 0 {
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migrate: begin %s: %w", m.version, err)
		}

		if _, err := tx.Exec(m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: exec %s: %w", m.version, err)
		}

		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, m.version); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate: record %s: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate: commit %s: %w", m.version, err)
		}
	}

	return nil
}
