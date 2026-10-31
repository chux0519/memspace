package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

func exportMechanismCases(ctx context.Context, d *sql.DB, c corpus, docs map[string]document, src, out string) error {
	index, err := filepath.Abs(filepath.Join(out, "index.db"))
	if err != nil {
		return err
	}
	var cases []benchutil.MechanismCase
	modelMin, _ := search.MinSimilarities()
	for _, q := range c.Questions {
		if q.Scope != model.ScopeCurrentProject {
			return fmt.Errorf("mechanism export requires current project: %s", q.ID)
		}
		req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: q.Project}, Query: q.Query, Budget: q.Budget, AgentType: "claude-code", NativeSessionID: "benchmark-new-session"}
		var hits []model.SearchResult
		baseline, e := contextpack.BuildWithTrace(ctx, d, owner, device, req, func(ev contextpack.TraceEvent) {
			if ev.Stage == "deduplicated" {
				hits = ev.Hits
			}
		})
		if e != nil {
			return e
		}
		control, e := contextpack.BuildFromCandidates(ctx, d, owner, device, req, hits, nil)
		if e != nil || !reflect.DeepEqual(baseline, control) {
			return fmt.Errorf("baseline replay differs for %s: %v", q.ID, e)
		}
		row, e := benchutil.MakeMechanismCase(ctx, d, q.ID, owner, device, index, req, modelMin, baseline, hits)
		if e != nil {
			return e
		}
		cases = append(cases, row)
	}
	if err = benchutil.SnapshotCheckpointedIndex(ctx, d, src, index); err != nil {
		return err
	}
	if err = benchutil.WriteMechanismCases(filepath.Join(out, "cases.jsonl"), cases); err != nil {
		return err
	}
	return benchutil.JSON(filepath.Join(out, "labels.json"), map[string]any{"questions": c.Questions, "documents": docs})
}
