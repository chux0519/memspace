package search_test

import (
	"context"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"testing"
)

func TestSessionSearchKeepsProjectKindAndSessionFilters(t *testing.T) {
	f := newFixture(t)
	req := model.SearchRequest{Mode: "session", Query: "Stripe webhook", Scope: model.ScopeCurrentProject, ProjectID: "billing", Limit: 5}
	r, err := search.Search(context.Background(), f.db, owner, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Results) != 1 || r.Results[0].SessionID != "s-bill-1" {
		t.Fatalf("session grouping: %+v", r)
	}
	req.ProjectID = "webapp"
	r, err = search.Search(context.Background(), f.db, owner, req)
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("scope leaked: %+v %v", r, err)
	}
	req.ProjectID = "billing"
	req.Kinds = []string{search.KindPrompt}
	r, err = search.Search(context.Background(), f.db, owner, req)
	if err != nil || len(r.Results) != 1 || r.Results[0].Kind != search.KindPrompt {
		t.Fatalf("kind filter lost: %+v %v", r, err)
	}
	req.SessionID = "s-web-1"
	r, err = search.Search(context.Background(), f.db, owner, req)
	if err != nil || len(r.Results) != 0 {
		t.Fatalf("session filter lost: %+v %v", r, err)
	}
	req.Scope = "invalid"
	req.Kinds = []string{"invalid-kind"}
	if _, err = search.Search(context.Background(), f.db, owner, req); err == nil {
		t.Fatal("invalid scope bypassed validation")
	}
}
