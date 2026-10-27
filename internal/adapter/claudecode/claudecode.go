// Package claudecode captures Claude Code activity through its command hooks.
//
// Claude Code runs each configured hook command with a JSON description of
// the event on stdin (session_id, cwd, hook_event_name, plus event fields).
// `memspace hook claude` normalizes that into MemSpace events, spools them,
// and returns immediately; uploading happens in a detached sync process.
package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/spool"
)

const agentName = "claude"

// AgentType is the agent_type stored on sessions.
const AgentType = "claude-code"

// Hooks MemSpace subscribes to.
var hookSpecs = []adapter.HookSpec{
	{Event: "SessionStart", Timeout: 10},
	{Event: "UserPromptSubmit", Timeout: 10},
	{Event: "PostToolUse", Matcher: "*", Timeout: 10},
	{Event: "PostToolUseFailure", Matcher: "*", Timeout: 10},
	{Event: "Stop", Timeout: 10},
	{Event: "PreCompact", Timeout: 10},
	{Event: "SessionEnd", Timeout: 10},
}

// Adapter implements adapter.Adapter for Claude Code.
type Adapter struct {
	// ConfigDir overrides the Claude config dir (default $CLAUDE_CONFIG_DIR or ~/.claude).
	ConfigDir string
}

func (a *Adapter) Name() string { return agentName }

func (a *Adapter) configDir() string {
	if a.ConfigDir != "" {
		return a.ConfigDir
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// SettingsPath is the user-level settings file hooks are installed into.
func (a *Adapter) SettingsPath() string { return filepath.Join(a.configDir(), "settings.json") }

func (a *Adapter) Capabilities() []string {
	return []string{"session.started", "user.prompt", "tool.completed", "tool.failed",
		"file.modified", "agent.turn_completed", "session.compacting", "session.ended"}
}

func (a *Adapter) Detect(ctx context.Context) adapter.Detection {
	d := adapter.Detection{ConfigDir: a.configDir()}
	if p, err := exec.LookPath("claude"); err == nil {
		d.Found = true
		d.Binary = p
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, p, "--version").Output(); err == nil {
			d.Version = strings.TrimSpace(string(out))
		}
	} else if _, err := os.Stat(d.ConfigDir); err == nil {
		d.Found = true
	}
	return d
}

func (a *Adapter) Install(ctx context.Context, opts adapter.InstallOptions) error {
	path := a.SettingsPath()
	doc, _, err := adapter.LoadJSONFile(path)
	if err != nil {
		return err
	}
	if !adapter.MergeHooks(doc, agentName, opts.HookCommand, hookSpecs) {
		fmt.Fprintf(opts.Out, "Claude Code hooks already installed in %s\n", path)
		return nil
	}
	if err := adapter.SaveJSONFile(path, doc); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "Installed Claude Code hooks in %s\n", path)
	fmt.Fprintf(opts.Out, "  events:  %s\n", eventNames())
	fmt.Fprintf(opts.Out, "  command: %s\n", opts.HookCommand)
	fmt.Fprintf(opts.Out, "New Claude Code sessions will be captured; restart running ones.\n")
	return nil
}

func eventNames() string {
	var names []string
	for _, s := range hookSpecs {
		names = append(names, s.Event)
	}
	return strings.Join(names, ", ")
}

func (a *Adapter) Uninstall(ctx context.Context, out io.Writer) error {
	path := a.SettingsPath()
	doc, exists, err := adapter.LoadJSONFile(path)
	if err != nil {
		return err
	}
	if !exists || !adapter.RemoveHooks(doc, agentName) {
		fmt.Fprintf(out, "No MemSpace hooks found in %s\n", path)
		return nil
	}
	if err := adapter.SaveJSONFile(path, doc); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed MemSpace hooks from %s\n", path)
	return nil
}

func (a *Adapter) Doctor(ctx context.Context, opts adapter.InstallOptions) []adapter.Check {
	var checks []adapter.Check
	d := a.Detect(ctx)
	switch {
	case d.Binary != "":
		checks = append(checks, adapter.Check{Name: "claude binary", Status: adapter.OK, Detail: d.Binary + " " + d.Version})
	case d.Found:
		checks = append(checks, adapter.Check{Name: "claude binary", Status: adapter.Warn, Detail: "not on PATH, but " + d.ConfigDir + " exists"})
	default:
		checks = append(checks, adapter.Check{Name: "claude binary", Status: adapter.Fail, Detail: "Claude Code not found"})
	}

	doc, exists, err := adapter.LoadJSONFile(a.SettingsPath())
	if err != nil {
		return append(checks, adapter.Check{Name: "settings", Status: adapter.Fail, Detail: err.Error()})
	}
	if !exists {
		return append(checks, adapter.Check{Name: "hooks", Status: adapter.Fail,
			Detail: a.SettingsPath() + " missing (run 'memspace agent install claude')"})
	}
	checks = append(checks, adapter.HookChecks(doc, agentName, opts.HookCommand, hookSpecs))
	checks = append(checks, adapter.SelfTest(ctx, agentName, installedCommand(doc), samplePostToolUse()))
	return checks
}

func installedCommand(doc map[string]any) string {
	for _, cmds := range adapter.InstalledHooks(doc, agentName) {
		return cmds[0]
	}
	return ""
}

func samplePostToolUse() []byte {
	b, _ := json.Marshal(map[string]any{
		"session_id":      "memspace-doctor",
		"cwd":             os.TempDir(),
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_use_id":     "toolu_doctor",
		"tool_input":      map[string]any{"command": "echo ok"},
		"tool_response":   map[string]any{"stdout": "ok", "stderr": ""},
	})
	return b
}

// ---------------------------------------------------------------------------
// Normalization

// input is the union of the hook payload fields MemSpace uses.
type input struct {
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

// fileTools modify the file named in tool_input.file_path / notebook_path.
var fileTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// Normalize maps one hook invocation to MemSpace events. Unknown or
// uninteresting hook events yield no events.
func (a *Adapter) Normalize(raw []byte, now time.Time) ([]spool.Record, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("claude hook: parse input: %w", err)
	}
	if in.SessionID == "" || in.HookEventName == "" {
		return nil, fmt.Errorf("claude hook: session_id and hook_event_name are required")
	}
	hint := project.Detect(in.Cwd)
	rec := func(id, typ string, payload map[string]any) spool.Record {
		return spool.Record{
			EventID:         id,
			Agent:           AgentType,
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
		if !failed && fileTools[in.ToolName] {
			if path := filePath(in.ToolInput); path != "" {
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

func filePath(toolInput map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		if s, ok := toolInput[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// sanitizeTool drops captured content of secret files and of shell commands
// that read them.
func sanitizeTool(tool string, toolInput map[string]any, response any) (map[string]any, any) {
	if toolInput == nil {
		toolInput = map[string]any{}
	}
	sensitive := redact.SensitivePath(filePath(toolInput))
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
