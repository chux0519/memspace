package main

import (
	"context"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"math"
	"path/filepath"
	"testing"
)

func TestMetricsUseAllSourcesAndExcludeAbstention(t *testing.T) {
	q := question{ID: "q", AnswerIDs: []string{"a", "b"}}
	r := score(q, []string{"a", "a", "x", "b"})
	if *r.All5 != 1 || *r.Fractional5 != 1 || *r.Any5 != 1 {
		t.Fatalf("dedup/all sources: %+v", r)
	}
	r = score(q, []string{"a", "x", "y", "z", "w", "b"})
	if *r.All5 != 0 || *r.Fractional5 != .5 || *r.All10 != 1 {
		t.Fatalf("any is not all: %+v", r)
	}
	want := (1 + 1/math.Log2(7)) / (1 + 1/math.Log2(3))
	if math.Abs(*r.NDCG10-want) > 1e-10 {
		t.Fatal("NDCG formula")
	}
	q.ID = "q_abs"
	if score(q, []string{"a"}).All5 != nil {
		t.Fatal("abstention incorrectly scored")
	}
}
func TestDateAndRealIngestKeepRepeatedSessionsInSeparateProjects(t *testing.T) {
	d, e := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	qs := []question{{ID: "first", SessionIDs: []string{"same", "same"}, Dates: []string{"2023/05/20 (Sat) 02:21", "2023/05/21 (Sun) 02:21"}, Sessions: [][]turn{{{Role: "user", Content: "quartz repair marker"}}, {{Role: "user", Content: "quartz repair marker"}}}}, {ID: "second", SessionIDs: []string{"same"}, Dates: []string{"2023/05/20 (Sat) 02:21"}, Sessions: [][]turn{{{Role: "assistant", Content: "cobalt recovery marker"}}}}}
	st := indexState{Projects: map[string]string{}, Sessions: map[string]string{}}
	if e = ingest(context.Background(), d, qs, &st, filepath.Join(t.TempDir(), "state.json")); e != nil {
		t.Fatal(e)
	}
	if st.Duplicates != 1 || len(st.Sessions) != 2 || st.Projects["first"] == st.Projects["second"] {
		t.Fatalf("source identity collision: %+v", st)
	}
	r, e := search.Search(context.Background(), d, evalOwner, model.SearchRequest{Query: "cobalt", Mode: "keyword", Scope: model.ScopeCurrentProject, ProjectID: st.Projects["first"], Limit: 100})
	if e != nil || len(r.Results) != 0 {
		t.Fatalf("source escaped its question: %+v %v", r, e)
	}
	var year string
	if e = d.QueryRow(`SELECT substr(occurred_at,1,4) FROM events LIMIT 1`).Scan(&year); e != nil || year != "2023" {
		t.Fatal("historical timestamps shifted")
	}
}
