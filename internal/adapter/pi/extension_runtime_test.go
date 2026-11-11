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
// retry and a repeated text, tool output preservation, non-blocking hook
// execution, an unaccepted queued message, an expanded (template) queued
// message, a cancelled-then-resubmitted prompt, the session switch boundary
// and the context injection.
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
		Returns []struct {
			Prompt   string `json:"prompt"`
			Injected bool   `json:"injected"`
		} `json:"returns"`
		SentAtStartReturn int `json:"sentAtStartReturn"`
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("harness output: %v\n%s", err, out)
	}
	// session_start records the session and injects the card when the hook
	// answers; it must have returned long before that.
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
	if len(result.Sent) != 3 {
		t.Fatalf("injected messages = %+v", result.Sent)
	}
	for _, m := range result.Sent {
		if m.CustomType != "memspace-context" || !strings.Contains(m.Content, "MEMSPACE-CTX") || m.Deliver != "steer" {
			t.Errorf("injected message = %+v", m)
		}
	}
	// A prompt that starts a run is injected; a queued message has no channel.
	injected := map[string]bool{}
	for _, r := range result.Returns {
		injected[r.Prompt] = injected[r.Prompt] || r.Injected
	}
	if !injected["Expanded opening task"] {
		t.Errorf("the template prompt was not injected: %v", result.Returns)
	}
	if !injected["重投的问题"] {
		t.Errorf("a resubmitted prompt after a cancelled queue was not injected: %v", result.Returns)
	}
	if injected["Task B: 运行 bash: echo B-TOOL，然后只回复 B-ANSWER"] {
		t.Error("a queued message cannot be injected, but the harness saw a message")
	}

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var prompts, starts, ends, tools, failures, stops []map[string]any
	var compact map[string]any
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
			stops = append(stops, payload)
		case "PreCompact":
			compact = payload
		}
	}

	if len(prompts) != 12 {
		t.Fatalf("prompt payloads = %v", prompts)
	}
	// A template submitted while idle is one submission: the text before_agent_start
	// announced, recorded once (plus its retry under the same id), and its opening
	// message is not recorded again.
	var expandedOpening []string
	for _, p := range prompts {
		switch asString(p["prompt"]) {
		case "Expanded opening task":
			expandedOpening = append(expandedOpening, asString(p["prompt_id"]))
		case "/opening":
			t.Errorf("the raw template command was recorded: %v", p)
		}
	}
	if len(expandedOpening) != 2 || expandedOpening[0] != expandedOpening[1] {
		t.Errorf("the expanded opening prompt was recorded as %v", expandedOpening)
	}
	byText := map[string]map[string]any{}
	var repeated []string
	for _, p := range prompts {
		text, _ := p["prompt"].(string)
		byText[text] = p
		if text == "继续检查" {
			repeated = append(repeated, asString(p["prompt_id"]))
		}
	}
	id := func(t *testing.T, text string) string {
		t.Helper()
		p := byText[text]
		if p == nil || asString(p["prompt_id"]) == "" {
			t.Fatalf("no prompt recorded for %q: %v", text, byText)
		}
		return asString(p["prompt_id"])
	}

	// One submission keeps its identity across a retry; the same text sent again
	// is a second submission.
	if len(repeated) != 3 {
		t.Fatalf("same-text prompts = %v", repeated)
	}
	if repeated[0] != repeated[1] {
		t.Errorf("retry changed the submission id: %v", repeated)
	}
	if repeated[0] == repeated[2] {
		t.Errorf("two submissions share an id: %v", repeated)
	}
	// A message pi never accepted is not recorded (nothing reached the model).
	if p := byText["排队未消费"]; p != nil {
		t.Errorf("unaccepted queued message was recorded: %v", p)
	}
	// A queued template is recorded with the text the model actually received,
	// under its own submission.
	expanded := id(t, "Task B: 运行 bash: echo B-TOOL，然后只回复 B-ANSWER")
	runPrompt := id(t, "阻塞检查")
	if expanded == runPrompt {
		t.Error("the expanded queued message shares the running turn's id")
	}

	if len(tools) != 6 {
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
	// The template run's tool belongs to the one submission it was recorded as.
	if got, want := byCall["call_0"]["prompt_id"], id(t, "Expanded opening task"); got != want {
		t.Errorf("call_0 prompt_id = %v, want %v", got, want)
	}
	// Every tool of the run carries its own submission, including the one that
	// finished after a queued message was rejected.
	for _, call := range []string{"call_1", "call_2", "call_4"} {
		if byCall[call] == nil || byCall[call]["prompt_id"] != runPrompt {
			t.Errorf("%s prompt_id = %v, want the run's %v", call, byCall[call], runPrompt)
		}
	}
	// A tool bound before the expanded message was accepted keeps its turn; the
	// one that ran after it belongs to the accepted message.
	if got, want := byCall["call_5"]["prompt_id"], id(t, "第二个问题"); got != want {
		t.Errorf("call_5 prompt_id = %v, want %v", got, want)
	}
	if got := byCall["call_6"]["prompt_id"]; got != expanded {
		t.Errorf("call_6 prompt_id = %v, want the accepted message's %v", got, expanded)
	}
	response, _ := bash["tool_response"].(map[string]any)
	if response["output"] != "hi\n" || response["exitCode"] != float64(0) {
		t.Errorf("tool_response lost content or details: %v", response)
	}
	response, _ = edit["tool_response"].(map[string]any)
	if response["patch"] != "@@ -1 +1 @@" || !strings.Contains(asString(response["output"]), "Successfully replaced") {
		t.Errorf("edit result lost a part: %v", response)
	}
	if len(failures) != 1 {
		t.Fatalf("failure payloads = %v", failures)
	}
	if failures[0]["error"] != "boom" || failures[0]["prompt_id"] != runPrompt {
		t.Errorf("failure = %v", failures[0])
	}
	if response, _ := failures[0]["tool_response"].(map[string]any); response["exitCode"] != float64(1) {
		t.Errorf("failed tool lost its details: %v", failures[0]["tool_response"])
	}

	// One agent.turn_completed per settled run, with the answer that run
	// produced and the submission it belongs to.
	if len(stops) != 6 {
		t.Fatalf("agent.turn_completed payloads = %v", stops)
	}
	settled := map[string]string{}
	for _, stop := range stops {
		settled[asString(stop["prompt_id"])] = asString(stop["last_assistant_message"])
	}
	for _, want := range []struct{ prompt, answer string }{
		{id(t, "Expanded opening task"), "ANSWER-OPENING"},
		{runPrompt, "DONE"},
		{id(t, "取消的新问题"), ""},
		{expanded, "ANSWER-QUEUED"},
		{id(t, "第三个问题"), ""},
		{id(t, "重投的问题"), "ANSWER-H"},
	} {
		got, ok := settled[want.prompt]
		if !ok {
			t.Errorf("no turn_completed for %q: %v", byText[want.prompt], settled)
			continue
		}
		if got != want.answer {
			t.Errorf("turn_completed for %q = %q, want %q", byText[want.prompt]["prompt"], got, want.answer)
		}
	}
	// The cancelled run must not report the previous run's answer.
	if got := settled[id(t, "取消的新问题")]; got != "" {
		t.Errorf("cancelled run reported %q", got)
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
	// /new ends the old session as switched; quit ends the current one; a fork
	// keeps the original session usable, so nothing is ended for it.
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
const returns = [];
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
	let first;
	let i = 0;
	for (const fn of handlers.get(name) || []) {
		const value = await fn(event, ctx);
		if (i === 0) first = value;
		i += 1;
	}
	elapsed[name] = Math.max(elapsed[name] || 0, Date.now() - started);
	return first;
};
// A run of the agent: the prompt message, then the model's work.
const openRun = async (prompt) => {
	await emit("input", { type: "input", text: prompt, source: "interactive" });
	const result = await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt });
	returns.push({ prompt, injected: Boolean(result && result.message) });
	await emit("agent_start", { type: "agent_start" });
	await emit("message_start", { type: "message_start", message: { role: "user", content: [{ type: "text", text: prompt }] } });
};

await emit("session_start", { type: "session_start", reason: "startup" });
const sentAtStartReturn = sent.length;

// A template submitted while idle: pi expands it before the run, so input and
// before_agent_start disagree on the text. It is one submission, recorded once,
// and the run's opening message must not be recorded again.
await emit("input", { type: "input", text: "/opening", source: "interactive" });
const templateRun = await emit("before_agent_start", {
	type: "before_agent_start",
	messageType: "user",
	prompt: "Expanded opening task",
});
returns.push({ prompt: "Expanded opening task", injected: Boolean(templateRun && templateRun.message) });
await emit("agent_start", { type: "agent_start" });
await emit("message_start", {
	type: "message_start",
	message: { role: "user", content: [{ type: "text", text: "Expanded opening task" }] },
});
// A retry of that run must keep the identity the announced text now names.
const templateRetry = await emit("before_agent_start", {
	type: "before_agent_start",
	messageType: "user",
	prompt: "Expanded opening task",
});
returns.push({ prompt: "Expanded opening task", injected: Boolean(templateRetry && templateRetry.message) });
await emit("tool_execution_start", { type: "tool_execution_start", toolCallId: "call_0", toolName: "bash" });
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_0",
	toolName: "bash",
	input: { command: "echo expanded" },
	content: [{ type: "text", text: "expanded\n" }],
	details: { exitCode: 0 },
	isError: false,
});
await emit("message_end", { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "ANSWER-OPENING" }] } });
await emit("agent_settled", { type: "agent_settled", aborted: false });

// One submission, retried: the retry reuses its identity.
await openRun("继续检查");
await emit("before_agent_start", { type: "before_agent_start", messageType: "user", prompt: "继续检查" });
returns.push({ prompt: "继续检查", injected: true });
// The same text sent again is a new submission.
await openRun("继续检查");

// A run another extension starts is not a user submission.
await emit("before_agent_start", { type: "before_agent_start", messageType: "custom", message: { content: "x" } });

// The prompt hook runs asynchronously: timers keep firing while it waits.
let ticks = 0;
const interval = setInterval(() => {
	ticks++;
}, 10);
await openRun("阻塞检查");
clearInterval(interval);

await emit("tool_execution_start", { type: "tool_execution_start", toolCallId: "call_1", toolName: "bash" });
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
// A queued message that is never accepted must not take the running turn.
await emit("input", { type: "input", text: "排队未消费", source: "rpc", streamingBehavior: "followUp" });
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_4",
	toolName: "bash",
	input: { command: "echo late" },
	content: [{ type: "text", text: "late\n" }],
	details: { exitCode: 0 },
	isError: false,
});
await emit("message_end", { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "DONE" }] } });
await emit("agent_settled", { type: "agent_settled", aborted: false });

// A prompt cancelled before it answers must not report the previous answer.
await openRun("取消的新问题");
await emit("message_end", { type: "message_end", message: { role: "assistant", content: [], stopReason: "aborted" } });
await emit("agent_settled", { type: "agent_settled", aborted: true });

// A queued template: the user typed a name, the model received the expanded
// text. The tool and the answer of that turn belong to what was received.
await openRun("第二个问题");
await emit("tool_execution_start", { type: "tool_execution_start", toolCallId: "call_5", toolName: "bash" });
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_5",
	toolName: "bash",
	input: { command: "echo first" },
	content: [{ type: "text", text: "first\n" }],
	details: { exitCode: 0 },
	isError: false,
});
await emit("input", { type: "input", text: "/queuedprobe", source: "rpc", streamingBehavior: "followUp" });
await emit("message_start", {
	type: "message_start",
	message: { role: "user", content: [{ type: "text", text: "Task B: 运行 bash: echo B-TOOL，然后只回复 B-ANSWER" }] },
});
await emit("tool_execution_start", { type: "tool_execution_start", toolCallId: "call_6", toolName: "review_tool" });
await emit("tool_result", {
	type: "tool_result",
	toolCallId: "call_6",
	toolName: "review_tool",
	input: { subject: "queued" },
	content: [{ type: "text", text: "reviewed\n" }],
	details: { ok: true },
	isError: false,
});
await emit("message_end", { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "ANSWER-QUEUED" }] } });
await emit("agent_settled", { type: "agent_settled", aborted: false });

// A queued message the user cancels (pi emits no event for clearQueue) and then
// sends again with the same text: the cancelled one is gone, the new one is a
// submission of its own, recorded and injected.
await openRun("第三个问题");
await emit("input", { type: "input", text: "重投的问题", source: "rpc", streamingBehavior: "followUp" });
await emit("agent_settled", { type: "agent_settled", aborted: true });
await openRun("重投的问题");
await emit("message_end", { type: "message_end", message: { role: "assistant", content: [{ type: "text", text: "ANSWER-H" }] } });
await emit("agent_settled", { type: "agent_settled", aborted: false });

await emit("session_before_compact", { type: "session_before_compact", reason: "threshold" });

// A steered message is received the same way, mid-run.
await emit("input", { type: "input", text: "串话", source: "rpc", streamingBehavior: "steer" });
await emit("message_start", { type: "message_start", message: { role: "user", content: [{ type: "text", text: "串话" }] } });

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

process.stdout.write(JSON.stringify({ elapsed, ticks, sent, returns, sentAtStartReturn }) + "\n");
`
