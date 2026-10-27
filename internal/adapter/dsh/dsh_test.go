package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/spool"
)

// frame compresses records as one zstd frame, the way DSH appends them.
func frame(t *testing.T, recs ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(append(b, '\n'))
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	return enc.EncodeAll(buf.Bytes(), nil)
}

func ev(typ string, seq, ms int64, data any) map[string]any {
	return map[string]any{"type": typ, "seq": seq, "time": ms, "data": data}
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

const t0 = int64(1793000000000) // ms

func header(id, cwd string, depth int, parent string) map[string]any {
	h := map[string]any{"type": "session", "version": 4, "id": id, "createdAt": t0, "cwd": cwd,
		"delegationDepth": depth, "agentPreset": "standard"}
	if parent != "" {
		h["parentSession"], h["origin"] = parent, "subagent"
	}
	return h
}

func turn(n int64, seq int64, prompt string) []any {
	return []any{
		ev("turn/start", seq, t0+seq, map[string]any{"turn": n}),
		ev("user/message", seq+1, t0+seq, map[string]any{"content": text("Current runtime context."), "source": map[string]any{"kind": "runtime-context"}}),
		ev("user/message", seq+2, t0+seq, map[string]any{"content": text(prompt), "source": map[string]any{"kind": "user"}, "role": "user"}),
	}
}

func toolPair(seq int64, id, name, args, out string, isErr bool) []any {
	return []any{
		ev("tool/call", seq, t0+seq, map[string]any{"turn": 1, "callId": id, "name": name, "arguments": args}),
		ev("tool/result", seq+1, t0+seq, map[string]any{"message": map[string]any{
			"role": "tool", "toolCallId": id, "content": text(out), "isError": isErr}}),
	}
}

type env struct {
	home, state string
	sp          *spool.Spool
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{home: t.TempDir(), state: t.TempDir()}
	var err error
	if e.sp, err = spool.Open(t.TempDir(), spool.Limits{}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) log(t *testing.T, session string) string {
	p := filepath.Join(e.home, "sessions", "--work--", session, LogName)
	os.MkdirAll(filepath.Dir(p), 0o755)
	return p
}

func appendFile(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(b)
	f.Close()
	// Give each append its own mtime, as the importer skips unchanged files.
	now := time.Now().Add(time.Duration(len(b)) * time.Millisecond)
	os.Chtimes(path, now, now)
}

func (e *env) importAll(t *testing.T) []*spool.Record {
	t.Helper()
	im := &Importer{DSHHome: e.home, StateDir: e.state}
	if _, err := im.Import(context.Background(), e.sp, nil); err != nil {
		t.Fatal(err)
	}
	recs, err := e.sp.Claim(1000)
	if err != nil {
		t.Fatal(err)
	}
	e.sp.Ack(recs)
	return recs
}

// types lists event types sorted: records with equal timestamps are
// claimed from the spool in ID order.
func types(recs []*spool.Record) string {
	var s []string
	for _, r := range recs {
		s = append(s, r.EventType)
	}
	sort.Strings(s)
	return strings.Join(s, " ")
}

func sorted(s string) string {
	f := strings.Fields(s)
	sort.Strings(f)
	return strings.Join(f, " ")
}

// find returns the record of type typ whose payload satisfies ok.
func find(t *testing.T, recs []*spool.Record, typ string, ok func(map[string]any) bool) map[string]any {
	t.Helper()
	for _, r := range recs {
		if p := payload(r); r.EventType == typ && ok(p) {
			return p
		}
	}
	t.Fatalf("no %s matching", typ)
	return nil
}

func input(p map[string]any) map[string]any { m, _ := p["tool_input"].(map[string]any); return m }

func anyPayload(map[string]any) bool { return true }

func payload(r *spool.Record) map[string]any {
	var m map[string]any
	json.Unmarshal(r.Payload, &m)
	return m
}

func TestImportSession(t *testing.T) {
	e := newEnv(t)
	cwd := t.TempDir()
	path := e.log(t, "session-a")

	var recs []any
	recs = append(recs, header("session-a", cwd, 0, ""))
	recs = append(recs, turn(1, 1, "fix the flaky build")...)
	recs = append(recs, ev("assistant/message", 4, t0+4, map[string]any{"message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "reasoning", "text": "thinking"}}}}))
	recs = append(recs, toolPair(5, "c1", "bash", `{"command":"make test"}`, "FAIL\n[exit code: 2]", false)...)
	recs = append(recs, toolPair(7, "c2", "bash", `{"command":"go vet ./..."}`, "ok", false)...)
	recs = append(recs, toolPair(9, "c3", "edit", `{"file_path":"/w/a.go","old_string":"x","new_string":"y"}`, "updated", false)...)
	recs = append(recs, toolPair(11, "c4", "write", `{"file_path":"/w/b.go","content":"package b"}`, "denied", true)...)
	recs = append(recs, toolPair(13, "c5", "web_search", `{"queries":["zstd frames","go zstd"]}`, "results", false)...)
	recs = append(recs, ev("assistant/message", 15, t0+15, map[string]any{"message": map[string]any{
		"role": "assistant", "content": text("Fixed: the test raced on a temp dir.")}}))
	recs = append(recs, ev("turn/end", 16, t0+16, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}))
	appendFile(t, path, frame(t, recs...))

	got := e.importAll(t)
	want := sorted("session.started user.prompt tool.failed tool.completed tool.completed file.modified tool.failed tool.completed agent.turn_completed")
	if types(got) != want {
		t.Fatalf("events:\n got %s\nwant %s", types(got), want)
	}
	for _, r := range got {
		if r.Agent != AgentType || r.NativeSessionID != "session-a" || !strings.HasPrefix(r.EventID, "dsh_") {
			t.Errorf("record %+v", r)
		}
	}
	if p := find(t, got, "user.prompt", anyPayload); p["prompt"] != "fix the flaky build" || p["turn_id"] != "t1" {
		t.Errorf("prompt %v", p)
	}
	if p := find(t, got, "tool.failed", func(p map[string]any) bool { return p["tool_name"] == "bash" }); p["exit_code"] != float64(2) ||
		!strings.Contains(p["error"].(string), "FAIL") || input(p)["command"] != "make test" {
		t.Errorf("failed command %v", p)
	}
	if p := find(t, got, "tool.failed", func(p map[string]any) bool { return p["tool_name"] == "write" }); p["error"] != "denied" {
		t.Errorf("failed write %v", p)
	}
	if p := find(t, got, "tool.completed", func(p map[string]any) bool { return p["tool_name"] == "edit" }); input(p)["file_path"] != "/w/a.go" || input(p)["new_string"] != nil {
		t.Errorf("edit keeps only the path: %v", p)
	}
	if p := find(t, got, "file.modified", anyPayload); p["path"] != "/w/a.go" {
		t.Errorf("file.modified %v", p)
	}
	if p := find(t, got, "tool.completed", func(p map[string]any) bool { return p["tool_name"] == "web_search" }); input(p)["query"] != "zstd frames\ngo zstd" {
		t.Errorf("web_search %v", p)
	}
	if p := find(t, got, "agent.turn_completed", anyPayload); p["last_assistant_message"] != "Fixed: the test raced on a temp dir." {
		t.Errorf("reply %v", p)
	}

	// Nothing new: nothing imported. Re-reading from scratch yields the same
	// IDs, so the server deduplicates.
	if again := e.importAll(t); len(again) != 0 {
		t.Errorf("re-import: %s", types(again))
	}
	e2 := newEnv(t)
	e2.home = e.home
	ids := func(recs []*spool.Record) string {
		var s []string
		for _, r := range recs {
			s = append(s, r.EventID)
		}
		sort.Strings(s)
		return strings.Join(s, " ")
	}
	if again := e2.importAll(t); ids(again) != ids(got) {
		t.Errorf("fresh import differs")
	}
}

// Logs grow by appended frames; a frame still being written is retried, and
// turn state (the pending tool call, the last reply) carries over.
func TestImportIncremental(t *testing.T) {
	e := newEnv(t)
	path := e.log(t, "s")
	appendFile(t, path, frame(t, append([]any{header("s", t.TempDir(), 0, "")}, turn(1, 1, "deploy it")...)...))
	appendFile(t, path, frame(t, ev("tool/call", 4, t0+4, map[string]any{"callId": "c1", "name": "bash", "arguments": `{"command":"make deploy"}`}),
		ev("assistant/message", 5, t0+5, map[string]any{"message": map[string]any{"role": "assistant", "content": text("Deployed.")}})))
	if got := types(e.importAll(t)); got != sorted("session.started user.prompt") {
		t.Fatalf("first: %s", got)
	}

	last := frame(t, ev("tool/result", 6, t0+6, map[string]any{"message": map[string]any{"toolCallId": "c1", "content": text("done"), "isError": false}}),
		ev("turn/end", 7, t0+7, map[string]any{"turn": 1, "reason": map[string]any{"kind": "interrupted"}}))
	appendFile(t, path, last[:len(last)/2])
	if got := e.importAll(t); len(got) != 0 {
		t.Fatalf("partial frame imported: %s", types(got))
	}
	appendFile(t, path, last[len(last)/2:])
	got := e.importAll(t)
	if types(got) != sorted("tool.completed agent.turn_aborted") {
		t.Fatalf("after completing the frame: %s", types(got))
	}
	if p := find(t, got, "agent.turn_aborted", anyPayload); p["reason"] != "interrupted" || p["last_assistant_message"] != "Deployed." {
		t.Errorf("aborted turn %v", p)
	}
}

// A subagent's work belongs to its parent session; its prompts come from
// the parent agent, not the user.
func TestImportSubagent(t *testing.T) {
	e := newEnv(t)
	recs := append([]any{header("child", t.TempDir(), 1, "parent")}, turn(1, 1, "research X")...)
	recs = append(recs, toolPair(4, "c1", "write", `{"file_path":"/w/notes.md","content":"x"}`, "ok", false)...)
	recs = append(recs, ev("turn/end", 6, t0+6, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}))
	appendFile(t, e.log(t, "child"), frame(t, recs...))
	got := e.importAll(t)
	if types(got) != sorted("tool.completed file.modified") {
		t.Fatalf("subagent: %s", types(got))
	}
	if got[0].NativeSessionID != "parent" {
		t.Errorf("session %q", got[0].NativeSessionID)
	}
}

func TestImportSince(t *testing.T) {
	e := newEnv(t)
	recs := append([]any{header("s", t.TempDir(), 0, "")}, turn(1, 1, "old")...)
	recs = append(recs, turn(2, 100, "new")...)
	recs[len(recs)-1].(map[string]any)["time"] = t0 + 3_600_000
	appendFile(t, e.log(t, "s"), frame(t, recs...))
	im := &Importer{DSHHome: e.home, StateDir: e.state, Since: time.UnixMilli(t0 + 60_000)}
	if _, err := im.Import(context.Background(), e.sp, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := e.sp.Claim(100)
	if len(got) != 1 || payload(got[0])["prompt"] != "new" {
		t.Fatalf("since: %s", types(got))
	}
}

// ---------------------------------------------------------------------------
// cordis.patch.yml

func installed(t *testing.T, patch string) (*Adapter, string) {
	t.Helper()
	a := &Adapter{Home: t.TempDir(), StateDir: t.TempDir()}
	if patch != "" {
		os.WriteFile(a.PatchPath(), []byte(patch), 0o600)
	}
	if err := a.Install(context.Background(), adapter.InstallOptions{HookCommand: "/bin/memspace hook dsh", Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterMCP(context.Background(), adapter.MCPOptions{
		Command: append([]string{"/bin/memspace", "mcp", "stdio"}, MCPArgs...), Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(a.PatchPath())
	return a, string(b)
}

func TestPatchInstall(t *testing.T) {
	const user = "# my tweaks\n- id: model\n  config:\n    model: deepseek-v4\n"
	cases := []struct {
		name, before, afterUninstall string
		removed                      bool
	}{
		{name: "missing file", before: "", removed: true},
		{name: "empty list", before: "# Your patch layer\n[]\n", afterUninstall: "# Your patch layer\n[]\n"},
		{name: "block list", before: user, afterUninstall: user},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, text := installed(t, c.before)
			for _, want := range []string{"- insert:", "id: " + hooksRowID, `"` + hooksPackage + `"`, a.HooksPath(),
				"id: " + mcpRowID, `args: ["mcp", "stdio", "--sync-interval", "1m"]`} {
				if !strings.Contains(text, want) {
					t.Errorf("missing %q in:\n%s", want, text)
				}
			}
			if strings.Contains(text, "\n[]") {
				t.Errorf("empty list kept:\n%s", text)
			}
			if c.before == user && !strings.HasPrefix(text, user) {
				t.Errorf("user entries changed:\n%s", text)
			}
			if chk := a.MCPCheck(context.Background()); chk.Status != adapter.OK {
				t.Errorf("mcp check: %+v", chk)
			}

			// Installing again replaces the block instead of adding one.
			if err := a.Install(context.Background(), adapter.InstallOptions{HookCommand: "/bin/memspace hook dsh", Out: io.Discard}); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(a.PatchPath())
			if strings.Count(string(b), blockBegin) != 1 || string(b) != text {
				t.Errorf("reinstall changed the file:\n%s", b)
			}

			if err := a.Uninstall(context.Background(), io.Discard); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(a.PatchPath())
			switch {
			case c.removed:
				if !os.IsNotExist(err) {
					t.Errorf("patch file created by install not removed: %q", b)
				}
			case string(b) != c.afterUninstall:
				t.Errorf("after uninstall:\n%q\nwant\n%q", b, c.afterUninstall)
			}
			if a.Enabled() {
				t.Error("still enabled")
			}
		})
	}
}

func TestPatchFlowList(t *testing.T) {
	a := &Adapter{Home: t.TempDir(), StateDir: t.TempDir()}
	os.WriteFile(a.PatchPath(), []byte("[{id: x, disabled: true}]\n"), 0o600)
	err := a.Install(context.Background(), adapter.InstallOptions{HookCommand: "/bin/memspace hook dsh", Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "flow list") {
		t.Fatalf("flow list: %v", err)
	}
}

func TestDSHHomeFromWrapper(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "dsh"), []byte("#!/bin/sh\nexport DSH_HOME=/opt/dsh/data\nexec node bin.js \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin)
	t.Setenv("DSH_HOME", "")
	if got := (&Adapter{StateDir: t.TempDir()}).DSHHome(); got != "/opt/dsh/data" {
		t.Errorf("DSHHome = %q", got)
	}
	t.Setenv("DSH_HOME", "/env/dsh")
	if got := (&Adapter{StateDir: t.TempDir()}).DSHHome(); got != "/env/dsh" {
		t.Errorf("DSHHome = %q", got)
	}
}

func TestHomeFromWrapperScripts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", `C:\Users\me`)
	for name, body := range map[string]string{
		"sh":      "#!/bin/sh\nexport DSH_HOME=\"$USERPROFILE/dsh\"\nexec node bin.js \"$@\"\n",
		"dsh.cmd": "@echo off\r\nset \"DSH_HOME=%USERPROFILE%\\dsh\"\r\nnode bin.js %*\r\n",
		"dsh.ps1": "$env:DSH_HOME = \"C:\\Users\\me\\dsh\"\nnode bin.js @args\n",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o755)
		got := homeFromWrapper(p)
		if !strings.HasSuffix(filepath.ToSlash(got), "dsh") || !strings.Contains(got, "me") {
			t.Errorf("%s: %q", name, got)
		}
	}
	os.WriteFile(filepath.Join(dir, "dsh.exe"), []byte("MZ binary DSH_HOME=/x\n"), 0o755)
	if got := homeFromWrapper(filepath.Join(dir, "dsh.exe")); got != "" {
		t.Errorf("binary read as a script: %q", got)
	}
}

// DSH runs hooks with PowerShell on Windows: a quoted program path needs
// the call operator, and the command must still be recognized as ours.
func TestPowerShellCommand(t *testing.T) {
	cmd := PowerShellCommand([]string{`C:\Users\Jo Doe\bin\memspace.exe`, "--home", `C:\Users\Jo Doe\.memspace`, "hook", "dsh"})
	want := `& 'C:\Users\Jo Doe\bin\memspace.exe' --home 'C:\Users\Jo Doe\.memspace' hook dsh`
	if cmd != want {
		t.Errorf("got  %s\nwant %s", cmd, want)
	}
	if !adapter.IsMemSpaceHook(cmd, "dsh") {
		t.Error("not recognized as a MemSpace hook")
	}
	if got := PowerShellCommand([]string{`C:\bin\it's\memspace.exe`, "hook", "dsh"}); got != `& 'C:\bin\it''s\memspace.exe' hook dsh` {
		t.Errorf("quote escaping: %s", got)
	}
}
