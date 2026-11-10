package omp

import (
	"context"
	"errors"
	"fmt"

	"github.com/chux0519/memspace/internal/adapter"
)

// RegisterMCP writes MemSpace's stdio bridge into omp's native MCP config
// (<agent dir>/mcp.json), preserving every other server in the file. It writes
// no token: the bridge reads this device's credentials itself.
func (a *Adapter) RegisterMCP(ctx context.Context, opts adapter.MCPOptions) error {
	if len(opts.Command) == 0 {
		return errors.New("omp: empty MCP command")
	}
	path := a.MCPConfigPath()
	doc, _, err := adapter.LoadJSONFile(path)
	if err != nil {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	entry := map[string]any{"type": "stdio", "command": opts.Command[0]}
	if len(opts.Command) > 1 {
		args := make([]any, 0, len(opts.Command)-1)
		for _, arg := range opts.Command[1:] {
			args = append(args, arg)
		}
		entry["args"] = args
	}
	servers[adapter.MCPServerName] = entry
	if err := adapter.SaveJSONFile(path, doc); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "Registered MCP server %q for omp in %s (stdio bridge to %s)\n", adapter.MCPServerName, path, opts.URL)
	return nil
}

func (a *Adapter) UnregisterMCP(ctx context.Context) error {
	path := a.MCPConfigPath()
	doc, exists, err := adapter.LoadJSONFile(path)
	if err != nil || !exists {
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, ok := servers[adapter.MCPServerName]; !ok {
		return nil
	}
	delete(servers, adapter.MCPServerName)
	return adapter.SaveJSONFile(path, doc)
}

// MCPCheck verifies that omp's config points at the stdio bridge.
func (a *Adapter) MCPCheck(ctx context.Context) adapter.Check {
	const name = "mcp"
	path := a.MCPConfigPath()
	doc, exists, err := adapter.LoadJSONFile(path)
	if err != nil {
		return adapter.Check{Name: name, Status: adapter.Fail, Detail: err.Error()}
	}
	if !exists {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: path + " missing (re-run 'memspace agent install omp')"}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	entry, _ := servers[adapter.MCPServerName].(map[string]any)
	if entry == nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install omp')"}
	}
	command, _ := entry["command"].(string)
	var args []string
	if list, ok := entry["args"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}
	if adapter.IsBridgeCommand(command, args) {
		return adapter.Check{Name: name, Status: adapter.OK, Detail: "registered (stdio bridge)"}
	}
	return adapter.Check{Name: name, Status: adapter.Warn,
		Detail: "registered with a different command (re-run 'memspace agent install omp')"}
}
