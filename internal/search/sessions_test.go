package search

import (
	"github.com/chux0519/memspace/internal/model"
	"testing"
)

func TestSessionFusionDoesNotRewardDuplicatePassages(t *testing.T) {
	a := model.SearchResult{SessionID: "a", RefID: "a-summary", ProjectID: "p"}
	b := model.SearchResult{SessionID: "b", RefID: "b-summary", ProjectID: "p"}
	c := model.SearchResult{SessionID: "c", RefID: "c-summary", ProjectID: "p"}
	whole := []model.SearchResult{a, b, c}
	passages := []model.SearchResult{b, a, c}
	want := FuseSessionRanks([][]model.SearchResult{whole, passages}, 3)
	got := FuseSessionRanks([][]model.SearchResult{whole, {b, b, b, b, a, c, {RefID: "unsessioned"}}}, 3)
	if len(got) != 3 {
		t.Fatalf("session count: %+v", got)
	}
	for i := range want {
		if got[i].SessionID != want[i].SessionID || got[i].Score != want[i].Score {
			t.Fatalf("duplicate passages changed rank: %+v vs %+v", got, want)
		}
	}
	if got[0].RefID != "a-summary" {
		t.Fatalf("lost whole-session representative: %+v", got)
	}
	if len(FuseSessionRanks([][]model.SearchResult{whole}, 2)) != 2 {
		t.Fatal("session limit ignored")
	}
}
