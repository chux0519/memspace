//go:build cgo && sqlite_fts5

package simple

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
)

func init() {
	sql.Register("simple_test_sqlite3", &sqlite3.SQLiteDriver{})
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	if err := Init(filepath.Join(os.TempDir(), "memspace-simple-test")); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("simple_test_sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE VIRTUAL TABLE t USING fts5(body, tokenize = 'simple')`); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{
		"修复了认证模块的超时问题",
		"Stripe webhook 幂等处理已实现，用 event_id 去重",
		"等待处理的幂次运算",
		"重构数据库迁移脚本",
		"灰度发布时熔断降级没生效",
		"去年重新部署了服务",
	} {
		if _, err := db.Exec(`INSERT INTO t VALUES (?)`, d); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func search(t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT simple_highlight(t, 0, '[', ']') FROM t WHERE body MATCH jieba_query(?) ORDER BY rank`, q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestChineseSearch(t *testing.T) {
	db := openDB(t)
	cases := []struct {
		q    string
		want []string
	}{
		{"认证", []string{"修复了[认证]模块的超时问题"}},
		{"超时问题", []string{"修复了认证模块的[超时问题]"}},
		// Technical terms come from MemSpace's user dictionary: matched as
		// words, so the character-level false positives are gone.
		{"幂等", []string{"Stripe webhook [幂等]处理已实现，用 event_id 去重"}},
		{"去重", []string{"Stripe webhook 幂等处理已实现，用 event_id [去重]"}},
		{"熔断降级", []string{"灰度发布时[熔断降级]没生效"}},
		{"迁移", []string{"重构数据库[迁移]脚本"}},
		{"webhook", []string{"Stripe [webhook] 幂等处理已实现，用 event_id 去重"}},
		{"renzheng", []string{"修复了[认证]模块的超时问题"}}, // pinyin
	}
	for _, c := range cases {
		got := search(t, db, c.q)
		if len(got) != len(c.want) || (len(got) > 0 && got[0] != c.want[0]) {
			t.Errorf("%s: got %q, want %q", c.q, got, c.want)
		}
	}
}

func TestQuerySyntaxIsData(t *testing.T) {
	db := openDB(t)
	for _, q := range []string{`"`, `NEAR(a b)`, `a OR b`, `*`, `-x`, `认证" OR "超时`, `(`, `col:val`} {
		rows, err := db.Query(`SELECT count(*) FROM t WHERE body MATCH jieba_query(?)`, q)
		if err != nil {
			t.Errorf("query %q errored: %v", q, err)
			continue
		}
		rows.Close()
	}
}
