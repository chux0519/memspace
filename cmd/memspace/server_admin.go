package main

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/server/handler"
)

// Server administration works on the database directly, so it needs to run
// on the server machine (with the same config) and needs no token. WAL mode
// lets it run while the server is up.

func serverInviteCmd() *cobra.Command {
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "invite",
		Short: "Create a one-time invite for registering a device",
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
			code, err := auth.CreateInvite(cmd.Context(), database, cfg.Auth.OwnerUserID, ttl)
			if err != nil {
				return err
			}
			fmt.Printf("Invite (single use, expires %s):\n\n  %s\n\n", time.Now().Add(ttl).Format("2006-01-02 15:04"), code)
			fmt.Printf("On the device:\n\n  memspace client connect <server-url> --invite %s\n", code)
			return nil
		},
	}
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the invite stays valid")
	return cmd
}

func serverDeviceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "device",
		Short: "List or revoke registered devices",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List registered devices",
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
			devices, err := handler.QueryDevices(cmd.Context(), database, cfg.Auth.OwnerUserID)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "DEVICE ID\tNAME\tPLATFORM\tLAST SEEN\tSTATUS")
			for _, d := range devices {
				seen, status := "never", "active"
				if d.LastSeenAt != nil {
					seen = d.LastSeenAt.Local().Format("2006-01-02 15:04")
				}
				if d.RevokedAt != nil {
					status = "revoked " + d.RevokedAt.Local().Format("2006-01-02")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.DeviceID, d.Name, d.Platform, seen, status)
			}
			return tw.Flush()
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <device-id>",
		Short: "Revoke a device's token (its events are kept)",
		Args:  cobra.ExactArgs(1),
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
			if err := auth.RevokeDevice(cmd.Context(), database, args[0]); err != nil {
				if errors.Is(err, auth.ErrUnknownDevice) {
					return fmt.Errorf("no device %s (see 'memspace server device list')", args[0])
				}
				return err
			}
			fmt.Printf("Revoked %s. To reconnect it, create an invite and run 'memspace client connect --force' on it.\n", args[0])
			return nil
		},
	})
	return cmd
}
