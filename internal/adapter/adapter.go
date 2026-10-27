// Package adapter defines the agent adapter contract and the
// pieces shared by concrete adapters: hook-config editing, event IDs and
// payload sanitizing.
package adapter

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/redact"
)

// Adapter integrates one coding agent with MemSpace.
type Adapter interface {
	// Name is the CLI name, e.g. "claude" or "codex".
	Name() string
	// Detect reports whether the agent is present on this machine.
	Detect(ctx context.Context) Detection
	// Install registers MemSpace hooks with the agent.
	Install(ctx context.Context, opts InstallOptions) error
	// Uninstall removes MemSpace hooks, leaving the user's other config intact.
	Uninstall(ctx context.Context, out io.Writer) error
	// Doctor checks that capture works end to end on this machine.
	Doctor(ctx context.Context, opts InstallOptions) []Check
	// Capabilities lists the event types this adapter can capture.
	Capabilities() []string
	// Normalizer converts a hook invocation (stdin of `memspace hook <name>`).
	Normalizer
}

// MCPOptions describe how an agent should reach MemSpace's MCP endpoint
// without storing a secret in the agent's configuration.
type MCPOptions struct {
	URL string // e.g. https://memory.example.com/mcp
	// HeaderCommand prints {"Authorization": "Bearer …"} (Claude Code headersHelper).
	HeaderCommand string
	// TokenEnv names the environment variable holding the token (Codex).
	TokenEnv string
	Out      io.Writer
}

// MCPRegistrar is implemented by adapters that can register the MCP server.
type MCPRegistrar interface {
	RegisterMCP(ctx context.Context, opts MCPOptions) error
	UnregisterMCP(ctx context.Context) error
	MCPCheck(ctx context.Context) Check
}

// MCPServerName is the name MemSpace registers under in agent configs.
const MCPServerName = "memspace"

// Detection describes an agent installation.
type Detection struct {
	Found     bool
	Binary    string
	Version   string
	ConfigDir string
}

// InstallOptions are shared by Install and Doctor.
type InstallOptions struct {
	// HookCommand is the shell command the agent runs for each hook, e.g.
	// `"/usr/local/bin/memspace" hook claude`.
	HookCommand string
	// Trust pre-approves the installed hooks where the agent requires review
	// (Codex). Only set when the user asked for it explicitly.
	Trust bool
	Out   io.Writer
}

// Check is one doctor finding.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Status of a Check.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) Symbol() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// HookCommand builds the command string agents execute. Paths are quoted
// for POSIX shells, which both Claude Code and Codex use to run hooks.
func HookCommand(exe, home, configPath, agent string) string {
	return Command(exe, home, configPath, "hook", agent)
}

// Command builds a shell command line that runs this binary with the given
// client home/config pinned.
func Command(exe, home, configPath string, args ...string) string {
	var b strings.Builder
	b.WriteString(shellQuote(exe))
	if home != "" {
		b.WriteString(" --home " + shellQuote(home))
	}
	if configPath != "" {
		b.WriteString(" --config " + shellQuote(configPath))
	}
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	return b.String()
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:\\", r))
	}) < 0 {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(s) + `"`
}

// IsMemSpaceHook reports whether a hook command was installed by MemSpace
// for the given agent.
func IsMemSpaceHook(command, agent string) bool {
	return strings.Contains(command, "memspace") && strings.HasSuffix(strings.TrimSpace(command), " hook "+agent)
}

// ---------------------------------------------------------------------------
// Event IDs

// StableID derives a deterministic event ID, so re-capturing the same
// observation (hook retries, re-imports) deduplicates on the server.
func StableID(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + "_" + hex.EncodeToString(h[:16])
}

// RandomID returns a time-ordered unique ID for observations that have no
// natural identity.
func RandomID(prefix string) string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("%s_%012x%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// ---------------------------------------------------------------------------
// Payloads

const (
	maxStringBytes  = 8 << 10
	maxPayloadBytes = 64 << 10
)

// Payload redacts and size-bounds an event payload.
func Payload(v map[string]any) json.RawMessage {
	clean := redact.Bound(redact.Value(v), maxStringBytes, maxPayloadBytes)
	b, err := json.Marshal(clean)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// OmittedSensitive replaces content captured from a secret file.
const OmittedSensitive = "[omitted: sensitive file]"

// MentionsSensitivePath reports whether a shell command references a
// secret file (e.g. `cat .env`), in which case its output is not captured.
func MentionsSensitivePath(command string) bool {
	for _, tok := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == '<' || r == '>' || r == '|' || r == ';' || r == '&' || r == '='
	}) {
		if redact.SensitivePath(tok) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Hook configuration files (Claude Code settings.json, Codex hooks.json)
//
// Both use: {"hooks": {"<Event>": [{"matcher": "...", "hooks": [{"type": "command", "command": "..."}]}]}}

// HookSpec is one event MemSpace subscribes to.
type HookSpec struct {
	Event   string
	Matcher string // empty: no matcher (non-tool events)
	Timeout int    // seconds
}

// LoadJSONFile reads a JSON object file. A missing file yields an empty
// object and exists=false.
func LoadJSONFile(path string) (doc map[string]any, exists bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, true, nil
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, true, nil
}

// SaveJSONFile atomically writes doc, keeping a copy of the previous file at
// <path>.memspace.bak.
func SaveJSONFile(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
		if old, err := os.ReadFile(path); err == nil {
			if err := os.WriteFile(path+".memspace.bak", old, mode); err != nil {
				return fmt.Errorf("backup %s: %w", path, err)
			}
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".memspace.tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func hookGroups(doc map[string]any, event string) []any {
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	groups, _ := hooks[event].([]any)
	return groups
}

func groupHandlers(g any) []any {
	m, _ := g.(map[string]any)
	if m == nil {
		return nil
	}
	hs, _ := m["hooks"].([]any)
	return hs
}

func handlerCommand(h any) string {
	m, _ := h.(map[string]any)
	if m == nil {
		return ""
	}
	c, _ := m["command"].(string)
	return c
}

// InstalledHooks returns, per event, the MemSpace hook commands present.
func InstalledHooks(doc map[string]any, agent string) map[string][]string {
	out := map[string][]string{}
	hooks, _ := doc["hooks"].(map[string]any)
	for event := range hooks {
		for _, g := range hookGroups(doc, event) {
			for _, h := range groupHandlers(g) {
				if c := handlerCommand(h); IsMemSpaceHook(c, agent) {
					out[event] = append(out[event], c)
				}
			}
		}
	}
	return out
}

// MergeHooks makes doc contain exactly one MemSpace handler running command
// for each spec. Existing identical entries are left in place (so agents
// that key trust on position keep it); stale MemSpace entries are removed.
// Reports whether doc changed.
func MergeHooks(doc map[string]any, agent, command string, specs []HookSpec) bool {
	changed := false
	hooks, _ := doc["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		doc["hooks"] = hooks
		changed = true
	}
	for _, spec := range specs {
		found := false
		for _, g := range hookGroups(doc, spec.Event) {
			for _, h := range groupHandlers(g) {
				if handlerCommand(h) == command {
					found = true
				}
			}
		}
		if found {
			continue
		}
		removeFromEvent(doc, spec.Event, agent)
		group := map[string]any{
			"hooks": []any{map[string]any{
				"type":    "command",
				"command": command,
				"timeout": spec.Timeout,
			}},
		}
		if spec.Matcher != "" {
			group["matcher"] = spec.Matcher
		}
		hooks[spec.Event] = append(hookGroups(doc, spec.Event), group)
		changed = true
	}
	return changed
}

// RemoveHooks deletes every MemSpace handler for agent. Reports whether doc
// changed.
func RemoveHooks(doc map[string]any, agent string) bool {
	hooks, _ := doc["hooks"].(map[string]any)
	changed := false
	for event := range hooks {
		if removeFromEvent(doc, event, agent) {
			changed = true
		}
	}
	if hooks != nil && len(hooks) == 0 {
		delete(doc, "hooks")
	}
	return changed
}

func removeFromEvent(doc map[string]any, event, agent string) bool {
	hooks, _ := doc["hooks"].(map[string]any)
	groups := hookGroups(doc, event)
	changed := false
	var kept []any
	for _, g := range groups {
		m, _ := g.(map[string]any)
		hs := groupHandlers(g)
		var keepH []any
		for _, h := range hs {
			if IsMemSpaceHook(handlerCommand(h), agent) {
				changed = true
				continue
			}
			keepH = append(keepH, h)
		}
		if m != nil && len(keepH) != len(hs) {
			if len(keepH) == 0 {
				continue // group only held our handler
			}
			m["hooks"] = keepH
		}
		kept = append(kept, g)
	}
	if !changed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	return true
}
