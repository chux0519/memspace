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
	"github.com/chux0519/memspace/internal/adapter/hookpayload"
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

// ProjectDirEnv is set by Claude Code for hook commands: the directory the
// session was started in. DSH's hook bridge sets it too.
const ProjectDirEnv = "CLAUDE_PROJECT_DIR"

// ProjectDir is the directory that identifies the session's project: the
// one the session was started in, not the hook's cwd, which follows the
// agent's cd and can be a subagent's temporary directory.
func ProjectDir(cwd string) string {
	if d := os.Getenv(ProjectDirEnv); d != "" {
		return d
	}
	return cwd
}

// Normalize maps one hook invocation to MemSpace events. Unknown or
// uninteresting hook events yield no events.
func (a *Adapter) Normalize(raw []byte, now time.Time) ([]spool.Record, error) {
	return hookpayload.Normalize(AgentType, raw, now, ProjectDir)
}
