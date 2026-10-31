package summary

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/jobs"
	"github.com/chux0519/memspace/internal/memory"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
)

// JobKind is the job queue kind for summarization.
const JobKind = "session.summarize"

// Debounce: summarize once a session has been quiet for IdleDelay, but at
// least every MaxDelay while it stays busy.
var (
	IdleDelay = 3 * time.Minute
	MaxDelay  = 10 * time.Minute
)

type jobPayload struct {
	SessionID string `json:"session_id"`
}

// Enqueue schedules (re)summarization of a session; immediate when it ended.
func Enqueue(ctx context.Context, x jobs.Execer, sessionID string, ended bool) error {
	b, _ := json.Marshal(jobPayload{SessionID: sessionID})
	runAfter := time.Now().UTC().Add(IdleDelay)
	if ended {
		runAfter = time.Now().UTC()
	}
	return jobs.Enqueue(ctx, x, JobKind, JobKind+":"+sessionID, string(b), runAfter, MaxDelay)
}

// Handler is the job handler for JobKind.
func Handler(db *sql.DB) jobs.Handler {
	return func(ctx context.Context, payload string) error {
		var p jobPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil || p.SessionID == "" {
			return fmt.Errorf("summary job: bad payload %q", payload)
		}
		_, err := Generate(ctx, db, p.SessionID)
		return err
	}
}

// ErrNoSession means the session does not exist.
var ErrNoSession = errors.New("summary: no such session")

// Compute builds a fresh summary for a session without storing it.
func Compute(ctx context.Context, db *sql.DB, sessionID string) (*Summary, string, error) {
	var projectID, status string
	err := db.QueryRowContext(ctx, `SELECT project_id, status FROM sessions WHERE session_id = ?`, sessionID).Scan(&projectID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNoSession
	}
	if err != nil {
		return nil, "", err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT event_id, event_type, occurred_at, payload_json FROM events
		WHERE session_id = ? ORDER BY occurred_at, event_id`, sessionID)
	if err != nil {
		return nil, "", err
	}
	var evs []Event
	for rows.Next() {
		var ev Event
		var at, payload string
		if err := rows.Scan(&ev.EventID, &ev.EventType, &at, &payload); err != nil {
			rows.Close()
			return nil, "", err
		}
		ev.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
		ev.Payload = []byte(payload)
		evs = append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	s := Build(evs, time.Now())
	if status == "ended" {
		s.Status = "ended"
	}
	return &s, projectID, nil
}

// Generate computes, stores and indexes a session's summary.
func Generate(ctx context.Context, db *sql.DB, sessionID string) (*Summary, error) {
	s, projectID, err := Compute(ctx, db, sessionID)
	if err != nil {
		return nil, err
	}
	if s.EventCount == 0 {
		return s, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// A session nobody talked in (opened and closed, or a CLI subcommand
	// firing SessionEnd) gets no summary: an empty one only adds noise to
	// search and context. Drop one stored before this rule.
	var prompted bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM events WHERE session_id = ? AND event_type = 'user.prompt')`,
		sessionID).Scan(&prompted); err != nil {
		return nil, err
	}
	if !prompted {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_summaries WHERE session_id = ?`, sessionID); err != nil {
			return nil, err
		}
		if err := search.Delete(ctx, tx, search.KindSummary, sessionID); err != nil {
			return nil, err
		}
		return s, tx.Commit()
	}
	j := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_summaries (session_id, project_id, status, objective, latest_request, completed_work,
			changed_files, test_results, unresolved_issues, next_steps, evidence, event_count,
			first_event_at, last_event_at, generator, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id) DO UPDATE SET
			project_id = excluded.project_id, status = excluded.status, objective = excluded.objective,
			latest_request = excluded.latest_request, completed_work = excluded.completed_work,
			changed_files = excluded.changed_files, test_results = excluded.test_results,
			unresolved_issues = excluded.unresolved_issues, next_steps = excluded.next_steps,
			evidence = excluded.evidence, event_count = excluded.event_count,
			first_event_at = excluded.first_event_at, last_event_at = excluded.last_event_at,
			generator = excluded.generator, generated_at = excluded.generated_at`,
		sessionID, projectID, s.Status, s.Objective, s.LatestRequest, s.CompletedWork,
		j(s.ChangedFiles), j(s.TestResults), j(s.Unresolved), j(s.NextSteps), j(s.Evidence), s.EventCount,
		model.FormatTime(s.FirstEventAt), model.FormatTime(s.LastEventAt), s.Generator, model.FormatTime(s.GeneratedAt),
	); err != nil {
		return nil, fmt.Errorf("summary: store: %w", err)
	}
	var deviceID, agentType string
	tx.QueryRowContext(ctx, `SELECT device_id, agent_type FROM sessions WHERE session_id = ?`, sessionID).Scan(&deviceID, &agentType)
	if err := search.Upsert(ctx, tx, &search.Doc{
		Kind: search.KindSummary, RefID: sessionID, ProjectID: projectID, SessionID: sessionID,
		DeviceID: deviceID, AgentType: agentType, OccurredAt: s.LastEventAt,
		Title: s.Objective, Body: searchBody(s), Files: strings.Join(s.ChangedFiles, "\n"),
	}); err != nil {
		return nil, err
	}
	sources := []evidence.SourceRef{{Kind: "session", ID: sessionID}}
	for _, id := range s.Evidence {
		sources = append(sources, evidence.SourceRef{Kind: "event", ID: id})
	}
	if err := evidence.RecordDerived(ctx, tx, evidence.Derived{Kind: "summary_version", Ref: sessionID, Project: projectID, Session: sessionID, Title: s.Objective, Content: searchBody(s), Sources: sources, At: s.LastEventAt}); err != nil {
		return nil, err
	}
	for _, m := range Memories(sessionID, projectID, s) {
		if err := memory.Upsert(ctx, tx, &m, s.LastEventAt); err != nil {
			return nil, err
		}
	}
	return s, tx.Commit()
}

// Memories derives rule-based memories from a summary:
// one episodic memory for the session, and a procedural memory per fix.
func Memories(sessionID, projectID string, s *Summary) []memory.Memory {
	var out []memory.Memory
	if s.Objective != "" || s.CompletedWork != "" {
		title := s.Objective
		if title == "" {
			title = "Session " + s.FirstEventAt.Format("2006-01-02 15:04")
		}
		refs := []memory.SourceRef{{Kind: "session", ID: sessionID}}
		for _, id := range s.Evidence {
			refs = append(refs, memory.SourceRef{Kind: "event", ID: id})
		}
		out = append(out, memory.Memory{
			DedupKey: "episodic:" + sessionID, ProjectID: projectID, SessionID: sessionID,
			Type: memory.TypeEpisodic, Title: oneLine(redact.Truncate(title, 160)), Content: episodicText(s),
			Confidence: 0.6, SourceRefs: refs, Generator: Generator,
		})
	}
	for _, f := range s.Fixes {
		refs := []memory.SourceRef{{Kind: "session", ID: sessionID}, {Kind: "event", ID: f.FailedEventID}}
		for _, id := range f.FileEventIDs {
			refs = append(refs, memory.SourceRef{Kind: "event", ID: id})
		}
		refs = append(refs, memory.SourceRef{Kind: "event", ID: f.FixedEventID})
		var b strings.Builder
		fmt.Fprintf(&b, "Failed: %s\n%s\n\nChanged before it passed:\n", f.Title, f.Error)
		for _, file := range f.Files {
			fmt.Fprintf(&b, "- %s\n", file)
		}
		b.WriteString("\nThe same operation then succeeded. (Inferred from the event sequence; the change is a lead, not a verified cause.)")
		out = append(out, memory.Memory{
			DedupKey: "fix:" + sessionID + ":" + f.FailedEventID, ProjectID: projectID, SessionID: sessionID,
			Type: memory.TypeProcedural, Title: oneLine(redact.Truncate("Fixed: "+f.Title, 160)), Content: b.String(),
			Confidence: 0.5, SourceRefs: refs, Generator: Generator,
		})
	}
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func episodicText(s *Summary) string {
	var b strings.Builder
	field := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", label, v)
		}
	}
	field("Objective", s.Objective)
	if s.LatestRequest != s.Objective {
		field("Latest request", s.LatestRequest)
	}
	field("Outcome (agent's report)", s.CompletedWork)
	if len(s.ChangedFiles) > 0 {
		field("Changed files", strings.Join(s.ChangedFiles, ", "))
	}
	for _, t := range s.TestResults {
		state := "passed"
		if !t.Passed {
			state = "failed"
		}
		field("Test", t.Command+" → "+state)
	}
	for _, i := range s.Unresolved {
		field("Unresolved", i.Title+": "+i.Error)
	}
	for _, n := range s.NextSteps {
		field("Next step", n)
	}
	fmt.Fprintf(&b, "Status: %s. %s", s.Status, s.Disclaimer)
	return b.String()
}

func searchBody(s *Summary) string {
	var b strings.Builder
	for _, part := range []string{s.LatestRequest, s.CompletedWork} {
		if part != "" {
			b.WriteString(part + "\n")
		}
	}
	for _, i := range s.Unresolved {
		b.WriteString(i.Title + " " + i.Error + "\n")
	}
	for _, n := range s.NextSteps {
		b.WriteString(n + "\n")
	}
	for _, t := range s.TestResults {
		b.WriteString(t.Command + "\n")
	}
	return b.String()
}

// Load returns a stored summary.
func Load(ctx context.Context, db *sql.DB, sessionID string) (*Summary, bool, error) {
	var s Summary
	var files, tests, issues, next, evidence, first, last, generated string
	err := db.QueryRowContext(ctx, `
		SELECT status, objective, latest_request, completed_work, changed_files, test_results, unresolved_issues,
			next_steps, evidence, event_count, first_event_at, last_event_at, generator, generated_at
		FROM session_summaries WHERE session_id = ?`, sessionID).Scan(
		&s.Status, &s.Objective, &s.LatestRequest, &s.CompletedWork, &files, &tests, &issues,
		&next, &evidence, &s.EventCount, &first, &last, &s.Generator, &generated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	json.Unmarshal([]byte(files), &s.ChangedFiles)
	json.Unmarshal([]byte(tests), &s.TestResults)
	json.Unmarshal([]byte(issues), &s.Unresolved)
	json.Unmarshal([]byte(next), &s.NextSteps)
	json.Unmarshal([]byte(evidence), &s.Evidence)
	s.FirstEventAt, _ = time.Parse(time.RFC3339Nano, first)
	s.LastEventAt, _ = time.Parse(time.RFC3339Nano, last)
	s.GeneratedAt, _ = time.Parse(time.RFC3339Nano, generated)
	s.Disclaimer = Disclaimer
	s.normalize()
	return &s, true, nil
}

// Requests derives a session's objective and latest request from its prompts
// the way a summary does, for sessions whose summary is not generated yet
// (summaries wait until the session has been quiet for IdleDelay).
func Requests(ctx context.Context, db *sql.DB, sessionID string) (objective, latest string, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(payload_json, '$.prompt') FROM events
		WHERE session_id = ? AND event_type = 'user.prompt'
		ORDER BY occurred_at, sequence_number`, sessionID)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			return "", "", err
		}
		text := strings.TrimSpace(p.String)
		if !substantive(text) {
			continue
		}
		if objective == "" {
			objective = text
		}
		latest = text
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	return redact.Truncate(objective, maxText), redact.Truncate(latest, maxText), nil
}
