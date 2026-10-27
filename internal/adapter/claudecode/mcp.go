package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
)

func (a *Adapter) claudeCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = os.Environ()
	if a.ConfigDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+a.ConfigDir)
	}
	return cmd
}

// RegisterMCP adds MemSpace as a user-scoped stdio MCP server running the
// bridge (`memspace mcp stdio`), which reads this device's credentials
// itself, so Claude Code's configuration never contains the token.
func (a *Adapter) RegisterMCP(ctx context.Context, opts adapter.MCPOptions) error {
	cfg, _ := json.Marshal(map[string]any{"type": "stdio", "command": opts.Command[0], "args": opts.Command[1:]})
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Fprintf(opts.Out, "claude CLI not found; register the MCP server manually:\n  claude mcp add-json -s user %s '%s'\n", adapter.MCPServerName, cfg)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.claudeCmd(ctx, "mcp", "remove", "-s", "user", adapter.MCPServerName).Run() // replace any previous entry
	if out, err := a.claudeCmd(ctx, "mcp", "add-json", "-s", "user", adapter.MCPServerName, string(cfg)).CombinedOutput(); err != nil {
		return fmt.Errorf("claude mcp add-json: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(opts.Out, "Registered MCP server %q for Claude Code (stdio bridge to %s)\n", adapter.MCPServerName, opts.URL)
	return nil
}

func (a *Adapter) UnregisterMCP(ctx context.Context) error {
	if _, err := exec.LookPath("claude"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.claudeCmd(ctx, "mcp", "remove", "-s", "user", adapter.MCPServerName).Run()
	return nil
}

// MCPCheck asks Claude Code itself whether it can connect.
func (a *Adapter) MCPCheck(ctx context.Context) adapter.Check {
	name := "mcp"
	if _, err := exec.LookPath("claude"); err != nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "claude CLI not found; cannot verify"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := a.claudeCmd(ctx, "mcp", "list").CombinedOutput()
	if err != nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "claude mcp list failed: " + strings.TrimSpace(string(out))}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), adapter.MCPServerName+":") {
			if strings.Contains(line, "Connected") || strings.Contains(line, "✓") {
				return adapter.Check{Name: name, Status: adapter.OK, Detail: "connected"}
			}
			return adapter.Check{Name: name, Status: adapter.Fail, Detail: strings.TrimSpace(line)}
		}
	}
	return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install claude')"}
}
