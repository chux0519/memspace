package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter/codex"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/syncer"
)

func clientCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Device client: connect, sync, status",
	}
	cmd.AddCommand(clientConnectCmd())
	cmd.AddCommand(clientStatusCmd())
	cmd.AddCommand(clientSyncCmd())
	cmd.AddCommand(clientDeadCmd())
	return cmd
}

func clientConnectCmd() *cobra.Command {
	var name, invite string
	var force bool
	cmd := &cobra.Command{
		Use:   "connect <server-url> --invite <code>",
		Short: "Register this device with a MemSpace server",
		Long: `Register this device. The invite code comes from the server operator:

    memspace server invite        # run on the server machine`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			path := cfg.Client.CredentialsPath()
			if existing, err := syncer.LoadCredentials(path); err == nil && !force {
				return fmt.Errorf("already connected to %s as %q (device %s); use --force to register again",
					existing.ServerURL, existing.DeviceName, existing.DeviceID)
			}
			if invite == "" {
				invite = os.Getenv("MEMSPACE_INVITE")
			}
			if invite == "" {
				return fmt.Errorf("--invite is required (on the server: memspace server invite)")
			}
			if name == "" {
				name, _ = os.Hostname()
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			c := client.New(args[0], "")
			if _, err := c.Health(ctx); err != nil {
				return fmt.Errorf("server not reachable: %w", err)
			}
			deviceID, token, err := c.Register(ctx, name, runtime.GOOS, invite)
			if err != nil {
				return err
			}
			creds := &syncer.Credentials{
				ServerURL: c.ServerURL, DeviceID: deviceID, DeviceName: name,
				Token: token, ConnectedAt: time.Now().UTC(),
			}
			if err := syncer.SaveCredentials(path, creds); err != nil {
				return fmt.Errorf("save credentials: %w", err)
			}
			fmt.Printf("Connected to %s as %q (device %s)\n", c.ServerURL, name, deviceID)
			fmt.Printf("Credentials saved to %s\n", path)
			fmt.Printf("\nNext: memspace agent install claude   (and/or codex)\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "device name (default hostname)")
	cmd.Flags().StringVar(&invite, "invite", "", "one-time invite code (or $MEMSPACE_INVITE)")
	cmd.Flags().BoolVar(&force, "force", false, "register a new device even if already connected")
	return cmd
}

// newSyncer wires the uploader with the importers of installed adapters.
func newSyncer(cfg *config.Config) (*syncer.Syncer, error) {
	creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
	if err != nil {
		return nil, err
	}
	sp, err := openSpool(cfg)
	if err != nil {
		return nil, err
	}
	var importers []syncer.Importer
	if cx := (&codex.Adapter{StateDir: cfg.Client.StateDir()}); cx.Enabled() {
		importers = append(importers, cx.NewImporter())
	}
	return syncer.New(cfg.Client, sp, creds, importers...), nil
}

func clientSyncCmd() *cobra.Command {
	var watch, force, quiet bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Import agent records and upload spooled events",
		Long: `Upload spooled events to the server, deleting each one only after the
server acknowledges it. Hooks start this automatically in the background;
run it by hand to retry immediately, or with --watch as a long-running
replay loop (e.g. under systemd/launchd) for devices that are often offline.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			s, err := newSyncer(cfg)
			if err != nil {
				return err
			}
			if !quiet {
				s.Log = os.Stderr
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			once := func(force bool) error {
				res, err := s.Run(ctx, force)
				if errors.Is(err, syncer.ErrLocked) {
					if !quiet {
						fmt.Println("Another sync is running.")
					}
					return nil
				}
				if !quiet || res.Uploaded+res.Rejected > 0 || err != nil {
					stamp := time.Now().Format("2006-01-02 15:04:05")
					fmt.Printf("%s imported %d, uploaded %d, duplicate %d, rejected %d, retry later %d\n",
						stamp, res.Imported, res.Uploaded, res.Duplicates, res.Rejected, res.Retried)
				}
				return err
			}

			if !watch {
				return once(force || !quiet)
			}
			for {
				err := once(false)
				if err != nil && !errors.Is(err, syncer.ErrBackoff) {
					fmt.Fprintln(os.Stderr, "sync failed:", err)
				}
				wait := interval
				if st := syncer.LoadState(cfg.Client.StateDir()); st.Offline() {
					if d := time.Until(st.NextAttemptAt); d > 0 && d < wait {
						wait = d
					}
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(wait):
				}
			}
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "keep running, syncing every --interval")
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "sync interval with --watch")
	cmd.Flags().BoolVar(&force, "force", false, "ignore the retry backoff")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "only print when something was uploaded or failed (used by hooks)")
	return cmd
}

func clientStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show connection, spool and sync status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			creds, credErr := syncer.LoadCredentials(cfg.Client.CredentialsPath())
			if credErr != nil {
				fmt.Printf("Server:     %v\n", credErr)
			} else {
				fmt.Printf("Server:     %s (device %q, %s)\n", creds.ServerURL, creds.DeviceName, creds.DeviceID)
			}

			st := syncer.LoadState(cfg.Client.StateDir())
			switch {
			case st.LastAttemptAt.IsZero():
				fmt.Printf("Sync:       never attempted\n")
			case st.Offline():
				fmt.Printf("Sync:       OFFLINE — %d failed attempts, next retry %s\n",
					st.ConsecutiveFailures, st.NextAttemptAt.Local().Format("15:04:05"))
				fmt.Printf("            last error: %s\n", st.LastError)
			default:
				fmt.Printf("Sync:       ok\n")
			}
			if !st.LastSuccessAt.IsZero() {
				fmt.Printf("Last sync:  %s (%s ago)\n", st.LastSuccessAt.Local().Format("2006-01-02 15:04:05"),
					time.Since(st.LastSuccessAt).Round(time.Second))
			}
			fmt.Printf("Uploaded:   %d events total (%d duplicates, %d rejected)\n",
				st.TotalUploaded, st.TotalDuplicates, st.TotalRejected)

			sp, err := openSpool(cfg)
			if err != nil {
				return err
			}
			ss, err := sp.Stats()
			if err != nil {
				return err
			}
			fmt.Printf("Spool:      %s\n", sp.Dir())
			pending := fmt.Sprintf("%d pending (%s)", ss.Pending, humanBytes(ss.PendingBytes))
			if !ss.OldestPending.IsZero() {
				pending += ", oldest " + ss.OldestPending.Local().Format("2006-01-02 15:04:05")
			}
			fmt.Printf("            %s, %d uploading\n", pending, ss.Inflight)
			if ss.Dead > 0 {
				fmt.Printf("            %d rejected by server (inspect: memspace client dead)\n", ss.Dead)
			}
			if ss.Dropped > 0 {
				fmt.Printf("            %d DROPPED because the spool was full (limit %d events / %s)\n",
					ss.Dropped, cfg.Client.SpoolMaxEvents, humanBytes(cfg.Client.SpoolMaxBytes))
			}

			if credErr == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				defer cancel()
				url := creds.ServerURL
				if cfg.Client.ServerURL != "" {
					url = cfg.Client.ServerURL
				}
				if rs, err := client.New(url, creds.Token).SyncStatus(ctx); err != nil {
					fmt.Printf("Server view: unavailable (%v)\n", err)
				} else {
					fmt.Printf("Server view: %d events, %d sessions from this device", rs.EventsTotal, rs.SessionsTotal)
					if rs.LastEventReceivedAt != nil {
						fmt.Printf(", last received %s", rs.LastEventReceivedAt.Local().Format("2006-01-02 15:04:05"))
					}
					fmt.Println()
				}
			}
			return nil
		},
	}
}

func clientDeadCmd() *cobra.Command {
	var requeue bool
	cmd := &cobra.Command{
		Use:   "dead",
		Short: "List events the server rejected, or requeue them",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			sp, err := openSpool(cfg)
			if err != nil {
				return err
			}
			if requeue {
				n, err := sp.RequeueDead()
				if err != nil {
					return err
				}
				fmt.Printf("Requeued %d events\n", n)
				return nil
			}
			recs, err := sp.ListDead()
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				fmt.Println("No rejected events")
			}
			for _, r := range recs {
				fmt.Printf("%s  %-22s %-12s %s\n    %s\n", r.OccurredAt.Local().Format("2006-01-02 15:04:05"),
					r.EventType, r.Agent, r.EventID, r.LastError)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&requeue, "requeue", false, "move rejected events back to pending")
	return cmd
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
