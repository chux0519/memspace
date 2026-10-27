package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/adapter/dsh"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/mcpbridge"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/syncer"
)

// mcpSyncLoop starts a sync every interval while DSH's session logs keep
// changing, and once more when stopped. DSH runs hooks in its workspace
// sandbox, where a sync cannot write its state; the MCP bridge runs outside
// it for as long as DSH does.
func mcpSyncLoop(cfg *config.Config, interval time.Duration) (stop func()) {
	logf := func(format string, a ...any) { hookLog(cfg, format, a...) }
	d := &dsh.Adapter{StateDir: cfg.Client.StateDir()}
	if !d.Enabled() {
		return func() {}
	}
	im := d.NewImporter()
	var synced time.Time
	sync := func() {
		if latest := im.LatestChange(); latest.After(synced) {
			synced = latest
			triggerSync(cfg, logf)
		}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				sync()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		sync()
	}
}

func mcpCmd() *cobra.Command {
	var syncInterval time.Duration
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MemSpace's MCP server for agents (stdio bridge, credentials)",
	}
	stdio := &cobra.Command{
		Use:   "stdio",
		Short: "Serve MemSpace's MCP tools on stdin/stdout (what agents run)",
		Long: `Serve MemSpace's MCP tools on stdin/stdout, forwarding each call to the
server with this device's read-only MCP token. Agents start it as a local
MCP server, so their configuration holds a command, not a secret:

    codex mcp add memspace -- memspace mcp stdio
    claude mcp add -s user memspace -- memspace mcp stdio

'memspace agent install' registers it this way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			creds, tok, err := ensureMCPToken(cmd.Context(), cfg, false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if syncInterval > 0 {
				// The agent may stop the bridge with a signal rather than by
				// closing stdin; either way the final sync runs.
				var cancel context.CancelFunc
				ctx, cancel = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
				defer cancel()
				stop := mcpSyncLoop(cfg, syncInterval)
				defer stop()
			}
			return mcpbridge.Run(ctx, mcpbridge.Options{
				URL:       strings.TrimRight(serverURL(cfg, creds), "/") + "/mcp",
				Token:     tok,
				Transport: client.New("", "").HTTPClient.Transport,
				Version:   model.BuildVersion(),
			}, &mcp.StdioTransport{})
		},
	}
	stdio.Flags().DurationVar(&syncInterval, "sync-interval", 0,
		"while running, sync this often when DSH session logs changed, and once on exit (DSH, whose hooks cannot sync)")
	var rotate bool
	token := &cobra.Command{
		Use:   "token",
		Short: "Print this device's read-only MCP token",
		Long: `Print this device's read-only MCP token, issuing one if needed. Agents
normally use 'memspace mcp stdio' instead; the token is for clients that
can only reach the HTTP endpoint (Authorization: Bearer <token>).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			_, tok, err := ensureMCPToken(cmd.Context(), cfg, rotate)
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
	token.Flags().BoolVar(&rotate, "rotate", false, "issue a new token, invalidating the old one")
	header := &cobra.Command{
		Use:    "header",
		Short:  "Print the MCP Authorization header as JSON (for HTTP clients' header helpers)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			_, tok, err := ensureMCPToken(cmd.Context(), cfg, false)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(map[string]string{"Authorization": "Bearer " + tok})
		},
	}
	cmd.AddCommand(stdio, token, header)
	return cmd
}

// ensureMCPToken returns the stored MCP token, issuing one when missing (or
// when rotate is set).
func ensureMCPToken(ctx context.Context, cfg *config.Config, rotate bool) (*syncer.Credentials, string, error) {
	creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
	if err != nil {
		return nil, "", err
	}
	if creds.MCPToken != "" && !rotate {
		return creds, creds.MCPToken, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tok, err := client.New(serverURL(cfg, creds), creds.Token).RotateMCPToken(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("issue MCP token: %w", err)
	}
	creds.MCPToken = tok
	if err := syncer.SaveCredentials(cfg.Client.CredentialsPath(), creds); err != nil {
		return nil, "", err
	}
	return creds, tok, nil
}

func serverURL(cfg *config.Config, creds *syncer.Credentials) string {
	if cfg.Client.ServerURL != "" {
		return cfg.Client.ServerURL
	}
	return creds.ServerURL
}

// registerMCP registers `memspace mcp stdio` as the agent's MemSpace MCP
// server; the bridge reads this device's credentials at run time.
func registerMCP(ctx context.Context, cfg *config.Config, bin string, a adapter.Adapter) error {
	r, ok := a.(adapter.MCPRegistrar)
	if !ok {
		return nil
	}
	creds, _, err := ensureMCPToken(ctx, cfg, false)
	if err != nil {
		fmt.Printf("Skipping MCP registration: %v\n", err)
		return nil
	}
	argv, err := selfArgv(cfg, bin, "mcp", "stdio")
	if err != nil {
		return err
	}
	if _, ok := a.(*dsh.Adapter); ok {
		argv = append(argv, dsh.MCPArgs...)
	}
	return r.RegisterMCP(ctx, adapter.MCPOptions{
		URL:     strings.TrimRight(serverURL(cfg, creds), "/") + "/mcp",
		Command: argv,
		Out:     os.Stdout,
	})
}
