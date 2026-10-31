package contextpack_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
)

func TestDialogueWindowScopesAndPreservesRoles(t *testing.T) {
	f := setup(t)
	f.session("source", "linux-box", "codex", "ended")
	f.session("other", "mac-mini", "codex", "ended")
	f.event("source", 4*time.Hour, "user.prompt", map[string]any{"prompt": "Only staging; production stays 4."})
	f.event("other", 3*time.Hour+time.Minute, "user.prompt", map[string]any{"prompt": "Unrelated session; never include."})
	f.event("source", 3*time.Hour, "agent.turn_completed", map[string]any{"last_assistant_message": "Set to 8."})
	f.event("source", 2*time.Hour, "user.prompt", map[string]any{"prompt": "Correction: staging should be 6."})
	var h model.SearchResult
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE ref_id='evt_ctx_0003'`).Scan(&h.DocID); err != nil {
		t.Fatal(err)
	}
	h.Match = "semantic"
	h.Similarity = .9
	h.EvidenceText = "Set to 8."
	records, err := contextpack.DialogueWindow(context.Background(), f.db, owner, "webapp", h)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("window = %+v", records)
	}
	for i, want := range []string{"evt_ctx_0001", "evt_ctx_0003", "evt_ctx_0004"} {
		if records[i].Hit.RefID != want || records[i].Hit.SessionID != "source" {
			t.Fatalf("neighbor %d: %+v", i, records[i])
		}
	}
	if records[1].Chunk != "Set to 8." || records[1].Hit.Similarity != .9 {
		t.Fatal("lost anchor chunk/signals")
	}
	for _, pair := range [][2]string{{"foreign", "webapp"}, {owner, "foreign"}} {
		if _, err = contextpack.DialogueWindow(context.Background(), f.db, pair[0], pair[1], h); err == nil {
			t.Fatal("unscoped anchor accepted")
		}
	}
	// A message in the right project but another session cannot enter the window.
	for _, r := range records {
		if strings.Contains(r.Body, "Unrelated") {
			t.Fatal("session boundary leaked")
		}
	}
	none := admission.WithPolicy(context.Background(), admission.Policy{Mode: "none"})
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: "webapp"}, Query: "What staging value was set?"}
	hits := []model.SearchResult{records[0].Hit, records[1].Hit}
	normal, err := contextpack.BuildFromCandidates(none, f.db, owner, "linux-box", req, hits, nil)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := contextpack.BuildFromEvidence(none, f.db, owner, "linux-box", req, hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(normal.Items) != 1 || len(explicit.Items) != 2 || !strings.Contains(explicit.Text, "Only staging") {
		t.Fatalf("normal=%+v explicit=%+v", normal, explicit)
	}
	again, err := contextpack.BuildFromCandidates(none, f.db, owner, "linux-box", req, hits, nil)
	if err != nil || !reflect.DeepEqual(normal, again) {
		t.Fatal("diagnostic replay changed normal behavior")
	}
}
