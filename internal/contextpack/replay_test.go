package contextpack_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
)

func TestCandidateReplayPreservesPipelineAndRejectsForeignScope(t *testing.T) {
	f := setup(t)
	history(f)
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: webapp, Query: "认证刷新令牌", AgentType: "claude-code", NativeSessionID: "native-current"}
	var hits []model.SearchResult
	original, err := contextpack.BuildWithTrace(context.Background(), f.db, owner, "linux-box", req, func(e contextpack.TraceEvent) {
		if e.Stage == "search" {
			hits = e.Hits
		}
	})
	if err != nil || len(hits) == 0 {
		t.Fatalf("capture: %v", err)
	}
	replay, err := contextpack.BuildFromCandidates(context.Background(), f.db, owner, "linux-box", req, hits, nil)
	if err != nil || !reflect.DeepEqual(original, replay) {
		t.Fatalf("replay changed text/items: %v", err)
	}
	hits[0].ProjectID = "foreign"
	if _, err := contextpack.BuildFromCandidates(context.Background(), f.db, owner, "linux-box", req, hits, nil); err == nil {
		t.Fatal("foreign replay accepted")
	}
}
func TestQualificationOverrideKeepsCurrentSessionExclusion(t *testing.T) {
	f := setup(t)
	history(f)
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: webapp, Query: "认证刷新令牌", AgentType: "claude-code", NativeSessionID: "native-current"}
	var hits []model.SearchResult
	_, err := contextpack.BuildWithTrace(context.Background(), f.db, owner, "linux-box", req, func(e contextpack.TraceEvent) {
		if e.Stage == "search" {
			hits = e.Hits
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := admission.WithPolicy(context.Background(), admission.Policy{Mode: "none"})
	p, err := contextpack.BuildFromCandidates(ctx, f.db, owner, "linux-box", req, hits, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range p.Items {
		for _, h := range hits {
			if i.Ref == h.RefID && h.SessionID == "current" {
				t.Fatal("disabled qualification bypassed current-session exclusion")
			}
		}
	}
}
