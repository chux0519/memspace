package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
)

// Export candidates before injection qualification, without evidence labels.
// Only the benchmark's disposable database is read. Preserve each depth's real
// fusion result rather than assuming the top 12 of a depth-24 search is equal.
func exportCandidates(ctx context.Context, db *sql.DB, c corpus, depths, out string) error {
	f, err := os.OpenFile(filepath.Join(out, "candidates.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, raw := range strings.Split(depths, ",") {
		depth, err := strconv.Atoi(raw)
		if err != nil || depth < 1 || depth > 100 {
			return fmt.Errorf("invalid candidate depth %q", raw)
		}
		for _, q := range c.Questions {
			if q.Scope == model.ScopeAllProjects {
				continue
			}
			resp, err := search.Search(ctx, db, owner, model.SearchRequest{
				Query: q.Query, Scope: model.ScopeSelectedProjects, ProjectIDs: []string{q.Project},
				Kinds: []string{search.KindMemory, search.KindSummary, search.KindPrompt, search.KindReply, search.KindFailure, search.KindChange},
				Limit: depth, Highlight: model.HighlightMarks,
			})
			if err != nil {
				return err
			}
			candidates := []map[string]any{}
			for _, h := range resp.Results {
				var body, title, files string
				err = db.QueryRowContext(ctx, `SELECT d.body,d.title,d.files FROM search_docs d JOIN projects p ON p.project_id=d.project_id WHERE d.doc_id=? AND d.project_id=? AND p.user_id=?`, h.DocID, q.Project, owner).Scan(&body, &title, &files)
				if err != nil {
					return err
				}
				var parts []string
				for _, part := range []string{title, body, files} {
					if strings.TrimSpace(part) != "" {
						parts = append(parts, strings.TrimSpace(part))
					}
				}
				text := strings.Join(parts, "\n")
				// Use the matching chunk for long sources, not a display snippet.
				if h.EvidenceText != "" && strings.Contains(text, h.EvidenceText) {
					text = h.EvidenceText
				}
				candidates = append(candidates, map[string]any{"hit": h, "source_text": text})
			}
			if err = enc.Encode(map[string]any{"question_id": q.ID, "query": q.Query, "project": q.Project, "depth": depth, "candidates": candidates}); err != nil {
				return err
			}
		}
	}
	return nil
}
