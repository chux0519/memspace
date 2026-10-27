package server_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/jobs"
	"github.com/chux0519/memspace/internal/memory"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/internal/summary"
)

// The UI read APIs: overview, project detail, event detail, memories with
// resolved sources, and client spool reports.
func TestUIReadAPIs(t *testing.T) {
	srv := newTestServer(t)
	dbv, _ := testDBs.Load(srv)
	database := dbv.(*sql.DB)
	tok := registerDevice(t, srv, "laptop")
	git := "github.com/me/webapp"
	var proj model.EnsureProjectResponse
	postJSON(t, srv, tok, "/api/v1/projects", model.EnsureProjectRequest{Name: "webapp", CanonicalGitIdentity: &git}, &proj)
	var sess model.EnsureSessionResponse
	branch, head := "main", "abc123"
	postJSON(t, srv, tok, "/api/v1/sessions", model.EnsureSessionRequest{
		ProjectID: proj.ProjectID, AgentType: "claude-code", NativeSessionID: "n1",
		Location: &model.LocationInput{AbsolutePath: "/home/me/webapp", GitBranch: &branch, GitHead: &head},
	}, &sess)

	at := time.Now().Add(-time.Hour)
	ev := func(id, typ, payload string) model.Event {
		at = at.Add(time.Minute)
		return model.Event{EventID: id, SessionID: &sess.SessionID, EventType: typ, OccurredAt: at, PayloadJSON: payload}
	}
	postBatch(t, srv, tok, proj.ProjectID, []model.Event{
		ev("evt_ui_0001", "user.prompt", `{"prompt":"修复 lint 报错"}`),
		ev("evt_ui_0002", "tool.failed", `{"tool_name":"Bash","tool_input":{"command":"golangci-lint run"},"error":"pool.go:3: unused import"}`),
		ev("evt_ui_0003", "file.modified", `{"path":"/src/pool.go","tool_name":"Edit"}`),
		ev("evt_ui_0004", "tool.completed", `{"tool_name":"Bash","tool_input":{"command":"golangci-lint run"},"tool_response":"ok"}`),
		ev("evt_ui_0005", "session.ended", `{"reason":"exit"}`),
	})
	runner := &jobs.Runner{DB: database, Logf: func(string, ...any) {}, Handlers: map[string]jobs.Handler{summary.JobKind: summary.Handler(database)}}
	if ran, err := runner.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("summary job: ran=%v err=%v", ran, err)
	}

	// Memories: one episodic for the session, one procedural fix.
	var mems []memory.Memory
	if code := getJSON(t, srv, tok, "/api/v1/memories?project_id="+proj.ProjectID, &mems); code != http.StatusOK || len(mems) != 2 {
		t.Fatalf("memories %d: %+v", code, mems)
	}
	// Paging one at a time visits both memories, although they were
	// generated together and share updated_at.
	var p1, p2, p3 []memory.Memory
	getJSON(t, srv, tok, "/api/v1/memories?limit=1", &p1)
	cur := func(m memory.Memory) string {
		return "&before=" + url.QueryEscape(m.UpdatedAt.Format(time.RFC3339Nano)) + "&before_id=" + m.ID
	}
	getJSON(t, srv, tok, "/api/v1/memories?limit=1"+cur(p1[0]), &p2)
	if len(p2) != 1 || p2[0].ID == p1[0].ID {
		t.Fatalf("memory page 2 = %+v (page 1 %s)", p2, p1[0].ID)
	}
	getJSON(t, srv, tok, "/api/v1/memories?limit=1"+cur(p2[0]), &p3)
	if len(p3) != 0 {
		t.Errorf("memory page 3 = %+v", p3)
	}
	var fix *memory.Memory
	for i := range mems {
		if mems[i].Type == memory.TypeProcedural {
			fix = &mems[i]
		}
	}
	if fix == nil || fix.ProjectName != "webapp" || fix.Status != memory.StatusCandidate {
		t.Fatalf("no fix memory: %+v", mems)
	}
	var md handler.MemoryDetail
	if code := getJSON(t, srv, tok, "/api/v1/memories/"+fix.ID, &md); code != http.StatusOK {
		t.Fatalf("memory detail %d", code)
	}
	var sawFailure bool
	for _, s := range md.Sources {
		if s.Missing {
			t.Errorf("unresolved source %+v", s)
		}
		if s.ID == "evt_ui_0002" && s.EventType == "tool.failed" && strings.Contains(s.Summary, "golangci-lint") {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Errorf("fix sources = %+v", md.Sources)
	}
	if code := getJSON(t, srv, tok, "/api/v1/memories/mem_nope", nil); code != http.StatusNotFound {
		t.Errorf("unknown memory: %d", code)
	}

	// Session list pages backwards by (last activity, id).
	postJSON(t, srv, tok, "/api/v1/sessions", model.EnsureSessionRequest{ProjectID: proj.ProjectID, AgentType: "codex", NativeSessionID: "n2"}, nil)
	var s1, s2 []model.SessionInfo
	getJSON(t, srv, tok, "/api/v1/sessions?limit=1", &s1)
	last := s1[0].StartedAt
	if s1[0].LastEventAt != nil {
		last = *s1[0].LastEventAt
	}
	getJSON(t, srv, tok, "/api/v1/sessions?limit=5&before="+url.QueryEscape(last.Format(time.RFC3339Nano))+"&before_id="+s1[0].SessionID, &s2)
	if len(s1) != 1 || len(s2) != 1 || s2[0].SessionID == s1[0].SessionID {
		t.Errorf("session pages = %+v / %+v", s1, s2)
	}

	// Event detail links back to its session.
	var ed handler.EventDetail
	if code := getJSON(t, srv, tok, "/api/v1/events/evt_ui_0002", &ed); code != http.StatusOK || ed.Session == nil || ed.Session.SessionID != sess.SessionID {
		t.Fatalf("event detail %d: %+v", code, ed)
	}

	// Project detail: location per device, stats, sessions.
	var pd handler.ProjectDetail
	if code := getJSON(t, srv, tok, "/api/v1/projects/"+proj.ProjectID, &pd); code != http.StatusOK {
		t.Fatalf("project detail %d", code)
	}
	if len(pd.Locations) != 1 || pd.Locations[0].DeviceName != "laptop" || pd.Locations[0].GitHead != "abc123" ||
		pd.Stats.Events != 5 || pd.Stats.Memories != 2 || len(pd.Sessions) != 2 {
		t.Fatalf("project detail = %+v", pd)
	}

	// Spool report shows up in the overview.
	oldest := time.Now().Add(-2 * time.Hour).UTC()
	if code := postJSON(t, srv, tok, "/api/v1/sync/report", model.SyncReport{Pending: 7, Dead: 1, OldestPending: &oldest, ClientVersion: "test"}, nil); code != http.StatusNoContent {
		t.Fatalf("sync report %d", code)
	}
	var ov handler.Overview
	if code := getJSON(t, srv, tok, "/api/v1/overview", &ov); code != http.StatusOK {
		t.Fatalf("overview %d", code)
	}
	if ov.Counts.Projects != 1 || ov.Counts.EventsAll != 5 || ov.Counts.Memories != 2 || len(ov.RecentSessions) != 2 || len(ov.LatestMemories) != 2 {
		t.Errorf("overview counts = %+v", ov.Counts)
	}
	if len(ov.Devices) != 1 || ov.Devices[0].SpoolPending == nil || *ov.Devices[0].SpoolPending != 7 || ov.Devices[0].ClientVersion != "test" || ov.Devices[0].EventsTotal != 5 {
		t.Errorf("overview devices = %+v", ov.Devices)
	}
	if ov.Embedding.Enabled {
		t.Errorf("embedding should be disabled")
	}
}

type uiBrowser struct {
	t   *testing.T
	srv string
	c   *http.Client
}

func newBrowser(t *testing.T, srvURL string) *uiBrowser {
	jar, _ := cookiejar.New(nil)
	return &uiBrowser{t: t, srv: srvURL, c: &http.Client{Jar: jar}}
}

func (b *uiBrowser) do(method, path, body string, header bool) int {
	b.t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, b.srv+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if header {
		req.Header.Set(auth.UIHeader, "1")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestUILogin(t *testing.T) {
	srv := newTestServer(t)
	dbv, _ := testDBs.Load(srv)
	database := dbv.(*sql.DB)
	tok := registerDevice(t, srv, "laptop")

	var link handler.LoginLinkResponse
	if code := postJSON(t, srv, tok, "/api/v1/ui/login-links", struct{}{}, &link); code != http.StatusCreated || !strings.HasPrefix(link.Path, "/#/login/msl_") {
		t.Fatalf("login link %d: %+v", code, link)
	}
	if code := postJSON(t, srv, "bad", "/api/v1/ui/login-links", struct{}{}, nil); code != http.StatusUnauthorized {
		t.Errorf("login link without device token: %d", code)
	}

	b := newBrowser(t, srv.URL)
	if code := b.do("GET", "/api/v1/overview", "", true); code != http.StatusUnauthorized {
		t.Errorf("before login: %d", code)
	}
	body := `{"code":"` + link.Code + `"}`
	if code := b.do("POST", "/api/v1/ui/login", body, false); code != http.StatusForbidden {
		t.Errorf("login without UI header: %d", code)
	}
	if code := b.do("POST", "/api/v1/ui/login", body, true); code != http.StatusNoContent {
		t.Fatalf("login: %d", code)
	}
	if code := newBrowser(t, srv.URL).do("POST", "/api/v1/ui/login", body, true); code != http.StatusUnauthorized {
		t.Errorf("reused login code: %d", code)
	}
	for _, p := range []string{"/api/v1/ui/session", "/api/v1/overview", "/api/v1/projects", "/api/v1/sessions", "/api/v1/memories", "/api/v1/devices", "/api/v1/embedding/status"} {
		if code := b.do("GET", p, "", true); code != http.StatusOK && code != http.StatusNoContent {
			t.Errorf("GET %s with session: %d", p, code)
		}
	}
	if code := b.do("POST", "/api/v1/search", `{"query":"x","scope":"all_projects"}`, true); code != http.StatusOK {
		t.Errorf("search with session: %d", code)
	}
	// The cookie alone (as a cross-site request would send it) is not enough.
	if code := b.do("GET", "/api/v1/overview", "", false); code != http.StatusForbidden {
		t.Errorf("session without UI header: %d", code)
	}
	// A UI session cannot write or act as a device.
	for _, p := range []string{"/api/v1/projects", "/api/v1/events/batch", "/api/v1/sync/report", "/api/v1/devices/mcp-token", "/api/v1/ui/login-links"} {
		if code := b.do("POST", p, `{}`, true); code != http.StatusUnauthorized {
			t.Errorf("POST %s with session: %d", p, code)
		}
	}

	// Expired codes are rejected.
	code, _ := auth.CreateLoginCode(context.Background(), database, ownerUserID, "", auth.RoleAdmin)
	database.Exec(`UPDATE ui_login_links SET expires_at = ? WHERE link_hash = ?`, model.FormatTime(time.Now().Add(-time.Second)), auth.HashToken(code))
	if got := newBrowser(t, srv.URL).do("POST", "/api/v1/ui/login", `{"code":"`+code+`"}`, true); got != http.StatusUnauthorized {
		t.Errorf("expired code: %d", got)
	}

	// Logout ends the session.
	if code := b.do("POST", "/api/v1/ui/logout", "", true); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	if code := b.do("GET", "/api/v1/overview", "", true); code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", code)
	}

	// Revoking the device that issued the link ends its sessions.
	postJSON(t, srv, tok, "/api/v1/ui/login-links", struct{}{}, &link)
	b2 := newBrowser(t, srv.URL)
	if code := b2.do("POST", "/api/v1/ui/login", `{"code":"`+link.Code+`"}`, true); code != http.StatusNoContent {
		t.Fatalf("login 2: %d", code)
	}
	deviceID, _ := auth.VerifyToken(context.Background(), database, tok)
	auth.RevokeDevice(context.Background(), database, deviceID)
	if code := b2.do("GET", "/api/v1/overview", "", true); code != http.StatusUnauthorized {
		t.Errorf("session of revoked device: %d", code)
	}

	// Operator links (no device) work too.
	code, _ = auth.CreateLoginCode(context.Background(), database, ownerUserID, "", auth.RoleAdmin)
	b3 := newBrowser(t, srv.URL)
	if got := b3.do("POST", "/api/v1/ui/login", `{"code":"`+code+`"}`, true); got != http.StatusNoContent {
		t.Fatalf("operator login: %d", got)
	}
	if got := b3.do("GET", "/api/v1/overview", "", true); got != http.StatusOK {
		t.Errorf("operator session: %d", got)
	}
}

func TestStaticUI(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("index: %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = http.Get(srv.URL + "/api/v1/nope")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("unknown api: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
