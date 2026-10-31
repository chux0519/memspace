package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/server"
)

func TestConfiguredPolicyReachesRESTAndMCP(t *testing.T) {
	for _, mode := range []string{"hybrid", "none"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			search.SetSemantic(nil)
			database, err := db.Open(filepath.Join(t.TempDir(), "policy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			ts := httptest.NewServer(server.NewWithOptions(database, "owner", server.Options{ContextPolicy: admission.Policy{Mode: mode}}))
			defer ts.Close()
			code, err := auth.CreateInvite(ctx, database, "owner", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			c := client.New(ts.URL, "")
			_, token, err := c.Register(ctx, "laptop", "linux", code)
			if err != nil {
				t.Fatal(err)
			}
			c.Token = token
			project, err := c.EnsureProject(ctx, "policy", "github.com/test/policy")
			if err != nil {
				t.Fatal(err)
			}
			session, err := c.EnsureSession(ctx, model.EnsureSessionRequest{ProjectID: project, AgentType: "codex", NativeSessionID: "old", Location: &model.LocationInput{AbsolutePath: "/src/policy"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.BatchEvents(ctx, project, []model.Event{{EventID: "evt-policy-evidence", SessionID: &session.SessionID, EventType: "agent.turn_completed", OccurredAt: time.Now(), PayloadJSON: `{"last_assistant_message":"quartz evidence-for-policy-test"}`}})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: project}, Query: "quartz cobalt amber"})
			req, _ := http.NewRequest("POST", ts.URL+"/api/v1/context", bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var pack contextpack.Pack
			if err = json.NewDecoder(resp.Body).Decode(&pack); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(pack.Text, "evidence-for-policy-test") != (mode == "none") {
				t.Fatalf("REST ignored policy: %s", pack.Text)
			}
			s := connect(t, ts.URL, token)
			text, isErr := call(t, s, "memspace_project_context", map[string]any{"cwd": "/src/policy", "query": "quartz cobalt amber"})
			if isErr || strings.Contains(text, "evidence-for-policy-test") != (mode == "none") {
				t.Fatalf("MCP ignored policy: %s", text)
			}
		})
	}
}
