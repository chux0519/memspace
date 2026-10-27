package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/syncer"
)

func onboardCmd() *cobra.Command {
	var invite, name, bin string
	var agents []string
	var trust bool
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "onboard <server-url> [--invite <code>]",
		Short: "Set up this device: connect, install agent hooks and MCP, check",
		Long: `One-step setup for a device:

  1. connect to the server (skipped if already connected to it): without an
     invite, ask the administrator to approve this device (a code is shown;
     approve it in the web UI or with 'memspace server approve <code>')
  2. for each agent (default: those found on this machine) install capture
     hooks and register the MemSpace MCP server
  3. run the doctor

Waiting for approval: up to --wait. With --wait 0 it shows the code and
exits; run the same command again after approving to finish (AI agents
running setup should do this, so they can relay the code).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			// Check before connecting: hooks run this binary by absolute path.
			if bin == "" {
				if exe, _ := os.Executable(); looksTemporary(exe) {
					return fmt.Errorf("this binary lives in a temporary directory (%s); build it (make build) or pass --bin", exe)
				}
			}
			url := strings.TrimRight(args[0], "/")
			creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
			switch {
			case err == nil && strings.TrimRight(creds.ServerURL, "/") == url:
				fmt.Printf("Already connected to %s as %q\n", url, creds.DeviceName)
			case err == nil:
				return fmt.Errorf("connected to a different server (%s); run 'memspace client connect --force %s' first", creds.ServerURL, url)
			case errors.Is(err, syncer.ErrNotConnected):
				if invite == "" {
					invite = os.Getenv("MEMSPACE_INVITE")
				}
				if _, err := connectDevice(cmd.Context(), cfg, url, name, invite, wait, os.Stdout); err != nil {
					return err
				}
			default:
				return err
			}

			if len(agents) == 0 {
				for _, n := range adapterOrder {
					if adapters(cfg)[n].Detect(cmd.Context()).Found {
						agents = append(agents, n)
					}
				}
			}
			if len(agents) == 0 {
				fmt.Println("No supported agents found (claude, codex); install one and run 'memspace agent install <agent>'.")
			}
			for _, n := range agents {
				a, err := lookupAdapter(cfg, n)
				if err != nil {
					return err
				}
				fmt.Printf("\n== %s ==\n", a.Name())
				hc, err := hookCommand(cfg, bin, a.Name())
				if err != nil {
					return err
				}
				if err := a.Install(cmd.Context(), adapter.InstallOptions{HookCommand: hc, Trust: trust, Out: os.Stdout}); err != nil {
					return err
				}
				if err := registerMCP(cmd.Context(), cfg, bin, a); err != nil {
					return err
				}
			}

			fmt.Println("\n== doctor ==")
			// Run the handler directly: Execute with no args would parse
			// os.Args, i.e. onboard's own flags, and fail on --invite.
			doctor := agentDoctorCmd()
			doctor.SetContext(cmd.Context())
			if err := doctor.RunE(doctor, agents); err != nil {
				fmt.Println("\nSome checks need attention (see above).")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&invite, "invite", "", "one-time invite code (or $MEMSPACE_INVITE); without one, ask for approval")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for approval (0: show the code and exit)")
	cmd.Flags().StringVar(&name, "name", "", "device name (default hostname)")
	cmd.Flags().StringSliceVar(&agents, "agent", nil, "agents to set up (default: detected)")
	cmd.Flags().BoolVar(&trust, "trust", false, "codex: mark MemSpace hooks as reviewed")
	cmd.Flags().StringVar(&bin, "bin", "", "memspace binary path for hooks (default this executable)")
	return cmd
}
