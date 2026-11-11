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
	const content = message && message.content;
	return typeof content === "string" ? content : contentText(content);
}

function compactTrigger(reason) {
	return reason === "manual" ? "manual" : "auto";
}

export default function memspace(pi) {
	// The submission that owns the current turn, per session. A submission keeps
	// its identity when before_agent_start runs again for it (a retry), but two
	// identical prompts are two submissions and must both be recorded.
	const active = new Map();
	// Submissions input minted for a run that pi has not started yet, newest
	// last. Only a prompt submitted while pi is idle becomes a candidate here;
	// a queued or steered message is recorded when the model actually receives
	// it, because its text (templates, skills) and its run are only known then.
	const pending = new Map();
	// A candidate is fresh for the next run only when it was minted after the
	// previous run boundary: a prompt that was cancelled in between must not be
	// picked up by a later, unrelated run.
	let seq = 0;
	const boundary = new Map();
	// Sessions whose next user message is the prompt before_agent_start already
	// announced. pi delivers that prompt as the run's first message, and a
	// template or an input handler may have changed its text since input, so it
	// is recognised by position and never recorded twice.
	const openingExpected = new Set();
	// The submission a tool call belongs to, bound when the call starts: a tool
	// that finishes after a queued message was accepted still belongs to the
	// turn that called it.
	const toolOwner = new Map();
	// The assistant text of the current run, per session, reported with
	// agent_settled. It is cleared when the run starts and never filled from an
	// empty (aborted) message, so an answer is never carried into the next run.
	const runAssistant = new Map();
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

	// takePending returns the candidate this run is for: the newest one that
	// still has the text before_agent_start reports, or, when the text changed
	// (a template or skill expanded, an input handler rewrote it), simply the
	// newest fresh one. Candidates from before the last run boundary are
	// dropped: they were never used, so no later run owns them.
	const takePending = (id, text) => {
		const queue = pending.get(id);
		if (!queue || !queue.length) return null;
		const cutoff = boundary.get(id) || 0;
		const live = queue.filter((s) => s.seq > cutoff);
		pending.delete(id);
		if (!live.length) return null;
		let chosen = live[live.length - 1];
		for (let i = live.length - 1; i >= 0; i -= 1) {
			if (live[i].text === text) {
				chosen = live[i];
				break;
			}
		}
		const rest = live.filter((s) => s !== chosen);
		if (rest.length) pending.set(id, rest);
		return chosen;
	};

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
			// A message pi queues into the turn that is already running never
			// reaches before_agent_start, and by the time the model sees it the text
			// may be expanded from a template or skill. It is recorded from that
			// actual message instead, so it is not a candidate here.
			if (event.streamingBehavior) return;
			const queue = pending.get(id) || [];
			queue.push({ text: event.text.trim(), id: randomUUID(), seq: (seq += 1) });
			// Bound: a prompt that never reaches a run must not pile up.
			pending.set(id, queue.slice(-16));
		} catch {}
	});

	// A run starts: nothing has answered yet, so an aborted or empty run cannot
	// report the previous run's answer.
	pi.on("agent_start", (_event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			runAssistant.delete(id);
			boundary.set(id, seq);
		} catch {}
	});

	pi.on("before_agent_start", async (event, ctx) => {
		try {
			const id = sessionId(ctx);
			// messageType "user" is a prompt (typed, RPC, or sendUserMessage);
			// "custom" is another extension starting a run, which is not a user
			// submission and announces no prompt of ours.
			if (!id || !event) return;
			if (event.messageType !== "user") {
				openingExpected.delete(id);
				return;
			}
			if (typeof event.prompt !== "string" || !event.prompt.trim()) return;
			const prompt = event.prompt.trim();
			runAssistant.delete(id);
			const previous = active.get(id);
			// The run belongs to the submission input minted for it, or to the
			// submission this run is retrying; anything else is a message whose
			// input pi never reported.
			let submission = takePending(id, prompt);
			if (!submission) {
				submission =
					previous && previous.text === prompt ? previous : { text: prompt, id: randomUUID(), seq: (seq += 1) };
			}
			// Keep the text this run was announced with, so a retry still matches it
			// after a template, a skill or an input handler rewrote it.
			submission.text = prompt;
			active.set(id, submission);
			// A retried run has already delivered its opening message.
			if (submission === previous) openingExpected.delete(id);
			else openingExpected.add(id);
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

	// pi consumes queued and steered messages inside the run that is already
	// going: no new before_agent_start and no new agent_start, only the user
	// message itself, already expanded. This is the only reliable statement
	// about what the model received, so the submission is minted here and owns
	// the tools and the answer that follow. Several messages merged into one
	// request are one such message: one prompt, no invented pairs.
	pi.on("message_start", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			const message = event && event.message;
			if (!id || !message || message.role !== "user") return;
			const text = textOf(message).trim();
			if (openingExpected.has(id)) {
				// The prompt before_agent_start announced, delivered as the run's first
				// message: already recorded under its own submission, whatever text pi
				// or an input handler produced.
				openingExpected.delete(id);
				return;
			}
			const submission = { text, id: randomUUID(), seq: (seq += 1) };
			active.set(id, submission);
			fire(Object.assign(base(ctx, id, "UserPromptSubmit"), {
				prompt: text,
				prompt_id: submission.id,
				transcript_path: sessionFile(ctx),
			}));
		} catch {}
	});

	// A tool call belongs to the submission that was active when it started.
	pi.on("tool_execution_start", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			const submission = id ? active.get(id) : null;
			if (submission && event && event.toolCallId) toolOwner.set(event.toolCallId, submission.id);
		} catch {}
	});

	pi.on("tool_execution_end", (event) => {
		try {
			if (event && event.toolCallId) toolOwner.delete(event.toolCallId);
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
			const submission = active.get(id);
			const owner = toolOwner.get(event.toolCallId);
			const payload = Object.assign(base(ctx, id, failed ? "PostToolUseFailure" : "PostToolUse"), {
				tool_name: event.toolName,
				tool_use_id: event.toolCallId,
				tool_input: event.input || {},
				prompt_id: owner || (submission ? submission.id : ""),
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
			// An aborted run ends with an empty message; that must not overwrite
			// anything, and there is nothing to report for this run.
			const text = textOf(message);
			if (text) runAssistant.set(id, text);
		} catch {}
	});

	// agent_settled is pi's final idle signal: no retry, compaction retry or
	// queued message will follow it.
	pi.on("agent_settled", (event, ctx) => {
		try {
			const id = sessionId(ctx);
			if (!id) return;
			boundary.set(id, seq);
			openingExpected.delete(id);
			const submission = active.get(id);
			const answer = runAssistant.get(id) || "";
			runAssistant.delete(id);
			fire(Object.assign(base(ctx, id, "Stop"), {
				last_assistant_message: answer,
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
