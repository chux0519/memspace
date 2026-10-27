package summary

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type seq struct {
	t      time.Time
	events []Event
}

func (s *seq) add(typ string, p map[string]any) string {
	s.t = s.t.Add(time.Minute)
	b, _ := json.Marshal(p)
	id := "e" + string(rune('a'+len(s.events)))
	s.events = append(s.events, Event{EventID: id, EventType: typ, OccurredAt: s.t, Payload: b})
	return id
}

func shell(cmd string) map[string]any {
	return map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": cmd}}
}

func with(m map[string]any, k string, v any) map[string]any { m[k] = v; return m }

func TestBuildClaudeSession(t *testing.T) {
	s := &seq{t: time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)}
	s.add("session.started", map[string]any{"source": "startup"})
	first := s.add("user.prompt", map[string]any{"prompt": "重构认证模块，把 JWT 刷新逻辑拆出去"})
	s.add("tool.completed", map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": "/r/auth.go"}})
	s.add("file.modified", map[string]any{"path": "/r/auth/refresh.go", "tool_name": "Write"})
	s.add("file.modified", map[string]any{"path": "/r/auth/refresh.go", "tool_name": "Edit"})
	s.add("file.modified", map[string]any{"path": "/r/auth/session.go", "tool_name": "Edit"})
	failedTest := s.add("tool.failed", with(shell("go test ./internal/auth/..."), "error", "--- FAIL: TestRefresh\nexpected 200 got 401"))
	lint := s.add("tool.failed", with(shell("golangci-lint run ./..."), "error", "refresh.go:12: unused variable"))
	s.add("tool.failed", with(with(shell("sleep 100"), "error", "interrupted"), "is_interrupt", true))
	s.add("user.prompt", map[string]any{"prompt": "继续"})
	passed := s.add("tool.completed", shell("go test ./internal/auth/..."))
	latest := s.add("user.prompt", map[string]any{"prompt": "再补一下刷新令牌过期的测试用例"})
	reply := s.add("agent.turn_completed", map[string]any{"last_assistant_message": "已拆分刷新逻辑到 refresh.go，认证测试通过。\n\n下一步：\n- 补充刷新令牌过期的集成测试\n- 修复 lint 报错\n\n其他都完成了。"})

	got := Build(s.events, s.t)
	if got.Objective != "重构认证模块，把 JWT 刷新逻辑拆出去" || got.LatestRequest != "再补一下刷新令牌过期的测试用例" {
		t.Errorf("objective/latest = %q / %q", got.Objective, got.LatestRequest)
	}
	if got.Status != "active" || got.EventCount != len(s.events) {
		t.Errorf("status/count = %s/%d", got.Status, got.EventCount)
	}
	if strings.Join(got.ChangedFiles, ",") != "/r/auth/refresh.go,/r/auth/session.go" {
		t.Errorf("files = %v", got.ChangedFiles)
	}
	if len(got.TestResults) != 1 || !got.TestResults[0].Passed || got.TestResults[0].EventID != passed {
		t.Errorf("tests = %+v (failed run was %s)", got.TestResults, failedTest)
	}
	if len(got.Unresolved) != 1 || got.Unresolved[0].EventID != lint || !strings.Contains(got.Unresolved[0].Error, "unused variable") {
		t.Errorf("unresolved = %+v", got.Unresolved)
	}
	if strings.Join(got.NextSteps, "|") != "补充刷新令牌过期的集成测试|修复 lint 报错" {
		t.Errorf("next steps = %q", got.NextSteps)
	}
	if !strings.HasPrefix(got.CompletedWork, "已拆分刷新逻辑") {
		t.Errorf("completed = %q", got.CompletedWork)
	}
	for _, id := range []string{first, passed, latest, reply, lint} {
		if !strings.Contains(strings.Join(got.Evidence, ","), id) {
			t.Errorf("evidence lacks %s: %v", id, got.Evidence)
		}
	}
	if got.Disclaimer == "" || got.Generator != Generator {
		t.Error("missing provenance")
	}
}

// Codex: exit codes are explicit; a non-zero exit is never reported as a pass.
func TestBuildCodexExitCodes(t *testing.T) {
	s := &seq{t: time.Now()}
	s.add("user.prompt", map[string]any{"prompt": "run the test suite please"})
	s.add("tool.completed", map[string]any{"tool_name": "shell", "tool_input": map[string]any{"command": "pytest -q"}, "exit_code": 0.0})
	s.add("tool.failed", map[string]any{"tool_name": "shell", "tool_input": map[string]any{"command": "npm test"}, "exit_code": 1.0, "error": "1 failing"})
	s.add("agent.turn_aborted", map[string]any{"reason": "interrupted"})
	got := Build(s.events, s.t)
	if len(got.TestResults) != 2 || !got.TestResults[0].Passed || got.TestResults[1].Passed || *got.TestResults[1].ExitCode != 1 {
		t.Errorf("tests = %+v", got.TestResults)
	}
	if got.Status != "interrupted" {
		t.Errorf("status = %s", got.Status)
	}
	if len(got.NextSteps) != 0 || got.CompletedWork != "" {
		t.Errorf("no reply should mean no completed work: %+v", got)
	}
}

func TestFixes(t *testing.T) {
	s := &seq{t: time.Now()}
	s.add("user.prompt", map[string]any{"prompt": "fix the failing auth tests"})
	failed := s.add("tool.failed", with(shell("go test ./auth/..."), "error", "TestLogin: expected 200 got 401"))
	s.add("tool.failed", with(shell("go test ./auth/..."), "error", "TestLogin: still 401"))
	edit := s.add("file.modified", map[string]any{"path": "/r/auth/login.go", "tool_name": "Edit"})
	fixed := s.add("tool.completed", shell("go test ./auth/..."))
	// A flaky retry with no change in between is not a fix.
	s.add("tool.failed", with(shell("curl localhost:8080/health"), "error", "connection refused"))
	s.add("tool.completed", shell("curl localhost:8080/health"))
	got := Build(s.events, s.t)
	if len(got.Fixes) != 1 {
		t.Fatalf("fixes = %+v", got.Fixes)
	}
	f := got.Fixes[0]
	if f.FailedEventID != failed || f.FixedEventID != fixed || f.FileEventIDs[0] != edit || f.Files[0] != "/r/auth/login.go" || !strings.Contains(f.Error, "expected 200") {
		t.Errorf("fix = %+v", f)
	}
}

func TestNextStepsVariants(t *testing.T) {
	cases := map[string][]string{
		"Done.\n\nNext steps:\n1. Add retries\n2. Document the API\n\nThanks": {"Add retries", "Document the API"},
		"实现完成。还需要补充 Windows 上的集成测试。":                                          {"实现完成。还需要补充 Windows 上的集成测试。"},
		"All good, nothing left.":                          nil,
		"- TODO: handle empty input\n- shipped the parser": {"handle empty input"},
	}
	for in, want := range cases {
		if got := nextSteps(in); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestEmptyAndEnded(t *testing.T) {
	if got := Build(nil, time.Now()); got.EventCount != 0 || got.ChangedFiles == nil {
		t.Errorf("empty: %+v", got)
	}
	s := &seq{t: time.Now()}
	s.add("user.prompt", map[string]any{"prompt": "ok"})
	s.add("session.ended", map[string]any{"reason": "exit"})
	got := Build(s.events, s.t)
	if got.Status != "ended" || got.Objective != "" {
		t.Errorf("ended: %+v", got)
	}
}
