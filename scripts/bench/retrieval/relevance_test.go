package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRelevanceOverlayBoundToCorpusAndExplicitEmpty(t *testing.T) {
	c := corpus{Questions: []question{{ID: "q"}, {ID: "unjudged"}}}
	raw := []byte(`{"corpus_sha256":"sha","judgments":{"q":[]}}`)
	if err := applyRelevanceLabels(&c, raw, "wrong"); err == nil {
		t.Fatal("accepted wrong corpus")
	}
	if err := applyRelevanceLabels(&c, raw, "sha"); err != nil {
		t.Fatal(err)
	}
	if c.Questions[0].RelevantRefs == nil || c.Questions[1].RelevantRefs != nil {
		t.Fatal("lost judged vs unjudged distinction")
	}
	for _, bad := range []string{`{"corpus_sha256":"sha","judgments":{"unknown":[]}}`, `{"corpus_sha256":"sha","judgments":{"q":null}}`} {
		if err := applyRelevanceLabels(&c, []byte(bad), "sha"); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	s := score(c.Questions[0], []string{"a", "a", "b"}, "", false, nil)
	if s.JudgedReturned == nil || *s.JudgedReturned != 2 || *s.JudgedIrrelevant != 2 {
		t.Fatalf("judged empty/duplicate count: %+v", s)
	}
	if score(c.Questions[1], nil, "", false, nil).JudgedReturned != nil {
		t.Fatal("invented precision for unjudged question")
	}
}
func TestRelevanceLabelsCoverRequiredEvidence(t *testing.T) {
	b, _ := json.Marshal(relevanceLabels{CorpusSHA256: "sha", Judgments: map[string][]string{"q": {}}})
	c := corpus{Projects: []string{"p"}, Documents: []document{{ID: "a", Session: "s", Project: "p", Text: "evidence", Evidence: "evidence"}}, Questions: []question{{ID: "q", Split: "dev", Query: "evidence", Project: "p", Scope: "current_project", Groups: [][]string{{"a"}}}}}
	if err := applyRelevanceLabels(&c, b, "sha"); err != nil {
		t.Fatal(err)
	}
	if err := validate(c); err == nil || !strings.Contains(err.Error(), "required source missing") {
		t.Fatalf("expected relevance contradiction, got %v", err)
	}
}
