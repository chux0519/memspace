package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/search"
)

func testEngine(t *testing.T) *engine {
	t.Helper()
	en, e := open(filepath.Join(t.TempDir(), "user"), strings.Repeat("a", 64), "", "BAAI/bge-m3", "context_pack")
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
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.db"), []byte("untouched"), 0600)
	if _, e := open(dir, strings.Repeat("a", 64), "", "BAAI/bge-m3", "context_pack"); e == nil {
		t.Fatal("unowned directory opened")
	}
}
