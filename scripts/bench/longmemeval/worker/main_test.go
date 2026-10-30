package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionViewReturnsDistinctSourcesAndRetainsLongTail(t *testing.T) {
	w := &worker{}
	defer w.close()
	sources := []session{{ID: "a", Content: strings.Repeat("This is background context. ", 2000) + "Final decision used the quartz_recovery_marker."}, {ID: "b", Content: "A second quartz recovery record."}, {ID: "c", Content: "A third quartz recovery record."}}
	if err := w.index(sources, true); err != nil {
		t.Fatal(err)
	}
	r, err := w.handle(request{Op: "query", Query: "quartz recovery marker", K: 3, Mode: "session"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range r.Ranked {
		if seen[h.SessionID] {
			t.Fatal("duplicate session counted twice")
		}
		seen[h.SessionID] = true
	}
	if !seen["a"] || len(r.Ranked) != 3 {
		t.Fatalf("lost long-tail source or distinct sessions: %+v", r)
	}
}

func TestWorkerUsesRealStemmedScopedIndexAndFreshCases(t *testing.T) {
	w := &worker{}
	defer w.close()
	if err := w.index([]session{{ID: "opaque-a", Content: "Dependencies were tested with isolated databases."}, {ID: "opaque-b", Content: "The gardener planted carrots."}}, false); err != nil {
		t.Fatal(err)
	}
	r, err := w.handle(request{Op: "query", Query: "dependency testing database", K: 5, Mode: "bm25"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Ranked) != 1 || r.Ranked[0].SessionID != "opaque-a" {
		t.Fatalf("lost morphology or source ID: %+v", r)
	}
	if err = w.index([]session{{ID: "fresh", Content: "Nothing about cooking here."}}, false); err != nil {
		t.Fatal(err)
	}
	r, err = w.handle(request{Op: "query", Query: "dependencies", K: 5, Mode: "bm25"})
	if err != nil || len(r.Ranked) != 0 {
		t.Fatalf("previous question leaked: %+v %v", r, err)
	}
	b, _ := json.Marshal(r)
	if !bytes.Contains(b, []byte(`"ranked":[]`)) {
		t.Fatalf("empty results must remain a valid list: %s", b)
	}
}

func TestWorkerInputRejectsGoldLabels(t *testing.T) {
	for _, s := range []string{`{"op":"query","query":"x","answer":"gold"}`, `{"op":"index","sessions":[{"id":"a","content":"x","gold":true}]}`} {
		dec := json.NewDecoder(bytes.NewBufferString(s))
		dec.DisallowUnknownFields()
		var r request
		if dec.Decode(&r) == nil {
			t.Fatal("answer metadata could enter the retrieval protocol")
		}
	}
}
