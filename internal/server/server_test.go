// Integration tests for the MemSpace server (M1 acceptance).
// Package is server_test (external) so it can import both server and handler
// without creating an import cycle.
package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/jobs"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/internal/summary"
)

const ownerUserID = "test-owner"

// testDBs lets helpers reach the database behind a test server (e.g. to
// create invites the way `memspace server invite` does).
var testDBs sync.Map // *httptest.Server → *sql.DB

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ts := httptest.NewServer(server.New(database, ownerUserID))
	t.Cleanup(ts.Close)
	testDBs.Store(ts, database)
	return ts
}

func invite(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	database, _ := testDBs.Load(srv)
	code, err := auth.CreateInvite(context.Background(), database.(*sql.DB), ownerUserID, time.Hour)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return code
}

func postRegister(t *testing.T, srv *httptest.Server, req model.RegisterDeviceRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(srv.URL+"/api/v1/devices", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("register device: %v", err)
	}
	return resp
}

func registerDevice(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	resp := postRegister(t, srv, model.RegisterDeviceRequest{Name: name, Platform: "linux", InviteCode: invite(t, srv)})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var r model.RegisterDeviceResponse
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Token == "" {
		t.Fatal("empty token")
	}
	return r.Token
}

func ensureProject(t *testing.T, srv *httptest.Server, token, name string) string {
	t.Helper()
	body, _ := json.Marshal(model.EnsureProjectRequest{Name: name})
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/projects", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	defer resp.Body.Close()
	var r model.EnsureProjectResponse
	json.NewDecoder(resp.Body).Decode(&r)
	return r.ProjectID
}

func postBatch(t *testing.T, srv *httptest.Server, token, projectID string, events []model.Event) model.BatchEventsResponse {
	t.Helper()
	body, _ := json.Marshal(model.BatchEventsRequest{ProjectID: projectID, Events: events})
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/events/batch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post batch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var r model.BatchEventsResponse
	json.NewDecoder(resp.Body).Decode(&r)
	return r
}

func randomEventID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

func makeEvents(projectID string, ids []string) []model.Event {
	events := make([]model.Event, len(ids))
	for i, id := range ids {
		events[i] = model.Event{
			EventID:     id,
			ProjectID:   projectID,
			EventType:   "tool.completed",
			OccurredAt:  time.Now().UTC(),
			PayloadJSON: fmt.Sprintf(`{"seq":%d}`, i),
		}
	}
	return events
}

// TestHealth verifies the health endpoint is reachable without auth.
func TestHealth(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var h map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&h)
	if h["status"] != "ok" {
		t.Fatalf("expected ok, got %v", h["status"])
	}
}

// TestDeviceAuth verifies registration, token auth, and rejection of bad tokens.
func TestDeviceAuth(t *testing.T) {
	srv := newTestServer(t)

	// No token → 401.
	resp, err := http.Get(srv.URL + "/api/v1/devices")
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	tok := registerDevice(t, srv, "device-a")
	registerDevice(t, srv, "device-b")

	// Valid token → 200, two devices.
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var devices []model.Device
	json.NewDecoder(resp.Body).Decode(&devices)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}

	// Tampered token → 401.
	req, _ = http.NewRequest("GET", srv.URL+"/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok+"X")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered token, got %d", resp.StatusCode)
	}
}

// TestEnsureProjectIdempotent verifies the same name returns the same project_id.
func TestEnsureProjectIdempotent(t *testing.T) {
	srv := newTestServer(t)
	tok := registerDevice(t, srv, "device-a")
	id1 := ensureProject(t, srv, tok, "proj-1")
	id2 := ensureProject(t, srv, tok, "proj-1")
	if id1 != id2 {
		t.Fatalf("expected same project id, got %s vs %s", id1, id2)
	}
}

// TestBatchEventsDeduplication is the M1 acceptance test:
// Two devices concurrently upload events including overlapping ids.
// The server must deduplicate by event_id; each unique event is accepted once.
func TestBatchEventsDeduplication(t *testing.T) {
	srv := newTestServer(t)

	tokA := registerDevice(t, srv, "device-a")
	tokB := registerDevice(t, srv, "device-b")

	projID := ensureProject(t, srv, tokA, "project-x")
	projB := ensureProject(t, srv, tokB, "project-x")
	if projID != projB {
		t.Fatalf("same project name must resolve to same id, got %s vs %s", projID, projB)
	}

	const nUnique = 100
	const nDup = 20
	const nNew = 20

	// 100 unique event ids for device A.
	eventIDs := make([]string, nUnique)
	for i := range eventIDs {
		eventIDs[i] = randomEventID()
	}

	// Device B sends the last 20 of A's events (potential dups) + 20 fresh ones.
	dupIDs := eventIDs[nUnique-nDup:]
	newIDs := make([]string, nNew)
	for i := range newIDs {
		newIDs[i] = randomEventID()
	}
	bIDs := append(append([]string{}, dupIDs...), newIDs...)

	var wg sync.WaitGroup
	var respA, respB model.BatchEventsResponse

	wg.Add(2)
	go func() {
		defer wg.Done()
		respA = postBatch(t, srv, tokA, projID, makeEvents(projID, eventIDs))
	}()
	go func() {
		defer wg.Done()
		respB = postBatch(t, srv, tokB, projID, makeEvents(projID, bIDs))
	}()
	wg.Wait()

	// Each device must report either accepted or duplicate for every event sent.
	if respA.Accepted+respA.Duplicate != nUnique {
		t.Fatalf("device A: expected %d total receipts, got accepted=%d dup=%d err=%d",
			nUnique, respA.Accepted, respA.Duplicate, respA.Errors)
	}
	if respA.Errors != 0 {
		t.Fatalf("device A: unexpected errors: %d", respA.Errors)
	}

	if respB.Accepted+respB.Duplicate != nDup+nNew {
		t.Fatalf("device B: expected %d total receipts, got accepted=%d dup=%d err=%d",
			nDup+nNew, respB.Accepted, respB.Duplicate, respB.Errors)
	}
	if respB.Errors != 0 {
		t.Fatalf("device B: unexpected errors: %d", respB.Errors)
	}

	// Across both batches, each unique event_id must be accepted exactly once.
	acceptedSet := map[string]bool{}
	for _, r := range respA.Receipts {
		if r.Status == "accepted" {
			acceptedSet[r.EventID] = true
		}
	}
	for _, r := range respB.Receipts {
		if r.Status == "accepted" {
			if acceptedSet[r.EventID] {
				t.Fatalf("event %s was accepted twice (duplicate not detected)", r.EventID)
			}
			acceptedSet[r.EventID] = true
		}
	}
	expectedTotal := nUnique + nNew
	if len(acceptedSet) != expectedTotal {
		t.Fatalf("expected %d total accepted events, got %d", expectedTotal, len(acceptedSet))
	}
}

// TestBatchRequiresProjectID validates input rejection.
func TestBatchRequiresProjectID(t *testing.T) {
	srv := newTestServer(t)
	tok := registerDevice(t, srv, "device-a")

	body, _ := json.Marshal(model.BatchEventsRequest{
		ProjectID: "",
		Events:    []model.Event{{EventID: randomEventID(), EventType: "x", PayloadJSON: `{}`}},
	})
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/events/batch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// TestProtectedRoutesNeedAuth spot-checks that auth-gated routes enforce auth.
func TestProtectedRoutesNeedAuth(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/api/v1/projects", "/api/v1/devices"} {
		resp, _ := http.Get(srv.URL + path)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401 without token, got %d", path, resp.StatusCode)
		}
	}
}

// Registration needs a valid, unused, unexpired invite; one invite registers
// exactly one device.
func TestRegistrationRequiresInvite(t *testing.T) {
	srv := newTestServer(t)
	status := func(req model.RegisterDeviceRequest) int {
		resp := postRegister(t, srv, req)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(model.RegisterDeviceRequest{Name: "a", Platform: "linux"}); got != http.StatusForbidden {
		t.Errorf("no invite: %d", got)
	}
	if got := status(model.RegisterDeviceRequest{Name: "a", Platform: "linux", InviteCode: "msi_bogus"}); got != http.StatusForbidden {
		t.Errorf("bogus invite: %d", got)
	}
	code := invite(t, srv)
	if got := status(model.RegisterDeviceRequest{Name: "a", Platform: "linux", InviteCode: code}); got != http.StatusCreated {
		t.Fatalf("valid invite: %d", got)
	}
	if got := status(model.RegisterDeviceRequest{Name: "b", Platform: "linux", InviteCode: code}); got != http.StatusForbidden {
		t.Errorf("reused invite: %d", got)
	}

	database, _ := testDBs.Load(srv)
	expired, err := auth.CreateInvite(context.Background(), database.(*sql.DB), ownerUserID, -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := status(model.RegisterDeviceRequest{Name: "c", Platform: "linux", InviteCode: expired}); got != http.StatusForbidden {
		t.Errorf("expired invite: %d", got)
	}
	var devices int
	database.(*sql.DB).QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&devices)
	if devices != 1 {
		t.Errorf("devices = %d, want 1 (failed registrations must not leave rows)", devices)
	}
}

func TestRevokedDeviceIsRejected(t *testing.T) {
	srv := newTestServer(t)
	tok := registerDevice(t, srv, "laptop")
	database, _ := testDBs.Load(srv)
	deviceID, err := auth.VerifyToken(context.Background(), database.(*sql.DB), tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.RevokeDevice(context.Background(), database.(*sql.DB), deviceID); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/devices", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("401 content type = %q", ct)
	}
	if err := auth.RevokeDevice(context.Background(), database.(*sql.DB), "nope"); !errors.Is(err, auth.ErrUnknownDevice) {
		t.Errorf("revoke unknown: %v", err)
	}
}

// Stored timestamps must sort chronologically as text.
func TestTimeFormatSortsChronologically(t *testing.T) {
	base := time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(100 * time.Millisecond), base.Add(500 * time.Millisecond), base.Add(500*time.Millisecond + 10), base.Add(time.Second)}
	for i := 1; i < len(times); i++ {
		a, b := model.FormatTime(times[i-1]), model.FormatTime(times[i])
		if !(a < b) {
			t.Errorf("%s !< %s", a, b)
		}
		if got, err := time.Parse(time.RFC3339Nano, b); err != nil || !got.Equal(times[i]) {
			t.Errorf("round trip %s: %v %v", b, got, err)
		}
	}
}

func postJSON(t *testing.T, srv *httptest.Server, token, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// Events ingested through the API are searchable, in Chinese, with source
// attribution; duplicates are not indexed twice.
func TestIngestedEventsAreSearchable(t *testing.T) {
	srv := newTestServer(t)
	tok := registerDevice(t, srv, "laptop")
	git := "github.com/me/billing"
	var proj model.EnsureProjectResponse
	postJSON(t, srv, tok, "/api/v1/projects", model.EnsureProjectRequest{Name: "billing", CanonicalGitIdentity: &git}, &proj)
	var sess model.EnsureSessionResponse
	postJSON(t, srv, tok, "/api/v1/sessions", model.EnsureSessionRequest{ProjectID: proj.ProjectID, AgentType: "codex", NativeSessionID: "n1"}, &sess)

	events := []model.Event{
		{EventID: "evt_search_0001", SessionID: &sess.SessionID, EventType: "user.prompt", OccurredAt: time.Now(),
			PayloadJSON: `{"prompt":"实现 Stripe webhook 的幂等处理"}`},
		{EventID: "evt_search_0002", SessionID: &sess.SessionID, EventType: "tool.completed", OccurredAt: time.Now(),
			PayloadJSON: `{"tool_name":"shell","tool_input":{"command":"go test ./webhook/..."},"tool_response":"ok"}`},
	}
	for i := 0; i < 2; i++ { // second round: duplicates
		postBatch(t, srv, tok, proj.ProjectID, events)
	}

	var resp model.SearchResponse
	if code := postJSON(t, srv, tok, "/api/v1/search", model.SearchRequest{Query: "幂等", GitIdentity: git}, &resp); code != http.StatusOK {
		t.Fatalf("search status %d", code)
	}
	if len(resp.Results) != 1 || resp.Results[0].RefID != "evt_search_0001" || resp.Results[0].DeviceName != "laptop" || resp.Results[0].AgentType != "codex" {
		t.Fatalf("results = %+v", resp.Results)
	}
	if code := postJSON(t, srv, tok, "/api/v1/search", model.SearchRequest{Query: "x", Scope: "nope"}, nil); code != http.StatusBadRequest {
		t.Errorf("bad scope: %d", code)
	}
	if code := postJSON(t, srv, "bad-token", "/api/v1/search", model.SearchRequest{Query: "x"}, nil); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated search: %d", code)
	}
}

func getJSON(t *testing.T, srv *httptest.Server, token, path string, out any) int {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func TestSessionSummaryLifecycle(t *testing.T) {
	srv := newTestServer(t)
	dbv, _ := testDBs.Load(srv)
	database := dbv.(*sql.DB)
	tok := registerDevice(t, srv, "laptop")
	git := "github.com/me/webapp"
	var proj model.EnsureProjectResponse
	postJSON(t, srv, tok, "/api/v1/projects", model.EnsureProjectRequest{Name: "webapp", CanonicalGitIdentity: &git}, &proj)
	var sess model.EnsureSessionResponse
	postJSON(t, srv, tok, "/api/v1/sessions", model.EnsureSessionRequest{ProjectID: proj.ProjectID, AgentType: "claude-code", NativeSessionID: "native-1"}, &sess)

	at := time.Now().Add(-time.Hour)
	ev := func(id, typ, payload string) model.Event {
		at = at.Add(time.Minute)
		return model.Event{EventID: id, SessionID: &sess.SessionID, EventType: typ, OccurredAt: at, PayloadJSON: payload}
	}
	postBatch(t, srv, tok, proj.ProjectID, []model.Event{
		ev("evt_sum_0001", "user.prompt", `{"prompt":"修复登录接口的认证超时问题"}`),
		ev("evt_sum_0002", "file.modified", `{"path":"/src/auth/pool.go","tool_name":"Edit"}`),
		ev("evt_sum_0003", "tool.failed", `{"tool_name":"Bash","tool_input":{"command":"golangci-lint run"},"error":"pool.go:3: unused import"}`),
		ev("evt_sum_0004", "agent.turn_completed", `{"last_assistant_message":"连接池调大后认证不再超时。下一步：修复 lint 报错。"}`),
		ev("evt_sum_0005", "session.ended", `{"reason":"exit"}`),
	})

	// Before the summary exists, the list takes the requests from the prompts.
	var pending []model.SessionInfo
	getJSON(t, srv, tok, "/api/v1/sessions?git_identity="+git, &pending)
	if len(pending) != 1 || pending[0].Objective != "修复登录接口的认证超时问题" || pending[0].LatestRequest != "修复登录接口的认证超时问题" {
		t.Fatalf("list before the summary = %+v", pending)
	}

	// session.ended makes the summary job due immediately.
	runner := &jobs.Runner{DB: database, Logf: func(string, ...any) {}, Handlers: map[string]jobs.Handler{summary.JobKind: summary.Handler(database)}}
	if ran, err := runner.RunOnce(context.Background()); !ran || err != nil {
		t.Fatalf("summary job: ran=%v err=%v", ran, err)
	}

	var list []model.SessionInfo
	getJSON(t, srv, tok, "/api/v1/sessions?git_identity="+git, &list)
	if len(list) != 1 || list[0].Objective != "修复登录接口的认证超时问题" || list[0].Status != "ended" || list[0].EventCount != 5 || list[0].DeviceName != "laptop" {
		t.Fatalf("list = %+v", list)
	}

	var detail handler.SessionDetail
	if code := getJSON(t, srv, tok, "/api/v1/sessions/"+sess.SessionID, &detail); code != http.StatusOK {
		t.Fatalf("detail status %d", code)
	}
	s := detail.Summary
	if s == nil || s.Status != "ended" || len(s.ChangedFiles) != 1 || len(s.Unresolved) != 1 || len(s.NextSteps) != 1 || s.Disclaimer == "" {
		t.Fatalf("summary = %+v", s)
	}

	var found model.SearchResponse
	postJSON(t, srv, tok, "/api/v1/search", model.SearchRequest{Query: "连接池", GitIdentity: git, Kinds: []string{"summary"}}, &found)
	if len(found.Results) != 1 || found.Results[0].RefID != sess.SessionID {
		t.Errorf("summary not searchable: %+v", found.Results)
	}

	// A new event makes the stored summary stale; the detail recomputes.
	postBatch(t, srv, tok, proj.ProjectID, []model.Event{ev("evt_sum_0006", "user.prompt", `{"prompt":"顺便把 lint 也修掉"}`)})
	getJSON(t, srv, tok, "/api/v1/sessions/"+sess.SessionID, &detail)
	if detail.Summary.LatestRequest != "顺便把 lint 也修掉" || detail.Summary.EventCount != 6 {
		t.Errorf("stale summary served: %+v", detail.Summary)
	}

	// Timeline pagination.
	var page model.EventPage
	getJSON(t, srv, tok, "/api/v1/sessions/"+sess.SessionID+"/events?limit=4", &page)
	if len(page.Events) != 4 || page.Next == "" || page.Events[0].EventID != "evt_sum_0001" {
		t.Fatalf("page 1 = %+v", page)
	}
	var page2 model.EventPage
	getJSON(t, srv, tok, "/api/v1/sessions/"+sess.SessionID+"/events?limit=4&cursor="+page.Next, &page2)
	if len(page2.Events) != 2 || page2.Next != "" || page2.Events[0].EventID != "evt_sum_0005" {
		t.Fatalf("page 2 = %+v", page2)
	}

	if code := getJSON(t, srv, tok, "/api/v1/sessions/nope", nil); code != http.StatusNotFound {
		t.Errorf("unknown session: %d", code)
	}
}
