package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/summary"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	// macOS commonly reports /var/folders while its physical path starts
	// with /private/var. Fixtures must satisfy the worker's canonical-path
	// contract, just like an evaluation root supplied by a real caller.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func testEngine(t *testing.T) *engine {
	t.Helper()
	en, e := open(filepath.Join(canonicalTempDir(t), "user"), strings.Repeat("a", 64), "", "BAAI/bge-m3", "context_pack")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { en.d.Close(); search.SetSemantic(nil) })
	return en
}
func TestProductionIngestReplayAndConflictingRetry(t *testing.T) {
	en := testEngine(t)
	ctx := context.Background()
	r := request{Session: "session", Batch: "batch", Messages: []message{{Role: "user", Content: "supermarket staging pool change", ChatTime: "2025-01-01T00:00:00Z"}, {Role: "assistant", Content: "supermarket staging pool was set to 8, production stays 4", ChatTime: "2025-01-01T00:00:01Z"}}}
	if _, e := en.add(ctx, r); e != nil {
		t.Fatal(e)
	}
	if _, e := en.add(ctx, r); e != nil {
		t.Fatal(e)
	}
	var n int
	en.d.QueryRow(`SELECT count(*) FROM events`).Scan(&n)
	if n != 2 {
		t.Fatal(n)
	}
	result, e := en.query(ctx, request{Query: "supermarket staging pool", TopK: 20})
	if e != nil {
		t.Fatal(e)
	}
	got := result.(map[string]any)["pack"].(*contextpack.Pack)
	want, e := contextpack.Build(ctx, en.d, owner, device, contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: en.st.Project}, Query: "supermarket staging pool", AgentType: "benchmark", NativeSessionID: "omni-read-only-query"})
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("actual pack differs: %v", e)
	}
	if !strings.Contains(got.Text, "production stays 4") {
		t.Fatal(got.Text)
	}
	r.Messages[1].Content = "different content"
	if _, e = en.add(ctx, r); e == nil {
		t.Fatal("conflicting retry accepted")
	}
	en.d.QueryRow(`SELECT count(*) FROM events`).Scan(&n)
	if n != 2 {
		t.Fatal("query or conflict wrote events")
	}
}
func TestValidationBeforeWritingAndOwnership(t *testing.T) {
	en := testEngine(t)
	r := request{Session: "s", Batch: "b", Messages: []message{{Role: "user", Content: "valid", ChatTime: "2025-01-01T00:00:00Z"}, {Role: "assistant", Content: "invalid timestamp", ChatTime: ""}}}
	if _, e := en.add(context.Background(), r); e == nil {
		t.Fatal("invalid accepted")
	}
	var n int
	en.d.QueryRow(`SELECT count(*) FROM events`).Scan(&n)
	if n != 0 {
		t.Fatal("partial invalid request")
	}
	dir := canonicalTempDir(t)
	os.WriteFile(filepath.Join(dir, "existing.db"), []byte("untouched"), 0600)
	if _, e := open(dir, strings.Repeat("a", 64), "", "BAAI/bge-m3", "context_pack"); e == nil {
		t.Fatal("unowned directory opened")
	}
}

func TestWorkerRejectsSymlinkedRoot(t *testing.T) {
	base := canonicalTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := open(alias, strings.Repeat("a", 64), "", "BAAI/bge-m3", "context_pack"); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("symlink path was not rejected by the canonical-path guard: %v", err)
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("symlink rejection wrote worker data: %v %v", entries, err)
	}
}

func TestShadowRecordsAndPreservesVisiblePack(t *testing.T) {
	en := testEngine(t)
	ctx := context.Background()
	r := request{Session: "session", Batch: "batch", Messages: []message{{Role: "user", Content: "supermarket staging pool change", ChatTime: "2025-01-01T00:00:00Z"}, {Role: "assistant", Content: "supermarket staging pool set to8, production stays4", ChatTime: "2025-01-01T00:00:01Z"}}}
	if _, e := en.add(ctx, r); e != nil {
		t.Fatal(e)
	}
	q := request{Query: "supermarket staging pool", TopK: 20}
	before, e := en.query(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	capture := evidence.WithPolicy(ctx, evidence.Policy{Mode: "shadow"})
	rows, e := en.d.Query(`SELECT event_id FROM events ORDER BY event_id`)
	if e != nil {
		t.Fatal(e)
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	tx, e := en.d.BeginTx(capture, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range ids {
		if e = evidence.IndexEvent(capture, tx, id); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var sessionID string
	en.d.QueryRow(`SELECT session_id FROM sessions LIMIT 1`).Scan(&sessionID)
	if _, e = summary.Generate(capture, en.d, sessionID); e != nil {
		t.Fatal(e)
	}
	after, e := en.query(capture, q)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(before.(map[string]any)["pack"], after.(map[string]any)["pack"]) {
		t.Fatal("shadow changed visible pack")
	}
	var n int
	en.d.QueryRow(`SELECT count(*) FROM evidence_links WHERE relation='reply_to' AND status='active'`).Scan(&n)
	if n != 1 {
		t.Fatalf("native/ordered link count=%d", n)
	}
	en.d.QueryRow(`SELECT count(*) FROM evidence_links WHERE relation='derived_from'`).Scan(&n)
	if n == 0 {
		t.Fatal("existing source_refs not captured")
	}
}
