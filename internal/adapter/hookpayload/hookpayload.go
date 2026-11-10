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
	"io/fs"
	"os"
	"path/filepath"
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
		files := AppliedFiles(in.ToolName, in.ToolInput, in.ToolResponse, in.Cwd)
		referenced := ReferencedFiles(in.ToolName, in.ToolInput, in.ToolResponse, in.Cwd)
		sensitive := sensitiveInput(in.ToolInput, referenced)
		toolInput, response := sanitizeTool(in.ToolInput, in.ToolResponse, sensitive)
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
		if !failed {
			for _, path := range files {
				fid := adapter.RandomID("cc")
				if in.ToolUseID != "" {
					fid = adapter.StableID("cc", sid, "file.modified", in.ToolUseID, path)
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

// FilePath returns the single file a tool names in its input, when it names
// one directly.
func FilePath(toolInput map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s, ok := toolInput[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// fileTools modify the files named in tool_input. Names are compared
// case-insensitively: Claude Code sends "Write", omp sends "write".
var fileTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "notebookedit": true, "ast_edit": true}

// IsFileTool reports whether a tool writes files.
func IsFileTool(name string) bool { return fileTools[strings.ToLower(name)] }

// AppliedFiles is the set of files the tool actually changed.
//
// omp's ast_edit rewrites files, directories and globs, so its input names
// targets, not results: a preview or a zero-replacement run changes nothing,
// and one glob can hit several files. Its tool response carries the applied
// file list, which is what MemSpace records. A write to an internal xd:// URL
// dispatches another tool; that tool's result is nested under
// tool_response.xdev.inner.sourceResultDetails, so the write's own "path"
// is never recorded as a changed file. Every other file tool names its single
// target in the input.
func AppliedFiles(name string, toolInput map[string]any, response any, cwd string) []string {
	if resultAuthoritative(name, response) {
		if _, applied, ok := resultFiles(name, response); ok {
			return resolvePaths(cwd, applied)
		}
	}
	lower := strings.ToLower(name)
	if !fileTools[lower] {
		return nil
	}
	p := FilePath(toolInput)
	if p == "" || isInternalURL(p) {
		return nil
	}
	return []string{p}
}

// ReferencedFiles is every file the tool's result mentions, including files an
// ast_edit preview would touch. Sensitivity checks use it so staged edits of
// secret files are redacted even before they are applied.
func ReferencedFiles(name string, toolInput map[string]any, response any, cwd string) []string {
	files := pathsOf(toolInput)
	if resultAuthoritative(name, response) {
		if all, _, ok := resultFiles(name, response); ok {
			files = append(files, all...)
		}
	}
	return resolvePaths(cwd, files)
}

// resultAuthoritative reports whether the response, not the input, names the
// files a tool changed. That is true for xd:// dispatch results and for
// ast_edit's applied/preview details; a plain write reports bytes, not paths.
func resultAuthoritative(name string, response any) bool {
	if m, _ := response.(map[string]any); m != nil {
		if _, ok := xdevSourceDetails(m); ok {
			return true
		}
	}
	return strings.EqualFold(name, "ast_edit")
}

// resultFiles extracts the files and applied files from a structured tool
// result, unwrapping an xd:// dispatch when present.
func resultFiles(name string, response any) (all, applied []string, ok bool) {
	m, _ := response.(map[string]any)
	if m == nil {
		return nil, nil, false
	}
	if inner, found := xdevSourceDetails(m); found {
		m = inner
	} else if !IsFileTool(name) {
		return nil, nil, false
	}
	files := stringList(m["files"])
	if list, ok := m["fileReplacements"].([]any); ok {
		for _, item := range list {
			if entry, ok := item.(map[string]any); ok {
				if p, ok := entry["path"].(string); ok && p != "" {
					files = append(files, p)
				}
			}
		}
	}
	files = dedupePaths(files)
	appliedOK := true
	if v, ok := m["applied"].(bool); ok && !v {
		appliedOK = false
	}
	if n, ok := m["totalReplacements"].(float64); ok && n <= 0 {
		appliedOK = false
	}
	if appliedOK {
		applied = files
	}
	return files, applied, true
}

// xdevSourceDetails unwraps the device result of a write to an internal URL.
func xdevSourceDetails(m map[string]any) (map[string]any, bool) {
	xdev, _ := m["xdev"].(map[string]any)
	inner, _ := xdev["inner"].(map[string]any)
	src, _ := inner["sourceResultDetails"].(map[string]any)
	return src, src != nil
}

func isInternalURL(p string) bool {
	return strings.Contains(p, "://")
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := paths[:0]
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// resolvePaths makes relative result paths absolute against the session cwd
// and removes duplicates while keeping order.
func resolvePaths(cwd string, paths []string) []string {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		if p == "" || isInternalURL(p) {
			continue
		}
		if !filepath.IsAbs(p) && cwd != "" {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func stringList(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return list
	case string:
		if list != "" {
			return []string{list}
		}
	}
	return nil
}

// pathsOf returns every path a tool input names.
func pathsOf(toolInput map[string]any) []string {
	var out []string
	if p := FilePath(toolInput); p != "" {
		out = append(out, p)
	}
	out = append(out, stringList(toolInput["paths"])...)
	return out
}

// sanitizeTool drops captured content of secret files and of shell commands
// that read them.
func sanitizeTool(toolInput map[string]any, response any, sensitive bool) (map[string]any, any) {
	if toolInput == nil {
		toolInput = map[string]any{}
	}
	if !sensitive {
		return toolInput, response
	}
	for _, k := range []string{"content", "new_string", "old_string", "edits", "new_source", "pat", "out"} {
		if _, ok := toolInput[k]; ok {
			toolInput[k] = adapter.OmittedSensitive
		}
	}
	// omp's ast_edit carries the matched text and its replacement in ops.
	if ops, ok := toolInput["ops"].([]any); ok {
		for _, op := range ops {
			m, _ := op.(map[string]any)
			for _, k := range []string{"pat", "out"} {
				if _, ok := m[k]; ok {
					m[k] = adapter.OmittedSensitive
				}
			}
		}
	}
	return toolInput, adapter.OmittedSensitive
}

// sensitiveInput reports whether any file the tool touches is sensitive.
// Directories and globs are expanded, bounded, so "config/*.json" that hits
// config/secrets.json is treated as sensitive even though the input string
// itself is not a sensitive name. files are the applied results.
func sensitiveInput(toolInput map[string]any, files []string) bool {
	for _, path := range append(pathsOf(toolInput), files...) {
		if pathSensitive(path) {
			return true
		}
	}
	if cmd, ok := toolInput["command"].(string); ok && adapter.MentionsSensitivePath(cmd) {
		return true
	}
	return false
}

// scanLimit bounds directory walks and glob expansion done from a hook, so
// inspecting a large tree never delays the agent.
const scanLimit = 4096

// pathSensitive resolves a literal path, a glob, or a directory to the files
// it stands for and reports whether any of them is sensitive.
func pathSensitive(p string) bool {
	if p == "" {
		return false
	}
	if redact.SensitivePath(p) {
		return true
	}
	if strings.ContainsAny(p, "*?[") {
		matches, err := filepath.Glob(p)
		if err != nil {
			return false
		}
		if len(matches) > scanLimit {
			matches = matches[:scanLimit]
		}
		for _, match := range matches {
			if redact.SensitivePath(match) {
				return true
			}
		}
		return false
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return false
	}
	found, visited := false, 0
	filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > scanLimit {
			return filepath.SkipAll
		}
		if !d.IsDir() && redact.SensitivePath(path) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
