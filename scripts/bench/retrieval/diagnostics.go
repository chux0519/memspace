package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
)

// This diagnostic mirrors the pre-fix history gates for attribution, but scores
// the real Build output. It never changes ranking, thresholds or fixture labels.
func diagnose(ctx context.Context, db *sql.DB, c corpus, docs map[string]document, out string) error {
	type evidence struct {
		Ref    string              `json:"ref"`
		Reason string              `json:"baseline_gate_reason"`
		Hit    *model.SearchResult `json:"hit,omitempty"`
	}
	type row struct {
		Question question          `json:"question"`
		Terms    []string          `json:"terms"`
		Pack     *contextpack.Pack `json:"pack"`
		Score    evidenceScore     `json:"score"`
		Evidence []evidence        `json:"evidence"`
	}
	var rows []row
	counts := map[string]int{}
	inject, _ := search.MinSimilarities()
	for _, q := range c.Questions {
		if q.Scope == model.ScopeAllProjects {
			continue
		}
		p, err := contextpack.Build(ctx, db, owner, device, contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: q.Project}, Query: q.Query, Budget: q.Budget})
		if err != nil {
			return err
		}
		resp, err := search.Search(ctx, db, owner, model.SearchRequest{Query: q.Query, Scope: model.ScopeSelectedProjects, ProjectIDs: []string{q.Project}, Kinds: []string{search.KindMemory, search.KindSummary, search.KindPrompt, search.KindReply, search.KindFailure, search.KindChange}, Limit: 12})
		if err != nil {
			return err
		}
		var expr string
		if err = db.QueryRowContext(ctx, `SELECT jieba_query(?)`, q.Query).Scan(&expr); err != nil {
			return err
		}
		r := row{Question: q, Terms: search.Terms(expr), Pack: p}
		var refs []string
		for _, it := range p.Items {
			refs = append(refs, it.Ref)
		}
		r.Score = score(q, refs, p.Text, true, docs)
		n := 0
		for _, ch := range q.Query {
			if unicode.IsLetter(ch) || unicode.IsDigit(ch) {
				n++
			}
		}
		for _, g := range q.Groups {
			// OR groups are satisfied if any alternative meets the visible check.
			if score(question{Groups: [][]string{g}}, refs, p.Text, true, docs).AllHit {
				continue
			}
			for _, ref := range g {
				e := evidence{Ref: ref, Reason: "not_in_candidates"}
				for i := range resp.Results {
					h := &resp.Results[i]
					if h.RefID == ref {
						e.Hit = h
						e.Reason = "selection_or_budget"
						break
					}
				}
				if n < 6 {
					e.Reason = "short_query_gate"
				} else if e.Hit != nil {
					h := e.Hit
					switch {
					case h.Match == "semantic" && h.Similarity < inject:
						e.Reason = "semantic_threshold"
					case h.Match == "any" && h.MatchedTerms*2 < h.TotalTerms:
						e.Reason = "keyword_coverage"
						if h.Similarity >= inject {
							e.Reason = "keyword_gate_overrides_strong_semantic"
						}
					default:
						for _, it := range p.Items {
							if it.Ref == ref {
								e.Reason = "visible_evidence_missing"
								break
							}
						}
					}
				}
				counts[e.Reason]++
				r.Evidence = append(r.Evidence, e)
			}
		}
		rows = append(rows, r)
	}
	b, err := json.MarshalIndent(map[string]any{"note": "Counterfactual baseline-gate labels, NOT a trace of current policy. Scores/pack use current Build. Counts are missing evidence alternatives, not unique questions. Use the saved pre-fix run for attribution.", "counts": counts, "rows": rows}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(out, "diagnostics.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	lines := []string{}
	for reason, n := range counts {
		lines = append(lines, reason+": "+strconv.Itoa(n))
	}
	_, err = os.Stdout.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}
