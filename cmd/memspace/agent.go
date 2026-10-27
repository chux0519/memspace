package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/adapter/claudecode"
	"github.com/chux0519/memspace/internal/adapter/codex"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/inject"
	"github.com/chux0519/memspace/internal/syncer"
)

func agentCmd(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Install, remove and check coding-agent integrations (claude, codex)",
	}
	if use != "agent" {
		cmd.Hidden = true
	}
	cmd.AddCommand(agentListCmd(), agentInstallCmd(), agentUninstallCmd(), agentDoctorCmd())
	return cmd
}

// hookCommand is the command line agents run, pinned to this binary and to
// any non-default --home/--config so hooks find the same client state.
func hookCommand(cfg *config.Config, bin, agent string) (string, error) {
	return selfCommand(cfg, bin, "hook", agent)
}

// selfCommand is a command line that runs this binary with args, pinned
// like hookCommand.
func selfCommand(cfg *config.Config, bin string, args ...string) (string, error) {
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		bin = exe
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		return "", err
	}
	home := ""
	if cfg.Client.Home != config.Default().Client.Home || homeFlag != "" {
		home = cfg.Client.Home
	}
	cfgFile := config.ResolvePath(cfgPath)
	if cfgFile != "" && cfgFile != filepath.Join(config.DefaultHome(), "config.toml") {
		if abs, err := filepath.Abs(cfgFile); err == nil {
			cfgFile = abs
		}
	} else {
		cfgFile = ""
	}
	return adapter.Command(bin, home, cfgFile, args...), nil
}

func looksTemporary(bin string) bool {
	return strings.Contains(bin, "go-build") || strings.HasPrefix(bin, os.TempDir())
}

func agentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Show which agents are present on this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			for _, name := range adapterOrder {
				a := adapters(cfg)[name]
				d := a.Detect(cmd.Context())
				state := "not found"
				if d.Found {
					state = strings.TrimSpace(d.Binary + " " + d.Version)
				}
				fmt.Printf("%-8s %s\n", name, state)
			}
			return nil
		},
	}
}

func agentInstallCmd() *cobra.Command {
	var bin string
	var trust, noMCP bool
	cmd := &cobra.Command{
		Use:   "install <claude|codex>",
		Short: "Register MemSpace hooks with an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			a, err := lookupAdapter(cfg, args[0])
			if err != nil {
				return err
			}
			hc, err := hookCommand(cfg, bin, a.Name())
			if err != nil {
				return err
			}
			if bin == "" {
				if exe, _ := os.Executable(); looksTemporary(exe) {
					return fmt.Errorf("this binary lives in a temporary directory (%s); build it (make build) or pass --bin", exe)
				}
			}
			if d := a.Detect(cmd.Context()); !d.Found {
				fmt.Printf("Warning: %s does not appear to be installed; configuring anyway.\n", a.Name())
			}
			if err := a.Install(cmd.Context(), adapter.InstallOptions{HookCommand: hc, Trust: trust, Out: os.Stdout}); err != nil {
				return err
			}
			if _, err := syncer.LoadCredentials(cfg.Client.CredentialsPath()); err != nil {
				fmt.Printf("\nEvents will be spooled locally until you run: memspace client connect <server-url>\n")
				return nil
			}
			if !noMCP {
				return registerMCP(cmd.Context(), cfg, bin, a)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&bin, "bin", "", "memspace binary path to put in hooks (default this executable)")
	cmd.Flags().BoolVar(&trust, "trust", false, "codex: mark the installed hooks as reviewed so Codex runs them")
	cmd.Flags().BoolVar(&noMCP, "no-mcp", false, "do not register the MemSpace MCP server")
	return cmd
}

func agentUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall <claude|codex>",
		Short: "Remove MemSpace hooks from an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			a, err := lookupAdapter(cfg, args[0])
			if err != nil {
				return err
			}
			if r, ok := a.(adapter.MCPRegistrar); ok {
				r.UnregisterMCP(cmd.Context())
			}
			return a.Uninstall(cmd.Context(), os.Stdout)
		},
	}
}

func agentDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [claude|codex]...",
		Short: "Check that capture and upload work end to end",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			names := args
			if len(names) == 0 {
				names = adapterOrder
			}
			failed := printChecks(os.Stdout, "client", clientChecks(cmd.Context(), cfg))
			for _, n := range names {
				a, err := lookupAdapter(cfg, n)
				if err != nil {
					return err
				}
				hc, _ := hookCommand(cfg, "", a.Name())
				checks := a.Doctor(cmd.Context(), adapter.InstallOptions{HookCommand: hc})
				if len(args) == 0 && !installed(checks) {
					fmt.Printf("\n%s: not installed (memspace agent install %s)\n", a.Name(), a.Name())
					continue
				}
				if r, ok := a.(adapter.MCPRegistrar); ok {
					checks = append(checks, r.MCPCheck(cmd.Context()))
				}
				checks = append(checks, adapter.Check{Name: "captures", Status: adapter.OK, Detail: strings.Join(a.Capabilities(), ", ")})
				failed = printChecks(os.Stdout, a.Name(), checks) || failed
			}
			if failed {
				return errors.New("some checks failed")
			}
			return nil
		},
	}
}

// installed reports whether doctor found any MemSpace integration, so that
// `doctor` without arguments skips agents the user never set up.
func installed(checks []adapter.Check) bool {
	for _, c := range checks {
		if (c.Name == "hooks" || c.Name == "importer") && strings.Contains(c.Detail, "not installed") {
			return false
		}
	}
	return true
}

func printChecks(w io.Writer, title string, checks []adapter.Check) (failed bool) {
	fmt.Fprintf(w, "\n%s\n", title)
	for _, c := range checks {
		fmt.Fprintf(w, "  %s %-15s %s\n", c.Status.Symbol(), c.Name, c.Detail)
		if c.Status == adapter.Fail {
			failed = true
		}
	}
	return failed
}

func clientChecks(ctx context.Context, cfg *config.Config) []adapter.Check {
	var checks []adapter.Check
	creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
	if err != nil {
		checks = append(checks, adapter.Check{Name: "credentials", Status: adapter.Fail, Detail: err.Error()})
	} else {
		checks = append(checks, adapter.Check{Name: "credentials", Status: adapter.OK, Detail: fmt.Sprintf("device %q → %s", creds.DeviceName, creds.ServerURL)})
		url := creds.ServerURL
		if cfg.Client.ServerURL != "" {
			url = cfg.Client.ServerURL
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := client.New(url, creds.Token).SyncStatus(ctx); err != nil {
			status := adapter.Fail
			detail := err.Error()
			if client.IsStatus(err, 401) {
				detail = "token rejected (device revoked?); run 'memspace client connect --force " + url + "'"
			} else {
				status = adapter.Warn // offline is a supported state
				detail = "unreachable, events will be buffered: " + detail
			}
			checks = append(checks, adapter.Check{Name: "server", Status: status, Detail: detail})
		} else {
			checks = append(checks, adapter.Check{Name: "server", Status: adapter.OK, Detail: url + " (token valid)"})
			checks = append(checks, semanticCheck(ctx, client.New(url, creds.Token)))
		}
	}

	sp, err := openSpool(cfg)
	if err != nil {
		return append(checks, adapter.Check{Name: "spool", Status: adapter.Fail, Detail: err.Error()})
	}
	probe := filepath.Join(sp.Dir(), "tmp", ".doctor")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		checks = append(checks, adapter.Check{Name: "spool", Status: adapter.Fail, Detail: "not writable: " + err.Error()})
	} else {
		os.Remove(probe)
		ss, _ := sp.Stats()
		st := adapter.OK
		detail := fmt.Sprintf("%s: %d pending, %d rejected, %d dropped", sp.Dir(), ss.Pending, ss.Dead, ss.Dropped)
		if ss.Dead > 0 || ss.Dropped > 0 {
			st = adapter.Warn
		}
		checks = append(checks, adapter.Check{Name: "spool", Status: st, Detail: detail})
	}
	if s := syncer.LoadState(cfg.Client.StateDir()); s.Offline() {
		checks = append(checks, adapter.Check{Name: "sync", Status: adapter.Warn,
			Detail: fmt.Sprintf("%d failed attempts: %s", s.ConsecutiveFailures, s.LastError)})
	}
	return checks
}

// ---------------------------------------------------------------------------
// memspace hook <agent> — invoked by agents, never interactively.

func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook <agent>",
		Short:  "Handle an agent hook event (stdin: hook JSON); used by installed hooks",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		// A hook must never break the agent: report problems in the log and
		// always exit 0. Stdout is reserved for the context JSON the agent
		// adds to the conversation (SessionStart / UserPromptSubmit).
		RunE: func(cmd *cobra.Command, args []string) error {
			runHook(args[0])
			return nil
		},
	}
}

func runHook(agent string) {
	dryRun := os.Getenv(adapter.DryRunEnv) != ""
	cfg, err := loadConfig()
	if err != nil {
		hookLog(nil, "config: %v", err)
		return
	}
	logf := func(format string, a ...any) { hookLog(cfg, format, a...) }

	a, err := lookupAdapter(cfg, agent)
	if err != nil {
		logf("%v", err)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 32<<20))
	if err != nil {
		logf("%s: read stdin: %v", agent, err)
		return
	}
	recs, err := a.Normalize(raw, time.Now())
	if err != nil {
		logf("%s: %v", agent, err)
		return
	}
	if dryRun {
		enc := json.NewEncoder(os.Stdout)
		for _, r := range recs {
			enc.Encode(r)
		}
		return
	}

	if len(recs) > 0 {
		sp, err := openSpool(cfg)
		if err != nil {
			logf("%s: %v", agent, err)
			return
		}
		for _, r := range recs {
			if err := sp.Write(r); err != nil {
				logf("%s: %s %s: %v", agent, r.EventType, r.EventID, err)
			}
		}
	}

	creds, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
	if err != nil {
		return // not connected yet: keep buffering
	}
	// Codex hooks exist to trigger imports even when they record nothing.
	_, codexHook := a.(*codex.Adapter)
	if len(recs) > 0 || codexHook {
		triggerSync(cfg, logf)
	}

	// Session start / prompt: add MemSpace context to the conversation.
	if cfg.Client.Inject != "off" {
		state := syncer.LoadState(cfg.Client.StateDir())
		out := inject.Run(context.Background(), inject.Config{
			ServerURL: serverURL(cfg, creds),
			Token:     creds.Token,
			StateDir:  cfg.Client.StateDir(),
			Agent:     agentType(a),
			Offline:   state.Offline() && time.Now().Before(state.NextAttemptAt),
			MCP:       creds.MCPToken != "",
		}, raw)
		if len(out) > 0 {
			os.Stdout.Write(out)
		}
	}
}

func agentType(a adapter.Adapter) string {
	if _, ok := a.(*codex.Adapter); ok {
		return codex.AgentType
	}
	return claudecode.AgentType
}

// triggerSync starts a detached `client sync` unless one is running or the
// client is backing off.
func triggerSync(cfg *config.Config, logf func(string, ...any)) {
	if !syncer.ShouldTrigger(cfg.Client.StateDir()) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logf("executable: %v", err)
		return
	}
	var syncArgs []string
	if homeFlag != "" {
		syncArgs = append(syncArgs, "--home", cfg.Client.Home)
	}
	if cfgPath != "" {
		syncArgs = append(syncArgs, "--config", cfgPath)
	}
	syncArgs = append(syncArgs, "client", "sync", "--quiet")
	if err := syncer.SpawnDetached(exe, syncArgs, filepath.Join(cfg.Client.LogDir(), "sync.log")); err != nil {
		logf("spawn sync: %v", err)
	}
}

func hookLog(cfg *config.Config, format string, a ...any) {
	dir := filepath.Join(config.DefaultHome(), "logs")
	if cfg != nil {
		dir = cfg.Client.LogDir()
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "hook.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 4<<20 {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	if os.Getenv(adapter.DryRunEnv) != "" {
		fmt.Fprintf(os.Stderr, format+"\n", a...)
	}
}

// ---------------------------------------------------------------------------
// memspace import codex — explicit history backfill.

func importCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Backfill history from agents' local records",
	}
	var since string
	var all bool
	codexCmd := &cobra.Command{
		Use:   "codex",
		Short: "Import Codex rollout history (default: since install)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			cx := &codex.Adapter{StateDir: cfg.Client.StateDir()}
			im := cx.NewImporter()
			switch {
			case all:
				im.Since, im.Reset = time.Time{}, true
			case since != "":
				t, err := time.ParseInLocation("2006-01-02", since, time.Local)
				if err != nil {
					return fmt.Errorf("--since: want YYYY-MM-DD: %w", err)
				}
				im.Since, im.Reset = t, true
			}
			sp, err := openSpool(cfg)
			if err != nil {
				return err
			}
			n, err := im.Import(cmd.Context(), sp, nil)
			fmt.Printf("Spooled %d events from %s\n", n, filepath.Join(im.CodexHome, "sessions"))
			if err != nil {
				return err
			}
			fmt.Println("Upload with: memspace client sync")
			return nil
		},
	}
	codexCmd.Flags().StringVar(&since, "since", "", "import sessions active since this date (YYYY-MM-DD)")
	codexCmd.Flags().BoolVar(&all, "all", false, "import all history")
	cmd.AddCommand(codexCmd)
	return cmd
}

func semanticCheck(ctx context.Context, c *client.Client) adapter.Check {
	name := "semantic search"
	st, err := c.EmbeddingStatus(ctx)
	switch {
	case err != nil:
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "status unavailable: " + err.Error()}
	case !st.Enabled:
		return adapter.Check{Name: name, Status: adapter.OK, Detail: "disabled on the server (keyword search only; see [embedding] in the server config)"}
	case st.LastError != "" && st.Active == nil:
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "embedding service failing: " + st.LastError}
	case st.Building != nil:
		total := st.Building.Docs + st.Building.Pending
		detail := fmt.Sprintf("building index for %s: %d/%d documents", st.Building.Model, st.Building.Docs, total)
		if st.Active != nil {
			detail += fmt.Sprintf(" (still serving %s meanwhile: keyword only until done)", st.Active.Model)
		}
		return adapter.Check{Name: name, Status: adapter.OK, Detail: detail}
	case st.Active != nil:
		detail := fmt.Sprintf("%s, %d documents", st.Active.Model, st.Active.Docs)
		if st.Active.Pending > 0 {
			detail += fmt.Sprintf(", %d pending", st.Active.Pending)
		}
		if st.LastError != "" {
			return adapter.Check{Name: name, Status: adapter.Warn, Detail: detail + "; last error: " + st.LastError}
		}
		return adapter.Check{Name: name, Status: adapter.OK, Detail: detail}
	default:
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "enabled, not indexed yet"}
	}
}
