package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/summary"
)

func TestEvidenceScoring(t *testing.T) {
	docs := map[string]document{"a": {Project: "p", Evidence: "atomic used_at"}, "b": {Project: "p", Evidence: "rotate token"}, "c": {Project: "p", Evidence: "other"}, "outside": {Project: "other"}}
	q := question{Project: "p", Scope: model.ScopeCurrentProject, Groups: [][]string{{"a", "c"}, {"b"}}}
	s := score(q, []string{"a"}, "", false, docs)
	if !s.AnyHit || s.AllHit || s.Recall != 0.5 || s.MRR != 1 {
		t.Fatalf("partial: %+v", s)
	}
	s = score(q, []string{"c", "b"}, "", false, docs)
	if !s.AllHit || s.Recall != 1 {
		t.Fatalf("OR alternatives: %+v", s)
	}
	s = score(q, []string{"a", "b"}, "atomic [used_at]", true, docs)
	if s.AllHit || s.Recall != 0.5 {
		t.Fatalf("reference without visible evidence: %+v", s)
	}
	s = score(q, []string{"a", "a", "outside"}, "", false, docs)
	if s.OutOfScope != 1 || s.Recall != 0.5 {
		t.Fatalf("duplicates/scope: %+v", s)
	}
	s = score(question{}, nil, "", true, docs)
	if !s.Negative || s.FalseInjection || s.AllHit {
		t.Fatalf("empty negative: %+v", s)
	}
	s = score(question{}, nil, "irrelevant memory", true, docs)
	if !s.FalseInjection {
		t.Fatal("negative injection not counted")
	}
	r := receipt{Results: []model.SearchResult{{RefID: "a", ProjectID: "other", ProjectName: "other"}}}
	auditProvenance(&r, q, docs)
	if r.Score.OutOfScope != 1 || r.Score.ProvenanceErrors != 1 {
		t.Fatalf("actual returned metadata was not checked: %+v", r.Score)
	}

}

func TestCorpusAndProductionPath(t *testing.T) {
	b, err := os.ReadFile("../data/coding-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if err = validate(c); err != nil {
		t.Fatal(err)
	}
	// Guard against accidental split leakage during future additions.
	copyC := c
	copyC.Questions = append([]question{}, c.Questions...)
	q := copyC.Questions[0]
	q.ID = "leak"
	if q.Split == "dev" {
		q.Split = "test"
	} else {
		q.Split = "dev"
	}
	copyC.Questions = append(copyC.Questions, q)
	if validate(copyC) == nil {
		t.Fatal("accepted evidence leakage")
	}
	work := t.TempDir()
	d, err := db.Open(filepath.Join(work, "bench.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, _, err = seed(context.Background(), d, c, work)
	if err != nil {
		t.Fatal(err)
	}
	r, err := search.Search(context.Background(), d, owner, model.SearchRequest{Query: "notification_id", Scope: model.ScopeCurrentProject, ProjectID: "billing", Mode: "keyword", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range r.Results {
		if h.ProjectID != "billing" {
			t.Fatalf("scope leaked: %+v", h)
		}
	}
	p, err := contextpack.Build(context.Background(), d, owner, device, contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: "unknown"}, Query: "寻找数据库迁移记录"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Text != "" {
		t.Fatal("unknown project injected history")
	}
	if _, err = summary.Generate(context.Background(), d, "s-jwt_failure"); err != nil {
		t.Fatal(err)
	}
	p, err = contextpack.Build(context.Background(), d, owner, device, contextpack.Request{Mode: contextpack.ModeSessionStart, Project: search.ProjectRef{ID: "webapp"}})
	if err != nil || len(p.Items) == 0 {
		t.Fatalf("session-start fixture missing: %+v %v", p, err)
	}

}

func TestEvidenceCorpusPreservesV1AndAllowsScopedSharedSessions(t *testing.T) {
	read := func(path string) corpus {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		var c corpus
		if e = json.Unmarshal(b, &c); e != nil {
			t.Fatal(e)
		}
		return c
	}
	a, b := read("../data/coding-v1.json"), read("../data/coding-v2-evidence.json")
	if err := validate(b); err != nil {
		t.Fatal(err)
	}
	for i, q := range a.Questions {
		x, _ := json.Marshal(q)
		y, _ := json.Marshal(b.Questions[i])
		if string(x) != string(y) {
			t.Fatalf("legacy question %s changed", q.ID)
		}
	}
	for i, d := range a.Documents {
		if b.Documents[i] != d {
			t.Fatalf("legacy document %s changed", d.ID)
		}
	}
	// Same-session facts may share a project, but can never merge projects.
	b.Documents[len(b.Documents)-1].Session = b.Documents[len(b.Documents)-2].Session
	b.Documents[len(b.Documents)-1].Project = "webapp"
	if validate(b) == nil {
		t.Fatal("cross-project session merge accepted")
	}
}

func TestRawDialogueSeedKeepsRolesAndOrder(t *testing.T) {
	raw, err := os.ReadFile("../data/dialogue-mechanisms-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if err = validate(c); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	d, err := db.Open(filepath.Join(work, "bench.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	docs, _, err := seed(context.Background(), d, c, work)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	d.QueryRow(`SELECT COUNT(*) FROM search_docs`).Scan(&count)
	if count != 48 || len(docs) != 48 {
		t.Fatalf("extra synthetic prompts: count=%d docs=%d", count, len(docs))
	}
	rows, err := d.Query(`SELECT kind FROM search_docs WHERE session_id='supermarket' ORDER BY occurred_at,doc_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	for i, want := range []string{"prompt", "reply", "prompt", "reply"} {
		if len(kinds) != 4 || kinds[i] != want {
			t.Fatalf("ordered roles=%v", kinds)
		}
	}
}
