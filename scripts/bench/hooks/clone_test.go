package main

import (
	"context"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"path/filepath"
	"testing"
	"time"
)

func TestCloneKeepsFTSAndVectorPartitionsConsistent(t *testing.T) {
	d, e := db.Open(filepath.Join(t.TempDir(), "copy.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	ctx := context.Background()
	for _, q := range []string{`INSERT INTO users(user_id) VALUES ('u')`, `INSERT INTO projects(project_id,user_id,name) VALUES ('p','u','p')`} {
		if _, e = d.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	tx, _ := d.Begin()
	e = search.Upsert(ctx, tx, &search.Doc{Kind: "reply", RefID: "ref", ProjectID: "p", Body: "quartz repair", OccurredAt: time.Now()})
	if e != nil {
		t.Fatal(e)
	}
	tx.Commit()
	var docID int64
	d.QueryRow(`SELECT doc_id FROM search_docs`).Scan(&docID)
	space, e := embedding.EnsureSpace(ctx, d, embedding.ServiceInfo{Model: "unit-test", Dimensions: 2})
	if e != nil {
		t.Fatal(e)
	}
	e = embedding.StoreChunks(ctx, d, space, []embedding.Embedded{{DocID: docID, ProjectID: "p", ChunkNo: 0, Text: "quartz repair", Vector: []float32{1, 0}}}, map[int64]string{docID: "quartz repair"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = embedding.Activate(ctx, d, space); e != nil {
		t.Fatal(e)
	}
	if e = cloneIndex(ctx, d, 3); e != nil {
		t.Fatal(e)
	}
	for _, p := range []string{"p", "bench-clone-1-p", "bench-clone-2-p"} {
		h, e := embedding.KNN(ctx, d, space, []float32{1, 0}, []string{p}, 5)
		if e != nil || len(h) != 1 {
			t.Fatalf("vec partition %s: %+v %v", p, h, e)
		}
		r, e := search.Search(ctx, d, "u", model.SearchRequest{Mode: "keyword", Query: "quartz", Scope: model.ScopeCurrentProject, ProjectID: p, Limit: 5})
		if e != nil || len(r.Results) != 1 {
			t.Fatalf("FTS partition %s: %+v %v", p, r, e)
		}
	}
	if e = cloneIndex(ctx, d, 3); e == nil {
		t.Fatal("duplicate clone accepted")
	}
}
