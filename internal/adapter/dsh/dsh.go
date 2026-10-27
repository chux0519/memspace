// Package dsh captures DeepSeek Harness (DSH) activity and gives its agents
// MemSpace context and tools.
//
// DSH has no hooks of its own, but ships a bridge plugin
// (@deepseek-ai/dsh-hooks-claude-code) that runs Claude Code-style command
// hooks. That bridge omits assistant replies and session ends, so the
// source of truth is the session log DSH writes (see Importer); the hooks
// only add context to prompts and trigger an import. Install adds two rows
// to the home-level $DSH_HOME/cordis.patch.yml, inside a marked block: the
// hook bridge, pointed at a hooks file MemSpace owns, and an MCP client
// running `memspace mcp stdio`.
package dsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/spool"
)

const agentName = "dsh"

// AgentType is the agent_type stored on sessions.
const AgentType = "dsh"

// DSH runs hooks in its workspace sandbox, so they only read and fetch
// context; DSH's MCP bridge does the importing (see MCPArgs).
var hookSpecs = []adapter.HookSpec{
	{Event: "UserPromptSubmit", Timeout: 10},
}

// MCPArgs are added to `memspace mcp stdio` in DSH's MCP row: the bridge
// runs outside the sandbox for as long as DSH does, so it imports and
// uploads new session logs.
var MCPArgs = []string{"--sync-interval", "1m"}

// Adapter implements adapter.Adapter for DSH.
type Adapter struct {
	// Home overrides the DSH home (default: see DSHHome).
	Home string
	// StateDir is MemSpace's client state dir (import cursors, install
	// state, the hooks file).
	StateDir string
}

func (a *Adapter) Name() string { return agentName }

// wrapperHomeRE finds DSH_HOME in a launcher script: `export DSH_HOME=…`
// (sh), `set "DSH_HOME=…"` (cmd) or `$env:DSH_HOME = "…"` (PowerShell).
var wrapperHomeRE = regexp.MustCompile(`(?mi)^\s*(?:export\s+|set\s+|\$env:)?"?DSH_HOME"?\s*=\s*["']?([^"'\r\n]+?)["']?\s*$`)

var percentVarRE = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)%`)

// homeFromWrapper reads DSH_HOME from a small launcher script.
func homeFromWrapper(path string) string {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() >= 64<<10 {
		return ""
	}
	b, err := os.ReadFile(path)
	ext := strings.ToLower(filepath.Ext(path))
	if err != nil || !(bytes.HasPrefix(b, []byte("#!")) || ext == ".cmd" || ext == ".bat" || ext == ".ps1") {
		return ""
	}
	m := wrapperHomeRE.FindSubmatch(b)
	if m == nil {
		return ""
	}
	v := percentVarRE.ReplaceAllStringFunc(string(m[1]), func(s string) string { return os.Getenv(s[1 : len(s)-1]) })
	return filepath.Clean(os.ExpandEnv(v))
}

// HooksRunInPowerShell: DSH runs hook commands with PowerShell on Windows
// and bash elsewhere.
var HooksRunInPowerShell = runtime.GOOS == "windows"

// PowerShellCommand renders argv as a PowerShell command line. The call
// operator makes a quoted program path a command rather than a string.
func PowerShellCommand(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(`/\._-:`, r))
		}) < 0 {
			parts[i] = a
		} else {
			parts[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
		}
	}
	return "& " + strings.Join(parts, " ")
}

// DSHHome returns the DSH home: $DSH_HOME, else the one a `dsh` wrapper
// script on PATH exports (installers that keep DSH outside ~/.dsh write
// one), else ~/.dsh.
func (a *Adapter) DSHHome() string {
	if a.Home != "" {
		return a.Home
	}
	if v := os.Getenv("DSH_HOME"); v != "" {
		return v
	}
	if st, ok := a.loadState(); ok && st.DSHHome != "" {
		return st.DSHHome
	}
	if p, err := exec.LookPath("dsh"); err == nil {
		if h := homeFromWrapper(p); h != "" {
			return h
		}
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".dsh")
}

// PatchPath is the home-level patch layer, applied to every profile.
func (a *Adapter) PatchPath() string { return filepath.Join(a.DSHHome(), "cordis.patch.yml") }

// HooksPath is the Claude Code-format hooks file the bridge runs.
func (a *Adapter) HooksPath() string { return filepath.Join(a.StateDir, "dsh-hooks.json") }

func (a *Adapter) Capabilities() []string {
	return []string{"session.started", "user.prompt", "tool.completed", "tool.failed",
		"file.modified", "agent.turn_completed", "agent.turn_aborted"}
}

func (a *Adapter) Detect(ctx context.Context) adapter.Detection {
	d := adapter.Detection{ConfigDir: a.DSHHome()}
	if p, err := exec.LookPath("dsh"); err == nil {
		d.Found = true
		d.Binary = p
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, p, "--version").Output(); err == nil {
			d.Version = strings.TrimSpace(firstLine(string(out)))
		}
	} else if _, err := os.Stat(filepath.Join(d.ConfigDir, "sessions")); err == nil {
		d.Found = true
	}
	return d
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ---------------------------------------------------------------------------
// Install state

type installState struct {
	// ImportSince is the cutoff for importing session logs: sessions
	// before installation are only imported by an explicit backfill.
	ImportSince time.Time `json:"import_since"`
	DSHHome     string    `json:"dsh_home"`
	// MCPCommand runs the stdio bridge; empty: no MCP row.
	MCPCommand []string `json:"mcp_command,omitempty"`
	// CreatedPatch: the patch file did not exist before install, so
	// uninstall removes it rather than leaving an empty one (which DSH
	// refuses to boot with).
	CreatedPatch bool `json:"created_patch,omitempty"`
}

func (a *Adapter) statePath() string { return filepath.Join(a.StateDir, "dsh.json") }

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

func (a *Adapter) saveState(st *installState) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := os.MkdirAll(a.StateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.statePath(), b, 0o600)
}

// Enabled reports whether the DSH adapter is installed (the importer only
// runs then).
func (a *Adapter) Enabled() bool { _, ok := a.loadState(); return ok }

// NewImporter returns the session-log importer configured at install time.
func (a *Adapter) NewImporter() *Importer {
	im := &Importer{DSHHome: a.DSHHome(), StateDir: a.StateDir}
	if st, ok := a.loadState(); ok {
		im.Since = st.ImportSince
	}
	return im
}

func (a *Adapter) Install(ctx context.Context, opts adapter.InstallOptions) error {
	out := opts.Out
	st, ok := a.loadState()
	if !ok {
		st = &installState{ImportSince: time.Now().UTC()}
	}
	st.DSHHome = a.DSHHome()

	doc, _, err := adapter.LoadJSONFile(a.HooksPath())
	if err != nil {
		return err
	}
	if adapter.MergeHooks(doc, agentName, opts.HookCommand, hookSpecs) {
		if err := adapter.SaveJSONFile(a.HooksPath(), doc); err != nil {
			return err
		}
	}
	if err := a.writeBlock(st); err != nil {
		return err
	}
	if err := a.saveState(st); err != nil {
		return err
	}
	fmt.Fprintf(out, "Added MemSpace to %s (hook bridge → %s)\n", a.PatchPath(), a.HooksPath())
	fmt.Fprintf(out, "  hook command: %s\n", opts.HookCommand)
	if !ok {
		fmt.Fprintf(out, "DSH sessions from now on will be imported from %s\n", filepath.Join(st.DSHHome, "sessions"))
		fmt.Fprintf(out, "  (older history: memspace import dsh --all)\n")
	}
	fmt.Fprintf(out, "Restart DSH to load it (the web profile reloads on its own).\n")
	return nil
}

func (a *Adapter) Uninstall(ctx context.Context, out io.Writer) error {
	st, ok := a.loadState()
	if !ok {
		st = &installState{}
	}
	removed, err := a.removeBlock(st.CreatedPatch)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(out, "Removed MemSpace from %s\n", a.PatchPath())
	} else {
		fmt.Fprintf(out, "No MemSpace block found in %s\n", a.PatchPath())
	}
	os.Remove(a.HooksPath())
	os.Remove(a.HooksPath() + ".memspace.bak")
	if err := os.Remove(a.statePath()); err == nil {
		fmt.Fprintf(out, "Stopped importing DSH sessions.\n")
	}
	return nil
}

// ---------------------------------------------------------------------------
// MCP

// RegisterMCP adds an MCP client row running the stdio bridge.
func (a *Adapter) RegisterMCP(ctx context.Context, opts adapter.MCPOptions) error {
	st, ok := a.loadState()
	if !ok {
		return errors.New("install the DSH integration first")
	}
	st.MCPCommand = opts.Command
	if err := a.writeBlock(st); err != nil {
		return err
	}
	if err := a.saveState(st); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "Registered MCP server %q for DSH (stdio bridge to %s)\n", adapter.MCPServerName, opts.URL)
	return nil
}

func (a *Adapter) UnregisterMCP(ctx context.Context) error {
	st, ok := a.loadState()
	if !ok || len(st.MCPCommand) == 0 {
		return nil
	}
	st.MCPCommand = nil
	if err := a.writeBlock(st); err != nil {
		return err
	}
	return a.saveState(st)
}

func (a *Adapter) MCPCheck(ctx context.Context) adapter.Check {
	name := "mcp"
	b, err := os.ReadFile(a.PatchPath())
	if err != nil {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install dsh')"}
	}
	block, ok := findBlock(string(b))
	switch {
	case !ok || !strings.Contains(block, "id: "+mcpRowID):
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "not registered (re-run 'memspace agent install dsh')"}
	case !strings.Contains(block, `"mcp", "stdio"`):
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "registered, but not the stdio bridge (re-run 'memspace agent install dsh')"}
	case !strings.Contains(block, `"--sync-interval"`):
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "registered without syncing; sessions are uploaded only by 'memspace client sync' (re-run 'memspace agent install dsh')"}
	}
	return adapter.Check{Name: name, Status: adapter.OK, Detail: "registered (stdio bridge, syncs while DSH runs)"}
}

// ---------------------------------------------------------------------------
// The managed block in cordis.patch.yml

const (
	blockBegin   = "# >>> memspace: managed by `memspace agent install dsh`; remove with `memspace agent uninstall dsh`"
	blockEnd     = "# <<< memspace"
	hooksRowID   = "memspace-hooks"
	mcpRowID     = "memspace-mcp"
	hooksPackage = "@deepseek-ai/dsh-hooks-claude-code"
	mcpPackage   = "@deepseek-ai/dsh-mcp-client"
)

// renderBlock returns the patch entry. Strings are written as JSON, which
// is valid YAML.
func (a *Adapter) renderBlock(st *installState) string {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	var b strings.Builder
	b.WriteString(blockBegin + "\n")
	b.WriteString("- insert:\n")
	b.WriteString("    - id: " + hooksRowID + "\n")
	b.WriteString("      name: " + q(hooksPackage) + "\n")
	b.WriteString("      config:\n")
	b.WriteString("        configPath: " + q(a.HooksPath()) + "\n")
	b.WriteString("        defaultTimeoutMs: 15000\n")
	if len(st.MCPCommand) > 0 {
		args := make([]string, len(st.MCPCommand)-1)
		for i, s := range st.MCPCommand[1:] {
			args[i] = q(s)
		}
		b.WriteString("    - id: " + mcpRowID + "\n")
		b.WriteString("      name: " + q(mcpPackage) + "\n")
		b.WriteString("      config:\n")
		b.WriteString("        serverName: " + adapter.MCPServerName + "\n")
		b.WriteString("        transport: stdio\n")
		b.WriteString("        command: " + q(st.MCPCommand[0]) + "\n")
		b.WriteString("        args: [" + strings.Join(args, ", ") + "]\n")
	}
	b.WriteString(blockEnd + "\n")
	return b.String()
}

// findBlock returns the managed block of a patch file.
func findBlock(text string) (string, bool) {
	i := strings.Index(text, blockBegin)
	if i < 0 {
		return "", false
	}
	j := strings.Index(text[i:], blockEnd)
	if j < 0 {
		return "", false
	}
	return text[i : i+j+len(blockEnd)], true
}

// writeBlock adds or replaces the managed block. The patch file is a YAML
// list: an empty list (`[]`) is replaced by the block, a block-style list
// gets it appended, and a non-empty flow list is left for the user to
// convert.
func (a *Adapter) writeBlock(st *installState) error {
	path := a.PatchPath()
	block := a.renderBlock(st)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		st.CreatedPatch = true
		return writeFile(path, []byte(block))
	case err != nil:
		return err
	}
	text := string(b)
	if old, ok := findBlock(text); ok {
		return writeFile(path, []byte(strings.Replace(text, old, strings.TrimSuffix(block, "\n"), 1)))
	}
	switch entries(text) {
	case "empty":
		text = emptyListRE.ReplaceAllString(text, "")
	case "flow":
		return fmt.Errorf("%s is a YAML flow list; rewrite it as a block list (one '- ' entry per line) and re-run, or add this entry yourself:\n%s", path, block)
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return writeFile(path, []byte(text+block))
}

// removeBlock deletes the managed block. A file left without entries gets
// `[]` (DSH refuses an empty patch file), or is deleted if install created
// it.
func (a *Adapter) removeBlock(created bool) (bool, error) {
	path := a.PatchPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	text := string(b)
	old, ok := findBlock(text)
	if !ok {
		return false, nil
	}
	text = strings.Replace(text, old+"\n", "", 1)
	text = strings.Replace(text, old, "", 1)
	if entries(text) == "empty" {
		if created && strings.TrimSpace(text) == "" {
			return true, os.Remove(path)
		}
		if !emptyListRE.MatchString(text) {
			text += "[]\n"
		}
	}
	return true, writeFile(path, []byte(text))
}

var emptyListRE = regexp.MustCompile(`(?m)^\[\]\s*$\n?`)

// entries classifies a patch file: "empty" (only comments, blank lines or
// `[]`), "flow" (a non-empty `[...]` list) or "block".
func entries(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "" || strings.HasPrefix(l, "#") || l == "[]":
		case strings.HasPrefix(l, "["):
			return "flow"
		default:
			return "block"
		}
	}
	return "empty"
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if old, err := os.ReadFile(path); err == nil {
			os.WriteFile(path+".memspace.bak", old, mode)
		}
	}
	tmp := path + ".memspace.tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// Doctor

func (a *Adapter) Doctor(ctx context.Context, opts adapter.InstallOptions) []adapter.Check {
	var checks []adapter.Check
	d := a.Detect(ctx)
	switch {
	case d.Binary != "":
		checks = append(checks, adapter.Check{Name: "dsh binary", Status: adapter.OK, Detail: strings.TrimSpace(d.Binary + " " + d.Version)})
	case d.Found:
		checks = append(checks, adapter.Check{Name: "dsh binary", Status: adapter.Warn, Detail: "not on PATH, but " + d.ConfigDir + " exists"})
	default:
		checks = append(checks, adapter.Check{Name: "dsh binary", Status: adapter.Fail, Detail: "DSH not found"})
	}

	st, ok := a.loadState()
	if !ok {
		return append(checks, adapter.Check{Name: "importer", Status: adapter.Fail, Detail: "not installed (run 'memspace agent install dsh')"})
	}
	sessions := filepath.Join(a.DSHHome(), "sessions")
	if _, err := os.Stat(sessions); err != nil {
		checks = append(checks, adapter.Check{Name: "session logs", Status: adapter.Warn, Detail: sessions + " does not exist yet"})
	} else {
		files, last := a.NewImporter().Stats()
		detail := fmt.Sprintf("%s, since %s, %d files tracked", sessions, st.ImportSince.Local().Format("2006-01-02 15:04"), files)
		if !last.IsZero() {
			detail += ", last import " + last.Local().Format("2006-01-02 15:04:05")
		}
		checks = append(checks, adapter.Check{Name: "session logs", Status: adapter.OK, Detail: detail})
	}

	b, err := os.ReadFile(a.PatchPath())
	if _, found := findBlock(string(b)); err != nil || !found {
		return append(checks, adapter.Check{Name: "patch", Status: adapter.Fail,
			Detail: "MemSpace block missing from " + a.PatchPath() + " (re-run 'memspace agent install dsh')"})
	}
	checks = append(checks, adapter.Check{Name: "patch", Status: adapter.OK, Detail: a.PatchPath()})

	doc, _, err := adapter.LoadJSONFile(a.HooksPath())
	if err != nil {
		return append(checks, adapter.Check{Name: "hooks", Status: adapter.Fail, Detail: err.Error()})
	}
	checks = append(checks, adapter.HookChecks(doc, agentName, opts.HookCommand, hookSpecs))
	cmd := ""
	for _, cmds := range adapter.InstalledHooks(doc, agentName) {
		cmd = cmds[0]
	}
	return append(checks, selfTest(ctx, cmd))
}

// selfTest runs the hook command with a sample prompt in dry-run mode. DSH
// hooks record nothing themselves, so success is a clean exit.
func selfTest(ctx context.Context, command string) adapter.Check {
	name := "hook self-test"
	if command == "" {
		return adapter.Check{Name: name, Status: adapter.Fail, Detail: "no installed hook command to run"}
	}
	if runtime.GOOS == "windows" {
		return adapter.Check{Name: name, Status: adapter.Warn, Detail: "skipped on Windows"}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sample, _ := json.Marshal(map[string]any{
		"session_id": "memspace-doctor", "cwd": os.TempDir(), "hook_event_name": "SessionEnd", "transcript_path": "",
	})
	c := exec.CommandContext(ctx, "sh", "-c", command)
	c.Stdin = bytes.NewReader(sample)
	c.Env = append(os.Environ(), adapter.DryRunEnv+"=1")
	c.Dir = os.TempDir()
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return adapter.Check{Name: name, Status: adapter.Fail, Detail: fmt.Sprintf("%v: %s", err, strings.TrimSpace(stderr.String()))}
	}
	return adapter.Check{Name: name, Status: adapter.OK, Detail: "hook command runs"}
}

// ---------------------------------------------------------------------------
// Hooks

// Normalize records nothing: the session log has everything the bridge's
// hook payloads carry, and more. The hook exists to add context to
// prompts.
func (a *Adapter) Normalize(raw []byte, now time.Time) ([]spool.Record, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("dsh hook: parse input: %w", err)
	}
	return nil, nil
}
