package codex

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

func (a *Adapter) codexCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+a.CodexHome())
	return cmd
}

// RegisterMCP adds MemSpace as a stdio MCP server running the bridge
// (`memspace mcp stdio`), which reads this device's credentials itself, so
// config.toml holds a command and no token.
func (a *Adapter) RegisterMCP(ctx context.Context, opts adapter.MCPOptions) error {
	add := append([]string{"mcp", "add", adapter.MCPServerName, "--"}, opts.Command...)
	if _, err := exec.LookPath("codex"); err != nil {
		fmt.Fprintf(opts.Out, "codex CLI not found; register the MCP server manually:\n  codex %s\n",
			adapter.Command(add[0], "", "", add[1:]...))
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.codexCmd(ctx, "mcp", "remove", adapter.MCPServerName).Run()
	if out, err := a.codexCmd(ctx, add...).CombinedOutput(); err != nil {
		return fmt.Errorf("codex mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(opts.Out, "Registered MCP server %q for Codex (stdio bridge to %s)\n", adapter.MCPServerName, opts.URL)
	return nil
}

func (a *Adapter) UnregisterMCP(ctx context.Context) error {
	if _, err := exec.LookPath("codex"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.codexCmd(ctx, "mcp", "remove", adapter.MCPServerName).Run()
	return nil
}

// MCPCheck verifies that Codex runs MemSpace's stdio bridge.
func (a *Adapter) MCPCheck(ctx context.Context) adapter.Check {
	name := "mcp"
	if _, err := exec.LookPath("codex"); err != nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "codex CLI not found; cannot verify"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := a.codexCmd(ctx, "mcp", "list", "--json").Output()
	if err != nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "codex mcp list failed"}
	}
	var servers []struct {
		Name      string `json:"name"`
		Transport struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"transport"`
	}
	json.Unmarshal(out, &servers)
	for _, s := range servers {
		if s.Name != adapter.MCPServerName {
			continue
		}
		if s.Transport.Type == "stdio" && adapter.IsBridgeCommand(s.Transport.Command, s.Transport.Args) {
			return adapter.Check{Name: name, Status: adapter.OK, Detail: "registered (stdio bridge)"}
		}
		return adapter.Check{Name: name, Status: adapter.Warn,
			Detail: "registered as " + s.Transport.Type + ", not the stdio bridge (re-run 'memspace agent install codex')"}
	}
	return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install codex')"}
}
