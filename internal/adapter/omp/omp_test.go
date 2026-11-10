package omp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
)

func TestNormalizeHookPayloads(t *testing.T) {
	a := &Adapter{}
	now := time.Now()
	inputs := []map[string]any{
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "SessionStart", "source": "startup", "model": "alibaba/deepseek", "transcript_path": "/tmp/s1.jsonl"},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit", "prompt": "修复登录超时", "prompt_id": "p1"},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "PostToolUse", "tool_name": "write", "tool_use_id": "call_1", "tool_input": map[string]any{"path": "/repo/main.go"}, "tool_response": map[string]any{"bytes": 10}},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "PostToolUse", "tool_name": "ast_edit", "tool_use_id": "call_3", "tool_input": map[string]any{"paths": []any{"/repo/lib.go"}}},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "PostToolUseFailure", "tool_name": "bash", "tool_use_id": "call_2", "tool_input": map[string]any{"command": "false"}, "error": "exit 1"},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "Stop", "last_assistant_message": "done"},
		{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "SessionEnd", "reason": "shutdown"},
	}
	var recs []struct {
		typ  string
		body map[string]any
	}
	for _, in := range inputs {
		raw, _ := json.Marshal(in)
		out, err := a.Normalize(raw, now)
		if err != nil {
			t.Fatalf("%s: %v", in["hook_event_name"], err)
		}
		for _, r := range out {
			if r.Agent != AgentType || r.NativeSessionID != "s1" || r.Project.Root != "" && r.Project.Root != "/tmp" {
				t.Errorf("identity: %+v", r)
			}
			var body map[string]any
			if err := json.Unmarshal(r.Payload, &body); err != nil {
				t.Fatal(err)
			}
			recs = append(recs, struct {
				typ  string
				body map[string]any
			}{r.EventType, body})
		}
	}
	var types []string
	for _, r := range recs {
		types = append(types, r.typ)
	}
	want := "session.started user.prompt tool.completed file.modified tool.completed file.modified tool.failed agent.turn_completed session.ended"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("types = %s", got)
	}
	if recs[1].body["prompt"] != "修复登录超时" {
		t.Errorf("prompt = %v", recs[1].body)
	}
	if p := recs[3].body["path"]; p != "/repo/main.go" {
		t.Errorf("file.modified path = %v", p)
	}
	if recs[5].body["path"] != "/repo/lib.go" {
		t.Errorf("ast_edit file.modified = %v", recs[5].body)
	}
	if recs[6].body["error"] != "exit 1" {
		t.Errorf("failure = %v", recs[6].body)
	}
	if recs[7].body["last_assistant_message"] != "done" {
		t.Errorf("stop = %v", recs[7].body)
	}
}

func TestNormalizeRequiresIdentity(t *testing.T) {
	a := &Adapter{}
	if _, err := a.Normalize([]byte(`{"cwd":"/tmp"}`), time.Now()); err == nil {
		t.Fatal("expected an error for a payload without session_id")
	}
}

func TestPromptAndToolIDsAreStable(t *testing.T) {
	a := &Adapter{}
	in, _ := json.Marshal(map[string]any{
		"session_id": "s1", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit",
		"prompt": "same prompt", "prompt_id": "p1",
	})
	first, _ := a.Normalize(in, time.Now())
	second, _ := a.Normalize(in, time.Now())
	if first[0].EventID != second[0].EventID {
		t.Errorf("prompt id is not stable: %s != %s", first[0].EventID, second[0].EventID)
	}
}

func TestExtensionSourceEmbedsArgv(t *testing.T) {
	argv := []string{"/opt/My Tools/memspace", "--home", "/tmp/home", "hook", "omp"}
	src, err := ExtensionSource(argv)
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	if !strings.Contains(text, extensionMarker) {
		t.Error("marker missing")
	}
	encoded, _ := json.Marshal(argv)
	if !strings.Contains(text, string(encoded)) {
		t.Errorf("argv %s not embedded", encoded)
	}
	if !strings.Contains(text, `pi.on("tool_result"`) || !strings.Contains(text, `pi.on("session_shutdown"`) {
		t.Error("expected lifecycle handlers")
	}
}

func TestInstallAndUninstall(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	opts := adapter.InstallOptions{
		HookCommand: `"/bin/memspace" hook omp`,
		HookArgv:    []string{"/bin/memspace", "hook", "omp"},
		Out:         &bytes.Buffer{},
	}
	if err := a.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ExtensionPath()); err != nil {
		t.Fatal(err)
	}
	// Reinstall is idempotent.
	if err := a.Install(context.Background(), opts); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	checks := a.Doctor(context.Background(), opts)
	for _, c := range checks {
		if c.Name == "hooks" && c.Status == adapter.Fail {
			t.Errorf("doctor hooks: %+v", c)
		}
	}
	if err := a.Uninstall(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ExtensionPath()); !os.IsNotExist(err) {
		t.Fatal("extension was not removed")
	}
}

func TestInstallRefusesForeignFile(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	path := a.ExtensionPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("// user extension\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := a.Install(context.Background(), adapter.InstallOptions{
		HookCommand: "/bin/memspace hook omp",
		HookArgv:    []string{"/bin/memspace", "hook", "omp"},
		Out:         &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "not generated") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "// user extension\n" {
		t.Error("foreign file was overwritten")
	}
}

func TestMCPRegistrationPreservesOtherServers(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	if err := os.WriteFile(a.MCPConfigPath(), []byte(`{"mcpServers":{"other":{"command":"other"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	argv := []string{"/bin/memspace", "mcp", "stdio"}
	if err := a.RegisterMCP(context.Background(), adapter.MCPOptions{URL: "http://x/mcp", Command: argv, Out: &bytes.Buffer{}}); err != nil {
		t.Fatal(err)
	}
	if c := a.MCPCheck(context.Background()); c.Status != adapter.OK {
		t.Errorf("check = %+v", c)
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	data, _ := os.ReadFile(a.MCPConfigPath())
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MCPServers["other"].Command != "other" {
		t.Error("unrelated server was dropped")
	}
	if got := strings.Join(doc.MCPServers["memspace"].Args, " "); got != "mcp stdio" {
		t.Errorf("memspace args = %q", got)
	}
	if err := a.UnregisterMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(a.MCPConfigPath())
	if strings.Contains(string(data), "memspace") || !strings.Contains(string(data), "other") {
		t.Errorf("after unregister: %s", data)
	}
}
