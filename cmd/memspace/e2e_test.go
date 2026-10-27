package main

// End-to-end acceptance: with the real binary, hooks installed
// into a sandboxed Claude Code / Codex config deliver events to a running
// server automatically; when the server is down events are buffered, the
// client reports being offline, and once it is back they are replayed.

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/db"
)

type e2e struct {
	t       *testing.T
	bin     string
	dir     string
	addr    string
	env     []string
	server  *exec.Cmd
	db      *sql.DB
	project string
}

func newE2E(t *testing.T) *e2e {
	if testing.Short() {
		t.Skip("e2e: skipped with -short")
	}
	if runtime.GOOS == "windows" {
		t.Skip("e2e uses sh to run hook commands")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "memspace")
	build := exec.Command("go", "build", "-tags", "sqlite_fts5", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	// A git repository the "agent" works in.
	proj := filepath.Join(dir, "work", "webapp")
	os.MkdirAll(filepath.Join(proj, ".git", "refs", "heads"), 0o755)
	os.WriteFile(filepath.Join(proj, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".git", "refs", "heads", "main"), []byte("0123abcd\n"), 0o644)
	os.WriteFile(filepath.Join(proj, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:me/webapp.git\n"), 0o644)

	e := &e2e{t: t, bin: bin, dir: dir, addr: addr, project: proj}
	// Agents only pass their own environment to hooks; MEMSPACE_HOME is
	// deliberately absent so the test proves the installed command carries
	// --home. PATH excludes real agents so nothing outside the sandbox runs.
	e.env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + filepath.Join(dir, "userhome"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "claude"),
		"CODEX_HOME=" + filepath.Join(dir, "codex"),
		"MEMSPACE_BACKOFF_BASE=300ms",
		"MEMSPACE_DB_PATH=" + filepath.Join(dir, "server", "memspace.db"),
	}
	t.Cleanup(e.stopServer)
	return e
}

func (e *e2e) run(args ...string) string {
	e.t.Helper()
	out, err := e.runErr(args...)
	if err != nil {
		e.t.Fatalf("memspace %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (e *e2e) runErr(args ...string) (string, error) {
	full := append([]string{"--home", filepath.Join(e.dir, "mshome")}, args...)
	cmd := exec.Command(e.bin, full...)
	cmd.Env = e.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (e *e2e) startServer() {
	e.t.Helper()
	cmd := exec.Command(e.bin, "server", "start")
	cmd.Env = append(e.env, "MEMSPACE_ADDRESS="+e.addr)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.server = cmd
	e.waitFor("server up", func() bool {
		c, err := net.Dial("tcp", e.addr)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if e.db == nil {
		var err error
		if e.db, err = db.Open(filepath.Join(e.dir, "server", "memspace.db")); err != nil {
			e.t.Fatal(err)
		}
		e.t.Cleanup(func() { e.db.Close() })
	}
}

// connect registers the device with an invite created on the "server side".
func (e *e2e) connect(extra ...string) {
	e.t.Helper()
	out := e.run("server", "invite")
	code := ""
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "msi_") {
			code = f
			break
		}
	}
	if code == "" {
		e.t.Fatalf("no invite in: %s", out)
	}
	e.run(append([]string{"client", "connect", "http://" + e.addr, "--invite", code}, extra...)...)
}

func (e *e2e) stopServer() {
	if e.server != nil && e.server.Process != nil {
		e.server.Process.Kill()
		e.server.Wait()
		e.server = nil
	}
}

func (e *e2e) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	logs, _ := os.ReadFile(filepath.Join(e.dir, "mshome", "logs", "sync.log"))
	hooks, _ := os.ReadFile(filepath.Join(e.dir, "mshome", "logs", "hook.log"))
	status, _ := e.runErr("client", "status")
	for _, f := range []string{"codex.json", "codex-cursors.json"} {
		b, _ := os.ReadFile(filepath.Join(e.dir, "mshome", "state", f))
		status += "\n" + f + ": " + string(b)
	}
	e.t.Fatalf("timed out waiting for %s\nsync.log:\n%s\nhook.log:\n%s\nstatus:\n%s", what, logs, hooks, status)
}

func (e *e2e) count(q string, args ...any) int {
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// hookCommand reads the command the installer wrote for an event.
func (e *e2e) hookCommand(settings, agent string) string {
	e.t.Helper()
	doc, _, err := adapter.LoadJSONFile(settings)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, cmds := range adapter.InstalledHooks(doc, agent) {
		return cmds[0]
	}
	e.t.Fatalf("no %s hook in %s", agent, settings)
	return ""
}

// fireHook runs a hook command the way the agent does: via the shell, JSON
// on stdin, cwd in the project. It must succeed quickly. Only context events
// may print, and then only the hook JSON; it returns the injected context.
func (e *e2e) fireHook(command string, payload []byte) string {
	e.t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = e.env
	cmd.Dir = e.project
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	start := time.Now()
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		e.t.Fatalf("hook: err=%v stderr=%q", err, stderr.String())
	}
	if d := time.Since(start); d > 3*time.Second {
		e.t.Errorf("hook took %v", d)
	}
	var in struct {
		Event string `json:"hook_event_name"`
	}
	json.Unmarshal(payload, &in)
	if len(out) == 0 {
		return ""
	}
	if in.Event != "SessionStart" && in.Event != "UserPromptSubmit" {
		e.t.Fatalf("%s hook printed %q", in.Event, out)
	}
	var res struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.HookSpecificOutput.HookEventName != in.Event {
		e.t.Fatalf("bad hook output %q", out)
	}
	return res.HookSpecificOutput.AdditionalContext
}

func claudePayloads(t *testing.T, session, cwd string) [][]byte {
	f, err := os.Open("../../internal/adapter/claudecode/testdata/hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		json.Unmarshal(sc.Bytes(), &m)
		m["session_id"] = session
		m["cwd"] = cwd
		b, _ := json.Marshal(m)
		out = append(out, b)
	}
	return out
}

func TestE2EClaudeOnlineOfflineReplay(t *testing.T) {
	e := newE2E(t)
	e.startServer()
	e.connect("--name", "e2e-box")
	out := e.run("agent", "install", "claude", "--bin", e.bin)
	if !strings.Contains(out, "Installed Claude Code hooks") {
		t.Fatalf("install: %s", out)
	}
	settings := filepath.Join(e.dir, "claude", "settings.json")
	cmd := e.hookCommand(settings, "claude")
	if !strings.Contains(cmd, "--home") {
		t.Fatalf("hook command does not pin the client home: %s", cmd)
	}

	// Online: a whole session arrives without anyone running sync.
	for _, p := range claudePayloads(t, "sess-online", e.project) {
		e.fireHook(cmd, p)
	}
	e.waitFor("online events on server", func() bool { return e.count(`SELECT COUNT(*) FROM events`) == 6 })
	if n := e.count(`SELECT COUNT(*) FROM sessions WHERE status = 'ended'`); n != 1 {
		t.Errorf("SessionEnd did not end the session (%d)", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM projects WHERE canonical_git_identity = 'github.com/me/webapp'`); n != 1 {
		t.Errorf("project identity not derived from git remote")
	}
	if n := e.count(`SELECT COUNT(*) FROM project_locations WHERE absolute_path = ? AND git_head = '0123abcd'`, e.project); n != 1 {
		t.Errorf("location not recorded")
	}

	// A new session starts with the earlier, unfinished session (it left an
	// unresolved failure) injected as context.
	got := e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"sess-next","cwd":%q,"hook_event_name":"SessionStart","source":"startup"}`, e.project)))
	if !strings.Contains(got, "<memspace_context") || !strings.Contains(got, "Run the bash command") || !strings.Contains(got, "nonexistent_zz") {
		t.Errorf("session start context:\n%s", got)
	}
	// A related prompt recalls history; an unrelated one stays quiet.
	if got := e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"sess-next","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":"why did ls nonexistent_zz fail before?"}`, e.project))); !strings.Contains(got, "nonexistent_zz") {
		t.Errorf("prompt recall:\n%s", got)
	}
	if got := e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"sess-next","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":"write a haiku about autumn leaves"}`, e.project))); got != "" {
		t.Errorf("unrelated prompt injected:\n%s", got)
	}
	e.waitFor("sess-next events uploaded", func() bool { return e.count(`SELECT COUNT(*) FROM events`) == 9 })

	// Offline: the server goes away while the agent keeps working.
	e.stopServer()
	for _, p := range claudePayloads(t, "sess-offline", e.project) {
		e.fireHook(cmd, p)
	}
	e.waitFor("client to notice it is offline", func() bool {
		out, _ := e.runErr("client", "status")
		return strings.Contains(out, "OFFLINE") && strings.Contains(out, "6 pending")
	})

	// Recovery: the next hook after the backoff replays everything.
	e.startServer()
	time.Sleep(2 * time.Second) // let the backoff elapse
	e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"sess-offline","cwd":%q,"hook_event_name":"UserPromptSubmit","prompt":"back online?","prompt_id":"p-2"}`, e.project)))
	e.waitFor("offline events replayed", func() bool { return e.count(`SELECT COUNT(*) FROM events`) == 16 })
	e.waitFor("status ok", func() bool {
		out, _ := e.runErr("client", "status")
		return strings.Contains(out, "Sync:       ok") && strings.Contains(out, "0 pending")
	})
	if n := e.count(`SELECT COUNT(DISTINCT event_id) FROM events`); n != 16 {
		t.Errorf("duplicate events stored")
	}

	// Uploaded history is searchable from the CLI.
	if out := e.run("search", "--all", "nonexistent_zz"); !strings.Contains(out, "failure") || !strings.Contains(out, "webapp") {
		t.Errorf("search: %s", out)
	}

	// Doctor exercises the installed command and the server round trip.
	out, err := e.runErr("agent", "doctor", "claude")
	if err != nil || !strings.Contains(out, "✓ hook self-test") || !strings.Contains(out, "token valid") {
		t.Errorf("doctor: %v\n%s", err, out)
	}

	// Uninstall restores the settings file.
	e.run("agent", "uninstall", "claude")
	b, _ := os.ReadFile(settings)
	if strings.Contains(string(b), "memspace") {
		t.Errorf("hooks left after uninstall: %s", b)
	}
}

func TestE2ECodexImport(t *testing.T) {
	e := newE2E(t)
	e.startServer()
	e.connect()
	out := e.run("agent", "install", "codex", "--bin", e.bin)
	if !strings.Contains(out, "Installed Codex hooks") || !strings.Contains(out, "/hooks") {
		t.Fatalf("install: %s", out)
	}

	// Codex writes a rollout after installation.
	fixture, err := os.ReadFile("../../internal/adapter/codex/testdata/rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Minute).Format("2006-01-02T15:04:")
	fixture = bytes.ReplaceAll(fixture, []byte("2026-10-26T02:36:"), []byte(now))
	fixture = bytes.ReplaceAll(fixture, []byte(`"cwd":"/tmp/work"`), []byte(fmt.Sprintf(`"cwd":%q`, e.project)))
	dir := filepath.Join(e.dir, "codex", "sessions", "2026", "10", "26")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "rollout-2026-10-26T10-36-16-01a17610-bd86-7172-86a8-399df8c1ead9.jsonl"), fixture, 0o600)

	// Codex's Stop hook triggers the import + upload.
	cmd := e.hookCommand(filepath.Join(e.dir, "codex", "hooks.json"), "codex")
	e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"01a17610-bd86-7172-86a8-399df8c1ead9","cwd":%q,"hook_event_name":"Stop","last_assistant_message":"DONE"}`, e.project)))
	e.waitFor("codex events on server", func() bool {
		return e.count(`SELECT COUNT(*) FROM events e JOIN sessions s ON s.session_id = e.session_id WHERE s.agent_type = 'codex'`) == 8
	})
	e.fireHook(cmd, []byte(fmt.Sprintf(`{"session_id":"01a17610-bd86-7172-86a8-399df8c1ead9","cwd":%q,"hook_event_name":"SessionEnd","reason":"exit"}`, e.project)))
	e.waitFor("codex session ended", func() bool {
		return e.count(`SELECT COUNT(*) FROM sessions WHERE agent_type = 'codex' AND status = 'ended'`) == 1
	})
	// Re-importing history is idempotent.
	e.run("import", "codex", "--all")
	e.run("client", "sync")
	if n := e.count(`SELECT COUNT(*) FROM events`); n != 9 {
		t.Errorf("events after re-import = %d, want 9", n)
	}
}
