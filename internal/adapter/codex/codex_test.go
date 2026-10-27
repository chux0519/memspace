package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/spool"
)

type env struct {
	codexHome, stateDir, rollout string
	sp                           *spool.Spool
	fixture                      []byte
}

func setup(t *testing.T, limits spool.Limits) *env {
	t.Helper()
	e := &env{codexHome: t.TempDir(), stateDir: t.TempDir()}
	var err error
	e.fixture, err = os.ReadFile("testdata/rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.codexHome, "sessions", "2026", "10", "26")
	os.MkdirAll(dir, 0o755)
	e.rollout = filepath.Join(dir, "rollout-2026-10-26T10-36-16-01a17610-bd86-7172-86a8-399df8c1ead9.jsonl")
	if e.sp, err = spool.Open(t.TempDir(), limits); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) importer() *Importer { return &Importer{CodexHome: e.codexHome, StateDir: e.stateDir} }

func drain(t *testing.T, sp *spool.Spool) []*spool.Record {
	t.Helper()
	recs, err := sp.Claim(1000)
	if err != nil {
		t.Fatal(err)
	}
	sp.Ack(recs)
	return recs
}

func types(recs []*spool.Record) string {
	var s []string
	for _, r := range recs {
		s = append(s, r.EventType)
	}
	return strings.Join(s, " ")
}

// The fixture mirrors a real codex 0.163 rollout (exec session with a tool call).
func TestImportRollout(t *testing.T) {
	e := setup(t, spool.Limits{})
	os.WriteFile(e.rollout, e.fixture, 0o600)

	n, err := e.importer().Import(context.Background(), e.sp, nil)
	if err != nil {
		t.Fatal(err)
	}
	recs := drain(t, e.sp)
	want := "session.started user.prompt tool.completed tool.completed tool.failed file.modified agent.message agent.turn_completed"
	if got := types(recs); got != want || n != len(recs) {
		t.Fatalf("got %d: %s\nwant: %s", n, got, want)
	}
	for _, r := range recs {
		if r.Agent != AgentType || r.NativeSessionID != "01a17610-bd86-7172-86a8-399df8c1ead9" || r.Project.Root != "/tmp/work" {
			t.Errorf("identity: %+v", r)
		}
	}
	var p map[string]any
	json.Unmarshal(recs[2].Payload, &p)
	if p["tool_input"].(map[string]any)["command"] != "echo hi" || p["tool_response"] != "hi\n" {
		t.Errorf("command payload = %v", p)
	}
	if strings.Contains(string(recs[3].Payload), "OPENAI_KEY") {
		t.Errorf("output of `cat .env` captured: %s", recs[3].Payload)
	}
	if strings.Contains(string(recs[0].Payload), "You are Codex") {
		t.Errorf("base instructions captured")
	}

	// Nothing new: nothing imported.
	if n, _ := e.importer().Import(context.Background(), e.sp, nil); n != 0 {
		t.Errorf("re-import produced %d events", n)
	}
}

func TestIncrementalAppendAndPartialLine(t *testing.T) {
	e := setup(t, spool.Limits{})
	lines := bytes.SplitAfter(e.fixture, []byte("\n"))
	// First 6 lines, plus half of the 7th (still being written).
	head := bytes.Join(lines[:6], nil)
	half := lines[6][:len(lines[6])/2]
	os.WriteFile(e.rollout, append(append([]byte{}, head...), half...), 0o600)
	e.importer().Import(context.Background(), e.sp, nil)
	if got := types(drain(t, e.sp)); got != "session.started user.prompt" {
		t.Fatalf("first pass: %s", got)
	}

	os.WriteFile(e.rollout, e.fixture, 0o600)
	e.importer().Import(context.Background(), e.sp, nil)
	got := types(drain(t, e.sp))
	if strings.Contains(got, "session.started") || strings.Contains(got, "user.prompt") || !strings.HasSuffix(got, "agent.turn_completed") {
		t.Fatalf("second pass re-imported or missed lines: %s", got)
	}
}

func TestDeterministicIDsAcrossReset(t *testing.T) {
	e := setup(t, spool.Limits{})
	os.WriteFile(e.rollout, e.fixture, 0o600)
	e.importer().Import(context.Background(), e.sp, nil)
	first := drain(t, e.sp)

	im := e.importer()
	im.Reset = true // backfill re-reads everything
	im.Import(context.Background(), e.sp, nil)
	second := drain(t, e.sp)
	if len(first) != len(second) {
		t.Fatalf("%d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].EventID != second[i].EventID {
			t.Errorf("event %d id changed on re-import", i)
		}
	}
}

func TestSpoolFullResumesWithoutLoss(t *testing.T) {
	e := setup(t, spool.Limits{MaxEvents: 3})
	os.WriteFile(e.rollout, e.fixture, 0o600)
	var all []*spool.Record
	for round := 0; round < 10; round++ {
		_, err := e.importer().Import(context.Background(), e.sp, nil)
		recs := drain(t, e.sp)
		all = append(all, recs...)
		if err == nil {
			break
		}
		if !errors.Is(err, spool.ErrFull) {
			t.Fatal(err)
		}
		os.Remove(filepath.Join(e.sp.Dir(), "usage.json")) // force recount after draining
	}
	seen := map[string]bool{}
	for _, r := range all {
		seen[r.EventID] = true
	}
	if len(seen) != 8 {
		t.Errorf("imported %d distinct events across rounds, want 8: %s", len(seen), types(all))
	}
}

func TestSinceCutoff(t *testing.T) {
	e := setup(t, spool.Limits{})
	os.WriteFile(e.rollout, e.fixture, 0o600)
	im := e.importer()
	im.Since = time.Date(2026, 10, 26, 2, 36, 33, 0, time.UTC)
	im.Import(context.Background(), e.sp, nil)
	if got := types(drain(t, e.sp)); got != "agent.message agent.turn_completed" {
		t.Errorf("after cutoff: %s", got)
	}

	// Files untouched since the cutoff are not even opened.
	e2 := setup(t, spool.Limits{})
	os.WriteFile(e2.rollout, e2.fixture, 0o600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(e2.rollout, old, old)
	im = e2.importer()
	im.Since = time.Now().Add(-time.Hour)
	if n, _ := im.Import(context.Background(), e2.sp, nil); n != 0 {
		t.Errorf("imported %d from an old file", n)
	}
}

func TestNormalizeSessionEnd(t *testing.T) {
	a := &Adapter{}
	recs, err := a.Normalize([]byte(`{"session_id":"01a1","cwd":"/tmp","hook_event_name":"SessionEnd","reason":"exit"}`), time.Now())
	if err != nil || len(recs) != 1 || recs[0].EventType != "session.ended" {
		t.Fatalf("%v %v", recs, err)
	}
	// Stop carries nothing the rollout does not already have.
	recs, _ = a.Normalize([]byte(`{"session_id":"01a1","cwd":"/tmp","hook_event_name":"Stop","last_assistant_message":"DONE","transcript_path":"/x"}`), time.Now())
	if len(recs) != 0 {
		t.Errorf("Stop produced %v", recs)
	}
}

func TestUserMessageAttachments(t *testing.T) {
	item := map[string]any{"type": "UserMessage", "content": []any{
		map[string]any{"type": "text", "text": "看这张截图"},
		map[string]any{"type": "local_image", "path": "/tmp/shot.png"},
		map[string]any{"type": "image", "image_url": "data:image/png;base64,AAAA"},
	}}
	ev := mapItem(item, "t1")
	if len(ev) != 1 {
		t.Fatal(ev)
	}
	atts, _ := ev[0].payload["attachments"].([]prompt.Attachment)
	if len(atts) != 2 || atts[0].Ref != "/tmp/shot.png" || atts[1].Kind != "image" {
		t.Errorf("attachments = %+v", atts)
	}
	b, _ := json.Marshal(ev[0].payload)
	if strings.Contains(string(b), "base64") {
		t.Errorf("inline image data kept: %s", b)
	}
}
