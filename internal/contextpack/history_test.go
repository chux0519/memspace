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
	text, err := historyContent(context.Background(), d, "owner", "p", hit)
	if err != nil || !strings.Contains(text, "在事务中更新 used_at") {
		t.Fatalf("missing complete evidence: %q %v", text, err)
	}
	for _, scope := range [][2]string{{"other", "p"}, {"owner", "outside"}} {
		text, err = historyContent(context.Background(), d, scope[0], scope[1], hit)
		if err != nil || text != "" {
			t.Fatalf("scope leak: %q %v", text, err)
		}
	}
	hit.DocID = longID
	hit.Snippet = "查询附近的匹配片段"
	text, err = historyContent(context.Background(), d, "owner", "p", hit)
	if err != nil || text != hit.Snippet {
		t.Fatalf("long source expanded without a bound: %q %v", text, err)
	}
}
