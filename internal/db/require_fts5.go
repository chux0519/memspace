//go:build cgo && !sqlite_fts5

package db

// mattn/go-sqlite3 compiles FTS5 only with the sqlite_fts5 build tag, and
// MemSpace's search depends on it. This deliberately fails to compile so the
// mistake is caught at build time rather than at server start:
//
//	make build / make test, or go build -tags sqlite_fts5 ./...
var _ = build_with_tag_sqlite_fts5__see_Makefile
