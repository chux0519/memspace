package omp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtensionRuntimeBehavior runs the generated extension under Node and
// exercises the behaviors its source alone cannot show: submission identity,
// tool output preservation, and non-blocking hook execution.
func TestExtensionRuntimeBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "record.jsonl")
	hook := filepath.Join(dir, "hook.sh")
	script := `#!/bin/sh
cat >> "$MEMSPACE_TEST_RECORD"
printf '\n' >> "$MEMSPACE_TEST_RECORD"
sleep 0.25
printf '%s' '{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"MEMSPACE-CTX"}}'
`
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := ExtensionSource([]string{hook})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extension.mjs"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "harness.mjs"), []byte(harnessSource), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(node, "harness.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "MEMSPACE_TEST_RECORD="+record)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
	var result struct {
		Ticks int `json:"ticks"`
		Sent  int `json:"sent"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("harness output: %v\n%s", err, out)
	}
	// The interval must keep firing while the hook is still running; a
	// synchronous child would freeze it for the hook's whole duration.
	if result.Ticks < 3 {
		t.Errorf("hook blocked the event loop: ticks=%d", result.Ticks)
	}
	if result.Sent == 0 {
		t.Error("additionalContext was not injected")
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var promptIDs []string
	var tool map[string]any
	var starts []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("hook payload %q: %v", line, err)
		}
		switch payload["hook_event_name"] {
		case "UserPromptSubmit":
			id, _ := payload["prompt_id"].(string)
			promptIDs = append(promptIDs, id)
		case "PostToolUse":
			tool = payload
		case "SessionStart":
			starts = append(starts, payload)
		}
	}
	// input, before_agent_start, retry, second identical input, prompt.
	if len(promptIDs) != 3 {
		t.Fatalf("prompt payloads = %v", promptIDs)
	}
	if promptIDs[0] == "" || promptIDs[0] != promptIDs[1] {
		t.Errorf("retry changed the submission id: %v", promptIDs)
	}
	if promptIDs[2] == promptIDs[0] {
		t.Errorf("two submissions share an id: %v", promptIDs)
	}
	if tool == nil {
		t.Fatal("no tool payload recorded")
	}
	response, _ := tool["tool_response"].(map[string]any)
	if response["output"] != "hi\n" || response["exitCode"] != float64(0) {
		t.Errorf("tool_response lost content or details: %v", response)
	}
	if len(starts) != 2 {
		t.Fatalf("session.started payloads = %v", starts)
	}
	if starts[0]["model"] != "alibaba/deepseek" || starts[0]["session_id"] != "probe-session" {
		t.Errorf("first session.started = %v", starts[0])
	}
	// /new switches sessions in place; the new session must still start.
	if starts[1]["session_id"] != "probe-session-2" || starts[1]["source"] != "startup" {
		t.Errorf("session_switch was not recorded: %v", starts[1])
	}
}

const harnessSource = `import factory from "./extension.mjs";

const handlers = new Map();
const sent = [];
factory({
	on(name, fn) {
		const list = handlers.get(name) || [];
		list.push(fn);
		handlers.set(name, list);
	},
	sendMessage(message, options) {
		sent.push({ message, options });
	},
});

let currentSession = "probe-session";
const ctx = {
	cwd: "/tmp",
	sessionManager: { getSessionId: () => currentSession, getSessionFile: () => "/tmp/" + currentSession + ".jsonl" },
	model: { provider: "alibaba", id: "deepseek" },
};
const emit = async (name, event) => {
	for (const fn of handlers.get(name) || []) await fn(event, ctx);
};

// A slow hook must not freeze timers.
let ticks = 0;
const interval = setInterval(() => {
	ticks++;
}, 10);
const started = emit("session_start", { type: "session_start" });
await new Promise((resolve) => setTimeout(resolve, 120));
const ticksDuring = ticks;
await started;
clearInterval(interval);

await emit("input", { type: "input", text: "继续检查", source: "interactive" });
await emit("before_agent_start", { type: "before_agent_start", reason: "prompt", prompt: "继续检查" });
// Re-entry for the same submission (policy retry) must reuse the identity.
await emit("before_agent_start", { type: "before_agent_start", reason: "prompt", prompt: "继续检查" });
// The same text sent again is a new submission.
await emit("input", { type: "input", text: "继续检查", source: "interactive" });
await emit("before_agent_start", { type: "before_agent_start", reason: "prompt", prompt: "继续检查" });

await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_1",
	toolName: "bash",
	input: { command: "echo hi" },
	content: [{ type: "text", text: "hi\n" }],
	details: { exitCode: 0, durationMs: 3 },
	isError: false,
});
await emit("session_stop", {
	type: "session_stop",
	session_id: "probe-session",
	last_assistant_message: { content: [{ type: "text", text: "DONE" }] },
});

// /new keeps the process but switches sessions.
currentSession = "probe-session-2";
await emit("session_switch", { type: "session_switch", reason: "new", previousSessionFile: "/tmp/probe-session.jsonl" });

process.stdout.write(JSON.stringify({ ticks: ticksDuring, sent: sent.length }) + "\n");
`
