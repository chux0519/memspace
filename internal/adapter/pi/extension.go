package pi

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chux0519/memspace/internal/adapter"
)

// extensionSource is the pi extension MemSpace installs. It forwards pi's
// lifecycle, prompt and tool events to `memspace hook pi` using the hook
// payload shape the normalizer understands (the same one Claude Code sends),
// and injects the context MemSpace returns as an ordinary custom message, so
// pi keeps its MCP server instructions and everything else in the prompt.
//
// Every handler is wrapped in try/catch: a capture failure must never break
// the agent session. The hook process runs asynchronously with a deadline, so
// a slow MemSpace call never blocks pi's event loop, UI, cancellation or exit;
// only the prompt path waits for the hook, because it returns the context to
// inject.
const extensionSource = `// @ts-nocheck
// ` + extensionMarker + `; edits are overwritten.
// Reinstall: memspace agent install pi
// Hook command: __MEMSPACE_COMMAND__
import { randomUUID } from "node:crypto";
import { spawn } from "node:child_process";

const HOOK_ARGV = __MEMSPACE_ARGV__;
const HOOK_TIMEOUT_MS = 8000;
// The context message MemSpace adds; pi keeps the MCP server instructions and
// the rest of the system prompt untouched, this is one more message.
const CONTEXT_TYPE = "memspace-context";

// runHook spawns "memspace hook pi" and resolves with its stdout ("" on any
// failure, timeout, or when the process cannot start). detached is used for
// events fired while pi is tearing down: the hook must outlive us, and nobody
// waits for it.
function runHook(payload, options) {
	const opts = options || {};
	const timeout = typeof opts.timeout === "number" ? opts.timeout : HOOK_TIMEOUT_MS;
	const detached = opts.detached === true;
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
		try {
			child = spawn(HOOK_ARGV[0], HOOK_ARGV.slice(1), {
				stdio: detached ? ["pipe", "ignore", "ignore"] : ["pipe", "pipe", "pipe"],
				detached,
			});
		} catch {
			return finish("");
		}
		if (detached) {
			try {
				child.unref();
			} catch {}
		}
		let stdout = "";
		if (!detached && child.stdout) {
			child.stdout.setEncoding("utf8");
			child.stdout.on("data", (chunk) => {
				stdout += chunk;
			});
		}
		child.on("error", () => finish(stdout));
		child.on("close", () => finish(stdout));
		// A hook that fails to drain stdin can signal EPIPE; that must not
		// surface as an unhandled error on the agent process.
		if (child.stdin) child.stdin.on("error", () => {});
		timer = setTimeout(() => {
			try {
				if (child) child.kill();
			} catch {}
			finish(stdout);
		}, timeout);
		// A deadline timer must not hold pi's event loop open on its own.
		if (detached && timer && typeof timer.unref === "function") timer.unref();
		try {
			child.stdin.end(JSON.stringify(payload));
		} catch {
			finish(stdout);
		}
	});
}

// fire records an event without waiting for the hook; failures are swallowed.
function fire(payload, options) {
	try {
		runHook(payload, options).catch(() => {});
	} catch {}
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

function compactTrigger(reason) {
	return reason === "manual" ? "manual" : "auto";
}

export default function memspace(pi) {
	// The submission in flight, per session. A submission keeps its identity
	// when before_agent_start runs again for it (a retry), but two identical
	// prompts are two submissions and must both be recorded.
	const submissions = new Map();
	// The last assistant text per session, reported with agent_settled.
	const lastAssistant = new Map();
	// Sessions this instance already announced, so a session start that pi
	// emits twice for the same reason (it does after /new and /resume) is
	// recorded once. A replacement always builds a new instance, so a second
	// /resume of the same session still starts it.
	const announced = new Set();

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
	const modelName = (ctx) => {
		try {
			const m = ctx.model;
			return m ? String(m.provider || "") + "/" + String(m.id || "") : "";
		} catch {
			return "";
		}
	};
	const base = (ctx, id, event) => ({
		session_id: id,
		cwd: ctx.cwd || "",
		hook_event_name: event,
	});

	// Records a session boundary and queues its hand-off card. pi reports
	// "reload" for a runtime reload of the same session, which is neither a new
	// session nor a new conversation: nothing is sent, and the transcript keeps
	// the context it already received.
	const announceStart = async (ctx, reason) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			const out = await runHook({
				session_id: id,
				cwd: ctx.cwd || "",
				hook_event_name: "SessionStart",
				source: reason,
				model: modelName(ctx),
				transcript_path: sessionFile(ctx),
			});
			const text = additionalContext(out);
			if (!text) return;
			// steer without triggerTurn stores the message in the session without
			// starting a turn, so the next request carries it. nextTurn would only
			// reach pi's next user prompt, which print mode never sends.
			pi.sendMessage({ customType: CONTEXT_TYPE, content: text, display: false }, { deliverAs: "steer" });
		} catch {}
	};

	pi.on("session_start", (event, ctx) => {
		try {
			const reason = (event && event.reason) || "startup";
			if (reason === "reload") return;
			const id = sessionId(ctx);
			if (!id) return;
			const key = id + "\x00" + reason;
			if (announced.has(key)) return;
			announced.add(key);
			// Not awaited: the card arrives when the hook answers, and pi's UI
			// never waits for it.
			announceStart(ctx, reason);
		} catch {}
	});

	pi.on("session_shutdown", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			const reason = (event && event.reason) || "quit";
			// /new and /resume replace this runtime and leave the old session for
			// good, so it is closed as switched. A reload keeps the session, and a
			// fork keeps the original session resumable: both stay open.
			let end = "";
			if (reason === "new" || reason === "resume") end = "switch";
			else if (reason === "quit") end = "quit";
			if (!end) return;
			fire(Object.assign(base(ctx, id, "SessionEnd"), { reason: end }), { detached: true });
		} catch {}
	});

	// pi's input ingress fires once per user input, before command and template
	// expansion; it mints the identity that the later before_agent_start call
	// (and its retries) reuse.
	pi.on("input", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id || !event || typeof event.text !== "string" || !event.text.trim()) return;
			const submission = { text: event.text.trim(), id: randomUUID() };
			submissions.set(id, submission);
			// A steered or queued message is delivered into the run in progress
			// and never reaches before_agent_start, so it is recorded here.
			if (event.streamingBehavior) {
				fire(Object.assign(base(ctx, id, "UserPromptSubmit"), {
					prompt: event.text,
					prompt_id: submission.id,
					transcript_path: sessionFile(ctx),
				}));
			}
		} catch {}
	});

	pi.on("before_agent_start", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			// messageType "user" is a prompt (typed, RPC, or sendUserMessage);
			// "custom" is another extension starting a run, which is not a user
			// submission.
			if (!id || !event || event.messageType !== "user") return;
			if (typeof event.prompt !== "string" || !event.prompt.trim()) return;
			const prompt = event.prompt.trim();
			let submission = submissions.get(id);
			if (!submission || submission.text !== prompt) {
				submission = { text: prompt, id: randomUUID() };
				submissions.set(id, submission);
			}
			const out = await runHook(Object.assign(base(ctx, id, "UserPromptSubmit"), {
				prompt: event.prompt,
				prompt_id: submission.id,
				transcript_path: sessionFile(ctx),
			}));
			const text = additionalContext(out);
			if (!text) return;
			return { message: { customType: CONTEXT_TYPE, content: text, display: false } };
		} catch {}
	});

	pi.on("tool_result", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id || !event) return;
			const failed = event.isError === true;
			const text = contentText(event.content);
			const details =
				event.details && typeof event.details === "object" && !Array.isArray(event.details) ? event.details : {};
			// Keep both parts: the details a tool reports (a diff, exit code,
			// applied file list, ...) and the content body it returned.
			const response = Object.assign({}, details);
			if (text) response.output = text;
			const submission = submissions.get(id);
			const payload = Object.assign(base(ctx, id, failed ? "PostToolUseFailure" : "PostToolUse"), {
				tool_name: event.toolName,
				tool_use_id: event.toolCallId,
				tool_input: event.input || {},
				prompt_id: submission ? submission.id : "",
			});
			if (failed) payload.error = text;
			payload.tool_response = response;
			fire(payload);
		} catch {}
	});

	pi.on("message_end", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			const message = event && event.message;
			if (!id || !message || message.role !== "assistant") return;
			const text = textOf(message);
			if (text) lastAssistant.set(id, text);
		} catch {}
	});

	// agent_settled is pi's final idle signal: no retry, compaction retry or
	// queued message will follow it.
	pi.on("agent_settled", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			const submission = submissions.get(id);
			fire(Object.assign(base(ctx, id, "Stop"), {
				last_assistant_message: lastAssistant.get(id) || "",
				prompt_id: submission ? submission.id : "",
			}));
		} catch {}
	});

	pi.on("session_before_compact", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			fire(Object.assign(base(ctx, id, "PreCompact"), {
				trigger: compactTrigger(event && event.reason),
				custom_instructions: (event && event.customInstructions) || "",
			}));
		} catch {}
	});
}
`

// ExtensionSource renders the extension with the pinned hook argv embedded.
func ExtensionSource(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("pi: empty hook argv")
	}
	encoded, err := json.Marshal(argv)
	if err != nil {
		return nil, err
	}
	src := strings.ReplaceAll(extensionSource, "__MEMSPACE_ARGV__", string(encoded))
	src = strings.ReplaceAll(src, "__MEMSPACE_COMMAND__", adapter.Command(argv[0], "", "", argv[1:]...))
	return []byte(src), nil
}
