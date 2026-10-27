package claudecode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/spool"
)

func normalizeAll(t *testing.T) []spool.Record {
	t.Helper()
	f, err := os.Open("testdata/hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a := &Adapter{}
	var out []spool.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		recs, err := a.Normalize(sc.Bytes(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, recs...)
	}
	return out
}

func payload(t *testing.T, r spool.Record) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Payload, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Fixtures are real hook payloads captured from Claude Code 2.1.300.
func TestNormalizeRealHooks(t *testing.T) {
	recs := normalizeAll(t)
	var types []string
	for _, r := range recs {
		types = append(types, r.EventType)
		if r.Agent != AgentType || r.NativeSessionID != "d871102f-3913-4e50-8a5d-8026981e1a18" {
			t.Errorf("identity: %+v", r)
		}
		if r.Project.Root != "/tmp/work" {
			t.Errorf("project root = %q", r.Project.Root)
		}
	}
	want := "session.started user.prompt tool.completed tool.failed agent.turn_completed session.ended"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("types = %s", got)
	}

	if p := payload(t, recs[1]); p["prompt"] != "Run the bash command 'echo hi', then run 'ls /nonexistent_zz', then reply DONE" {
		t.Errorf("prompt payload = %v", p)
	}
	p := payload(t, recs[2])
	if p["tool_name"] != "Bash" || p["tool_response"].(map[string]any)["stdout"] != "hi" {
		t.Errorf("tool.completed payload = %v", p)
	}
	p = payload(t, recs[3])
	if !strings.Contains(p["error"].(string), "Exit code 2") || p["tool_response"] != nil {
		t.Errorf("tool.failed payload = %v", p)
	}
	if p := payload(t, recs[4]); !strings.HasPrefix(p["last_assistant_message"].(string), "DONE") {
		t.Errorf("stop payload = %v", p)
	}
}

func TestStableIDsForRedeliveredHooks(t *testing.T) {
	a, b := normalizeAll(t), normalizeAll(t)
	// Prompt and tool events have natural IDs and must dedupe on re-delivery.
	for _, i := range []int{1, 2, 3} {
		if a[i].EventID != b[i].EventID {
			t.Errorf("%s: id not stable", a[i].EventType)
		}
	}
	if a[0].EventID == b[0].EventID {
		t.Error("session.started (resume can repeat) should get a fresh id")
	}
}

func TestFileModifiedAndSensitiveFiles(t *testing.T) {
	a := &Adapter{}
	in := map[string]any{
		"session_id": "s", "cwd": "/tmp", "hook_event_name": "PostToolUse", "tool_name": "Write",
		"tool_use_id":   "toolu_1",
		"tool_input":    map[string]any{"file_path": "/repo/.env", "content": "STRIPE_KEY=sk_live_abcdefghijklmnop1234"},
		"tool_response": map[string]any{"filePath": "/repo/.env", "content": "STRIPE_KEY=sk_live_abcdefghijklmnop1234"},
	}
	raw, _ := json.Marshal(in)
	recs, err := a.Normalize(raw, time.Now())
	if err != nil || len(recs) != 2 {
		t.Fatalf("recs=%d err=%v", len(recs), err)
	}
	if strings.Contains(string(recs[0].Payload), "sk_live") {
		t.Errorf("secret file content captured: %s", recs[0].Payload)
	}
	if recs[1].EventType != "file.modified" || payload(t, recs[1])["path"] != "/repo/.env" {
		t.Errorf("file.modified = %+v", recs[1])
	}

	in["tool_name"] = "Bash"
	in["tool_input"] = map[string]any{"command": "export TOKEN=abcdef123456 && ./deploy.sh"}
	in["tool_response"] = map[string]any{"stdout": "using key sk-proj-zzzzzzzzzzzzzzzzzzzzzzzzzz"}
	raw, _ = json.Marshal(in)
	recs, _ = a.Normalize(raw, time.Now())
	if len(recs) != 1 {
		t.Fatalf("Bash should not produce file.modified")
	}
	s := string(recs[0].Payload)
	if strings.Contains(s, "abcdef123456") || strings.Contains(s, "zzzzzzzz") {
		t.Errorf("secret not redacted: %s", s)
	}
}

func TestNormalizeIgnoresUnknownEventsAndRejectsGarbage(t *testing.T) {
	a := &Adapter{}
	if recs, err := a.Normalize([]byte(`{"session_id":"s","hook_event_name":"Notification"}`), time.Now()); err != nil || len(recs) != 0 {
		t.Errorf("Notification: %v %v", recs, err)
	}
	if _, err := a.Normalize([]byte(`not json`), time.Now()); err == nil {
		t.Error("garbage accepted")
	}
}

func TestInstallUninstallRoundTrip(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(`{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`), 0o600)
	a := &Adapter{ConfigDir: dir}
	opts := adapter.InstallOptions{HookCommand: "/opt/memspace hook claude", Out: os.Stderr}
	if err := a.Install(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	doc, _, _ := adapter.LoadJSONFile(settings)
	if got := adapter.InstalledHooks(doc, "claude"); len(got) != len(hookSpecs) {
		t.Fatalf("installed = %v", got)
	}
	if c := adapter.HookChecks(doc, "claude", opts.HookCommand, hookSpecs); c.Status != adapter.OK {
		t.Errorf("check = %+v", c)
	}
	if fi, _ := os.Stat(settings); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings mode changed to %v", fi.Mode().Perm())
	}
	if err := a.Uninstall(t.Context(), os.Stderr); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(settings)
	var after map[string]any
	json.Unmarshal(b, &after)
	if after["theme"] != "dark" || strings.Contains(string(b), "memspace") || !strings.Contains(string(b), "say done") {
		t.Errorf("after uninstall: %s", b)
	}
}
