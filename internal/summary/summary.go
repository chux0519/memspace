// Package summary builds rule-based session summaries from a
// session's events. No LLM is involved; every claim is backed by an event,
// and a test is reported as passing only when an event shows it succeeded.
package summary

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/redact"
)

// Generator identifies the summarizer version stored with each summary.
const Generator = "rules-v1"

// Disclaimer accompanies every rendered summary.
const Disclaimer = "Auto-generated from recorded events; may be incomplete."

// Event is one session event, in occurrence order.
type Event struct {
	EventID    string
	EventType  string
	OccurredAt time.Time
	Payload    []byte
}

// TestResult is the latest outcome of one test command.
type TestResult struct {
	Command  string    `json:"command"`
	Passed   bool      `json:"passed"`
	ExitCode *int      `json:"exit_code,omitempty"`
	At       time.Time `json:"at"`
	EventID  string    `json:"event_id"`
}

// Issue is a failure that was not followed by a successful retry.
type Issue struct {
	Title   string    `json:"title"`
	Error   string    `json:"error"`
	At      time.Time `json:"at"`
	EventID string    `json:"event_id"`
}

// Fix is a failure that was resolved by changing files: the operation
// failed, files were modified, and the same operation then succeeded. A
// retry that succeeds without changes is not a fix (flaky, or transient).
type Fix struct {
	Title         string    `json:"title"`
	Error         string    `json:"error"`
	Files         []string  `json:"files"`
	FailedEventID string    `json:"failed_event_id"`
	FixedEventID  string    `json:"fixed_event_id"`
	FileEventIDs  []string  `json:"file_event_ids"`
	At            time.Time `json:"at"`
}

// Summary of one session.
type Summary struct {
	Status        string       `json:"status"` // active | ended | interrupted
	Objective     string       `json:"objective"`
	LatestRequest string       `json:"latest_request"`
	CompletedWork string       `json:"completed_work"`
	ChangedFiles  []string     `json:"changed_files"`
	TestResults   []TestResult `json:"test_results"`
	Unresolved    []Issue      `json:"unresolved_issues"`
	NextSteps     []string     `json:"next_steps"`
	Evidence      []string     `json:"evidence"`
	Fixes         []Fix        `json:"fixes,omitempty"`
	EventCount    int          `json:"event_count"`
	FirstEventAt  time.Time    `json:"first_event_at"`
	LastEventAt   time.Time    `json:"last_event_at"`
	Generator     string       `json:"generator"`
	GeneratedAt   time.Time    `json:"generated_at"`
	Disclaimer    string       `json:"disclaimer"`
}

const (
	maxFiles     = 50
	maxIssues    = 10
	maxNextSteps = 5
	maxText      = 1500
)

var (
	testCmdRE  = regexp.MustCompile(`(?i)(^|[\s;&|(])(go test|gotestsum|pytest|py\.test|python3? -m (pytest|unittest)|(npm|pnpm|yarn|bun)( run)? test|jest|vitest|mocha|cargo (test|nextest)|mvn( \S+)* test|gradle\S* test|make (test|check)|ctest|rspec|phpunit|dotnet test|tox|deno test|swift test|mix test)\b`)
	nextStepRE = regexp.MustCompile(`(?i)(下一步|接下来|后续|待办|待完成|还需要|尚未|未完成|仍需|TODO|next steps?|remaining|follow[- ]?up|still need|not yet)`)
	bulletRE   = regexp.MustCompile(`^\s*([-*•]|\d+[.)、])\s+`)
)

// normalize makes list fields empty rather than null in JSON.
func (s *Summary) normalize() {
	if s.ChangedFiles == nil {
		s.ChangedFiles = []string{}
	}
	if s.TestResults == nil {
		s.TestResults = []TestResult{}
	}
	if s.Unresolved == nil {
		s.Unresolved = []Issue{}
	}
	if s.NextSteps == nil {
		s.NextSteps = []string{}
	}
	if s.Evidence == nil {
		s.Evidence = []string{}
	}
}

// Build summarizes a session's events, which must be sorted by time.
func Build(events []Event, now time.Time) Summary {
	s := Summary{Status: "active", Generator: Generator, GeneratedAt: now.UTC(), Disclaimer: Disclaimer,
		ChangedFiles: []string{}, TestResults: []TestResult{}, Unresolved: []Issue{}, NextSteps: []string{}, Evidence: []string{}}
	if len(events) == 0 {
		return s
	}
	s.EventCount = len(events)
	s.FirstEventAt, s.LastEventAt = events[0].OccurredAt, events[len(events)-1].OccurredAt

	evidence := map[string]bool{}
	cite := func(id string) {
		if id != "" && !evidence[id] {
			evidence[id] = true
			s.Evidence = append(s.Evidence, id)
		}
	}
	seenFile := map[string]bool{}
	tests := map[string]int{}          // command → index in s.TestResults
	openFailures := map[string]Issue{} // retry key → latest unresolved failure
	firstFailure := map[string]Issue{} // retry key → first failure of the open streak
	type change struct{ files, eventIDs []string }
	changedSince := map[string]*change{} // retry key → files modified since its first failure
	var failureOrder []string
	var lastReplyID string

	for _, ev := range events {
		var p map[string]any
		json.Unmarshal(ev.Payload, &p)
		str := func(k string) string { v, _ := p[k].(string); return v }
		input, _ := p["tool_input"].(map[string]any)
		command, _ := input["command"].(string)
		command = strings.TrimSpace(command)

		switch ev.EventType {
		case "user.prompt":
			text := strings.TrimSpace(str("prompt"))
			if !substantive(text) {
				continue
			}
			if s.Objective == "" {
				s.Objective = redact.Truncate(text, maxText)
				cite(ev.EventID)
			}
			s.LatestRequest = redact.Truncate(text, maxText)
			cite(ev.EventID)
		case "agent.turn_completed":
			if msg := strings.TrimSpace(str("last_assistant_message")); msg != "" {
				s.CompletedWork = msg
				lastReplyID = ev.EventID
			}
		case "agent.turn_aborted":
			s.Status = "interrupted"
		case "session.ended":
			s.Status = "ended"
		case "file.modified":
			for _, c := range changedSince {
				c.files = append(c.files, filePaths(p, input)...)
				c.eventIDs = append(c.eventIDs, ev.EventID)
			}
			for _, f := range filePaths(p, input) {
				if !seenFile[f] && len(s.ChangedFiles) < maxFiles {
					seenFile[f] = true
					s.ChangedFiles = append(s.ChangedFiles, f)
					cite(ev.EventID)
				}
			}
		case "tool.completed", "tool.failed":
			key := retryKey(str("tool_name"), command, input)
			failed := ev.EventType == "tool.failed"
			if failed && p["is_interrupt"] == true {
				continue // the user stopped it; not a failure of the work
			}
			if testCmdRE.MatchString(command) {
				tr := TestResult{Command: redact.Truncate(command, 300), Passed: !failed, At: ev.OccurredAt, EventID: ev.EventID}
				if code, ok := p["exit_code"].(float64); ok {
					c := int(code)
					tr.ExitCode = &c
					tr.Passed = tr.Passed && c == 0
				}
				if i, ok := tests[tr.Command]; ok {
					s.TestResults[i] = tr
				} else {
					tests[tr.Command] = len(s.TestResults)
					s.TestResults = append(s.TestResults, tr)
				}
				cite(ev.EventID)
			}
			if failed {
				title := strings.TrimSpace(str("tool_name") + " " + firstLine(command))
				errText := str("error")
				if errText == "" {
					errText = str("status")
				}
				issue := Issue{Title: title, Error: redact.Truncate(strings.TrimSpace(errText), 500), At: ev.OccurredAt, EventID: ev.EventID}
				if _, open := openFailures[key]; !open {
					failureOrder = append(failureOrder, key)
					firstFailure[key] = issue
					changedSince[key] = &change{}
				}
				openFailures[key] = issue
			} else {
				if first, open := firstFailure[key]; open {
					if c := changedSince[key]; len(c.files) > 0 && len(s.Fixes) < maxIssues {
						s.Fixes = append(s.Fixes, Fix{Title: first.Title, Error: first.Error, Files: dedupe(c.files),
							FailedEventID: first.EventID, FixedEventID: ev.EventID, FileEventIDs: c.eventIDs, At: ev.OccurredAt})
					}
				}
				delete(openFailures, key)
				delete(firstFailure, key)
				delete(changedSince, key)
			}
		}
	}
	if lastReplyID != "" {
		s.NextSteps = nextSteps(s.CompletedWork)
		s.CompletedWork = redact.Truncate(s.CompletedWork, maxText)
		cite(lastReplyID)
	}
	// Newest unresolved failures first.
	for i := len(failureOrder) - 1; i >= 0 && len(s.Unresolved) < maxIssues; i-- {
		if issue, ok := openFailures[failureOrder[i]]; ok {
			s.Unresolved = append(s.Unresolved, issue)
			cite(issue.EventID)
		}
	}
	s.normalize()
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// substantive filters out prompts with no content of their own ("ok", "继续").
func substantive(s string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(s)) >= 4
}

// retryKey identifies "the same operation" so a later success resolves an
// earlier failure.
func retryKey(tool, command string, input map[string]any) string {
	if command != "" {
		return "cmd:" + strings.Join(strings.Fields(command), " ")
	}
	b, _ := json.Marshal(input)
	return "tool:" + tool + ":" + string(b)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return redact.Truncate(s, 200)
}

func filePaths(p, input map[string]any) []string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	add(p["path"])
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		add(input[k])
	}
	if files, ok := p["files"].([]any); ok {
		for _, f := range files {
			if m, ok := f.(map[string]any); ok {
				add(m["path"])
			}
		}
	}
	return out
}

// nextSteps extracts sentences or bullets about remaining work from the
// agent's final message. A heading such as "下一步：" or "Next steps:" makes
// the bullets under it count too.
func nextSteps(msg string) []string {
	var out []string
	underHeading := false
	for _, line := range strings.Split(msg, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			underHeading = false
			continue
		}
		isBullet := bulletRE.MatchString(t)
		switch {
		case nextStepRE.MatchString(t):
			if !isBullet && (strings.HasSuffix(t, ":") || strings.HasSuffix(t, "：")) {
				underHeading = true
				continue
			}
			// "…done. 下一步：fix X" → "fix X".
			if loc := nextStepRE.FindStringIndex(t); loc != nil {
				rest := strings.TrimLeft(t[loc[1]:], " ")
				if after, ok := cutColon(rest); ok && strings.TrimSpace(after) != "" {
					t = after
				}
			}
			out = append(out, clean(t))
		case underHeading && isBullet:
			out = append(out, clean(t))
		default:
			if !isBullet {
				underHeading = false
			}
		}
		if len(out) >= maxNextSteps {
			break
		}
	}
	return out
}

func cutColon(s string) (string, bool) {
	for _, c := range []string{"：", ":"} {
		if after, ok := strings.CutPrefix(s, c); ok {
			return strings.TrimSpace(after), true
		}
	}
	return s, false
}

func clean(s string) string {
	s = bulletRE.ReplaceAllString(s, "")
	s = strings.Trim(s, "*_ ")
	return redact.Truncate(s, 300)
}
