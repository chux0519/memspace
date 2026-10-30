package contextpack

import (
	"context"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryQualifiesByKeywordOrSemanticEvidence(t *testing.T) {
	cases := []struct {
		name string
		hit  model.SearchResult
		want bool
	}{
		{"full keyword", model.SearchResult{Match: "all"}, true},
		{"half keyword", model.SearchResult{Match: "any", MatchedTerms: 2, TotalTerms: 4}, true},
		{"weak partial keyword", model.SearchResult{Match: "any", MatchedTerms: 1, TotalTerms: 4, Similarity: 0.59}, false},
		{"strong semantic despite partial keyword", model.SearchResult{Match: "any", MatchedTerms: 1, TotalTerms: 4, Similarity: 0.61}, true},
		{"semantic at threshold", model.SearchResult{Match: "semantic", Similarity: 0.60}, true},
		{"weak semantic", model.SearchResult{Match: "semantic", Similarity: 0.59}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := useHistoryHit(c.hit, 0.60); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestSessionCandidateKeepsEvidenceInsteadOfOnlyQuestion(t *testing.T) {
	hits := []model.SearchResult{
		{RefID: "question", SessionID: "s1", Kind: "prompt", Match: "all"},
		{RefID: "user-decision", SessionID: "s2", Kind: "prompt", Match: "all"},
		{RefID: "answer", SessionID: "s1", Kind: "reply", Match: "semantic", Similarity: 0.7},
		{RefID: "irrelevant-answer", SessionID: "s2", Kind: "reply", Match: "semantic", Similarity: 0.4},
		{RefID: "current-answer", SessionID: "current", Kind: "reply", Match: "all"},
	}
	got := historyCandidates(hits, "current", 0.6)
	if len(got) != 2 || got[0].RefID != "answer" || got[1].RefID != "user-decision" {
		t.Fatalf("candidates %+v", got)
	}
}

func TestHistoryContentScopeAndBound(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users(user_id) VALUES ('owner'),('other')`,
		`INSERT INTO projects(project_id,user_id,name) VALUES ('p','owner','p'),('outside','other','outside')`,
	} {
		if _, err = d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []search.Doc{
		{Kind: search.KindReply, RefID: "short", ProjectID: "p", Body: "认证修复：在事务中更新 used_at。"},
		{Kind: search.KindReply, RefID: "long", ProjectID: "p", Body: strings.Repeat("漫长的历史记录。", 200)},
	} {
		if err = search.Upsert(context.Background(), tx, &doc); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var shortID, longID int64
	if err = d.QueryRow(`SELECT doc_id FROM search_docs WHERE ref_id='short'`).Scan(&shortID); err != nil {
		t.Fatal(err)
	}
	if err = d.QueryRow(`SELECT doc_id FROM search_docs WHERE ref_id='long'`).Scan(&longID); err != nil {
		t.Fatal(err)
	}
	hit := model.SearchResult{DocID: shortID, Snippet: "[认证]…"}
	text, err := historyContent(context.Background(), d, "owner", "p", hit, true)
	if err != nil || !strings.Contains(text, "在事务中更新 used_at") {
		t.Fatalf("missing complete evidence: %q %v", text, err)
	}
	for _, scope := range [][2]string{{"other", "p"}, {"owner", "outside"}} {
		text, err = historyContent(context.Background(), d, scope[0], scope[1], hit, true)
		if err != nil || text != "" {
			t.Fatalf("scope leak: %q %v", text, err)
		}
	}
	hit.DocID = longID
	hit.Snippet = "查询附近的匹配片段"
	text, err = historyContent(context.Background(), d, "owner", "p", hit, true)
	if err != nil || text != hit.Snippet {
		t.Fatalf("long source expanded without a bound: %q %v", text, err)
	}

	hit.Match = "any"
	hit.Snippet = search.MarkOpen + "漫长" + search.MarkClose + "…"
	hit.EvidenceText = "漫长的历史记录。"
	text, err = historyContent(context.Background(), d, "owner", "p", hit, false)
	if err != nil || text != "漫长…" {
		t.Fatalf("weak match expanded into long context: %q %v", text, err)
	}
	hit.Match = "semantic"
	hit.EvidenceText = "stale source that is no longer in the indexed document"
	hit.Snippet = hit.EvidenceText
	text, err = historyContent(context.Background(), d, "owner", "p", hit, true)
	if err != nil || text != "" {
		t.Fatalf("stale chunk was injected: %q %v", text, err)
	}

}

func TestOriginalEvidenceWindow(t *testing.T) {
	text := strings.Repeat("例行背景说明。", 40) + "Atlas 503 修复记录：" + strings.Repeat("核对现场版本。", 15) + "最终使用 atlas_route_epoch 验证快照。" + strings.Repeat("附录信息。", 80)
	h := model.SearchResult{Snippet: search.MarkOpen + "Atlas" + search.MarkClose + " 503 修复…"}
	got := evidenceWindow(text, highlightAnchor(h.Snippet))
	if !strings.Contains(got, "atlas_route_epoch") || len([]rune(got)) > completeHistoryChars {
		t.Fatalf("window lost evidence or exceeded bound: %q", got)
	}
}
