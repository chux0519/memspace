package search

import (
	"github.com/chux0519/memspace/internal/db"
	"path/filepath"
	"testing"
)

func TestPorterCanWrapMixedLanguageTokenizer(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "porter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{`CREATE VIRTUAL TABLE porter_probe USING fts5(body, tokenize='porter simple')`, `INSERT INTO porter_probe(body) VALUES ('Dependencies were tested; 数据库连接池修复完成。')`} {
		if _, err = d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"dependency test", "数据库"} {
		var expr string
		if err = d.QueryRow(`SELECT jieba_query(?)`, query).Scan(&expr); err != nil {
			t.Fatal(err)
		}
		var n int
		if err = d.QueryRow(`SELECT count(*) FROM porter_probe WHERE porter_probe MATCH ?`, expr).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("porter query %q (%s) missed inflection/CJK", query, expr)
		}
	}
}
