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

// RegisterMCP adds MemSpace as a streamable-HTTP MCP server whose bearer
// token Codex reads from an environment variable, so config.toml never
// holds the token.
func (a *Adapter) RegisterMCP(ctx context.Context, opts adapter.MCPOptions) error {
	if _, err := exec.LookPath("codex"); err != nil {
		fmt.Fprintf(opts.Out, "codex CLI not found; register the MCP server manually:\n  codex mcp add %s --url %s --bearer-token-env-var %s\n",
			adapter.MCPServerName, opts.URL, opts.TokenEnv)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	a.codexCmd(ctx, "mcp", "remove", adapter.MCPServerName).Run()
	if out, err := a.codexCmd(ctx, "mcp", "add", adapter.MCPServerName, "--url", opts.URL,
		"--bearer-token-env-var", opts.TokenEnv).CombinedOutput(); err != nil {
		return fmt.Errorf("codex mcp add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(opts.Out, "Registered MCP server %q for Codex (%s)\n", adapter.MCPServerName, opts.URL)
	fmt.Fprintf(opts.Out, "Codex reads the token from $%s. Add to your shell profile (~/.bashrc, ~/.zshrc):\n  export %s=\"$(memspace mcp token)\"\n",
		opts.TokenEnv, opts.TokenEnv)
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

// TokenEnv is the variable Codex reads the MCP token from.
const TokenEnv = "MEMSPACE_MCP_TOKEN"

// MCPCheck verifies the registration and that the token variable is set in
// this environment (Codex inherits the environment it is started from).
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
	var servers []map[string]any
	json.Unmarshal(out, &servers)
	found := false
	for _, s := range servers {
		if s["name"] == adapter.MCPServerName {
			found = true
		}
	}
	switch {
	case !found:
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install codex')"}
	case os.Getenv(TokenEnv) == "":
		return adapter.Check{Name: name, Status: adapter.Warn,
			Detail: fmt.Sprintf("registered, but $%s is not set in this shell; add: export %s=\"$(memspace mcp token)\"", TokenEnv, TokenEnv)}
	default:
		return adapter.Check{Name: name, Status: adapter.OK, Detail: "registered; token variable set"}
	}
}
