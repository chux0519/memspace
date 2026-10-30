package inject

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/contextpack"
)

type fakeServer struct {
	*httptest.Server
	calls atomic.Int32
	last  contextpack.Request
	reply contextpack.Pack
	delay time.Duration
	code  int
}

func newFake(t *testing.T) *fakeServer {
	f := &fakeServer{code: 200}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Path != "/api/v1/context" {
			w.WriteHeader(401)
			return
		}
		f.last = contextpack.Request{}
		json.NewDecoder(r.Body).Decode(&f.last)
		time.Sleep(f.delay)
		w.WriteHeader(f.code)
		json.NewEncoder(w).Encode(f.reply)
	}))
	t.Cleanup(f.Close)
	return f
}

func cfg(t *testing.T, f *fakeServer) Config {
	return Config{ServerURL: f.URL, Token: "tok", StateDir: t.TempDir(), Agent: "claude-code", MCP: true}
}

func input(event string, extra map[string]any) []byte {
	m := map[string]any{"session_id": "s1", "cwd": "/tmp", "hook_event_name": event}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func context_(t *testing.T, out []byte) string {
	t.Helper()
	if out == nil {
		return ""
	}
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("bad hook output %q: %v", out, err)
	}
	return v.HookSpecificOutput.AdditionalContext
}

func TestSessionStartAndCache(t *testing.T) {
	f := newFake(t)
	c := cfg(t, f)
	f.reply = contextpack.Pack{Text: "<memspace_context>card</memspace_context>"}
	out := Run(context.Background(), c, input("SessionStart", map[string]any{"source": "startup"}))
	if got := context_(t, out); got != f.reply.Text {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(string(out), `"hookEventName":"SessionStart"`) {
		t.Errorf("output: %s", out)
	}
	if f.last.Mode != contextpack.ModeSessionStart || f.last.NativeSessionID != "s1" || f.last.AgentType != "claude-code" {
		t.Errorf("request = %+v", f.last)
	}
	if Run(context.Background(), c, input("SessionStart", map[string]any{"source": "resume"})) != nil {
		t.Error("resume should not inject")
	}

	// Server down: the cached card is used, labelled as cached.
	f.Close()
	got := context_(t, Run(context.Background(), c, input("SessionStart", nil)))
	if !strings.Contains(got, "unreachable") || !strings.Contains(got, "card") {
		t.Errorf("cache fallback: %q", got)
	}
	// No cache: just the hint (only when MCP is set up).
	c.StateDir = t.TempDir()
	if got := context_(t, Run(context.Background(), c, input("SessionStart", nil))); got != Hint {
		t.Errorf("hint: %q", got)
	}
	c.MCP = false
	if Run(context.Background(), c, input("SessionStart", nil)) != nil {
		t.Error("hint without MCP")
	}
}

func TestNothingToSayIsTheHint(t *testing.T) {
	f := newFake(t)
	if got := context_(t, Run(context.Background(), cfg(t, f), input("SessionStart", nil))); got != Hint {
		t.Errorf("got %q", got)
	}
}

func TestOfflineSkipsNetwork(t *testing.T) {
	f := newFake(t)
	c := cfg(t, f)
	c.Offline = true
	Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "认证模块为什么会超时呢"}))
	Run(context.Background(), c, input("SessionStart", nil))
	if f.calls.Load() != 0 {
		t.Errorf("offline client made %d calls", f.calls.Load())
	}
}

func TestPromptRecall(t *testing.T) {
	f := newFake(t)
	c := cfg(t, f)
	if Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "好的"})) != nil || f.calls.Load() != 0 {
		t.Fatal("short prompt went to the server")
	}
	f.reply = contextpack.Pack{Text: "history", Items: []contextpack.Item{{Key: "doc:1"}, {Key: "session:x:3"}}}
	log := strings.Repeat("2026-10-26 10:00:01 INFO ok\n", 50)
	got := context_(t, Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": log + "认证模块为什么会超时"})))
	if got != "history" || f.last.Mode != contextpack.ModePrompt {
		t.Fatalf("got %q, req %+v", got, f.last)
	}
	if strings.Contains(f.last.Query, "INFO ok") || !strings.Contains(f.last.Query, "认证模块为什么会超时") {
		t.Errorf("pasted log sent as query: %q", f.last.Query)
	}
	f.reply = contextpack.Pack{}
	Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "继续处理认证模块超时"}))
	if strings.Join(f.last.Exclude, ",") != "doc:1,session:x:3" {
		t.Errorf("delivered items not excluded: %v", f.last.Exclude)
	}
	// Other sessions start fresh.
	Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"session_id": "s2", "prompt": "继续处理认证模块超时"}))
	if len(f.last.Exclude) != 0 {
		t.Errorf("exclusions leaked across sessions: %v", f.last.Exclude)
	}
}

func TestSlowServerDoesNotStallPrompt(t *testing.T) {
	f := newFake(t)
	f.delay = 3 * time.Second
	f.reply = contextpack.Pack{Text: "late"}
	start := time.Now()
	out := Run(context.Background(), cfg(t, f), input("UserPromptSubmit", map[string]any{"prompt": "认证模块为什么会超时"}))
	if out != nil || time.Since(start) > PromptTimeout+500*time.Millisecond {
		t.Errorf("out=%s after %v", out, time.Since(start))
	}
}

func TestIgnoresOtherEventsAndGarbage(t *testing.T) {
	f := newFake(t)
	c := cfg(t, f)
	for _, raw := range [][]byte{input("PostToolUse", nil), input("Stop", nil), []byte("not json"), []byte(`{"hook_event_name":"SessionStart"}`)} {
		if Run(context.Background(), c, raw) != nil {
			t.Errorf("injected for %s", raw)
		}
	}
	f.code = 500
	if got := context_(t, Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "认证模块为什么会超时"}))); got != "" {
		t.Errorf("server error injected %q", got)
	}
}

// DSH's SessionStart context can miss the first request, so it comes with
// the first prompt, once. An unwritable marker directory never repeats it.
func TestStartWithFirstPrompt(t *testing.T) {
	f := newFake(t)
	c := cfg(t, f)
	c.Agent, c.StartWithFirstPrompt, c.MarkerDir = "dsh", true, t.TempDir()
	f.reply = contextpack.Pack{Text: "<memspace_context>notes</memspace_context>"}
	prompt := input("UserPromptSubmit", map[string]any{"prompt": "fix the deploy script"})

	got := context_(t, Run(context.Background(), c, prompt))
	if f.calls.Load() != 2 || !strings.Contains(got, "notes") {
		t.Fatalf("first prompt: %d calls, %q", f.calls.Load(), got)
	}
	Run(context.Background(), c, prompt)
	if f.last.Mode != contextpack.ModePrompt || f.calls.Load() != 3 {
		t.Errorf("second prompt fetched start context again: %d calls, mode %s", f.calls.Load(), f.last.Mode)
	}

	c.MarkerDir = "/proc/memspace-unwritable"
	f.calls.Store(0)
	Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "fix the deploy script", "session_id": "s2"}))
	if f.calls.Load() != 1 || f.last.Mode != contextpack.ModePrompt {
		t.Errorf("unwritable markers: %d calls, mode %s", f.calls.Load(), f.last.Mode)
	}
}

func TestCompactChineseTechnicalQueryReachesServer(t *testing.T) {
	f := newFake(t)
	f.reply = contextpack.Pack{Text: "关闭结果通道修复协程泄漏", Items: []contextpack.Item{{Key: "doc:short"}}}
	c := cfg(t, f)
	if got := context_(t, Run(context.Background(), c, input("UserPromptSubmit", map[string]any{"prompt": "协程泄漏"}))); got != f.reply.Text {
		t.Fatalf("short technical query lost: %q", got)
	}
	if f.calls.Load() != 1 || f.last.Query != "协程泄漏" {
		t.Fatalf("wrong call: %+v", f.last)
	}
}
