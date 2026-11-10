package hookpayload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/spool"
)

func normalizeTool(t *testing.T, in map[string]any) (map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(in)
	recs, err := Normalize("claude-code", raw, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 {
		return nil, ""
	}
	var payload map[string]any
	if err := json.Unmarshal(recs[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload, string(recs[0].Payload)
}

// A secret file anywhere in the tool's paths must redact the whole event.
func normalizeRecords(t *testing.T, in map[string]any) []spool.Record {
	t.Helper()
	raw, _ := json.Marshal(in)
	recs, err := Normalize("claude-code", raw, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestSensitivePathInAnyPosition(t *testing.T) {
	payload, raw := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": "/tmp", "hook_event_name": "PostToolUse",
		"tool_name": "ast_edit", "tool_use_id": "toolu_1",
		"tool_input": map[string]any{
			"paths": []any{"/repo/main.go", "/repo/secrets.json"},
			"ops": []any{
				map[string]any{"pat": "TOKEN = $A", "out": "TOKEN = $A"},
			},
		},
		"tool_response": map[string]any{"applied": 1},
	})
	if strings.Contains(raw, "TOKEN = $A") || strings.Contains(raw, "applied") {
		t.Fatalf("secret edit leaked content: %s", raw)
	}
	ops := payload["tool_input"].(map[string]any)["ops"].([]any)
	op := ops[0].(map[string]any)
	if op["pat"] != adapter.OmittedSensitive || op["out"] != adapter.OmittedSensitive {
		t.Errorf("ops not redacted: %v", op)
	}
	if payload["tool_response"] != adapter.OmittedSensitive {
		t.Errorf("tool_response = %v", payload["tool_response"])
	}
}

func TestNonSensitiveASTEditKeepsOps(t *testing.T) {
	payload, _ := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": "/tmp", "hook_event_name": "PostToolUse",
		"tool_name": "ast_edit", "tool_use_id": "toolu_2",
		"tool_input": map[string]any{
			"paths": []any{"/repo/main.go"},
			"ops":   []any{map[string]any{"pat": "fmt.Println($$$A)", "out": "log.Println($$$A)"}},
		},
	})
	op := payload["tool_input"].(map[string]any)["ops"].([]any)[0].(map[string]any)
	if op["pat"] != "fmt.Println($$$A)" || op["out"] != "log.Println($$$A)" {
		t.Errorf("ops were altered: %v", op)
	}
}

// The command body of a successful tool is recorded, and details survive.
func TestToolResponseKeepsContent(t *testing.T) {
	payload, _ := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": "/tmp", "hook_event_name": "PostToolUse",
		"tool_name": "bash", "tool_use_id": "toolu_3",
		"tool_input":    map[string]any{"command": "echo hi"},
		"tool_response": map[string]any{"output": "hi\n", "exitCode": 0},
	})
	resp := payload["tool_response"].(map[string]any)
	if resp["output"] != "hi\n" || resp["exitCode"] != float64(0) {
		t.Errorf("tool_response = %v", resp)
	}
}
func TestGlobAndDirectoryTargetsAreExpanded(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "secrets.json"), []byte(`{"token":"s3cr3t"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "values.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"glob":      filepath.Join(config, "*.json"),
		"directory": config,
	} {
		t.Run(name, func(t *testing.T) {
			_, raw := normalizeTool(t, map[string]any{
				"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
				"tool_name": "ast_edit", "tool_use_id": "toolu_" + name,
				"tool_input": map[string]any{
					"paths": []any{target},
					"ops":   []any{map[string]any{"pat": "TOKEN = $A", "out": "TOKEN = $B"}},
				},
				"tool_response": map[string]any{"applied": true, "totalReplacements": float64(1), "files": []any{filepath.Join(config, "secrets.json")}},
			})
			if strings.Contains(raw, "TOKEN = $A") || strings.Contains(raw, "s3cr3t") {
				t.Fatalf("sensitive target leaked: %s", raw)
			}
		})
	}
}

// A path that does not match a sensitive name by itself is still sensitive
// when the tool reports a sensitive applied file.
func TestAppliedFilesMakeTheEventSensitive(t *testing.T) {
	dir := t.TempDir()
	_, raw := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
		"tool_name": "ast_edit", "tool_use_id": "toolu_1",
		"tool_input": map[string]any{
			"paths": []any{filepath.Join(dir, "missing-*.json")},
			"ops":   []any{map[string]any{"pat": "PASSWORD = $A", "out": "PASSWORD = $B"}},
		},
		"tool_response": map[string]any{"applied": true, "totalReplacements": float64(1), "files": []any{filepath.Join(dir, "config", "secrets.json")}},
	})
	if strings.Contains(raw, "PASSWORD = $A") || strings.Contains(raw, "/config/") {
		t.Fatalf("applied sensitive file leaked: %s", raw)
	}
}

func TestAppliedFilesDriveFileModifiedEvents(t *testing.T) {
	dir := t.TempDir()
	recs := normalizeRecords(t, map[string]any{
		"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
		"tool_name": "ast_edit", "tool_use_id": "toolu_1",
		"tool_input": map[string]any{"paths": []any{dir}},
		"tool_response": map[string]any{
			"applied": true, "totalReplacements": float64(2),
			"files": []any{"a.go", "b.go"},
		},
	})
	var paths []string
	var ids []string
	for _, r := range recs {
		if r.EventType != "file.modified" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p["path"].(string))
		ids = append(ids, r.EventID)
	}
	if len(paths) != 2 || paths[0] != filepath.Join(dir, "a.go") || paths[1] != filepath.Join(dir, "b.go") {
		t.Fatalf("paths = %v", paths)
	}
	if ids[0] == ids[1] {
		t.Error("each file needs a distinct event id")
	}
}

func TestNoFileEventsWithoutAppliedReplacements(t *testing.T) {
	dir := t.TempDir()
	for name, response := range map[string]map[string]any{
		"preview": map[string]any{"applied": false, "totalReplacements": float64(3), "files": []any{"a.go"}},
		"zero":    map[string]any{"applied": true, "totalReplacements": float64(0), "files": []any{}},
	} {
		recs := normalizeRecords(t, map[string]any{
			"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
			"tool_name": "ast_edit", "tool_use_id": "toolu_" + name,
			"tool_input":    map[string]any{"paths": []any{dir}},
			"tool_response": response,
		})
		for _, r := range recs {
			if r.EventType == "file.modified" {
				t.Errorf("%s: unexpected file.modified %s", name, r.Payload)
			}
		}
	}
}

// A write to an internal xd:// URL carries the dispatched tool's result; the
// real changed files are recorded, and the URL itself is not a file change.
func TestXDResolveRecordsUnderlyingFiles(t *testing.T) {
	dir := t.TempDir()
	recs := normalizeRecords(t, map[string]any{
		"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
		"tool_name": "write", "tool_use_id": "toolu_resolve",
		"tool_input": map[string]any{"path": "xd://resolve", "content": "apply the pending ast edit"},
		"tool_response": map[string]any{
			"output": "Applied 2 replacements in 2 files.",
			"xdev": map[string]any{"inner": map[string]any{"sourceResultDetails": map[string]any{
				"applied": true, "totalReplacements": float64(2),
				"files": []any{"a.go", "b.go"},
				"fileReplacements": []any{
					map[string]any{"path": "a.go", "count": float64(1)},
					map[string]any{"path": "b.go", "count": float64(1)},
				},
			}}},
		},
	})
	var paths []string
	for _, r := range recs {
		if r.EventType != "file.modified" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p["path"].(string))
	}
	want := []string{filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

// A staged (not applied) xd edit of a secret file must not leak the preview.
func TestXDPreviewSensitiveIsRedacted(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "config", "secrets.json")
	_, raw := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": dir, "hook_event_name": "PostToolUse",
		"tool_name": "write", "tool_use_id": "toolu_preview",
		"tool_input": map[string]any{
			"path":    "xd://ast_edit",
			"content": `{"ops":[{"pat":"API_KEY = $A","out":"API_KEY = $B"}],"paths":["config/*.json"]}`,
		},
		"tool_response": map[string]any{
			"output": "Staged as a proposal — files NOT modified yet.",
			"xdev": map[string]any{"inner": map[string]any{"sourceResultDetails": map[string]any{
				"applied": false, "totalReplacements": float64(1),
				"files": []any{"config/secrets.json"},
			}}},
		},
	})
	if strings.Contains(raw, "API_KEY = $A") || strings.Contains(raw, "secrets.json") {
		t.Fatalf("preview of a secret file leaked: %s", raw)
	}
	if strings.Contains(raw, `"file.modified"`) {
		t.Fatalf("preview produced a file.modified: %s", raw)
	}
	_ = secret
}

// A failing tool that names a secret file must not quote it in error.
func TestSensitiveFailureErrorIsRedacted(t *testing.T) {
	payload, raw := normalizeTool(t, map[string]any{
		"session_id": "s", "cwd": "/tmp", "hook_event_name": "PostToolUseFailure",
		"tool_name": "write", "tool_use_id": "toolu_fail",
		"tool_input": map[string]any{"file_path": "/repo/secrets.json", "content": `{"token":"s3cr3t"}`},
		"error":      `write /repo/secrets.json: invalid content {"token":"s3cr3t"}`,
	})
	if strings.Contains(raw, "s3cr3t") {
		t.Fatalf("error text leaked: %s", raw)
	}
	if payload["error"] != adapter.OmittedSensitive {
		t.Errorf("error = %v", payload["error"])
	}
}
