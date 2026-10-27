package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/internal/syncer"
)

// uiCmd opens the web UI with a one-time login link issued for this device.
func uiCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web UI (signs the browser in with a one-time link)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
			if err != nil {
				return err
			}
			url := creds.ServerURL
			if cfg.Client.ServerURL != "" {
				url = cfg.Client.ServerURL
			}
			c := client.New(url, creds.Token)
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			var resp handler.LoginLinkResponse
			if err := c.Do(ctx, "POST", "/api/v1/ui/login-links", struct{}{}, &resp); err != nil {
				return err
			}
			return showLoginLink(strings.TrimRight(url, "/")+resp.Path, noOpen)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link instead of opening a browser")
	return cmd
}

// serverLoginLinkCmd issues a login link on the server machine, without a
// device (e.g. for a browser on a machine that is not a registered device).
func serverLoginLinkCmd() *cobra.Command {
	var base string
	var viewer bool
	cmd := &cobra.Command{
		Use:   "login-link",
		Short: "Create a one-time web UI login link (admin unless --viewer)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			database, err := db.Open(cfg.Database.Path)
			if err != nil {
				return err
			}
			defer database.Close()
			role := auth.RoleAdmin
			if viewer {
				role = auth.RoleViewer
			}
			code, err := auth.CreateLoginCode(cmd.Context(), database, cfg.Auth.OwnerUserID, "", role)
			if err != nil {
				return err
			}
			if base == "" {
				base = cfg.Server.PublicURL
			}
			if base == "" {
				base = "http://" + cfg.Server.Address
			}
			fmt.Printf("%s login link (single use, expires in %s):\n\n  %s\n", role, auth.LoginLinkTTL, strings.TrimRight(base, "/")+handler.LoginPath(code))
			return nil
		},
	}
	cmd.Flags().BoolVar(&viewer, "viewer", false, "read-only session (default: admin, which can manage invites, devices and sessions)")
	cmd.Flags().StringVar(&base, "url", "", "server URL as seen by the browser (default server.public_url or http://<address>)")
	return cmd
}

func showLoginLink(link string, noOpen bool) error {
	if !noOpen {
		var c *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			c = exec.Command("open", link)
		case "windows":
			c = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
		default:
			c = exec.Command("xdg-open", link)
		}
		if err := c.Start(); err == nil {
			go c.Wait()
			fmt.Printf("Opened the web UI in your browser. If it did not open, use this link (single use, expires in %s):\n\n  %s\n", auth.LoginLinkTTL, link)
			return nil
		}
	}
	fmt.Printf("Open this link in a browser (single use, expires in %s):\n\n  %s\n", auth.LoginLinkTTL, link)
	return nil
}
