// Package sessions is the business layer for sessions: listing, details
// with summaries, and event timelines. The HTTP API, MCP tools and context
// builder all use it, so they apply the same ownership rules.
package sessions

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/summary"
)

func parseNullTime(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// HasPrompt is the SQL condition (on sessions aliased "s") for a session the
// user actually talked in. Sessions without a prompt (an agent opened and
// closed, or a CLI subcommand such as `claude mcp list` that fires
// SessionEnd) are kept but not listed, summarized or offered as context.
const HasPrompt = `EXISTS (SELECT 1 FROM events ep WHERE ep.session_id = s.session_id AND ep.event_type = 'user.prompt')`

// Query filters List.
type Query struct {
	ProjectIDs []string // empty: all of the owner's projects
	AgentType  string
	DeviceID   string
	Status     string
	ExcludeID  string
	Since      *time.Time
	// Before/BeforeID page backwards: sessions whose last activity is before
	// Before, or equal to it with a smaller session id (the last row of the
	// previous page).
	Before   *time.Time
	BeforeID string
	Limit    int
	// IncludeEmpty also lists sessions without a user prompt.
	IncludeEmpty bool
}

const sessionColumns = `
	s.session_id, s.project_id, p.name, s.device_id, COALESCE(d.name, ''), s.agent_type, s.native_session_id,
	s.status, s.started_at, s.ended_at,
	(SELECT MAX(occurred_at) FROM events e WHERE e.session_id = s.session_id) AS last_activity,
	(SELECT COUNT(*) FROM events e WHERE e.session_id = s.session_id),
	COALESCE(ss.objective, ''), COALESCE(ss.latest_request, '')`

const sessionFrom = `
	FROM sessions s
	JOIN projects p ON p.project_id = s.project_id
	LEFT JOIN devices d ON d.device_id = s.device_id
	LEFT JOIN session_summaries ss ON ss.session_id = s.session_id`

func scanSession(sc interface{ Scan(...any) error }) (model.SessionInfo, error) {
	var si model.SessionInfo
	var started string
	var ended, last sql.NullString
	err := sc.Scan(&si.SessionID, &si.ProjectID, &si.ProjectName, &si.DeviceID, &si.DeviceName, &si.AgentType,
		&si.NativeSessionID, &si.Status, &started, &ended, &last, &si.EventCount, &si.Objective, &si.LatestRequest)
	if err != nil {
		return si, err
	}
	si.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	si.EndedAt = parseNullTime(ended)
	si.LastEventAt = parseNullTime(last)
	return si, nil
}

// List returns the owner's sessions, most recently active first.
func List(ctx context.Context, db *sql.DB, owner string, q Query) ([]model.SessionInfo, error) {
	where := []string{"p.user_id = ?"}
	args := []any{owner}
	if !q.IncludeEmpty {
		where = append(where, HasPrompt)
	}
	if len(q.ProjectIDs) > 0 {
		where = append(where, "s.project_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.ProjectIDs)), ",")+")")
		for _, id := range q.ProjectIDs {
			args = append(args, id)
		}
	}
	for col, v := range map[string]string{"s.agent_type": q.AgentType, "s.device_id": q.DeviceID, "s.status": q.Status} {
		if v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	if q.ExcludeID != "" {
		where = append(where, "s.session_id <> ?")
		args = append(args, q.ExcludeID)
	}
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query := `SELECT * FROM (SELECT ` + sessionColumns + sessionFrom + ` WHERE ` + strings.Join(where, " AND ") + `) AS x`
	var outer []string
	if q.Since != nil {
		outer = append(outer, `x.last_activity >= ?`)
		args = append(args, model.FormatTime(*q.Since))
	}
	if q.Before != nil {
		b := model.FormatTime(*q.Before)
		outer = append(outer, `(COALESCE(x.last_activity, x.started_at) < ? OR (COALESCE(x.last_activity, x.started_at) = ? AND x.session_id < ?))`)
		args = append(args, b, b, q.BeforeID)
	}
	if len(outer) > 0 {
		query += ` WHERE ` + strings.Join(outer, " AND ")
	}
	query += ` ORDER BY COALESCE(x.last_activity, x.started_at) DESC, x.session_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SessionInfo{}
	for rows.Next() {
		si, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err := fillRequests(ctx, db, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillRequests gives a session whose summary is not generated yet its
// objective and latest request from the prompts.
func fillRequests(ctx context.Context, db *sql.DB, si *model.SessionInfo) error {
	if si.Objective != "" {
		return nil
	}
	var err error
	si.Objective, si.LatestRequest, err = summary.Requests(ctx, db, si.SessionID)
	return err
}

// Get returns one of the owner's sessions.
func Get(ctx context.Context, db *sql.DB, owner, sessionID string) (*model.SessionInfo, error) {
	si, err := scanSession(db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+sessionFrom+` WHERE s.session_id = ? AND p.user_id = ?`, sessionID, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := fillRequests(ctx, db, &si); err != nil {
		return nil, err
	}
	return &si, nil
}

// Summary returns the stored summary, recomputed when the session has
// events newer than it (a summary job is still pending).
func Summary(ctx context.Context, db *sql.DB, si *model.SessionInfo) (*summary.Summary, error) {
	s, ok, err := summary.Load(ctx, db, si.SessionID)
	if err != nil {
		return nil, err
	}
	if ok && s.EventCount >= si.EventCount {
		return s, nil
	}
	fresh, _, err := summary.Compute(ctx, db, si.SessionID)
	return fresh, err
}

// Events returns up to limit events after the (occurred_at, event_id)
// position.
func Events(ctx context.Context, db *sql.DB, sessionID, afterAt, afterID string, limit int) (*model.EventPage, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT event_id, event_type, occurred_at, payload_json FROM events
		WHERE session_id = ? AND (occurred_at > ? OR (occurred_at = ? AND event_id > ?))
		ORDER BY occurred_at, event_id LIMIT ?`,
		sessionID, afterAt, afterAt, afterID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &model.EventPage{Events: []model.EventRecord{}}
	var lastAt string
	for rows.Next() {
		var e model.EventRecord
		var at, payload string
		if err := rows.Scan(&e.EventID, &e.EventType, &at, &payload); err != nil {
			return nil, err
		}
		if len(page.Events) == limit {
			page.Next = base64.RawURLEncoding.EncodeToString([]byte(lastAt + "|" + page.Events[limit-1].EventID))
			break
		}
		e.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
		e.Payload = json.RawMessage(payload)
		page.Events = append(page.Events, e)
		lastAt = at
	}
	return page, rows.Err()
}
