package pi

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
	"github.com/chux0519/memspace/internal/spool"
)

// records normalizes one payload and returns the records in order.
func records(t *testing.T, in map[string]any) []spool.Record {
	t.Helper()
	raw, _ := json.Marshal(in)
	out, err := (&Adapter{}).Normalize(raw, time.Now())
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return out
}

func types(recs []spool.Record) string {
	var list []string
	for _, r := range recs {
		list = append(list, r.EventType)
	}
	return strings.Join(list, " ")
}

func body(t *testing.T, r spool.Record) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(r.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	return payload
}

func toolPayload(extra map[string]any) map[string]any {
	in := map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "PostToolUse"}
	for k, v := range extra {
		in[k] = v
	}
	return in
}

// The generated extension sends this payload shape for every event.
func TestNormalizeLifecycleEvents(t *testing.T) {
	cases := []struct {
		name  string
		in    map[string]any
		typ   string
		check func(*testing.T, map[string]any)
	}{
		{"startup", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionStart", "source": "startup", "model": "alibaba/deepseek", "transcript_path": "/repo/s1.jsonl"},
			"session.started", func(t *testing.T, p map[string]any) {
				if p["source"] != "startup" || p["model"] != "alibaba/deepseek" || p["cwd"] != "/repo" {
					t.Errorf("session.started = %v", p)
				}
			}},
		{"resume", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionStart", "source": "resume"},
			"session.started", func(t *testing.T, p map[string]any) {
				if p["source"] != "resume" {
					t.Errorf("source = %v", p["source"])
				}
			}},
		{"new", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionStart", "source": "new"},
			"session.started", func(t *testing.T, p map[string]any) {
				if p["source"] != "new" {
					t.Errorf("source = %v", p["source"])
				}
			}},
		{"fork", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionStart", "source": "fork"},
			"session.started", func(t *testing.T, p map[string]any) {
				if p["source"] != "fork" {
					t.Errorf("source = %v", p["source"])
				}
			}},
		{"prompt", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "UserPromptSubmit", "prompt": "修复登录超时", "prompt_id": "p1"},
			"user.prompt", func(t *testing.T, p map[string]any) {
				if p["prompt"] != "修复登录超时" || p["prompt_id"] != "p1" {
					t.Errorf("user.prompt = %v", p)
				}
			}},
		{"settled", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "Stop", "last_assistant_message": "done", "prompt_id": "p1"},
			"agent.turn_completed", func(t *testing.T, p map[string]any) {
				if p["last_assistant_message"] != "done" || p["prompt_id"] != "p1" {
					t.Errorf("turn_completed = %v", p)
				}
			}},
		{"compact", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "PreCompact", "trigger": "auto", "custom_instructions": "keep tests"},
			"session.compacting", func(t *testing.T, p map[string]any) {
				if p["trigger"] != "auto" || p["custom_instructions"] != "keep tests" {
					t.Errorf("compacting = %v", p)
				}
			}},
		{"switched", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionEnd", "reason": "switch"},
			"session.ended", func(t *testing.T, p map[string]any) {
				if p["reason"] != "switch" {
					t.Errorf("ended = %v", p)
				}
			}},
		{"quit", map[string]any{"session_id": "s1", "cwd": "/repo", "hook_event_name": "SessionEnd", "reason": "quit"},
			"session.ended", func(t *testing.T, p map[string]any) {
				if p["reason"] != "quit" {
					t.Errorf("ended = %v", p)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := records(t, c.in)
			if len(recs) != 1 || recs[0].EventType != c.typ {
				t.Fatalf("types = %q, want %s", types(recs), c.typ)
			}
			if recs[0].Agent != "pi" {
				t.Errorf("agent = %q", recs[0].Agent)
			}
			c.check(t, body(t, recs[0]))
		})
	}
}

// A submission keeps its identity across a retry, but two submissions of the
// same text are two events.
func TestPromptIDsArePerSubmission(t *testing.T) {
	first := records(t, map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit", "prompt": "same", "prompt_id": "p1"})
	retry := records(t, map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit", "prompt": "same", "prompt_id": "p1"})
	second := records(t, map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": "UserPromptSubmit", "prompt": "same", "prompt_id": "p2"})
	if first[0].EventID != retry[0].EventID {
		t.Errorf("retry of one submission changed the event id: %s != %s", first[0].EventID, retry[0].EventID)
	}
	if first[0].EventID == second[0].EventID {
		t.Error("two submissions of the same text collapsed into one event")
	}
}

// pi's write and edit tools name one file in their input; the result carries
// the body and the details, and the file is recorded once.
func TestNormalizeFileTools(t *testing.T) {
	write := records(t, toolPayload(map[string]any{
		"tool_name": "write", "tool_use_id": "call_w",
		"tool_input":    map[string]any{"path": "/repo/main.go", "content": "package main"},
		"tool_response": map[string]any{"output": "Successfully wrote to /repo/main.go"},
	}))
	if got := types(write); got != "tool.completed file.modified" {
		t.Fatalf("write = %s", got)
	}
	if p := body(t, write[1]); p["path"] != "/repo/main.go" {
		t.Errorf("file.modified = %v", p)
	}
	// pi resolves a relative tool path against the session cwd.
	relative := records(t, toolPayload(map[string]any{
		"cwd": "/repo/sub", "tool_name": "write", "tool_use_id": "call_r",
		"tool_input":    map[string]any{"path": "note.txt", "content": "x"},
		"tool_response": map[string]any{"output": "ok"},
	}))
	if p := body(t, relative[1]); p["path"] != "/repo/sub/note.txt" {
		t.Errorf("relative path = %v", p["path"])
	}
	resp, _ := body(t, write[0])["tool_response"].(map[string]any)
	if resp["output"] != "Successfully wrote to /repo/main.go" {
		t.Errorf("content body lost: %v", resp)
	}

	edit := records(t, toolPayload(map[string]any{
		"tool_name": "edit", "tool_use_id": "call_e",
		"tool_input":    map[string]any{"path": "/repo/auth.go", "edits": []any{map[string]any{"oldText": "a", "newText": "b"}}},
		"tool_response": map[string]any{"patch": "@@ -1 +1 @@", "output": "Successfully replaced 1 block(s) in /repo/auth.go."},
	}))
	if got := types(edit); got != "tool.completed file.modified" {
		t.Fatalf("edit = %s", got)
	}
	// Both parts of the result survive, side by side.
	resp, _ = body(t, edit[0])["tool_response"].(map[string]any)
	if resp["patch"] != "@@ -1 +1 @@" || resp["output"] == "" {
		t.Errorf("edit result = %v", resp)
	}
}

// An extension or MCP tool in pi reports the files it applied; each one gets
// its own stable file.modified event.
func TestMultiFileToolProducesOneEventPerFile(t *testing.T) {
	in := toolPayload(map[string]any{
		"tool_name": "apply_patch", "tool_use_id": "call_m",
		"tool_input":    map[string]any{"patch": "*** Begin Patch"},
		"tool_response": map[string]any{"applied": true, "totalReplacements": float64(4), "files": []any{"/repo/a.go", "/repo/b.go"}},
	})
	recs := records(t, in)
	if got := types(recs); got != "tool.completed file.modified file.modified" {
		t.Fatalf("types = %s", got)
	}
	first, second := body(t, recs[1]), body(t, recs[2])
	if first["path"] != "/repo/a.go" || second["path"] != "/repo/b.go" {
		t.Errorf("files = %v / %v", first["path"], second["path"])
	}
	if recs[1].EventID == recs[2].EventID {
		t.Error("each modified file needs its own event id")
	}
	// Re-normalizing the same observation keeps both ids stable.
	again := records(t, in)
	if again[1].EventID != recs[1].EventID || again[2].EventID != recs[2].EventID {
		t.Error("file.modified ids are not stable")
	}
}

// A preview, a zero-replacement run, and a tool that merely lists files change
// nothing, so they record no file.modified.
func TestNoFileEventsWithoutAppliedChanges(t *testing.T) {
	cases := map[string]map[string]any{
		"preview": {"applied": false, "files": []any{"/repo/a.go"}},
		"zero":    {"totalReplacements": float64(0), "fileReplacements": []any{map[string]any{"path": "/repo/a.go"}}},
		"listing": {"files": []any{"/repo/a.go", "/repo/b.go"}},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			recs := records(t, toolPayload(map[string]any{
				"tool_name": "apply_patch", "tool_use_id": "call_x",
				"tool_input":    map[string]any{"patch": "*** Begin Patch"},
				"tool_response": response,
			}))
			if got := types(recs); got != "tool.completed" {
				t.Errorf("types = %s", got)
			}
		})
	}
}

// pi has no dispatch layer, so an internal URL is never a file path.
func TestInternalURLIsNotAFilePath(t *testing.T) {
	recs := records(t, toolPayload(map[string]any{
		"tool_name": "write", "tool_use_id": "call_u",
		"tool_input":    map[string]any{"path": "xd://notes/1", "content": "hello"},
		"tool_response": map[string]any{"output": "ok"},
	}))
	if got := types(recs); got != "tool.completed" {
		t.Errorf("types = %s", got)
	}
}

// Globs and directories are expanded before the sensitivity test, and the
// whole event is redacted when they hide a secret file.
func TestSensitiveGlobAndDirectoryAreRedacted(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "app.json"), []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "secrets.json"), []byte(`{"token":"sk-live"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	glob := records(t, toolPayload(map[string]any{
		"tool_name": "edit", "tool_use_id": "call_g",
		"tool_input": map[string]any{
			"path":  filepath.Join(config, "*.json"),
			"edits": []any{map[string]any{"oldText": "sk-live", "newText": "sk-other"}},
		},
		"tool_response": map[string]any{"patch": "-sk-live +sk-other"},
	}))
	if len(glob) != 2 {
		t.Fatalf("types = %s", types(glob))
	}
	if raw := string(glob[0].Payload); strings.Contains(raw, "sk-live") || strings.Contains(raw, "sk-other") {
		t.Errorf("secret edit leaked: %s", raw)
	}
	if p := body(t, glob[0]); p["tool_response"] != adapter.OmittedSensitive {
		t.Errorf("tool_response = %v", p["tool_response"])
	}

	// A directory holding a secret file is sensitive too.
	deep := filepath.Join(dir, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, ".env"), []byte("TOKEN=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirs := records(t, toolPayload(map[string]any{
		"tool_name": "write", "tool_use_id": "call_d",
		"tool_input":    map[string]any{"path": deep, "content": "TOKEN=1"},
		"tool_response": map[string]any{"output": "TOKEN=1"},
	}))
	if raw := string(dirs[0].Payload); strings.Contains(raw, "TOKEN=1") {
		t.Errorf("directory of secrets leaked: %s", raw)
	}
	if p := body(t, dirs[0]); p["tool_response"] != adapter.OmittedSensitive {
		t.Errorf("tool_response = %v", p["tool_response"])
	}
}

func TestSensitiveFailureErrorIsRedacted(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, ".env")
	if err := os.WriteFile(secret, []byte("TOKEN=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	recs := records(t, toolPayload(map[string]any{
		"hook_event_name": "PostToolUseFailure",
		"tool_name":       "edit", "tool_use_id": "call_f",
		"tool_input":    map[string]any{"path": secret, "edits": []any{map[string]any{"oldText": "TOKEN=1", "newText": "x"}}},
		"tool_response": map[string]any{"output": "failed"},
		"error":         "Could not edit /tmp/.env: TOKEN=1 did not match",
	}))
	if len(recs) != 1 || recs[0].EventType != "tool.failed" {
		t.Fatalf("types = %s", types(recs))
	}
	if raw := string(recs[0].Payload); strings.Contains(raw, "TOKEN=1") {
		t.Errorf("failure leaked the secret: %s", raw)
	}
	p := body(t, recs[0])
	if p["error"] != adapter.OmittedSensitive {
		t.Errorf("error = %v", p["error"])
	}
}

func TestExtensionSourceEmbedsArgv(t *testing.T) {
	argv := []string{"/opt/My Tools/memspace", "--home", "/tmp/home", "hook", "pi"}
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
	for _, handler := range []string{`pi.on("session_start"`, `pi.on("session_shutdown"`, `pi.on("input"`,
		`pi.on("before_agent_start"`, `pi.on("tool_result"`, `pi.on("agent_settled"`,
		`pi.on("session_before_compact"`} {
		if !strings.Contains(text, handler) {
			t.Errorf("%s missing", handler)
		}
	}
	if strings.Contains(text, "spawnSync") || !strings.Contains(text, "spawn(HOOK_ARGV[0]") {
		t.Error("hooks must run asynchronously")
	}
}

func TestInstallIsIdempotentAndRefusesForeignFile(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	opts := adapter.InstallOptions{
		HookCommand: `"/bin/memspace" hook pi`,
		HookArgv:    []string{"/bin/memspace", "hook", "pi"},
		Out:         &bytes.Buffer{},
	}
	if err := a.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(a.ExtensionPath())
	if err != nil {
		t.Fatal(err)
	}
	// Reinstall is idempotent: same content, no error.
	if err := a.Install(context.Background(), opts); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	second, err := os.ReadFile(a.ExtensionPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("reinstall changed the generated extension")
	}
	for _, c := range a.Doctor(context.Background(), opts) {
		if c.Name == "hooks" && c.Status != adapter.OK {
			t.Errorf("doctor hooks: %+v", c)
		}
	}

	// A user's own extension with our file name is never overwritten.
	path := a.ExtensionPath()
	if err := os.WriteFile(path, []byte("// my extension\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = a.Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "not generated") {
		t.Fatalf("expected refusal, got %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "// my extension\n" {
		t.Error("foreign file was overwritten")
	}
	// Uninstall leaves that file alone.
	var out bytes.Buffer
	if err := a.Uninstall(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "// my extension\n" {
		t.Error("uninstall removed a file memspace did not generate")
	}
}

func TestUninstallRemovesGeneratedExtension(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	opts := adapter.InstallOptions{
		HookCommand: `"/bin/memspace" hook pi`,
		HookArgv:    []string{"/bin/memspace", "hook", "pi"},
		Out:         &bytes.Buffer{},
	}
	if err := a.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(context.Background(), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ExtensionPath()); !os.IsNotExist(err) {
		t.Fatal("generated extension was not removed")
	}
}

// pi writes {"command": ..., "args": [...]} with no type; MemSpace adds the
// same shape and keeps every other server and key in the file.
func TestMCPRegistrationPreservesOtherContent(t *testing.T) {
	dir := t.TempDir()
	a := &Adapter{AgentDir: dir}
	existing := `{"mcpServers":{"other":{"command":"other","args":["--x"]}},"autoEnableCodemode":false}`
	if err := os.WriteFile(a.MCPConfigPath(), []byte(existing), 0o644); err != nil {
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
		MCPServers map[string]map[string]any `json:"mcpServers"`
		Auto       *bool                     `json:"autoEnableCodemode"`
	}
	data, _ := os.ReadFile(a.MCPConfigPath())
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.MCPServers["other"]["command"] != "other" {
		t.Error("unrelated server was dropped")
	}
	if doc.Auto == nil || *doc.Auto {
		t.Error("unrelated top-level key was dropped")
	}
	entry := doc.MCPServers[adapter.MCPServerName]
	if entry["command"] != "/bin/memspace" {
		t.Errorf("command = %v", entry["command"])
	}
	if _, ok := entry["type"]; ok {
		t.Error("entry carries a type pi does not write")
	}
	args, _ := entry["args"].([]any)
	if len(args) != 2 || args[0] != "mcp" || args[1] != "stdio" {
		t.Errorf("args = %v", entry["args"])
	}

	if err := a.UnregisterMCP(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(a.MCPConfigPath())
	if strings.Contains(string(data), "memspace") || !strings.Contains(string(data), "other") {
		t.Errorf("after unregister: %s", data)
	}
	if !strings.Contains(string(data), "autoEnableCodemode") {
		t.Error("unregister dropped an unrelated key")
	}
}
