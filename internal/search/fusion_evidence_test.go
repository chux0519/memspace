package search

import (
	"encoding/json"
	"github.com/chux0519/memspace/internal/model"
	"strings"
	"testing"
)

func TestFusionPreservesInternalSourceWithoutEnlargingPublicPreview(t *testing.T) {
	const source = "raw evidence: actual repair and validation details"
	r := fuse([]model.SearchResult{{DocID: 7, Match: "any", Snippet: "keyword display preview", MatchedTerms: 1, TotalTerms: 4}}, []model.SearchResult{{DocID: 7, Match: "semantic", Similarity: 0.8, Snippet: "semantic display preview", EvidenceText: source}}, 1)
	if len(r) != 1 || r[0].Snippet != "keyword display preview" || r[0].Match != "any" || r[0].EvidenceText != source || r[0].Similarity != 0.8 {
		t.Fatalf("lost evidence or keyword provenance: %+v", r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), source) || !strings.Contains(string(b), "keyword display preview") {
		t.Fatalf("public response grew to include internal source: %s", b)
	}
}
