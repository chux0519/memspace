package hookpayload

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
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