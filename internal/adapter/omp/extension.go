package omp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chux0519/memspace/internal/adapter"
)

// extensionSource is the omp extension MemSpace installs. It forwards the
// events omp exposes to `memspace hook omp` using the hook payload shape the
// normalizer understands (the same one Claude Code sends), and injects the
// context MemSpace returns.
//
// Every handler is wrapped in try/catch: a capture failure must never break
// the agent session.
const extensionSource = `// @ts-nocheck
// ` + extensionMarker + `; edits are overwritten.
// Reinstall: memspace agent install omp
// Hook command: __MEMSPACE_COMMAND__
import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";

const HOOK_ARGV = __MEMSPACE_ARGV__;

function runHook(payload) {
	try {
		const result = spawnSync(HOOK_ARGV[0], HOOK_ARGV.slice(1), {
			input: JSON.stringify(payload),
			encoding: "utf8",
			timeout: 8000,
			stdio: ["pipe", "pipe", "pipe"],
		});
		return result && typeof result.stdout === "string" ? result.stdout : "";
	} catch {
		return "";
	}
}

function additionalContext(stdout) {
	const trimmed = (stdout || "").trim();
	if (!trimmed) return "";
	try {
		const parsed = JSON.parse(trimmed);
		const out = parsed && parsed.hookSpecificOutput;
		return out && typeof out.additionalContext === "string" ? out.additionalContext : "";
	} catch {
		return "";
	}
}

function textOf(message) {
	if (!message || !Array.isArray(message.content)) return "";
	const parts = [];
	for (const block of message.content) {
		if (block && block.type === "text" && typeof block.text === "string") parts.push(block.text);
	}
	return parts.join("\n");
}

function contentText(content) {
	const parts = [];
	for (const block of Array.isArray(content) ? content : []) {
		if (block && block.type === "text" && typeof block.text === "string") parts.push(block.text);
	}
	return parts.join("\n");
}

export default function memspace(pi) {
	// Session start directories, keyed by session id: task subagents share
	// this module instance but have their own sessions.
	const cwds = new Map();

	const sessionId = (ctx) => {
		try {
			return ctx.sessionManager.getSessionId() || "";
		} catch {
			return "";
		}
	};
	const sessionFile = (ctx) => {
		try {
			return ctx.sessionManager.getSessionFile() || "";
		} catch {
			return "";
		}
	};

	pi.on("session_start", async (_event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			cwds.set(id, ctx.cwd || "");
			const out = runHook({
				session_id: id,
				cwd: ctx.cwd || "",
				hook_event_name: "SessionStart",
				source: "startup",
				model: ctx.model ? String(ctx.model.provider || "") + "/" + String(ctx.model.id || "") : "",
				transcript_path: sessionFile(ctx),
			});
			const text = additionalContext(out);
			if (text) {
				pi.sendMessage(
					{ customType: "memspace-context", content: text, display: false, attribution: "agent" },
					{ deliverAs: "nextTurn" },
				);
			}
		} catch {}
	});

	pi.on("before_agent_start", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id || !event || event.reason !== "prompt" || !event.prompt) return;
			const out = runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: "UserPromptSubmit",
				prompt: event.prompt,
				// Stable across re-entry of this handler for the same submission,
				// so a retry deduplicates on the server instead of double-writing.
				prompt_id: createHash("sha256").update(event.prompt).digest("hex").slice(0, 32),
				transcript_path: sessionFile(ctx),
			});
			const text = additionalContext(out);
			if (text) {
				return { message: { customType: "memspace-context", content: text, display: false, attribution: "agent" } };
			}
		} catch {}
	});

	pi.on("tool_result", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			const text = contentText(event.content);
			const failed = event.isError === true;
			runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: failed ? "PostToolUseFailure" : "PostToolUse",
				tool_name: event.toolName,
				tool_use_id: event.toolCallId,
				tool_input: event.input || {},
				tool_response: event.details === undefined ? text : event.details,
				error: failed ? text : undefined,
			});
		} catch {}
	});

	pi.on("session_stop", async (event, ctx) => {
		try {
			const id = (event && event.session_id) || sessionId(ctx);
			if (!id) return;
			runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: "Stop",
				last_assistant_message: textOf(event && event.last_assistant_message),
			});
		} catch {}
	});

	pi.on("session.compacting", async (event, ctx) => {
		try {
			const id = (event && event.sessionId) || sessionId(ctx);
			if (!id) return;
			runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: "PreCompact",
				trigger: "auto",
			});
		} catch {}
	});

	pi.on("session_shutdown", async (_event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: "SessionEnd",
				reason: "shutdown",
			});
		} catch {}
	});
}
`

// ExtensionSource renders the extension with the pinned hook argv embedded.
func ExtensionSource(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("omp: empty hook argv")
	}
	encoded, err := json.Marshal(argv)
	if err != nil {
		return nil, err
	}
	src := strings.ReplaceAll(extensionSource, "__MEMSPACE_ARGV__", string(encoded))
	src = strings.ReplaceAll(src, "__MEMSPACE_COMMAND__", adapter.Command(argv[0], "", "", argv[1:]...))
	return []byte(src), nil
}
