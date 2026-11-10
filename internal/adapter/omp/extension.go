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
// the agent session. The hook process runs asynchronously with a deadline, so
// a slow MemSpace call never blocks omp's event loop, UI, or cancellation.
const extensionSource = `// @ts-nocheck
// ` + extensionMarker + `; edits are overwritten.
// Reinstall: memspace agent install omp
// Hook command: __MEMSPACE_COMMAND__
import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";

const HOOK_ARGV = __MEMSPACE_ARGV__;
const HOOK_TIMEOUT_MS = 8000;

function runHook(payload) {
	return new Promise((resolve) => {
		let settled = false;
		let child = null;
		let timer = null;
		const finish = (value) => {
			if (settled) return;
			settled = true;
			if (timer) clearTimeout(timer);
			resolve(value);
		};
		const kill = () => {
			try {
				if (child) child.kill();
			} catch {}
		};
		try {
			child = spawn(HOOK_ARGV[0], HOOK_ARGV.slice(1), { stdio: ["pipe", "pipe", "pipe"] });
		} catch {
			return finish("");
		}
		let stdout = "";
		child.stdout.setEncoding("utf8");
		child.stdout.on("data", (chunk) => {
			stdout += chunk;
		});
		child.on("error", () => finish(""));
		child.on("close", () => finish(stdout));
		// A hook that fails to drain stdin can signal EPIPE; that must not
		// surface as an unhandled error on the agent process.
		child.stdin.on("error", () => {});
		timer = setTimeout(() => {
			kill();
			finish("");
		}, HOOK_TIMEOUT_MS);
		child.stdin.end(JSON.stringify(payload));
	});
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

function contentText(content) {
	const parts = [];
	for (const block of Array.isArray(content) ? content : []) {
		if (block && block.type === "text" && typeof block.text === "string") parts.push(block.text);
	}
	return parts.join("\n");
}

function textOf(message) {
	return contentText(message && message.content);
}

export default function memspace(pi) {
	// Session start directories, keyed by session id: task subagents share
	// this module instance but have their own sessions.
	const cwds = new Map();
	// The current submission, keyed by session id. A submission keeps its
	// identity across before_agent_start re-entry, but two identical prompts
	// are two submissions and must both be recorded.
	const submissions = new Map();

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

	// Records a session boundary and injects its hand-off card. omp emits
	// session_start only for the session the process loaded; /new and
	// /resume switch sessions in place and emit session_switch instead.
	const announceStart = async (ctx, source) => {
		const id = sessionId(ctx);
		if (!id) return;
		cwds.set(id, cwds.get(id) || ctx.cwd || "");
		const out = await runHook({
			session_id: id,
			cwd: cwds.get(id) || ctx.cwd || "",
			hook_event_name: "SessionStart",
			source,
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
	};

	pi.on("session_start", async (_event, ctx) => {
		try {
			await announceStart(ctx, "startup");
		} catch {}
	});

	pi.on("session_switch", async (event, ctx) => {
		try {
			const reason = event && event.reason;
			await announceStart(ctx, reason === "resume" ? "resume" : reason === "fork" ? "fork" : "startup");
		} catch {}
	});

	// The submission ingress fires once per user input; it mints the identity
	// that the later before_agent_start call (and its retries) reuse.
	pi.on("input", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id || !event || typeof event.text !== "string" || !event.text.trim()) return;
			submissions.set(id, { text: event.text.trim(), id: randomUUID() });
		} catch {}
	});

	pi.on("before_agent_start", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id || !event || event.reason !== "prompt" || !event.prompt) return;
			const prompt = event.prompt.trim();
			let submission = submissions.get(id);
			if (!submission || submission.text !== prompt) {
				submission = { text: prompt, id: randomUUID() };
				submissions.set(id, submission);
			}
			const out = await runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: "UserPromptSubmit",
				prompt: event.prompt,
				prompt_id: submission.id,
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
			let response;
			if (!failed) {
				const details =
					event.details && typeof event.details === "object" && !Array.isArray(event.details) ? event.details : {};
				// omp's Bash keeps its output in content and puts timing and
				// similar metadata in details; keep both.
				response = Object.assign({}, details);
				if (text) response.output = text;
			}
			await runHook({
				session_id: id,
				cwd: cwds.get(id) || ctx.cwd || "",
				hook_event_name: failed ? "PostToolUseFailure" : "PostToolUse",
				tool_name: event.toolName,
				tool_use_id: event.toolCallId,
				tool_input: event.input || {},
				tool_response: response,
				error: failed ? text : undefined,
			});
		} catch {}
	});

	pi.on("session_stop", async (event, ctx) => {
		try {
			const id = (event && event.session_id) || sessionId(ctx);
			if (!id) return;
			await runHook({
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
			await runHook({
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
			await runHook({
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
