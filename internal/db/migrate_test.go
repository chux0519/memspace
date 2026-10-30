package db

import (
	"path/filepath"
	"testing"
)

func TestEnglishStemmingMigrationPreservesSourceIDsAndCJK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(user_id) VALUES ('u')`,
		`INSERT INTO projects(project_id,user_id,name) VALUES ('p','u','p')`,
		`INSERT INTO search_docs(doc_id,kind,ref_id,project_id,occurred_at,body) VALUES (42,'reply','source','p','2026-01-01','Dependencies were tested 数据库连接池')`,
		`DROP TABLE search_fts`,
		`CREATE VIRTUAL TABLE search_fts USING fts5(title,body,files,content='search_docs',content_rowid='doc_id',tokenize='simple')`,
		`INSERT INTO search_fts(search_fts) VALUES ('rebuild')`,
		`DELETE FROM schema_migrations WHERE version='014_english_stemming'`,
	} {
		if _, err = d.Exec(q); err != nil {
			d.Close()
			t.Fatal(err)
		}
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var id int
	var body string
	if err = d.QueryRow(`SELECT doc_id,body FROM search_docs WHERE ref_id='source'`).Scan(&id, &body); err != nil || id != 42 || body != "Dependencies were tested 数据库连接池" {
		t.Fatalf("source changed: %d %q %v", id, body, err)
	}
	for _, query := range []string{"dependency test", "数据库"} {
		var n int
		if err = d.QueryRow(`SELECT count(*) FROM search_fts WHERE search_fts MATCH jieba_query(?)`, query).Scan(&n); err != nil || n != 1 {
			t.Fatalf("rebuilt index %q: %d %v", query, n, err)
		}
	}
}
