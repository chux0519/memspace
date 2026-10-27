// Package codex captures OpenAI Codex CLI activity and injects MemSpace
// context through its SessionStart / UserPromptSubmit hooks.
//
// Codex hooks do not see everything (there is no tool-failure hook, and new
// hooks run only after the user trusts them), so the source of truth is the
// rollout file Codex writes for every session. Hooks (SessionStart, Stop,
// SessionEnd) only trigger an incremental import, and SessionEnd records the
// end of the session, which the rollout does not. Without trusted hooks,
// `memspace client sync` still imports on its own.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/spool"
)

const agentName = "codex"

// AgentType is the agent_type stored on sessions.
const AgentType = "codex"

var hookSpecs = []adapter.HookSpec{
	{Event: "SessionStart", Timeout: 10},
	{Event: "UserPromptSubmit", Timeout: 10},
	{Event: "Stop", Timeout: 10},
	{Event: "SessionEnd", Timeout: 5},
}

// Adapter implements adapter.Adapter for Codex.
type Adapter struct {
	// Home overrides the Codex home (default $CODEX_HOME or ~/.codex).
	Home string
	// StateDir is MemSpace's client state dir (import cursors, cutoff).
	StateDir string
}

func (a *Adapter) Name() string { return agentName }

// CodexHome returns the Codex state directory.
func (a *Adapter) CodexHome() string {
	if a.Home != "" {
		return a.Home
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// HooksPath is the user-level hooks file.
func (a *Adapter) HooksPath() string { return filepath.Join(a.CodexHome(), "hooks.json") }

func (a *Adapter) Capabilities() []string {
	return []string{"session.started", "user.prompt", "agent.message", "tool.completed", "tool.failed",
		"file.modified", "agent.turn_completed", "agent.turn_aborted", "session.compacted", "session.ended"}
}

func (a *Adapter) Detect(ctx context.Context) adapter.Detection {
	d := adapter.Detection{ConfigDir: a.CodexHome()}
	if p, err := exec.LookPath("codex"); err == nil {
		d.Found = true
		d.Binary = p
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, p, "--version").Output(); err == nil {
			d.Version = strings.TrimSpace(string(out))
		}
	} else if _, err := os.Stat(d.ConfigDir); err == nil {
		d.Found = true
	}
	return d
}

// ---------------------------------------------------------------------------
// Install state

type installState struct {
	// ImportSince is the cutoff for importing rollout history: sessions
	// before installation are only imported by an explicit backfill.
	ImportSince time.Time `json:"import_since"`
	CodexHome   string    `json:"codex_home"`
}

func (a *Adapter) statePath() string { return filepath.Join(a.StateDir, "codex.json") }

func (a *Adapter) loadState() (*installState, bool) {
	b, err := os.ReadFile(a.statePath())
	if err != nil {
		return nil, false
	}
	var st installState
	if json.Unmarshal(b, &st) != nil {
		return nil, false
	}
	return &st, true
}

// Enabled reports whether the Codex adapter is installed (the importer only
// runs then).
func (a *Adapter) Enabled() bool { _, ok := a.loadState(); return ok }

// NewImporter returns the rollout importer configured at install time.
func (a *Adapter) NewImporter() *Importer {
	st, _ := a.loadState()
	im := &Importer{CodexHome: a.CodexHome(), StateDir: a.StateDir}
	if st != nil {
		im.Since = st.ImportSince
		if st.CodexHome != "" && a.Home == "" && os.Getenv("CODEX_HOME") == "" {
			im.CodexHome = st.CodexHome
		}
	}
	return im
}

func (a *Adapter) Install(ctx context.Context, opts adapter.InstallOptions) error {
	out := opts.Out
	if _, ok := a.loadState(); !ok {
		st := installState{ImportSince: time.Now().UTC(), CodexHome: a.CodexHome()}
		b, _ := json.MarshalIndent(st, "", "  ")
		if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(a.statePath(), b, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(out, "Codex sessions from now on will be imported from %s\n", filepath.Join(a.CodexHome(), "sessions"))
		fmt.Fprintf(out, "  (older history: memspace import codex --since 2026-01-01)\n")
	}

	path := a.HooksPath()
	doc, _, err := adapter.LoadJSONFile(path)
	if err != nil {
		return err
	}
	if adapter.MergeHooks(doc, agentName, opts.HookCommand, hookSpecs) {
		if err := adapter.SaveJSONFile(path, doc); err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed Codex hooks in %s\n", path)
		fmt.Fprintf(out, "  command: %s\n", opts.HookCommand)
	} else {
		fmt.Fprintf(out, "Codex hooks already installed in %s\n", path)
	}

	hooks, err := a.ourHooks(ctx)
	if err != nil {
		fmt.Fprintf(out, "Could not query Codex hook trust (%v).\n", err)
		fmt.Fprintf(out, "Codex runs new hooks only after you review them: start codex and approve them in /hooks.\n")
		return nil
	}
	untrusted := filterUntrusted(hooks)
	switch {
	case len(untrusted) == 0:
		fmt.Fprintf(out, "Codex hooks are trusted.\n")
	case opts.Trust:
		if err := a.trust(ctx, untrusted); err != nil {
			return fmt.Errorf("trust codex hooks: %w", err)
		}
		fmt.Fprintf(out, "Marked %d MemSpace hooks as trusted in Codex.\n", len(untrusted))
	default:
		fmt.Fprintf(out, "Codex will not run these hooks until they are reviewed. Either:\n")
		fmt.Fprintf(out, "  - start codex and approve them in /hooks, or\n")
		fmt.Fprintf(out, "  - re-run: memspace agent install codex --trust\n")
		fmt.Fprintf(out, "Until then, sessions are still imported whenever 'memspace client sync' runs.\n")
	}
	return nil
}

func filterUntrusted(hs []hookInfo) []hookInfo {
	var out []hookInfo
	for _, h := range hs {
		if h.TrustStatus != "trusted" {
			out = append(out, h)
		}
	}
	return out
}

// ourHooks lists MemSpace hooks as Codex sees them, including trust status.
func (a *Adapter) ourHooks(ctx context.Context) ([]hookInfo, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, fmt.Errorf("codex not on PATH")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := startAppServer(ctx, bin, a.CodexHome())
	if err != nil {
		return nil, err
	}
	defer s.close()
	all, err := s.listHooks(ctx, os.TempDir())
	if err != nil {
		return nil, err
	}
	var ours []hookInfo
	for _, h := range all {
		if adapter.IsMemSpaceHook(h.Command, agentName) {
			ours = append(ours, h)
		}
	}
	return ours, nil
}

func (a *Adapter) trust(ctx context.Context, hooks []hookInfo) error {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := startAppServer(ctx, bin, a.CodexHome())
	if err != nil {
		return err
	}
	defer s.close()
	return s.trustHooks(ctx, hooks)
}

func (a *Adapter) Uninstall(ctx context.Context, out io.Writer) error {
	path := a.HooksPath()
	doc, exists, err := adapter.LoadJSONFile(path)
	if err != nil {
		return err
	}
	if exists && adapter.RemoveHooks(doc, agentName) {
		if err := adapter.SaveJSONFile(path, doc); err != nil {
			return err
		}
		fmt.Fprintf(out, "Removed MemSpace hooks from %s\n", path)
	} else {
		fmt.Fprintf(out, "No MemSpace hooks found in %s\n", path)
	}
	if err := os.Remove(a.statePath()); err == nil {
		fmt.Fprintf(out, "Stopped importing Codex sessions.\n")
	}
	return nil
}

func (a *Adapter) Doctor(ctx context.Context, opts adapter.InstallOptions) []adapter.Check {
	var checks []adapter.Check
	d := a.Detect(ctx)
	switch {
	case d.Binary != "":
		checks = append(checks, adapter.Check{Name: "codex binary", Status: adapter.OK, Detail: d.Binary + " " + d.Version})
	case d.Found:
		checks = append(checks, adapter.Check{Name: "codex binary", Status: adapter.Warn, Detail: "not on PATH, but " + d.ConfigDir + " exists"})
	default:
		checks = append(checks, adapter.Check{Name: "codex binary", Status: adapter.Fail, Detail: "Codex not found"})
	}

	st, ok := a.loadState()
	if !ok {
		return append(checks, adapter.Check{Name: "importer", Status: adapter.Fail, Detail: "not installed (run 'memspace agent install codex')"})
	}
	sessions := filepath.Join(a.NewImporter().CodexHome, "sessions")
	if _, err := os.Stat(sessions); err != nil {
		checks = append(checks, adapter.Check{Name: "rollouts", Status: adapter.Warn, Detail: sessions + " does not exist yet"})
	} else {
		files, last := a.NewImporter().Stats()
		detail := fmt.Sprintf("%s, since %s, %d files tracked", sessions, st.ImportSince.Local().Format("2006-01-02 15:04"), files)
		if !last.IsZero() {
			detail += ", last import " + last.Local().Format("2006-01-02 15:04:05")
		}
		checks = append(checks, adapter.Check{Name: "rollouts", Status: adapter.OK, Detail: detail})
	}

	doc, exists, err := adapter.LoadJSONFile(a.HooksPath())
	switch {
	case err != nil:
		checks = append(checks, adapter.Check{Name: "hooks", Status: adapter.Fail, Detail: err.Error()})
	case !exists:
		checks = append(checks, adapter.Check{Name: "hooks", Status: adapter.Warn,
			Detail: a.HooksPath() + " missing: sessions are imported only when 'memspace client sync' runs"})
	default:
		checks = append(checks, adapter.HookChecks(doc, agentName, opts.HookCommand, hookSpecs))
		if hooks, err := a.ourHooks(ctx); err != nil {
			checks = append(checks, adapter.Check{Name: "hook trust", Status: adapter.Warn, Detail: "could not query codex app-server: " + err.Error()})
		} else if un := filterUntrusted(hooks); len(un) > 0 {
			checks = append(checks, adapter.Check{Name: "hook trust", Status: adapter.Warn,
				Detail: fmt.Sprintf("%d of %d hooks not trusted; approve in codex /hooks or run 'memspace agent install codex --trust'", len(un), len(hooks))})
		} else if len(hooks) > 0 {
			checks = append(checks, adapter.Check{Name: "hook trust", Status: adapter.OK, Detail: fmt.Sprintf("%d hooks trusted", len(hooks))})
		}
		cmd := ""
		for _, cmds := range adapter.InstalledHooks(doc, agentName) {
			cmd = cmds[0]
		}
		checks = append(checks, adapter.SelfTest(ctx, agentName, cmd, sampleSessionEnd()))
	}
	return checks
}

func sampleSessionEnd() []byte {
	b, _ := json.Marshal(map[string]any{
		"session_id": "memspace-doctor", "cwd": os.TempDir(), "hook_event_name": "SessionEnd", "reason": "other",
	})
	return b
}

// Normalize handles a Codex hook invocation. Only SessionEnd produces an
// event; everything else is captured from the rollout by the importer.
func (a *Adapter) Normalize(raw []byte, now time.Time) ([]spool.Record, error) {
	var in struct {
		SessionID     string `json:"session_id"`
		Cwd           string `json:"cwd"`
		HookEventName string `json:"hook_event_name"`
		Reason        string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("codex hook: parse input: %w", err)
	}
	if in.HookEventName != "SessionEnd" || in.SessionID == "" {
		return nil, nil
	}
	return []spool.Record{{
		EventID:         adapter.RandomID("cx"),
		Agent:           AgentType,
		NativeSessionID: in.SessionID,
		EventType:       "session.ended",
		OccurredAt:      now.UTC(),
		Project:         project.Detect(in.Cwd),
		Payload:         adapter.Payload(map[string]any{"reason": in.Reason}),
	}}, nil
}
