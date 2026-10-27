// Package db manages the SQLite connection and lifecycle.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"

	"github.com/chux0519/memspace/internal/fts/simple"
	"github.com/chux0519/memspace/internal/vec"
)

// driverName registers mattn/go-sqlite3 under a name of our own so the
// tokenizer is guaranteed to be initialized before any connection opens.
const driverName = "memspace_sqlite3"

func init() {
	sql.Register(driverName, &sqlite3.SQLiteDriver{})
}

// Open opens (or creates) the SQLite database at path, configures WAL mode and
// sensible pragmas, then runs any pending migrations.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("db: mkdir %s: %w", filepath.Dir(path), err)
	}
	// Chinese full-text search: jieba dictionaries live next to the database.
	if err := simple.Init(filepath.Join(filepath.Dir(path), "dict")); err != nil {
		return nil, fmt.Errorf("db: init tokenizer: %w", err)
	}
	if err := vec.Init(); err != nil {
		return nil, fmt.Errorf("db: init vector search: %w", err)
	}

	// synchronous=FULL: a committed transaction survives power loss, so an
	// event the server has ACKed is never lost. Batching keeps the
	// extra fsync cost per request, not per event.
	dsn := path + "?_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000"
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", path, err)
	}

	// Single writer is sufficient for v0.1; keep the pool small to avoid
	// "database is locked" errors from concurrent goroutines.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	if err := checkFTS(db); err != nil {
		db.Close()
		return nil, err
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("db: migrate: %w", err)
	}

	return db, nil
}

// checkFTS fails fast when the binary lacks FTS5 or the tokenizer, instead of
// failing on the first search. It also loads the jieba dictionaries now
// rather than during the first query.
func checkFTS(db *sql.DB) error {
	var q string
	if err := db.QueryRow(`SELECT jieba_query('全文检索')`).Scan(&q); err != nil {
		return fmt.Errorf("db: Chinese full-text search unavailable (%w); build with: make build (go build -tags sqlite_fts5)", err)
	}
	if err := db.QueryRow(`SELECT vec_version()`).Scan(&q); err != nil {
		return fmt.Errorf("db: vector search unavailable: %w", err)
	}
	return nil
}
