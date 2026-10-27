// memspace — CLI entry point.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/adapter/claudecode"
	"github.com/chux0519/memspace/internal/adapter/codex"
	"github.com/chux0519/memspace/internal/adapter/dsh"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/jobs"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/spool"
	"github.com/chux0519/memspace/internal/summary"
)

var (
	cfgPath  string
	homeFlag string
)

func main() {
	root := &cobra.Command{
		Use:           "memspace",
		Short:         "MemSpace — shared memory service for AI coding agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       model.BuildVersion(),
	}

	root.PersistentFlags().StringVar(&cfgPath, "config", "", "path to config file (TOML; default $MEMSPACE_CONFIG or <home>/config.toml)")
	root.PersistentFlags().StringVar(&homeFlag, "home", "", "client state directory (default $MEMSPACE_HOME or ~/.memspace)")

	root.AddCommand(serverCmd())
	root.AddCommand(clientCmd())
	root.AddCommand(agentCmd("agent"))
	root.AddCommand(agentCmd("adapter")) // alias kept for M2 drafts
	root.AddCommand(importCmd())
	root.AddCommand(searchCmd())
	root.AddCommand(mcpCmd())
	root.AddCommand(onboardCmd())
	root.AddCommand(hookCmd())
	root.AddCommand(uiCmd())

	if err := root.Execute(); err != nil {
		if errors.Is(err, errNotApproved) {
			os.Exit(3) // the next steps are already printed
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// loadConfig applies --home on top of the file/env configuration.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if homeFlag != "" {
		abs, err := filepath.Abs(homeFlag)
		if err != nil {
			return nil, err
		}
		cfg.Client.Home = abs
	}
	return cfg, nil
}

func openSpool(cfg *config.Config) (*spool.Spool, error) {
	return spool.Open(cfg.Client.SpoolDir(), spool.Limits{
		MaxEvents: cfg.Client.SpoolMaxEvents,
		MaxBytes:  cfg.Client.SpoolMaxBytes,
	})
}

// adapters returns all known agent adapters, keyed by CLI name.
func adapters(cfg *config.Config) map[string]adapter.Adapter {
	return map[string]adapter.Adapter{
		"claude": &claudecode.Adapter{},
		"codex":  &codex.Adapter{StateDir: cfg.Client.StateDir()},
		"dsh":    &dsh.Adapter{StateDir: cfg.Client.StateDir()},
	}
}

var adapterOrder = []string{"claude", "codex", "dsh"}

func lookupAdapter(cfg *config.Config, name string) (adapter.Adapter, error) {
	switch name {
	case "claude-code", "claudecode":
		name = "claude"
	}
	a, ok := adapters(cfg)[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (supported: claude, codex, dsh)", name)
	}
	return a, nil
}

func serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage the MemSpace server",
	}
	cmd.AddCommand(serverStartCmd(), serverInviteCmd(), serverApproveCmd(true), serverApproveCmd(false), serverDeviceCmd(), serverLoginLinkCmd())
	return cmd
}

func serverStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the MemSpace server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			database, err := db.Open(cfg.Database.Path)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer database.Close()

			if n, err := search.Backfill(cmd.Context(), database); err != nil {
				return fmt.Errorf("index existing events: %w", err)
			} else if n > 0 {
				fmt.Printf("Indexed %d existing events for search\n", n)
			}

			// Semantic search (optional): an OpenAI-compatible embeddings service.
			emb := embedding.New(database, embedding.Config{
				URL:          cfg.Embedding.URL,
				Model:        cfg.Embedding.Model,
				APIKey:       os.Getenv(cfg.Embedding.APIKeyEnv),
				Dimensions:   cfg.Embedding.Dimensions,
				QueryPrefix:  cfg.Embedding.QueryPrefix,
				BatchSize:    cfg.Embedding.BatchSize,
				QueryTimeout: cfg.Embedding.QueryTimeout.Duration,

				MinSimilarity:       cfg.Embedding.MinSimilarity,
				SearchMinSimilarity: cfg.Embedding.SearchMinSimilarity,
			})
			embedHandler := embedding.DisabledHandler
			if emb != nil {
				search.SetSemantic(emb)
				embedHandler = emb.Handler()
				// In the background: an unreachable embedding service must not
				// delay startup (it is retried by the job queue).
				go emb.Start(context.WithoutCancel(cmd.Context()))
				fmt.Printf("Semantic search: %s\n", cfg.Embedding.URL)
			}

			// Background jobs (session summaries, embeddings) run until shutdown.
			jobCtx, stopJobs := context.WithCancel(context.Background())
			jobsDone := make(chan struct{})
			go func() {
				defer close(jobsDone)
				(&jobs.Runner{DB: database, Handlers: map[string]jobs.Handler{
					summary.JobKind:   summary.Handler(database),
					embedding.JobKind: embedHandler,
				}}).Run(jobCtx)
			}()
			defer func() { stopJobs(); <-jobsDone }()

			srv := &http.Server{
				Addr:              cfg.Server.Address,
				Handler:           server.NewWithOptions(database, cfg.Auth.OwnerUserID, server.Options{Embedding: emb, DownloadsDir: cfg.Server.DownloadsDir}),
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       60 * time.Second,
			}

			// Graceful shutdown on SIGINT / SIGTERM.
			stop := make(chan os.Signal, 1)
			signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

			errCh := make(chan error, 1)
			go func() {
				fmt.Printf("MemSpace server listening on %s\n", cfg.Server.Address)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					errCh <- err
				}
			}()

			select {
			case err := <-errCh:
				return fmt.Errorf("server error: %w", err)
			case <-stop:
				fmt.Println("\nShutting down...")
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				return srv.Shutdown(ctx)
			}
		},
	}
}
