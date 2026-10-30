package main

import "testing"

func TestBM25UsesStemsWithoutAutocompletePrefix(t *testing.T) {
	w := &worker{}
	defer w.close()
	if err := w.index([]session{{ID: "right", Content: "Dependencies were tested in isolated environments."}, {ID: "wrong", Content: "A testament to discipline and honest relationships."}}, false); err != nil {
		t.Fatal(err)
	}
	r, err := w.handle(request{Op: "query", Query: "dependency testing", Mode: "bm25", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Ranked) != 1 || r.Ranked[0].SessionID != "right" {
		t.Fatalf("prefix-only match entered natural-language rank: %+v", r)
	}
}
