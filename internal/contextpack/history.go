package contextpack

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
)

// Collapse repeated source text before spending the candidate budget. Preserve
// timestamps, roles and agent/device provenance: identical text from a later
// event or another device may be the very evidence a temporal query needs.
// Full source text is used, never a display snippet or a matching sub-chunk.
func uniqueHistoryHits(ctx context.Context, db *sql.DB, owner, projectID string, hits []model.SearchResult, limit int) ([]model.SearchResult, error) {
	if limit <= 0 {
		return nil, nil
	}
	seen := map[[32]byte]bool{}
	var out []model.SearchResult
	for _, h := range hits {
		var title, body, files string
		err := db.QueryRowContext(ctx, `SELECT d.title,d.body,d.files FROM search_docs d JOIN projects p ON p.project_id=d.project_id WHERE d.doc_id=? AND d.project_id=? AND p.user_id=?`, h.DocID, projectID, owner).Scan(&title, &body, &files)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		identity, _ := json.Marshal([]string{h.Kind, h.AgentType, h.DeviceID, h.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z"), title, body, files})
		key := sha256.Sum256(identity)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// A fused hit retains its keyword Match label. A strong semantic match must
// still qualify when partial keyword coverage would reject it on its own.
func strongHistoryHit(h model.SearchResult, inject float64) bool {
	return h.Match == "all" || inject > 0 && h.Similarity >= inject
}

func useHistoryHit(h model.SearchResult, inject float64) bool {
	return strongHistoryHit(h, inject) || h.Match == "any" && h.TotalTerms > 0 && h.MatchedTerms*2 >= h.TotalTerms
}

// Search snippets are display previews. Keep short accepted documents intact
// and bound longer original chunks or keyword neighbourhoods in characters.
// Six 600-character sources leave room for provenance under PromptBudget.
const completeHistoryChars = 600

func historyContent(ctx context.Context, db *sql.DB, owner, projectID string, h model.SearchResult, expand bool) (string, error) {
	var body, title, files string
	err := db.QueryRowContext(ctx, `SELECT d.body,d.title,d.files FROM search_docs d
 JOIN projects p ON p.project_id=d.project_id
 WHERE d.doc_id=? AND d.project_id=? AND p.user_id=?`, h.DocID, projectID, owner).Scan(&body, &title, &files)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	} // removed since search
	if err != nil {
		return "", err
	}

	var sourceParts []string
	for _, part := range []string{title, body, files} {
		if part = strings.TrimSpace(part); part != "" {
			sourceParts = append(sourceParts, part)
		}
	}
	source := strings.Join(sourceParts, "\n")

	body = strings.TrimSpace(redact.String(body))
	chunk := strings.TrimSpace(h.EvidenceText)
	validChunk := chunk != "" && strings.Contains(source, chunk)
	// Never inject a semantic chunk removed by a concurrent source update.
	if chunk != "" && !validChunk && h.Match == "semantic" {
		return "", nil
	}
	if body != "" && utf8.RuneCountInString(body) <= completeHistoryChars {
		return oneLine(body), nil
	}
	// Weak partial-keyword matches retain the original compact preview. Expanding
	// them would amplify unrelated context without stronger relevance evidence.
	if !expand {
		return historyPreview(h), nil
	}
	if validChunk {
		return oneLine(evidenceWindow(redact.String(chunk), highlightAnchor(h.Snippet))), nil
	}
	// Keyword hits also deserve the original neighbourhood, not only the
	// display snippet's token window. Use private FTS marks to locate a match.
	if anchor := highlightAnchor(h.Snippet); anchor != "" && strings.Contains(strings.ToLower(body), strings.ToLower(anchor)) {
		return oneLine(evidenceWindow(body, anchor)), nil
	}
	return historyPreview(h), nil
}

func historyPreview(h model.SearchResult) string {
	preview := strings.NewReplacer(search.MarkOpen, "", search.MarkClose, "").Replace(h.Snippet)
	return oneLine(redact.Truncate(preview, 300))
}

// Preserve session rank, but don't let a question suppress an independently
// qualifying answer or failure from that same session. Prompt-only history
// (for example user decisions) remains available when no such evidence exists.
// Order candidates round-robin across sessions: cover other sessions before
// filling spare slots with more evidence from one session. The overall delivery
// cap and exact-content deduplication apply after scoped hydration in history().
func historyCandidates(hits []model.SearchResult, current string, inject float64) []model.SearchResult {
	var groups [][]model.SearchResult
	sessions := map[string]int{}
	for _, h := range hits {
		if current != "" && h.SessionID == current || !useHistoryHit(h, inject) {
			continue
		}
		i, ok := sessions[h.SessionID]
		if h.SessionID == "" || !ok {
			if h.SessionID != "" {
				sessions[h.SessionID] = len(groups)
			}
			groups = append(groups, []model.SearchResult{h})
			continue
		}
		group := groups[i]
		if group[0].Kind == search.KindPrompt && h.Kind != search.KindPrompt {
			switch h.Kind {
			case search.KindReply, search.KindFailure, search.KindSummary, search.KindMemory:
				groups[i] = []model.SearchResult{h}
			}
			continue
		}
		if group[0].Kind != search.KindPrompt && h.Kind == search.KindPrompt {
			continue
		}
		duplicate := false
		for _, old := range group {
			if old.RefID == h.RefID {
				duplicate = true
			}
		}
		if !duplicate {
			groups[i] = append(group, h)
		}
	}
	var out []model.SearchResult
	for round := 0; ; round++ {
		added := false
		for _, g := range groups {
			if len(g) > round {
				out = append(out, g[round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out
}

var historyMarks = regexp.MustCompile(search.MarkOpen + `([^` + search.MarkClose + `]+)` + search.MarkClose)

func highlightAnchor(preview string) string {
	matches := historyMarks.FindAllStringSubmatch(preview, -1)
	// Distinctive identifiers outrank short function words when choosing a window.
	sort.SliceStable(matches, func(i, j int) bool {
		return utf8.RuneCountInString(matches[i][1]) > utf8.RuneCountInString(matches[j][1])
	})
	if len(matches) == 0 {
		return ""
	}
	return matches[0][1]
}

func evidenceWindow(text, anchor string) string {
	runes := []rune(text)
	if len(runes) <= completeHistoryChars {
		return text
	}
	lower := strings.ToLower(text)
	if anchor != "" {
		if at := strings.Index(lower, strings.ToLower(anchor)); at >= 0 {
			pos := utf8.RuneCountInString(lower[:at])
			start := max(0, pos-80)
			end := min(len(runes), start+completeHistoryChars-4)
			// Make omissions explicit and count their markers inside the source bound.
			prefix, suffix := "", ""
			if start > 0 {
				prefix = "…\n"
			}
			if end < len(runes) {
				suffix = "\n…"
			}
			return prefix + string(runes[start:end]) + suffix
		}
	}
	// A semantic-only chunk has no literal highlight. Retain both ends rather
	// than silently pretending a long chunk was supplied in full.
	half := (completeHistoryChars - 3) / 2
	return string(runes[:half]) + "\n…\n" + string(runes[len(runes)-half:])
}
