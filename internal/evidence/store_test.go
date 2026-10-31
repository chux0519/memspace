package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	memdb "github.com/chux0519/memspace/internal/db"
)

func setup(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	d, e := memdb.Open(filepath.Join(t.TempDir(), "fixture.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	for _, q := range []string{`INSERT INTO users VALUES ('owner','now')`, `INSERT INTO devices(device_id,user_id,name,platform,token_hash,created_at) VALUES ('device','owner','test','test','hash','now')`, `INSERT INTO projects(project_id,user_id,name,created_at,updated_at) VALUES ('project','owner','test','now','now')`, `INSERT INTO projects(project_id,user_id,name,created_at,updated_at) VALUES ('other','owner','other','now','now')`, `INSERT INTO sessions(session_id,project_id,device_id,agent_type,native_session_id,started_at) VALUES ('session','project','device','codex','native','now')`} {
		if _, e = d.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	return WithPolicy(context.Background(), Policy{Mode: "shadow"}), d
}
func insert(t *testing.T, ctx context.Context, d *sql.DB, id, typ string, payload map[string]any) {
	t.Helper()
	b, _ := json.Marshal(payload)
	tx, e := d.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	_, e = tx.Exec(`INSERT OR IGNORE INTO events(event_id,session_id,project_id,device_id,event_type,occurred_at,payload_json) VALUES (?,'session','project','device',?,'2025-01-01T00:00:00Z',?)`, id, typ, string(b))
	if e != nil {
		t.Fatal(e)
	}
	if e = IndexEvent(ctx, tx, id); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}
func count(t *testing.T, d *sql.DB, q string) int {
	t.Helper()
	var n int
	if e := d.QueryRow(q).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func TestNativePairOutOfOrderAndAmbiguity(t *testing.T) {
	ctx, d := setup(t)
	insert(t, ctx, d, "reply", "agent.turn_completed", map[string]any{"last_assistant_message": "set to8", "turn_id": "turn"})
	if count(t, d, `SELECT count(*) FROM evidence_links`) != 0 {
		t.Fatal("invented parent")
	}
	insert(t, ctx, d, "prompt", "user.prompt", map[string]any{"prompt": "staging only", "turn_id": "turn"})
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active'`) != 1 {
		t.Fatal("missing native link")
	}
	insert(t, ctx, d, "prompt", "user.prompt", map[string]any{"prompt": "staging only", "turn_id": "turn"})
	if count(t, d, `SELECT count(*) FROM evidence_links`) != 1 {
		t.Fatal("not idempotent")
	}
	insert(t, ctx, d, "another", "user.prompt", map[string]any{"prompt": "different ambiguous question", "turn_id": "turn"})
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active'`) != 0 {
		t.Fatal("ambiguous parent still active")
	}
}
func TestNoTimestampGuessAndCompleteOrderedImport(t *testing.T) {
	ctx, d := setup(t)
	insert(t, ctx, d, "q", "user.prompt", map[string]any{"prompt": "one"})
	insert(t, ctx, d, "a", "agent.turn_completed", map[string]any{"last_assistant_message": "two"})
	if count(t, d, `SELECT count(*) FROM evidence_links`) != 0 {
		t.Fatal("timestamp guessed")
	}
	origin := func(i int) map[string]any {
		return map[string]any{"source_kind": "ordered_import", "batch_key": "b", "source_ordinal": i, "source_count": 2, "source_complete": true}
	}
	insert(t, ctx, d, "a2", "agent.turn_completed", map[string]any{"last_assistant_message": "two", "origin": origin(1)})
	if count(t, d, `SELECT count(*) FROM evidence_links`) != 0 {
		t.Fatal("partial batch paired")
	}
	insert(t, ctx, d, "q2", "user.prompt", map[string]any{"prompt": "one", "origin": origin(0)})
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE basis='ordered_source' AND status='active'`) != 1 {
		t.Fatal("missing ordered source pairing")
	}
}
func TestDerivedVersionsAndScopedText(t *testing.T) {
	ctx, d := setup(t)
	insert(t, ctx, d, "source", "user.prompt", map[string]any{"prompt": "中文\r\n原文"})
	tx, _ := d.BeginTx(ctx, nil)
	m := Derived{Kind: "memory_version", Ref: "memory", Project: "project", Content: "fact1", Sources: []SourceRef{{"event", "source"}}, At: time.Now()}
	if e := RecordDerived(ctx, tx, m); e != nil {
		t.Fatal(e)
	}
	m.Content = "fact2"
	if e := RecordDerived(ctx, tx, m); e != nil {
		t.Fatal(e)
	}
	tx.Commit()
	if count(t, d, `SELECT count(*) FROM evidence_objects WHERE kind='memory_version'`) != 2 {
		t.Fatal("old version overwritten")
	}
	var id string
	d.QueryRow(`SELECT object_id FROM evidence_objects WHERE kind='event'`).Scan(&id)
	o, e := GetObject(ctx, d, "owner", "project", id)
	if e != nil {
		t.Fatal(e)
	}
	text, e := Text(ctx, d, "owner", o)
	if e != nil || text != "中文\n原文" {
		t.Fatal(text, e)
	}
	if _, e = GetObject(ctx, d, "stranger", "project", id); e == nil {
		t.Fatal("owner leaked")
	}
	links, e := Related(ctx, d, "owner", "other", []string{id}, 24)
	if e != nil || len(links) != 0 {
		t.Fatal("project leaked")
	}
	if _, e = d.Exec(`UPDATE events SET payload_json='{"prompt":"modified"}' WHERE event_id='source'`); e != nil {
		t.Fatal(e)
	}
	if _, e = Text(ctx, d, "owner", o); e == nil {
		t.Fatal("stale source accepted")
	}
}
func TestOffDoesNotWriteEvidence(t *testing.T) {
	_, d := setup(t)
	insert(t, context.Background(), d, "q", "user.prompt", map[string]any{"prompt": "one", "turn_id": "t"})
	if count(t, d, `SELECT count(*) FROM evidence_objects`) != 0 {
		t.Fatal("off wrote evidence")
	}
}

func TestDerivedRollbackAndGenericSource(t *testing.T) {
	ctx, d := setup(t)
	insert(t, ctx, d, "tool", "tool.failed", map[string]any{"error": "utf8 中文"})
	tx, _ := d.BeginTx(ctx, nil)
	m := Derived{Kind: "memory_version", Ref: "memory", Project: "project", Content: "first", Sources: []SourceRef{{Kind: "event", ID: "tool"}}}
	for _, body := range []string{"first", "second", "first", "first"} {
		m.Content = body
		if e := RecordDerived(ctx, tx, m); e != nil {
			t.Fatal(e)
		}
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if got := count(t, d, `SELECT count(*) FROM evidence_objects WHERE kind='memory_version'`); got != 2 {
		t.Fatal(got)
	}
	if got := count(t, d, `SELECT count(*) FROM evidence_records WHERE kind='current_version_selected'`); got != 3 {
		t.Fatal(got)
	}
	if got := count(t, d, `SELECT count(*) FROM evidence_links WHERE relation='derived_from'`); got != 2 {
		t.Fatal(got)
	}
	var id string
	if e := d.QueryRow(`SELECT object_id FROM evidence_objects WHERE kind='memory_version' AND is_current=1`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	o, e := GetObject(ctx, d, "owner", "project", id)
	if e != nil {
		t.Fatal(e)
	}
	body, e := Text(ctx, d, "owner", o)
	if e != nil || body != "first" {
		t.Fatal(body, e)
	}
	if _, e = d.Exec(`INSERT INTO memories(memory_id,project_id,dedup_key,type,title,content,source_refs,confidence,generator,created_at,updated_at) VALUES ('memory','project','dedup','decision','','first','[{"kind":"event","id":"tool"}]',1,'test','now','now')`); e != nil {
		t.Fatal(e)
	}
	seeds := []Seed{{Kind: "memory", Ref: "memory"}}
	current, e := CurrentObjects(ctx, d, "owner", "project", seeds)
	if e != nil || len(current) != 1 {
		t.Fatal(current, e)
	}
	if _, e = d.Exec(`UPDATE memories SET content='changed while capture off' WHERE memory_id='memory'`); e != nil {
		t.Fatal(e)
	}
	current, e = CurrentObjects(ctx, d, "owner", "project", seeds)
	if e != nil || len(current) != 0 {
		t.Fatal(current, e)
	}
}
