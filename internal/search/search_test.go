package search_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
)

const owner = "owner"

type fixture struct {
	t  *testing.T
	db *sql.DB
	n  int
}

var base = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f := &fixture{t: t, db: d}
	f.exec(`INSERT INTO users (user_id, created_at) VALUES (?, ?)`, owner, model.FormatTime(base))
	for _, dev := range []string{"linux-box", "mac-mini"} {
		f.exec(`INSERT INTO devices (device_id, user_id, name, platform, token_hash, created_at) VALUES (?, ?, ?, 'linux', ?, ?)`,
			dev, owner, dev, "h-"+dev, model.FormatTime(base))
	}
	for _, p := range []struct{ id, name, git string }{
		{"webapp", "webapp", "github.com/me/webapp"},
		{"billing", "billing", "github.com/me/billing"},
		{"notes", "notes", ""},
	} {
		var git any
		if p.git != "" {
			git = p.git
		}
		f.exec(`INSERT INTO projects (project_id, user_id, name, canonical_git_identity, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			p.id, owner, p.name, git, model.FormatTime(base), model.FormatTime(base))
	}
	for _, s := range []struct{ id, project, device, agent string }{
		{"s-web-1", "webapp", "linux-box", "claude-code"},
		{"s-web-2", "webapp", "mac-mini", "codex"},
		{"s-bill-1", "billing", "linux-box", "codex"},
		{"s-notes-1", "notes", "mac-mini", "claude-code"},
	} {
		f.exec(`INSERT INTO sessions (session_id, project_id, device_id, agent_type, native_session_id, started_at) VALUES (?, ?, ?, ?, ?, ?)`,
			s.id, s.project, s.device, s.agent, s.id, model.FormatTime(base))
	}

	// Corpus: realistic, mixed Chinese/English agent history.
	f.event("s-web-1", "user.prompt", map[string]any{"prompt": "认证模块登录接口经常超时，帮我排查一下"})
	f.event("s-web-1", "tool.failed", map[string]any{"tool_name": "Bash",
		"tool_input": map[string]any{"command": "go test ./internal/auth/..."},
		"error":      "--- FAIL: TestLogin (30.01s)\n    auth_test.go:42: context deadline exceeded"})
	f.event("s-web-1", "file.modified", map[string]any{"path": "/src/webapp/internal/auth/session.go", "tool_name": "Edit"})
	f.event("s-web-1", "agent.turn_completed", map[string]any{"last_assistant_message": "已修复认证超时：数据库连接池耗尽导致登录请求排队，调大连接池并加了超时控制。"})
	f.event("s-web-2", "user.prompt", map[string]any{"prompt": "继续之前没完成的认证模块重构，把 JWT 刷新令牌逻辑拆出去",
		"attachments": []any{map[string]any{"kind": "log", "errors": []any{"ERROR token refresh failed: signature is invalid"}}}})
	f.event("s-web-2", "tool.completed", map[string]any{"tool_name": "shell",
		"tool_input": map[string]any{"command": "gofmt -l ./internal/auth"}, "tool_response": "ignored output"})
	f.event("s-bill-1", "user.prompt", map[string]any{"prompt": "实现 Stripe webhook 的幂等处理，用 event_id 去重"})
	f.event("s-bill-1", "agent.turn_completed", map[string]any{"last_assistant_message": "Webhook handler now stores processed Stripe event IDs in a unique table, so retries are idempotent."})
	f.event("s-bill-1", "file.modified", map[string]any{"tool_name": "apply_patch", "files": []any{map[string]any{"path": "/src/billing/webhook/stripe.go"}}})
	f.event("s-notes-1", "user.prompt", map[string]any{"prompt": "整理一下灰度发布和熔断降级的笔记"})
	f.event("s-notes-1", "agent.turn_completed", map[string]any{"last_assistant_message": "已整理：灰度发布按 5%/25%/100% 推进；熔断后降级到只读缓存。"})
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(q, args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *fixture) event(session, typ string, payload map[string]any) string {
	f.t.Helper()
	f.n++
	id := fmt.Sprintf("evt_%04d_%s", f.n, session)
	var project, device, agent string
	f.db.QueryRow(`SELECT project_id, device_id, agent_type FROM sessions WHERE session_id = ?`, session).Scan(&project, &device, &agent)
	b, _ := json.Marshal(payload)
	at := base.Add(time.Duration(f.n) * time.Hour)
	f.exec(`INSERT INTO events (event_id, session_id, project_id, device_id, event_type, occurred_at, received_at, payload_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, session, project, device, typ, model.FormatTime(at), model.FormatTime(at), string(b))
	tx, _ := f.db.Begin()
	if err := search.IndexEvent(context.Background(), tx, search.Event{
		EventID: id, EventType: typ, ProjectID: project, SessionID: session, DeviceID: device,
		AgentType: agent, OccurredAt: at, Payload: b,
	}); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) search(req model.SearchRequest) *model.SearchResponse {
	f.t.Helper()
	resp, err := search.Search(context.Background(), f.db, owner, req)
	if err != nil {
		f.t.Fatalf("search %+v: %v", req, err)
	}
	return resp
}

func all(q string) model.SearchRequest {
	return model.SearchRequest{Query: q, Scope: model.ScopeAllProjects}
}

func describe(r *model.SearchResponse) string {
	var s []string
	for _, h := range r.Results {
		s = append(s, fmt.Sprintf("%s/%s[%s] %q", h.ProjectName, h.Kind, h.Match, h.Snippet))
	}
	return strings.Join(s, "\n")
}

func TestChineseTechnicalTerms(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		query, project, kind, snippetHas string
	}{
		{"幂等", "billing", search.KindPrompt, "[幂等]"},
		{"去重", "billing", search.KindPrompt, "[去重]"},
		{"熔断降级", "notes", search.KindPrompt, "[熔断降级]"},
		{"灰度发布", "notes", search.KindPrompt, "[灰度发布]"},
		{"连接池", "webapp", search.KindReply, "[连接池]"},
		{"刷新令牌", "webapp", search.KindPrompt, "[刷新令牌]"},
		{"idempotent", "billing", search.KindReply, "[idempotent]"},
		{"mideng", "billing", search.KindPrompt, "[幂等]"}, // pinyin
	}
	for _, c := range cases {
		r := f.search(all(c.query))
		if len(r.Results) == 0 {
			t.Errorf("%s: no results", c.query)
			continue
		}
		top := r.Results[0]
		if top.ProjectName != c.project || top.Kind != c.kind || !strings.Contains(top.Snippet, c.snippetHas) {
			t.Errorf("%s: top = %s/%s %q\n%s", c.query, top.ProjectName, top.Kind, top.Snippet, describe(r))
		}
	}
}

// Scenario: "where did I implement Stripe webhook idempotency?"
func TestCrossProjectQuestion(t *testing.T) {
	f := newFixture(t)
	r := f.search(all("我以前在哪个项目实现过 Stripe Webhook 的幂等处理？"))
	if len(r.Results) == 0 || r.Results[0].ProjectName != "billing" {
		t.Fatalf("results:\n%s", describe(r))
	}
	for _, h := range r.Results {
		if h.ProjectName == "" || h.RefID == "" || h.SessionID == "" || h.DeviceName == "" {
			t.Errorf("result lacks source attribution: %+v", h)
		}
	}
}

func TestAllTermsRankBeforeAnyTerm(t *testing.T) {
	f := newFixture(t)
	r := f.search(all("认证 超时"))
	if len(r.Results) < 3 {
		t.Fatalf("results:\n%s", describe(r))
	}
	sawAny := false
	for _, h := range r.Results {
		if h.Match == "any" {
			sawAny = true
		} else if sawAny {
			t.Fatalf("an all-terms hit ranked after a partial hit:\n%s", describe(r))
		}
	}
	if r.Results[0].Match != "all" || !sawAny {
		t.Errorf("expected all-term hits followed by partial hits:\n%s", describe(r))
	}
}

func TestTermCoverageMatchesIndependentFullTextLookups(t *testing.T) {
	f := newFixture(t)
	f.event("s-web-1", "agent.turn_completed", map[string]any{"last_assistant_message": "Repairing token refresh retries and recovering the login connection pool"})
	f.event("s-bill-1", "agent.turn_completed", map[string]any{"last_assistant_message": "Webhook repairs use retries for transient connection failures"})
	for _, query := range []string{"认证 超时", "renzheng 超时", "repair retries connection", "刷新令牌 signature login", "webhook token recovering"} {
		t.Run(query, func(t *testing.T) {
			req := all(query)
			req.Mode, req.Limit = "keyword", 3
			resp := f.search(req)
			if len(resp.Results) == 0 {
				t.Fatal("fixture did not exercise term coverage")
			}
			var expression string
			if err := f.db.QueryRow(`SELECT jieba_query(?)`, query).Scan(&expression); err != nil {
				t.Fatal(err)
			}
			terms := search.Terms(expression)
			want := map[int64]int{}
			// Deliberately omit candidate rowid constraints in the reference.
			// Pinyin alternatives, prefixes and porter stemming remain FTS's job.
			for _, term := range terms {
				rows, err := f.db.Query(`SELECT rowid FROM search_fts WHERE search_fts MATCH ?`, term)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						t.Fatal(err)
					}
					want[id]++
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, hit := range resp.Results {
				if hit.TotalTerms != len(terms) || hit.MatchedTerms != want[hit.DocID] {
					t.Fatalf("doc %d: matched %d/%d, want %d/%d", hit.DocID, hit.MatchedTerms, hit.TotalTerms, want[hit.DocID], len(terms))
				}
			}
		})
	}
}

func TestErrorsFilesAndCommands(t *testing.T) {
	f := newFixture(t)
	if r := f.search(all("context deadline exceeded")); len(r.Results) == 0 || r.Results[0].Kind != search.KindFailure {
		t.Errorf("failure text:\n%s", describe(r))
	}
	if r := f.search(all("signature invalid")); len(r.Results) == 0 || r.Results[0].Kind != search.KindPrompt {
		t.Errorf("pasted-log error line:\n%s", describe(r))
	}
	if r := f.search(all("session.go")); len(r.Results) == 0 || r.Results[0].Kind != search.KindChange {
		t.Errorf("file path:\n%s", describe(r))
	}
	if r := f.search(all("gofmt")); len(r.Results) != 1 || r.Results[0].Kind != search.KindCommand {
		t.Errorf("shell command:\n%s", describe(r))
	}
	if r := f.search(all("ignored output")); len(r.Results) != 0 {
		t.Errorf("command output must not be indexed:\n%s", describe(r))
	}
}

// Results never include content outside the requested scope.
func TestScopeIsolation(t *testing.T) {
	f := newFixture(t)
	cur := f.search(model.SearchRequest{Query: "幂等 认证 熔断", GitIdentity: "github.com/me/webapp"})
	if len(cur.Results) == 0 {
		t.Fatal("no results in current project")
	}
	for _, h := range cur.Results {
		if h.ProjectID != "webapp" {
			t.Errorf("leaked %s into current_project webapp", h.ProjectName)
		}
	}
	sel := f.search(model.SearchRequest{Query: "幂等 熔断", Scope: model.ScopeSelectedProjects, ProjectIDs: []string{"notes"}})
	for _, h := range sel.Results {
		if h.ProjectID != "notes" {
			t.Errorf("leaked %s into selected_projects [notes]", h.ProjectName)
		}
	}
	if len(sel.Results) == 0 {
		t.Error("no results in selected project")
	}
	// A project without a Git identity is resolved by name.
	if r := f.search(model.SearchRequest{Query: "灰度", ProjectName: "notes"}); len(r.Results) == 0 {
		t.Error("name-based current project")
	}
	if r := f.search(model.SearchRequest{Query: "幂等", GitIdentity: "github.com/me/unknown"}); len(r.Results) != 0 || r.Note == "" {
		t.Errorf("unknown project should return a note, got %+v", r)
	}
	// Another owner sees nothing.
	if r, err := search.Search(context.Background(), f.db, "someone-else", all("幂等")); err != nil || len(r.Results) != 0 {
		t.Errorf("other owner: %v %+v", err, r)
	}
}

func TestFilters(t *testing.T) {
	f := newFixture(t)
	if r := f.search(model.SearchRequest{Query: "认证", Scope: model.ScopeAllProjects, AgentTypes: []string{"codex"}}); len(r.Results) == 0 {
		t.Error("agent filter found nothing")
	} else {
		for _, h := range r.Results {
			if h.AgentType != "codex" {
				t.Errorf("agent filter leaked %s", h.AgentType)
			}
		}
	}
	if r := f.search(model.SearchRequest{Query: "认证", Scope: model.ScopeAllProjects, DeviceIDs: []string{"mac-mini"}}); len(r.Results) == 0 {
		t.Error("device filter found nothing")
	} else {
		for _, h := range r.Results {
			if h.DeviceID != "mac-mini" {
				t.Errorf("device filter leaked %s", h.DeviceID)
			}
		}
	}
	if r := f.search(model.SearchRequest{Query: "认证", Scope: model.ScopeAllProjects, Kinds: []string{search.KindReply}}); len(r.Results) != 1 {
		t.Errorf("kind filter:\n%s", describe(r))
	}
	since := base.Add(5 * time.Hour)
	for _, h := range f.search(model.SearchRequest{Query: "认证", Scope: model.ScopeAllProjects, Since: &since}).Results {
		if h.OccurredAt.Before(since) {
			t.Errorf("since filter leaked %v", h.OccurredAt)
		}
	}
}

func TestQueryEdgeCases(t *testing.T) {
	f := newFixture(t)
	for _, q := range []string{`"`, `NEAR(认证 超时)`, `认证" OR "x`, `*`, `-认证`, `(`, `body:认证`, "认证\x00超时", strings.Repeat("认证", 2000)} {
		if _, err := search.Search(context.Background(), f.db, owner, all(q)); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	if r := f.search(all("的了吗？")); len(r.Results) != 0 || r.Note == "" {
		t.Errorf("stopword-only query: %+v", r)
	}
	if _, err := search.Search(context.Background(), f.db, owner, model.SearchRequest{Query: "x", Scope: "galaxy"}); err == nil {
		t.Error("unknown scope accepted")
	}
	if r := f.search(model.SearchRequest{Query: "认证", Scope: model.ScopeAllProjects, Limit: 2}); len(r.Results) != 2 {
		t.Errorf("limit: %d", len(r.Results))
	}
}

func TestUpsertReplacesDocument(t *testing.T) {
	f := newFixture(t)
	put := func(body string) {
		tx, _ := f.db.Begin()
		if err := search.Upsert(context.Background(), tx, &search.Doc{
			Kind: search.KindSummary, RefID: "s-web-1", ProjectID: "webapp", SessionID: "s-web-1", OccurredAt: base, Body: body,
		}); err != nil {
			t.Fatal(err)
		}
		tx.Commit()
	}
	put("第一版摘要：排查认证超时")
	put("第二版摘要：已定位到连接池耗尽")
	if r := f.search(model.SearchRequest{Query: "排查", Scope: model.ScopeAllProjects, Kinds: []string{search.KindSummary}}); len(r.Results) != 0 {
		t.Errorf("stale summary text still indexed:\n%s", describe(r))
	}
	if r := f.search(model.SearchRequest{Query: "耗尽", Scope: model.ScopeAllProjects, Kinds: []string{search.KindSummary}}); len(r.Results) != 1 {
		t.Errorf("updated summary:\n%s", describe(r))
	}
	var n int
	f.db.QueryRow(`SELECT COUNT(*) FROM search_docs WHERE kind = 'summary'`).Scan(&n)
	if n != 1 {
		t.Errorf("summary docs = %d", n)
	}
}

func TestBackfill(t *testing.T) {
	f := newFixture(t)
	f.exec(`DELETE FROM search_docs`)
	f.exec(`INSERT INTO search_fts(search_fts) VALUES ('rebuild')`)
	if r := f.search(all("幂等")); len(r.Results) != 0 {
		t.Fatal("index not cleared")
	}
	n, err := search.Backfill(context.Background(), f.db)
	if err != nil || n == 0 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	if r := f.search(all("幂等")); len(r.Results) == 0 {
		t.Error("backfilled events not searchable")
	}
	if n, _ := search.Backfill(context.Background(), f.db); n != 0 {
		t.Errorf("second backfill indexed %d", n)
	}
}

func TestEnglishStopwordsDropped(t *testing.T) {
	got := search.Terms(`( w+h+y* OR why* ) AND ( d+i+d* OR did* ) AND ( l+s* OR ls* ) AND ( f+a+i+l* OR fail* ) AND before* AND "认证"`)
	if strings.Join(got, "|") != `( l+s* OR ls* )|( f+a+i+l* OR fail* )|"认证"` {
		t.Errorf("Terms = %q", got)
	}
}

func TestTerms(t *testing.T) {
	got := search.Terms(`( s+t+r+i+p+e* OR stripe* ) AND "的" AND "幂等" AND "." AND """" AND "？" AND "v1"`)
	want := []string{`( s+t+r+i+p+e* OR stripe* )`, `"幂等"`, `"v1"`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Terms = %q", got)
	}
}
