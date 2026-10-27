package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/adapter/codex"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/syncer"
)

func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Credentials for MemSpace's MCP endpoint",
	}
	var rotate bool
	token := &cobra.Command{
		Use:   "token",
		Short: "Print this device's read-only MCP token",
		Long: `Print this device's read-only MCP token, issuing one if needed. Use it to
expose the token to agents at runtime without writing it into their config:

    export MEMSPACE_MCP_TOKEN="$(memspace mcp token)"`,
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
		Short:  "Print the MCP Authorization header as JSON (Claude Code headersHelper)",
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
	cmd.AddCommand(token, header)
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

// registerMCP points an agent at the MCP endpoint, with credentials supplied
// at runtime (Claude: headersHelper command; Codex: environment variable).
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
	headerCmd, err := selfCommand(cfg, bin, "mcp", "header")
	if err != nil {
		return err
	}
	return r.RegisterMCP(ctx, adapter.MCPOptions{
		URL:           strings.TrimRight(serverURL(cfg, creds), "/") + "/mcp",
		HeaderCommand: headerCmd,
		TokenEnv:      codex.TokenEnv,
		Out:           os.Stdout,
	})
}
