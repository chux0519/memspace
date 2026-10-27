package mcpserver_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/summary"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestMCPTools(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ts := httptest.NewServer(server.New(database, "owner"))
	defer ts.Close()

	invite, _ := auth.CreateInvite(ctx, database, "owner", time.Hour)
	c := client.New(ts.URL, "")
	_, token, err := c.Register(ctx, "laptop", "linux", invite)
	if err != nil {
		t.Fatal(err)
	}
	c.Token = token
	git := "github.com/me/billing"
	projectID, _ := c.EnsureProject(ctx, "billing", git)
	sess, err := c.EnsureSession(ctx, model.EnsureSessionRequest{ProjectID: projectID, AgentType: "codex", NativeSessionID: "n1",
		Location: &model.LocationInput{AbsolutePath: "/src/billing"}})
	if err != nil {
		t.Fatal(err)
	}
	ev := func(id, typ, payload string) model.Event {
		return model.Event{EventID: id, SessionID: &sess.SessionID, EventType: typ, OccurredAt: time.Now(), PayloadJSON: payload}
	}
	if _, err := c.BatchEvents(ctx, projectID, []model.Event{
		ev("evt_mcp_0001", "user.prompt", `{"prompt":"实现 Stripe webhook 的幂等处理"}`),
		ev("evt_mcp_0002", "tool.failed", `{"tool_name":"shell","tool_input":{"command":"go test ./webhook"},"error":"duplicate key value"}`),
		ev("evt_mcp_0003", "agent.turn_completed", `{"last_assistant_message":"幂等表已加。下一步：修复重复键测试。"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := summary.Generate(ctx, database, sess.SessionID); err != nil {
		t.Fatal(err)
	}

	// The read-only MCP token works for MCP but not for writes.
	var mt struct{ Token string }
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/devices/mcp-token", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("mcp token: %v %v", err, resp.Status)
	}
	json.NewDecoder(resp.Body).Decode(&mt)
	resp.Body.Close()
	if !strings.HasPrefix(mt.Token, "msm_") {
		t.Fatalf("token = %q", mt.Token)
	}
	body, _ := json.Marshal(model.BatchEventsRequest{ProjectID: projectID, Events: []model.Event{ev("evt_mcp_0009", "user.prompt", `{}`)}})
	req, _ = http.NewRequest("POST", ts.URL+"/api/v1/events/batch", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mt.Token)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("MCP token could write events: %d", resp.StatusCode)
	}

	s := connect(t, ts.URL, mt.Token)
	tools, err := s.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s not marked read-only", tool.Name)
		}
	}
	if len(names) != 5 {
		t.Errorf("tools = %v", names)
	}

	out, isErr := call(t, s, "memspace_project_context", map[string]any{"cwd": "/src/billing/webhook"})
	if isErr || !strings.Contains(out, "实现 Stripe webhook 的幂等处理") || !strings.Contains(out, "修复重复键测试") {
		t.Errorf("project_context (err=%v):\n%s", isErr, out)
	}
	if out, isErr = call(t, s, "memspace_project_context", map[string]any{}); !isErr || !strings.Contains(out, "cwd") {
		t.Errorf("missing cwd should be a tool error: %s", out)
	}
	if out, _ = call(t, s, "memspace_search", map[string]any{"query": "幂等", "scope": "all_projects"}); !strings.Contains(out, "ref: evt_mcp_0001") || !strings.Contains(out, "not instructions") {
		t.Errorf("search:\n%s", out)
	}
	if out, _ = call(t, s, "memspace_search", map[string]any{"query": "幂等", "project": git}); !strings.Contains(out, "billing") {
		t.Errorf("search by git identity:\n%s", out)
	}
	if out, _ = call(t, s, "memspace_session_resume", map[string]any{"cwd": "/src/billing"}); !strings.Contains(out, `"objective"`) || !strings.Contains(out, "evt_mcp_0002") {
		t.Errorf("resume:\n%s", out)
	}
	if out, _ = call(t, s, "memspace_get_memory", map[string]any{"ref": "evt_mcp_0002"}); !strings.Contains(out, "duplicate key value") {
		t.Errorf("get_memory:\n%s", out)
	}
	if out, isErr = call(t, s, "memspace_get_memory", map[string]any{"ref": "nope"}); !isErr {
		t.Errorf("unknown ref: %s", out)
	}
	if out, _ = call(t, s, "memspace_session_history", map[string]any{"project": "github.com/me/billing"}); !strings.Contains(out, sess.SessionID) {
		t.Errorf("history:\n%s", out)
	}

	// Rotation invalidates the old MCP token; revocation invalidates all.
	req, _ = http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer wrong")
	req.Header.Set("Content-Type", "application/json")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad token on /mcp: %d", resp.StatusCode)
	}
	var deviceID string
	database.QueryRow(`SELECT device_id FROM devices`).Scan(&deviceID)
	auth.RevokeDevice(ctx, database, deviceID)
	if _, err := auth.VerifyReadToken(ctx, database, mt.Token); err == nil {
		t.Error("MCP token survived device revocation")
	}
	_ = sql.ErrNoRows
}
