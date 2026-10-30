package main

import (
	"context"
	"encoding/json"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCandidateFloorOverrideKeepsInjectionGate(t *testing.T) {
	svc := embedding.New(nil, embedding.Config{URL: "http://unused.invalid", Model: "BAAI/bge-m3"})
	o := &observedSemantic{svc: svc}
	inject, floor := o.MinSimilarities()
	if inject != .6 || floor != .45 {
		t.Fatalf("unexpected baseline: %v %v", inject, floor)
	}
	o.candidateFloor = new(float64)
	*o.candidateFloor = .3
	i, f := o.MinSimilarities()
	if i != inject || f != .3 {
		t.Fatalf("candidate experiment changed injection gate: %v %v", i, f)
	}
	_, original := svc.MinSimilarities()
	if original != floor {
		t.Fatal("benchmark mutated service threshold")
	}
}

func TestCandidateExportUsesScopeAndOmitsJudgmentLabels(t *testing.T) {
	c := corpus{Projects: []string{"a", "b"}, Documents: []document{
		{ID: "a-source", Project: "a", Session: "a-session", Text: "Orion boundary uses QueueID", Evidence: "QueueID"},
		{ID: "b-source", Project: "b", Session: "b-session", Text: "Orion boundary uses billing_only", Evidence: "billing_only"},
	}, Questions: []question{{ID: "q", Query: "Orion boundary", Project: "a", Scope: model.ScopeCurrentProject, Groups: [][]string{{"a-source"}}}}}
	d, err := db.Open(filepath.Join(t.TempDir(), "bench.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, _, err = seed(context.Background(), d, c, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err = exportCandidates(context.Background(), d, c, "12,24", out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "candidates.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "billing_only") || strings.Contains(string(b), "evidence_groups") || strings.Contains(string(b), `"split"`) {
		t.Fatalf("scope leak or judgment labels exported: %s", b)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("rows: %d", len(lines))
	}
	for _, line := range lines {
		var row struct {
			Candidates []struct {
				Source string `json:"source_text"`
			}
		}
		if err = json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		if len(row.Candidates) == 0 || row.Candidates[0].Source != "Orion boundary uses QueueID" {
			t.Fatalf("missing original source: %s", line)
		}
	}
}
