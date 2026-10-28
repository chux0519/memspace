package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/syncer"
	"github.com/chux0519/memspace/internal/upgrade"
)

// enableAutoUpgrade makes s upgrade the client to the server's version after
// syncs, and report a failed upgrade to the server.
func enableAutoUpgrade(cfg *config.Config, s *syncer.Syncer) {
	dir := cfg.Client.StateDir()
	s.UpgradeStatus = func() (string, string) {
		st := upgrade.LoadState(dir)
		if st.Settle(model.BuildVersion()) {
			upgrade.SaveState(dir, st)
		}
		return st.Target, st.Error
	}
	if cfg.Client.AutoUpgrade == "off" {
		return
	}
	s.AfterSync = func(ctx context.Context, server string) {
		current := model.BuildVersion()
		if !upgrade.Newer(current, server) {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := upgrade.Auto(ctx, dir, current, server, upgrade.Options{}); err != nil {
			fmt.Fprintf(s.Log, "upgrade to %s: %v\n", server, err)
		} else if st := upgrade.LoadState(dir); st.UpgradedTo == server {
			fmt.Fprintf(s.Log, "upgraded %s → %s\n", current, server)
		}
	}
}

// upgradeNotice is the line the next session-start context carries about
// automatic upgrades: a failure (repeated daily while it lasts) or, once, a
// success. Markers live in markers (a writable directory) so a sandboxed
// hook can record them.
func upgradeNotice(cfg *config.Config, markers string) func() string {
	if markers == "" {
		markers = cfg.Client.StateDir()
	}
	return func() string {
		return noticeFor(upgrade.LoadState(cfg.Client.StateDir()), model.BuildVersion(), markers)
	}
}

func noticeFor(st upgrade.State, current, markers string) string {
	var msg string
	var again time.Duration // 0: once
	switch {
	case st.Error != "" && upgrade.Newer(current, st.Target):
		msg = fmt.Sprintf("MemSpace notice for the user: the automatic upgrade of this device's MemSpace client "+
			"from %s to %s failed (%s). Tell the user; they can upgrade with `memspace upgrade`.",
			current, st.Target, st.Error)
		again = 24 * time.Hour
	case st.UpgradedTo != "" && time.Since(st.UpgradedAt) < 7*24*time.Hour:
		msg = fmt.Sprintf("MemSpace notice for the user: this device's MemSpace client was upgraded "+
			"automatically from %s to %s.", st.UpgradedFrom, st.UpgradedTo)
	default:
		return ""
	}
	h := sha256.Sum256([]byte(msg))
	marker := filepath.Join(markers, fmt.Sprintf("upgrade-notice-%x", h[:6]))
	if fi, err := os.Stat(marker); err == nil && (again == 0 || time.Since(fi.ModTime()) < again) {
		return ""
	}
	os.MkdirAll(markers, 0o700)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return "" // cannot record it: stay quiet rather than repeat
	}
	now := time.Now()
	os.Chtimes(marker, now, now)
	return msg
}

func upgradeCmd() *cobra.Command {
	var version string
	var check bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade this binary to the server's version",
		Long: `Download the release matching the server's version from GitHub, check it
against the release's SHA256SUMS, and replace this binary (the old one is
kept as <binary>.prev). Clients do this automatically after a sync when the
server is newer, unless [client] auto_upgrade = "off". HTTPS_PROXY is used
when set.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			current := model.BuildVersion()
			target := version
			if target == "" {
				creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
				if err != nil {
					return fmt.Errorf("not connected to a server (%v); pass --version", err)
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				target, err = client.New(serverURL(cfg, creds), creds.Token).ServerVersion(ctx)
				if err != nil {
					return fmt.Errorf("ask the server for its version: %w", err)
				}
			}
			fmt.Printf("This binary: %s\nTarget:      %s\n", current, target)
			if !model.IsRelease(target) {
				return fmt.Errorf("%q is not a release version; pass --version", target)
			}
			if version == "" && !upgrade.Newer(current, target) {
				fmt.Println("Up to date.")
				return nil
			}
			if check {
				fmt.Println("An upgrade is available: run `memspace upgrade`.")
				return nil
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			dir := cfg.Client.StateDir()
			st := upgrade.LoadState(dir)
			if err := upgrade.Install(ctx, upgrade.Options{Version: target}); err != nil {
				st.Target, st.Error, st.AttemptedAt = target, err.Error(), time.Now().UTC()
				os.MkdirAll(dir, 0o700)
				upgrade.SaveState(dir, st)
				return err
			}
			os.MkdirAll(dir, 0o700)
			upgrade.SaveState(dir, upgrade.State{})
			fmt.Printf("Upgraded to %s. Restart your agents to use it everywhere.\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "install this release (e.g. v0.1.3) instead of the server's version")
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an upgrade is available")
	return cmd
}
