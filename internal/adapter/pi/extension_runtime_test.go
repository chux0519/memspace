package pi

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtensionRuntimeBehavior runs the generated extension under Node and
// exercises what its source alone cannot show: submission identity across a
// retry, tool output preservation, non-blocking hook execution, the session
// switch boundary, and the context injection.
func TestExtensionRuntimeBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "record.jsonl")
	hook := filepath.Join(dir, "hook.sh")
	// The hook is deliberately slow: a synchronous call would freeze the event
	// loop for longer than the harness allows.
	script := `#!/bin/sh
payload=$(cat)
# One write per event: concurrent hooks append to the same file.
printf '%s\n' "$payload" >> "$MEMSPACE_TEST_RECORD"
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
		Elapsed map[string]int `json:"elapsed"`
		Ticks   int            `json:"ticks"`
		Sent    []struct {
			CustomType string `json:"customType"`
			Content    string `json:"content"`
			Deliver    string `json:"deliver"`
		} `json:"sent"`
		SentAtStartReturn int `json:"sentAtStartReturn"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("harness output: %v\n%s", err, out)
	}
	// session_start records the session and injects the card when the hook
	// answers; it must have returned long before that, so nothing is injected
	// yet at that point (the hook sleeps far longer than the handler may take).
	if result.SentAtStartReturn != 0 {
		t.Errorf("session_start waited for the hook: %d messages were already injected", result.SentAtStartReturn)
	}
	// The interval keeps firing while the prompt hook runs; a synchronous spawn
	// would freeze it for the hook's whole duration.
	if result.Ticks < 3 {
		t.Errorf("hook blocked the event loop: ticks=%d", result.Ticks)
	}
	// Session-start context is injected as a message, and the prompt path
	// returns it, so pi's own prompt (MCP instructions included) stays intact.
	// One card per announced session start (startup, new, fork); the repeated
	// start pi sends after a replacement is deduplicated.
	if len(result.Sent) != 3 {
		t.Fatalf("injected messages = %+v", result.Sent)
	}
	for _, m := range result.Sent {
		if m.CustomType != "memspace-context" || !strings.Contains(m.Content, "MEMSPACE-CTX") || m.Deliver != "steer" {
			t.Errorf("injected message = %+v", m)
		}
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var promptIDs []string
	var prompts []map[string]any
	var starts, ends []map[string]any
	var tools, failures []map[string]any
	var stop, compact map[string]any
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
			prompts = append(prompts, payload)
		case "PostToolUse":
			tools = append(tools, payload)
		case "PostToolUseFailure":
			failures = append(failures, payload)
		case "SessionStart":
			starts = append(starts, payload)
		case "SessionEnd":
			ends = append(ends, payload)
		case "Stop":
			stop = payload
		case "PreCompact":
			compact = payload
		}
	}

	// input, before_agent_start, retry, same text again, blocking check, steer.
	if len(promptIDs) != 5 {
		t.Fatalf("prompt payloads = %v", promptIDs)
	}
	if promptIDs[0] == "" || promptIDs[0] != promptIDs[1] {
		t.Errorf("retry changed the submission id: %v", promptIDs)
	}
	if promptIDs[2] == promptIDs[0] {
		t.Errorf("two submissions share an id: %v", promptIDs)
	}
	if prompts[0]["prompt"] != "继续检查" {
		t.Errorf("prompt text = %v", prompts[0]["prompt"])
	}
	// A run started by another extension is not a user submission, and a run
	// started by a steered message is recorded even though it never reaches
	// before_agent_start. Its payload is the only one that is not awaited.
	var steer map[string]any
	for _, p := range prompts {
		if p["prompt"] == "steer this" {
			steer = p
		}
	}
	if steer == nil || steer["prompt_id"] == promptIDs[2] || steer["prompt_id"] == "" {
		t.Errorf("steered submission = %v", steer)
	}

	if len(tools) != 2 {
		t.Fatalf("tool payloads = %v", tools)
	}
	// Hooks of parallel tool calls finish in no fixed order.
	byCall := map[string]map[string]any{}
	for _, tool := range tools {
		byCall[asString(tool["tool_use_id"])] = tool
	}
	bash, edit := byCall["call_1"], byCall["call_2"]
	if bash == nil || edit == nil {
		t.Fatalf("tool payloads = %v", tools)
	}
	response, _ := bash["tool_response"].(map[string]any)
	if response["output"] != "hi\n" || response["exitCode"] != float64(0) {
		t.Errorf("tool_response lost content or details: %v", response)
	}
	response, _ = edit["tool_response"].(map[string]any)
	if response["patch"] != "@@ -1 +1 @@" || !strings.Contains(asString(response["output"]), "Successfully replaced") {
		t.Errorf("edit result lost a part: %v", response)
	}
	if input, _ := edit["tool_input"].(map[string]any); input["path"] != "/tmp/work/main.go" {
		t.Errorf("tool_input = %v", edit["tool_input"])
	}
	if len(failures) != 1 {
		t.Fatalf("failure payloads = %v", failures)
	}
	if failures[0]["error"] != "boom" {
		t.Errorf("error = %v", failures[0]["error"])
	}
	if response, _ := failures[0]["tool_response"].(map[string]any); response["exitCode"] != float64(1) {
		t.Errorf("failed tool lost its details: %v", failures[0]["tool_response"])
	}

	// agent_settled carries the last assistant message of the settled run.
	if stop == nil {
		t.Fatal("no agent.turn_completed payload")
	}
	if stop["last_assistant_message"] != "DONE" {
		t.Errorf("last_assistant_message = %v", stop["last_assistant_message"])
	}
	if stop["prompt_id"] != promptIDs[3] {
		t.Errorf("turn_completed prompt_id = %v, want %v", stop["prompt_id"], promptIDs[3])
	}
	if compact == nil || compact["trigger"] != "auto" {
		t.Errorf("compaction = %v", compact)
	}

	// session_start covers startup/new/resume/fork; a reload is not a new
	// session and records nothing. Session hooks run asynchronously, so the
	// payloads are matched by session, not by arrival order.
	if len(starts) != 3 {
		t.Fatalf("session.started payloads = %v", starts)
	}
	started := map[string]map[string]any{}
	for _, start := range starts {
		started[asString(start["session_id"])] = start
	}
	first := started["probe-session"]
	if first == nil || first["source"] != "startup" ||
		first["model"] != "alibaba/deepseek" || first["transcript_path"] != "/tmp/probe-session.jsonl" {
		t.Errorf("first session.started = %v", first)
	}
	if second := started["probe-session-2"]; second == nil || second["source"] != "new" {
		t.Errorf("new session was not recorded: %v", second)
	}
	if forked := started["probe-session-3"]; forked == nil || forked["source"] != "fork" {
		t.Errorf("forked session was not recorded: %v", forked)
	}
	// /new ends the old session as switched; a reload keeps it open; quit ends
	// the current one.
	if len(ends) != 2 {
		t.Fatalf("session.ended payloads = %v", ends)
	}
	ended := map[string]map[string]any{}
	for _, end := range ends {
		ended[asString(end["session_id"])] = end
	}
	if e := ended["probe-session"]; e == nil || e["reason"] != "switch" {
		t.Errorf("previous session was not ended on switch: %v", e)
	}
	if e := ended["probe-session-2"]; e == nil || e["reason"] != "quit" {
		t.Errorf("quit end = %v", e)
	}
	// A fork keeps the original session usable, so it must not be ended.
	if _, ok := ended["probe-session-3"]; ok {
		t.Errorf("fork ended the new session: %v", ended["probe-session-3"])
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
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
		sent.push({ customType: message.customType, content: String(message.content), deliver: options && options.deliverAs });
	},
});

let currentSession = "probe-session";
const ctx = {
	cwd: "/tmp/work",
	sessionManager: {
		getSessionId: () => currentSession,
		getSessionFile: () => "/tmp/" + currentSession + ".jsonl",
	},
	model: { provider: "alibaba", id: "deepseek" },
};
const elapsed = {};
const emit = async (name, event) => {
	const started = Date.now();
	for (const fn of handlers.get(name) || []) await fn(event, ctx);
	const took = Date.now() - started;
	elapsed[name] = Math.max(elapsed[name] || 0, took);
	return took;
};

await emit("session_start", { type: "session_start", reason: "startup" });
const sentAtStartReturn = sent.length;

await emit("input", { type: "input", text: "继续检查", source: "interactive" });
await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt: "继续检查" });
// Re-entry for the same submission must reuse the identity.
await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt: "继续检查" });
// The same text sent again is a new submission.
await emit("input", { type: "input", text: "继续检查", source: "interactive" });
await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt: "继续检查" });
// A run another extension starts is not a user submission.
await emit("before_agent_start", { type: "before_agent_start", messageType: "custom", message: { content: "x" } });

// The prompt hook runs asynchronously: timers keep firing while it waits.
let ticks = 0;
const interval = setInterval(() => {
	ticks++;
}, 10);
await emit("input", { type: "input", text: "阻塞检查", source: "interactive" });
await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt: "阻塞检查" });
clearInterval(interval);

await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_1",
	toolName: "bash",
	input: { command: "echo hi" },
	content: [{ type: "text", text: "hi\n" }],
	details: { exitCode: 0, durationMs: 3 },
	isError: false,
});
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_2",
	toolName: "edit",
	input: { path: "/tmp/work/main.go", edits: [{ oldText: "a", newText: "b" }] },
	content: [{ type: "text", text: "Successfully replaced 1 block(s) in /tmp/work/main.go." }],
	details: { patch: "@@ -1 +1 @@" },
	isError: false,
});
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_3",
	toolName: "bash",
	input: { command: "false" },
	content: [{ type: "text", text: "boom" }],
	details: { exitCode: 1 },
	isError: true,
});

await emit("message_end", { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "DONE" }] } });
await emit("agent_settled", { type: "agent_settled", aborted: false });
await emit("session_before_compact", { type: "session_before_compact", reason: "threshold" });

// A steered message continues the run in place and never reaches
// before_agent_start, so it is recorded from input.
await emit("input", { type: "input", text: "steer this", source: "interactive", streamingBehavior: "steer" });

// /new replaces the runtime: the old session ends as switched, the new starts.
// pi emits the new session's start twice after a replacement; it is one start.
await emit("session_shutdown", { type: "session_shutdown", reason: "new" });
currentSession = "probe-session-2";
await emit("session_start", { type: "session_start", reason: "new" });
await emit("session_start", { type: "session_start", reason: "new" });
// /reload replaces the extension runtime, not the session.
await emit("session_shutdown", { type: "session_shutdown", reason: "reload" });
await emit("session_start", { type: "session_start", reason: "reload" });
await emit("session_shutdown", { type: "session_shutdown", reason: "quit" });

// /fork replaces the runtime too, but the original session stays resumable:
// the forked session starts with source "fork" and nothing is ended.
currentSession = "probe-session-3";
await emit("session_start", { type: "session_start", reason: "fork" });
await emit("session_shutdown", { type: "session_shutdown", reason: "fork" });

// Let the detached shutdown hook and the other spawned hooks finish.
await new Promise((resolve) => setTimeout(resolve, 900));

process.stdout.write(JSON.stringify({ elapsed, ticks, sent, sentAtStartReturn }) + "\n");
`
