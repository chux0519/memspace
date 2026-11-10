// Package hookpayload normalizes the hook payloads MemSpace's hook-based
// agents send on stdin into spool records.
//
// Claude Code defines this payload shape; DSH's hook bridge and the OMP
// extension reuse it, so one normalizer serves every hook-based adapter. The
// agent_type stored on sessions is the caller's, not the shape's.
package hookpayload

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/spool"
)

// Input is the union of the hook payload fields MemSpace uses.
type Input struct {
	SessionID            string         `json:"session_id"`
	TranscriptPath       string         `json:"transcript_path"`
	Cwd                  string         `json:"cwd"`
	HookEventName        string         `json:"hook_event_name"`
	PermissionMode       string         `json:"permission_mode"`
	Source               string         `json:"source"`
	Model                string         `json:"model"`
	Prompt               string         `json:"prompt"`
	PromptID             string         `json:"prompt_id"`
	ToolName             string         `json:"tool_name"`
	ToolInput            map[string]any `json:"tool_input"`
	ToolResponse         any            `json:"tool_response"`
	ToolUseID            string         `json:"tool_use_id"`
	Error                string         `json:"error"`
	IsInterrupt          bool           `json:"is_interrupt"`
	DurationMS           *int64         `json:"duration_ms"`
	LastAssistantMessage string         `json:"last_assistant_message"`
	Reason               string         `json:"reason"`
	Trigger              string         `json:"trigger"`
	CustomInstructions   string         `json:"custom_instructions"`
}

// Normalize maps one hook invocation to MemSpace events for agentType.
// Unknown or uninteresting hook events yield no events. projectDir resolves
// the directory that identifies the session's project; nil means the
// payload's cwd.
func Normalize(agentType string, raw []byte, now time.Time, projectDir func(cwd string) string) ([]spool.Record, error) {
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("%s hook: parse input: %w", agentType, err)
	}
	if in.SessionID == "" || in.HookEventName == "" {
		return nil, fmt.Errorf("%s hook: session_id and hook_event_name are required", agentType)
	}
	dir := in.Cwd
	if projectDir != nil {
		dir = projectDir(in.Cwd)
	}
	hint := project.Detect(dir)
	rec := func(id, typ string, payload map[string]any) spool.Record {
		return spool.Record{
			EventID:         id,
			Agent:           agentType,
			NativeSessionID: in.SessionID,
			EventType:       typ,
			OccurredAt:      now.UTC(),
			Project:         hint,
			Payload:         adapter.Payload(payload),
		}
	}
	sid := in.SessionID

	switch in.HookEventName {
	case "SessionStart":
		return []spool.Record{rec(adapter.RandomID("cc"), "session.started", map[string]any{
			"source": in.Source, "model": in.Model, "cwd": in.Cwd,
			"permission_mode": in.PermissionMode, "transcript_path": in.TranscriptPath,
		})}, nil

	case "UserPromptSubmit":
		id := adapter.RandomID("cc")
		if in.PromptID != "" {
			id = adapter.StableID("cc", sid, "prompt", in.PromptID)
		}
		p := prompt.Payload(in.Prompt)
		p["prompt_id"], p["cwd"] = in.PromptID, in.Cwd
		return []spool.Record{rec(id, "user.prompt", p)}, nil

	case "PostToolUse", "PostToolUseFailure":
		failed := in.HookEventName == "PostToolUseFailure"
		toolInput, response := sanitizeTool(in.ToolName, in.ToolInput, in.ToolResponse)
		p := map[string]any{
			"tool_name": in.ToolName, "tool_use_id": in.ToolUseID, "tool_input": toolInput,
			"duration_ms": in.DurationMS, "prompt_id": in.PromptID,
		}
		typ := "tool.completed"
		if failed {
			typ = "tool.failed"
			p["error"] = in.Error
			p["is_interrupt"] = in.IsInterrupt
		} else {
			p["tool_response"] = response
		}
		id := adapter.RandomID("cc")
		if in.ToolUseID != "" {
			id = adapter.StableID("cc", sid, typ, in.ToolUseID)
		}
		out := []spool.Record{rec(id, typ, p)}
		if !failed && IsFileTool(in.ToolName) {
			if path := FilePath(in.ToolInput); path != "" {
				fid := adapter.RandomID("cc")
				if in.ToolUseID != "" {
					fid = adapter.StableID("cc", sid, "file.modified", in.ToolUseID)
				}
				out = append(out, rec(fid, "file.modified", map[string]any{
					"path": path, "tool_name": in.ToolName, "tool_use_id": in.ToolUseID,
				}))
			}
		}
		return out, nil

	case "Stop":
		return []spool.Record{rec(adapter.RandomID("cc"), "agent.turn_completed", map[string]any{
			"last_assistant_message": in.LastAssistantMessage, "prompt_id": in.PromptID,
		})}, nil

	case "PreCompact":
		return []spool.Record{rec(adapter.RandomID("cc"), "session.compacting", map[string]any{
			"trigger": in.Trigger, "custom_instructions": in.CustomInstructions,
		})}, nil

	case "SessionEnd":
		return []spool.Record{rec(adapter.RandomID("cc"), "session.ended", map[string]any{
			"reason": in.Reason,
		})}, nil
	}
	return nil, nil
}

// FilePath returns the file a tool modified, when the tool names one.
func FilePath(toolInput map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s, ok := toolInput[k].(string); ok && s != "" {
			return s
		}
	}
	// omp's ast_edit takes a list of files, directories or globs.
	if list, ok := toolInput["paths"].([]any); ok && len(list) > 0 {
		if s, ok := list[0].(string); ok {
			return s
		}
	}
	return ""
}

// fileTools modify the file named in tool_input. Names are compared
// case-insensitively: Claude Code sends "Write", omp sends "write".
var fileTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "notebookedit": true, "ast_edit": true}

// IsFileTool reports whether a tool writes the file named in its input.
func IsFileTool(name string) bool { return fileTools[strings.ToLower(name)] }

// sanitizeTool drops captured content of secret files and of shell commands
// that read them.
func sanitizeTool(tool string, toolInput map[string]any, response any) (map[string]any, any) {
	if toolInput == nil {
		toolInput = map[string]any{}
	}
	sensitive := redact.SensitivePath(FilePath(toolInput))
	if cmd, ok := toolInput["command"].(string); ok && adapter.MentionsSensitivePath(cmd) {
		sensitive = true
	}
	if !sensitive {
		return toolInput, response
	}
	for _, k := range []string{"content", "new_string", "old_string", "edits", "new_source"} {
		if _, ok := toolInput[k]; ok {
			toolInput[k] = adapter.OmittedSensitive
		}
	}
	return toolInput, adapter.OmittedSensitive
}
