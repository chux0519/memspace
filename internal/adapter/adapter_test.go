package adapter

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const userSettings = `{
  "model": "opus",
  "hooks": {
    "PostToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "~/bin/audit.sh"}]}
    ],
    "Stop": [
      {"hooks": [
        {"type": "command", "command": "notify-send done"},
        {"type": "command", "command": "/old/path/memspace hook claude"}
      ]}
    ]
  }
}`

var specs = []HookSpec{
	{Event: "PostToolUse", Matcher: "*", Timeout: 10},
	{Event: "Stop", Timeout: 10},
	{Event: "SessionEnd", Timeout: 10},
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func commands(doc map[string]any, event string) []string {
	var out []string
	for _, g := range hookGroups(doc, event) {
		for _, h := range groupHandlers(g) {
			out = append(out, handlerCommand(h))
		}
	}
	return out
}

func TestMergeHooksPreservesUserHooks(t *testing.T) {
	doc := parse(t, userSettings)
	cmd := `/usr/local/bin/memspace hook claude`
	if !MergeHooks(doc, "claude", cmd, specs) {
		t.Fatal("expected change")
	}
	if doc["model"] != "opus" {
		t.Error("unrelated setting lost")
	}
	if got := commands(doc, "PostToolUse"); !reflect.DeepEqual(got, []string{"~/bin/audit.sh", cmd}) {
		t.Errorf("PostToolUse = %v", got)
	}
	// The stale MemSpace handler is replaced; the user's handler in the same group stays.
	if got := commands(doc, "Stop"); !reflect.DeepEqual(got, []string{"notify-send done", cmd}) {
		t.Errorf("Stop = %v", got)
	}
	if got := commands(doc, "SessionEnd"); !reflect.DeepEqual(got, []string{cmd}) {
		t.Errorf("SessionEnd = %v", got)
	}
	// Idempotent.
	before, _ := json.Marshal(doc)
	if MergeHooks(doc, "claude", cmd, specs) {
		t.Error("second merge reported a change")
	}
	after, _ := json.Marshal(doc)
	if string(before) != string(after) {
		t.Error("second merge modified the document")
	}
	// Matcher only on tool events.
	g := hookGroups(doc, "PostToolUse")[1].(map[string]any)
	if g["matcher"] != "*" {
		t.Errorf("matcher = %v", g["matcher"])
	}
	if _, ok := hookGroups(doc, "SessionEnd")[0].(map[string]any)["matcher"]; ok {
		t.Error("non-tool event got a matcher")
	}
}

func TestRemoveHooksRestoresUserConfig(t *testing.T) {
	doc := parse(t, userSettings)
	MergeHooks(doc, "claude", "/usr/local/bin/memspace hook claude", specs)
	if !RemoveHooks(doc, "claude") {
		t.Fatal("expected change")
	}
	if got := commands(doc, "PostToolUse"); !reflect.DeepEqual(got, []string{"~/bin/audit.sh"}) {
		t.Errorf("PostToolUse = %v", got)
	}
	if got := commands(doc, "Stop"); !reflect.DeepEqual(got, []string{"notify-send done"}) {
		t.Errorf("Stop = %v", got)
	}
	if _, ok := doc["hooks"].(map[string]any)["SessionEnd"]; ok {
		t.Error("empty event left behind")
	}
	// Other agents' MemSpace hooks are untouched.
	doc = parse(t, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"memspace hook codex"}]}]}}`)
	if RemoveHooks(doc, "claude") {
		t.Error("removed another agent's hook")
	}
	if RemoveHooks(doc, "codex"); doc["hooks"] != nil {
		t.Errorf("empty hooks map left behind: %v", doc)
	}
}

func TestSaveJSONFileBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := SaveJSONFile(path, map[string]any{"a": 1.0}); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSONFile(path, map[string]any{"a": 2.0}); err != nil {
		t.Fatal(err)
	}
	cur, _, _ := LoadJSONFile(path)
	bak, _, _ := LoadJSONFile(path + ".memspace.bak")
	if cur["a"] != 2.0 || bak["a"] != 1.0 {
		t.Errorf("cur=%v bak=%v", cur, bak)
	}
	if doc, exists, err := LoadJSONFile(filepath.Join(t.TempDir(), "missing.json")); err != nil || exists || len(doc) != 0 {
		t.Errorf("missing file: %v %v %v", doc, exists, err)
	}
}

func TestHookCommandQuoting(t *testing.T) {
	got := HookCommand("/Users/Jane Doe/bin/memspace", "/tmp/ms home", "", "claude")
	want := `"/Users/Jane Doe/bin/memspace" --home "/tmp/ms home" hook claude`
	if got != want {
		t.Errorf("got %s", got)
	}
	if !IsMemSpaceHook(got, "claude") || IsMemSpaceHook(got, "codex") {
		t.Error("IsMemSpaceHook mismatch")
	}
	if HookCommand("/usr/bin/memspace", "", "", "codex") != "/usr/bin/memspace hook codex" {
		t.Error("plain path should not be quoted")
	}
}

func TestStableID(t *testing.T) {
	a, b := StableID("cc", "s", "x"), StableID("cc", "s", "x")
	if a != b || StableID("cc", "s", "y") == a || StableID("cc", "sx", "") == StableID("cc", "s", "x") {
		t.Error("StableID not deterministic/distinct")
	}
	if !strings.HasPrefix(RandomID("cc"), "cc_") || RandomID("cc") == RandomID("cc") {
		t.Error("RandomID")
	}
}

func TestMentionsSensitivePath(t *testing.T) {
	if !MentionsSensitivePath("cat .env | grep KEY") || !MentionsSensitivePath(`head -n 3 "config/server.pem"`) {
		t.Error("missed a sensitive path")
	}
	if MentionsSensitivePath("go test ./... && cat README.md") {
		t.Error("false positive")
	}
}
