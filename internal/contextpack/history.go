package contextpack

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
)

// A fused hit retains its keyword Match label. A strong semantic match must
// still qualify when partial keyword coverage would reject it on its own.
func useHistoryHit(h model.SearchResult, inject float64) bool {
	if h.Match == "all" {
		return true
	}
	if inject > 0 && h.Similarity >= inject {
		return true
	}
	return h.Match == "any" && h.TotalTerms > 0 && h.MatchedTerms*2 >= h.TotalTerms
}

// Search snippets are display previews, not complete evidence. Keep a short
// accepted document intact; for long sources retain the focused search snippet.
// Six 600-character sources leave room for provenance under PromptBudget.
const completeHistoryChars = 600

func historyContent(ctx context.Context, db *sql.DB, owner, projectID string, h model.SearchResult) (string, error) {
	var body string
	err := db.QueryRowContext(ctx, `SELECT d.body FROM search_docs d
 JOIN projects p ON p.project_id=d.project_id
 WHERE d.doc_id=? AND d.project_id=? AND p.user_id=?`, h.DocID, projectID, owner).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} // removed since search
	if err != nil {
		return "", err
	}
	body = strings.TrimSpace(redact.String(body))
	if body != "" && utf8.RuneCountInString(body) <= completeHistoryChars {
		return oneLine(body), nil
	}
	return oneLine(redact.Truncate(h.Snippet, 300)), nil
}

// Preserve session rank, but don't let a question suppress an independently
// qualifying answer or failure from that same session. Prompt-only history
// (for example user decisions) remains available when no such evidence exists.
func historyCandidates(hits []model.SearchResult, current string, inject float64) []model.SearchResult {
	var out []model.SearchResult
	sessions := map[string]int{}
	for _, h := range hits {
		if current != "" && h.SessionID == current || !useHistoryHit(h, inject) {
			continue
		}
		if h.SessionID != "" {
			if i, ok := sessions[h.SessionID]; ok {
				if out[i].Kind == search.KindPrompt {
					switch h.Kind {
					case search.KindReply, search.KindFailure, search.KindSummary, search.KindMemory:
						out[i] = h
					}
				}
				continue
			}
			sessions[h.SessionID] = len(out)
		}
		out = append(out, h)
	}
	return out
}
