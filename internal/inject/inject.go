// Package inject runs inside agent hooks and returns the context MemSpace
// adds to the conversation: a card for unfinished work at session start and
// related history when the user submits a prompt.
//
// Hooks are latency-sensitive and must never break the agent: every server
// call has a tight deadline, nothing is attempted while the client knows it
// is offline, and failures simply mean no injection. Session-start context
// falls back to a recent local cache when the server cannot be reached.
package inject

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/search"
)

// Deadlines for the server call; agents wait for these hooks.
var (
	SessionStartTimeout = 2 * time.Second
	PromptTimeout       = 1500 * time.Millisecond
	CacheMaxAge         = 24 * time.Hour
)

// Hint is injected at session start when there is no unfinished work, so
// the agent knows the tools exist.
const Hint = "MemSpace (shared memory across your coding agents and devices) is available through the memspace MCP tools: call memspace_project_context with your cwd when continuing earlier work, memspace_search to find how something was done before."

// Config is what a hook needs to fetch context.
type Config struct {
	ServerURL string
	Token     string
	StateDir  string
	Agent     string // agent_type, e.g. claude-code
	Offline   bool   // the client is backing off after failed syncs
	MCP       bool   // the MCP server is registered (the hint names its tools)
	HTTP      *http.Client
}

// Input is the subset of hook stdin MemSpace reads (same field names in
// Claude Code and Codex).
type Input struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	Source        string `json:"source"`
	Prompt        string `json:"prompt"`
}

// Run returns the hook's stdout (JSON with additionalContext) or nil.
func Run(ctx context.Context, cfg Config, raw []byte) []byte {
	var in Input
	if json.Unmarshal(raw, &in) != nil || in.SessionID == "" {
		return nil
	}
	var text string
	switch in.HookEventName {
	case "SessionStart":
		if in.Source == "resume" {
			return nil // the resumed transcript already carries its history
		}
		text = sessionStart(ctx, cfg, in)
	case "UserPromptSubmit":
		text = promptRecall(ctx, cfg, in)
	}
	if text == "" {
		return nil
	}
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": in.HookEventName, "additionalContext": text},
	})
	return out
}

func ref(cwd string) (search.ProjectRef, string) {
	h := project.Detect(cwd)
	if h.GitIdentity != "" {
		return search.ProjectRef{GitIdentity: h.GitIdentity}, h.Key()
	}
	return search.ProjectRef{Name: h.Name}, h.Key()
}

func sessionStart(ctx context.Context, cfg Config, in Input) string {
	pref, key := ref(in.Cwd)
	cache := filepath.Join(cfg.StateDir, "context-cache", hash(key)+".json")
	hint := ""
	if cfg.MCP {
		hint = Hint
	}
	if !cfg.Offline {
		pack, err := fetch(ctx, cfg, SessionStartTimeout, contextpack.Request{
			Mode: contextpack.ModeSessionStart, Project: pref, AgentType: cfg.Agent, NativeSessionID: in.SessionID,
		})
		if err == nil {
			writeCache(cache, pack.Text)
			if pack.Text != "" {
				return pack.Text
			}
			return hint
		}
	}
	if text, at, ok := readCache(cache); ok && text != "" {
		return fmt.Sprintf("(MemSpace server unreachable; showing notes cached %s.)\n%s", at.Local().Format("2006-01-02 15:04"), text)
	}
	return hint
}

func promptRecall(ctx context.Context, cfg Config, in Input) string {
	if cfg.Offline {
		return ""
	}
	query := prompt.OwnWords(in.Prompt)
	if !substantive(query) {
		return "" // skip the network call for "ok", "继续" and the like
	}
	pref, _ := ref(in.Cwd)
	deliveredPath := filepath.Join(cfg.StateDir, "delivered", hash(cfg.Agent+"|"+in.SessionID)+".json")
	delivered := readDelivered(deliveredPath)
	pack, err := fetch(ctx, cfg, PromptTimeout, contextpack.Request{
		Mode: contextpack.ModePrompt, Project: pref, AgentType: cfg.Agent, NativeSessionID: in.SessionID,
		Query: query, Exclude: delivered,
	})
	if err != nil || pack.Text == "" {
		return ""
	}
	for _, it := range pack.Items {
		delivered = append(delivered, it.Key)
	}
	writeJSON(deliveredPath, delivered)
	return pack.Text
}

func substantive(q string) bool {
	n := 0
	for _, r := range q {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			n++
		}
	}
	return n >= 6
}

func fetch(ctx context.Context, cfg Config, timeout time.Duration, req contextpack.Request) (*contextpack.Pack, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(cfg.ServerURL, "/")+"/api/v1/context", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+cfg.Token)
	r.Header.Set("Content-Type", "application/json")
	hc := cfg.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("context: HTTP %d", resp.StatusCode)
	}
	var pack contextpack.Pack
	if err := json.NewDecoder(resp.Body).Decode(&pack); err != nil {
		return nil, err
	}
	if len(pack.Text) > 4*contextpack.HardLimit { // defensive: never flood the agent
		return nil, fmt.Errorf("context: oversized pack")
	}
	return &pack, nil
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:12])
}

type cached struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

func writeCache(path, text string) { writeJSON(path, cached{Text: text, At: time.Now().UTC()}) }

func readCache(path string) (string, time.Time, bool) {
	var c cached
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &c) != nil || time.Since(c.At) > CacheMaxAge {
		return "", time.Time{}, false
	}
	return c.Text, c.At, true
}

func readDelivered(path string) []string {
	var keys []string
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &keys)
	}
	if len(keys) > 500 {
		keys = keys[len(keys)-500:]
	}
	return keys
}

func writeJSON(path string, v any) {
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}
