package main

import (
	"math"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

func TestFixedRankGateAndAnchorBudget(t *testing.T) {
	c := benchutil.MechanismCase{Request: contextpack.Request{Project: search.ProjectRef{ID: "p"}}}
	for i := int64(1); i <= 6; i++ {
		h := model.SearchResult{DocID: i, RefID: "source", ProjectID: "p", SessionID: "s"}
		c.Candidates = append(c.Candidates, benchutil.MechanismCandidate{Hit: h, Chunk: "literal", Window: []contextpack.EvidenceRecord{{Hit: h}, {Hit: model.SearchResult{DocID: i + 100, ProjectID: "p", SessionID: "s"}}}})
	}
	out, e := ranked(c, []float64{0, 2, -1, 2, 1, -3}, true, 0)
	if e != nil || len(out) != 3 || out[0].DocID != 2 || out[1].DocID != 4 || out[0].EvidenceText != "literal" {
		t.Fatalf("ranked %+v %v", out, e)
	}
	if _, e = ranked(c, []float64{math.NaN(), 0, 0, 0, 0, 0}, true, 0); e == nil {
		t.Fatal("NaN accepted")
	}
	p := &contextpack.Pack{Items: []contextpack.Item{{Key: "doc:1"}, {Key: "doc:2"}}}
	out, e = expand(c, p)
	if e != nil || len(out) != 4 || out[0].DocID != 1 || out[1].DocID != 2 {
		t.Fatalf("expanded %+v %v", out, e)
	}
	p.Items = append(p.Items, contextpack.Item{Key: "doc:3"}, contextpack.Item{Key: "doc:4"}, contextpack.Item{Key: "doc:5"}, contextpack.Item{Key: "doc:6"})
	out, e = expand(c, p)
	if e != nil || len(out) != 6 || out[5].DocID != 6 {
		t.Fatal("neighbors displaced anchor or exceeded cap")
	}
	c.Candidates[0].Window[1].Hit.ProjectID = "other"
	p.Items = p.Items[:1]
	if _, e = expand(c, p); e == nil {
		t.Fatal("foreign project accepted")
	}
}
