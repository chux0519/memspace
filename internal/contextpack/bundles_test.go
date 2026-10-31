package contextpack_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/model"
)

func captureEvidence(t *testing.T, f *fx) {
	t.Helper()
	rows, e := f.db.Query(`SELECT event_id FROM events ORDER BY event_id DESC`)
	if e != nil {
		t.Fatal(e)
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	rows.Close()
	ctx := evidence.WithPolicy(context.Background(), evidence.Policy{Mode: "shadow"})
	tx, e := f.db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	for _, id := range ids {
		if e = evidence.IndexEvent(ctx, tx, id); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}
func replyHit(t *testing.T, f *fx, id string) model.SearchResult {
	t.Helper()
	var h model.SearchResult
	e := f.db.QueryRow(`SELECT doc_id,kind,ref_id,project_id,session_id FROM search_docs WHERE ref_id=? AND kind='reply'`, id).Scan(&h.DocID, &h.Kind, &h.RefID, &h.ProjectID, &h.SessionID)
	if e != nil {
		t.Fatal(e)
	}
	h.Match = "all"
	h.MatchedTerms = 2
	h.TotalTerms = 2
	return h
}
func TestRelationBundlesPreserveConditionsAndShadowParity(t *testing.T) {
	f := setup(t)
	f.session("old", "linux-box", "codex", "ended")
	f.event("old", time.Hour, "user.prompt", map[string]any{"prompt": "超市测试环境连接池改为8，生产保持4。", "turn_id": "t1"})
	f.event("old", time.Hour, "user.prompt", map[string]any{"prompt": "另一件事：生产缓存保持原样。", "turn_id": "t2"})
	f.event("old", time.Hour, "agent.turn_completed", map[string]any{"last_assistant_message": "已经改好了。", "turn_id": "t1"})
	h := replyHit(t, f, fmt.Sprintf("evt_ctx_%04d", f.n))
	captureEvidence(t, f)
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: webapp, Query: "超市测试环境连接池是多少？"}
	build := func(mode string) *contextpack.Pack {
		p, e := contextpack.BuildFromCandidates(evidence.WithPolicy(context.Background(), evidence.Policy{Mode: mode}), f.db, owner, "linux-box", req, []model.SearchResult{h}, nil)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	off, shadow := build("off"), build("shadow")
	a, _ := json.Marshal(off)
	b, _ := json.Marshal(shadow)
	if string(a) != string(b) {
		t.Fatal("shadow changed pack")
	}
	on := build("on")
	if len(on.Items) != 2 || len(on.EvidenceGroups) != 1 || !on.EvidenceGroups[0].Complete {
		t.Fatalf("%+v", on)
	}
	if !strings.Contains(on.Text, "生产保持4") || !strings.Contains(on.Text, "已经改好了") || strings.Contains(on.Text, "生产缓存") {
		t.Fatal(on.Text)
	}
	if utf8.RuneCountInString(on.Text) > contextpack.PromptBudget {
		t.Fatal("budget")
	}
	req.Budget = 500
	p := build("on")
	if len(p.Items) != 0 || len(p.EvidenceGroups) != 0 || !p.Truncated {
		t.Fatalf("split bundle: %+v", p)
	}
	req.Budget = 6000
	req.Exclude = []string{on.Items[0].Key}
	p = build("on")
	if len(p.Items) != 0 || len(p.EvidenceGroups) != 0 {
		t.Fatal("excluded only one endpoint")
	}
}
func TestRelationBundleHonorsItemBudgetAndMarksPartial(t *testing.T) {
	f := setup(t)
	f.session("old", "linux-box", "codex", "ended")
	f.event("old", time.Hour, "user.prompt", map[string]any{"prompt": strings.Repeat("条件长文本中文。", 120), "turn_id": "t1"})
	f.event("old", time.Hour, "agent.turn_completed", map[string]any{"last_assistant_message": "supermarket staging pool set to8", "turn_id": "t1"})
	captureEvidence(t, f)
	ctx := evidence.WithPolicy(context.Background(), evidence.Policy{Mode: "on"})
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: webapp, Query: "supermarket staging pool"}
	p, e := contextpack.BuildWithOptions(ctx, f.db, owner, "linux-box", req, contextpack.BuildOptions{MaxItems: 1})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) > 1 || len(p.EvidenceGroups) > 0 {
		t.Fatal("split item cap", p)
	}
	p, e = contextpack.BuildWithOptions(ctx, f.db, owner, "linux-box", req, contextpack.BuildOptions{MaxItems: 2})
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 2 || len(p.EvidenceGroups) != 1 || p.EvidenceGroups[0].Complete {
		t.Fatalf("partial group: %+v", p)
	}
	if !strings.Contains(p.Text, "excerpt") || !strings.Contains(p.Text, "completeness=false") {
		t.Fatal(p.Text)
	}
}
