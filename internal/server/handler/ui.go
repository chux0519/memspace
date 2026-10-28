// Package handler — read endpoints behind the web UI: overview,
// project detail, single events, memories and client sync reports.
package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/memory"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/sessions"
	"github.com/chux0519/memspace/internal/summary"
)

// DeviceStatus is a device with its upload activity and last spool report.
type DeviceStatus struct {
	model.Device
	EventsTotal   int64      `json:"events_total"`
	Events24h     int64      `json:"events_24h"`
	SpoolPending  *int64     `json:"spool_pending,omitempty"`
	SpoolDead     *int64     `json:"spool_dead,omitempty"`
	SpoolDropped  *int64     `json:"spool_dropped,omitempty"`
	OldestPending *time.Time `json:"spool_oldest_pending_at,omitempty"`
	ReportedAt    *time.Time `json:"spool_reported_at,omitempty"`
	ClientVersion string     `json:"client_version,omitempty"`
	UpgradeTarget string     `json:"upgrade_target,omitempty"`
	UpgradeError  string     `json:"upgrade_error,omitempty"`
}

// UnfinishedSession is recent work that did not end cleanly.
type UnfinishedSession struct {
	Session    model.SessionInfo `json:"session"`
	Unresolved int               `json:"unresolved"`
	NextSteps  []string          `json:"next_steps"`
}

// Overview is the body of GET /api/v1/overview.
type Overview struct {
	Counts struct {
		Projects   int64 `json:"projects"`
		Devices    int64 `json:"devices"`
		Sessions7d int64 `json:"sessions_7d"`
		Events24h  int64 `json:"events_24h"`
		EventsAll  int64 `json:"events_total"`
		Memories   int64 `json:"memories"`
	} `json:"counts"`
	Devices        []DeviceStatus      `json:"devices"`
	RecentSessions []model.SessionInfo `json:"recent_sessions"`
	Unfinished     []UnfinishedSession `json:"unfinished"`
	LatestMemories []memory.Memory     `json:"latest_memories"`
	Embedding      embedding.Status    `json:"embedding"`
	Jobs           map[string]int64    `json:"jobs"`
}

// DeviceStatuses lists the owner's devices with activity and spool reports.
func DeviceStatuses(ctx context.Context, db *sql.DB, owner string) ([]DeviceStatus, error) {
	since := model.FormatTime(time.Now().Add(-24 * time.Hour))
	rows, err := db.QueryContext(ctx, `
		SELECT d.device_id, d.user_id, d.name, d.platform, d.created_at, d.last_seen_at, d.revoked_at,
			(SELECT COUNT(*) FROM events e WHERE e.device_id = d.device_id),
			(SELECT COUNT(*) FROM events e WHERE e.device_id = d.device_id AND e.received_at >= ?),
			d.spool_pending, d.spool_dead, d.spool_dropped, d.spool_oldest_pending_at, d.spool_reported_at,
			COALESCE(d.client_version, ''), COALESCE(d.upgrade_target, ''), COALESCE(d.upgrade_error, '')
		FROM devices d WHERE d.user_id = ? ORDER BY COALESCE(d.last_seen_at, d.created_at) DESC`, since, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceStatus{}
	for rows.Next() {
		var d DeviceStatus
		var created string
		var lastSeen, revoked, oldest, reported sql.NullString
		var pending, dead, dropped sql.NullInt64
		if err := rows.Scan(&d.DeviceID, &d.UserID, &d.Name, &d.Platform, &created, &lastSeen, &revoked,
			&d.EventsTotal, &d.Events24h, &pending, &dead, &dropped, &oldest, &reported, &d.ClientVersion,
			&d.UpgradeTarget, &d.UpgradeError); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		d.LastSeenAt, d.RevokedAt = parseNullTime(lastSeen), parseNullTime(revoked)
		d.OldestPending, d.ReportedAt = parseNullTime(oldest), parseNullTime(reported)
		if pending.Valid {
			d.SpoolPending, d.SpoolDead, d.SpoolDropped = &pending.Int64, &dead.Int64, &dropped.Int64
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// OverviewHandler handles GET /api/v1/overview.
func OverviewHandler(db *sql.DB, owner string, emb *embedding.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		var o Overview
		now := time.Now()
		err := db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM projects WHERE user_id = ?),
				(SELECT COUNT(*) FROM devices WHERE user_id = ? AND revoked_at IS NULL),
				(SELECT COUNT(*) FROM sessions s JOIN projects p ON p.project_id = s.project_id
					WHERE p.user_id = ? AND s.started_at >= ? AND `+sessions.HasPrompt+`),
				(SELECT COUNT(*) FROM events e JOIN projects p ON p.project_id = e.project_id
					WHERE p.user_id = ? AND e.received_at >= ?),
				(SELECT COUNT(*) FROM events e JOIN projects p ON p.project_id = e.project_id WHERE p.user_id = ?),
				(SELECT COUNT(*) FROM memories m JOIN projects p ON p.project_id = m.project_id WHERE p.user_id = ?)`,
			owner, owner, owner, model.FormatTime(now.Add(-7*24*time.Hour)), owner, model.FormatTime(now.Add(-24*time.Hour)), owner, owner,
		).Scan(&o.Counts.Projects, &o.Counts.Devices, &o.Counts.Sessions7d, &o.Counts.Events24h, &o.Counts.EventsAll, &o.Counts.Memories)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if o.Devices, err = DeviceStatuses(ctx, db, owner); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if o.RecentSessions, err = sessions.List(ctx, db, owner, sessions.Query{Limit: 10}); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if o.Unfinished, err = unfinished(ctx, db, owner, 6); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if o.LatestMemories, err = memory.List(ctx, db, owner, memory.Filter{Limit: 6}); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		o.Embedding = emb.Status(ctx)
		o.Jobs = map[string]int64{}
		rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
		if err == nil {
			for rows.Next() {
				var st string
				var n int64
				if rows.Scan(&st, &n) == nil {
					o.Jobs[st] = n
				}
			}
			rows.Close()
		}
		writeJSON(w, http.StatusOK, o)
	}
}

// unfinished lists recent sessions that did not end cleanly (still active,
// interrupted, unresolved failures or noted next steps). It reads stored
// summaries, which the summary job keeps current, instead of recomputing
// them: long sessions have thousands of events.
func unfinished(ctx context.Context, db *sql.DB, owner string, max int) ([]UnfinishedSession, error) {
	since := time.Now().Add(-72 * time.Hour)
	list, err := sessions.List(ctx, db, owner, sessions.Query{Since: &since, Limit: 30})
	if err != nil {
		return nil, err
	}
	out := []UnfinishedSession{}
	for i := range list {
		if list[i].EventCount == 0 {
			continue
		}
		s, ok, err := summary.Load(ctx, db, list[i].SessionID)
		if err != nil {
			return nil, err
		}
		u := UnfinishedSession{Session: list[i], NextSteps: []string{}}
		if ok {
			if s.Status == "ended" && len(s.Unresolved) == 0 && len(s.NextSteps) == 0 {
				continue
			}
			u.Unresolved, u.NextSteps = len(s.Unresolved), s.NextSteps
		} else if list[i].Status == "ended" {
			continue
		}
		out = append(out, u)
		if len(out) >= max {
			break
		}
	}
	return out, nil
}

// ProjectDetail is the body of GET /api/v1/projects/{id}.
type ProjectDetail struct {
	Project   model.Project       `json:"project"`
	Locations []ProjectLocation   `json:"locations"`
	Sessions  []model.SessionInfo `json:"sessions"`
	Stats     struct {
		Sessions int64      `json:"sessions"`
		Events   int64      `json:"events"`
		Memories int64      `json:"memories"`
		LastAt   *time.Time `json:"last_activity_at,omitempty"`
	} `json:"stats"`
}

// ProjectLocation is where a project lives on a device.
type ProjectLocation struct {
	DeviceID       string    `json:"device_id"`
	DeviceName     string    `json:"device_name"`
	AbsolutePath   string    `json:"absolute_path"`
	GitBranch      string    `json:"git_branch,omitempty"`
	GitHead        string    `json:"git_head,omitempty"`
	LastObservedAt time.Time `json:"last_observed_at"`
}

// ProjectHandler handles GET /api/v1/projects/{id}.
func ProjectHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := chi.URLParam(r, "id")
		var d ProjectDetail
		var git sql.NullString
		var created, updated string
		err := db.QueryRowContext(ctx, `SELECT project_id, user_id, name, canonical_git_identity, created_at, updated_at
			FROM projects WHERE project_id = ? AND user_id = ?`, id, owner).Scan(
			&d.Project.ProjectID, &d.Project.UserID, &d.Project.Name, &git, &created, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "unknown project")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if git.Valid {
			d.Project.CanonicalGitIdentity = &git.String
		}
		d.Project.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		d.Project.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)

		rows, err := db.QueryContext(ctx, `
			SELECT l.device_id, COALESCE(dv.name, ''), l.absolute_path, COALESCE(l.git_branch, ''), COALESCE(l.git_head, ''), l.last_observed_at
			FROM project_locations l LEFT JOIN devices dv ON dv.device_id = l.device_id
			WHERE l.project_id = ? ORDER BY l.last_observed_at DESC`, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		d.Locations = []ProjectLocation{}
		for rows.Next() {
			var l ProjectLocation
			var at string
			if rows.Scan(&l.DeviceID, &l.DeviceName, &l.AbsolutePath, &l.GitBranch, &l.GitHead, &at) == nil {
				l.LastObservedAt, _ = time.Parse(time.RFC3339Nano, at)
				d.Locations = append(d.Locations, l)
			}
		}
		rows.Close()

		var last sql.NullString
		db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sessions s WHERE s.project_id = ? AND `+sessions.HasPrompt+`),
			(SELECT COUNT(*) FROM events WHERE project_id = ?), (SELECT COUNT(*) FROM memories WHERE project_id = ?),
			(SELECT MAX(occurred_at) FROM events WHERE project_id = ?)`, id, id, id, id).Scan(
			&d.Stats.Sessions, &d.Stats.Events, &d.Stats.Memories, &last)
		d.Stats.LastAt = parseNullTime(last)
		if d.Sessions, err = sessions.List(ctx, db, owner, sessions.Query{ProjectIDs: []string{id}, Limit: 50}); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// EventDetail is the body of GET /api/v1/events/{id}.
type EventDetail struct {
	Event   model.EventRecord  `json:"event"`
	Session *model.SessionInfo `json:"session,omitempty"`
}

// EventHandler handles GET /api/v1/events/{id}.
func EventHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var d EventDetail
		var at, payload string
		var sessionID sql.NullString
		err := db.QueryRowContext(r.Context(), `
			SELECT e.event_id, e.event_type, e.occurred_at, e.payload_json, e.session_id
			FROM events e JOIN projects p ON p.project_id = e.project_id
			WHERE e.event_id = ? AND p.user_id = ?`, chi.URLParam(r, "id"), owner).Scan(
			&d.Event.EventID, &d.Event.EventType, &at, &payload, &sessionID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "unknown event")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		d.Event.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
		d.Event.Payload = json.RawMessage(payload)
		if sessionID.Valid {
			if d.Session, err = sessions.Get(r.Context(), db, owner, sessionID.String); err != nil {
				writeError(w, http.StatusInternalServerError, "query failed")
				return
			}
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// MemoriesHandler handles GET /api/v1/memories.
func MemoriesHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		f := memory.Filter{ProjectID: v.Get("project_id"), Type: v.Get("type"), Status: v.Get("status")}
		f.Limit, _ = strconv.Atoi(v.Get("limit"))
		if b := v.Get("before"); b != "" {
			t, err := time.Parse(time.RFC3339Nano, b)
			if err != nil {
				writeError(w, http.StatusBadRequest, "before must be RFC 3339")
				return
			}
			f.Before, f.BeforeID = &t, v.Get("before_id")
		}
		list, err := memory.List(r.Context(), db, owner, f)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, list)
	}
}

// SourceInfo describes a memory's source for display.
type SourceInfo struct {
	memory.SourceRef
	EventType  string    `json:"event_type,omitempty"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	Missing    bool      `json:"missing,omitempty"`
}

// MemoryDetail is the body of GET /api/v1/memories/{id}.
type MemoryDetail struct {
	Memory  memory.Memory `json:"memory"`
	Sources []SourceInfo  `json:"sources"`
}

// MemoryHandler handles GET /api/v1/memories/{id}.
func MemoryHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		m, err := memory.Get(ctx, db, owner, chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if m == nil {
			writeError(w, http.StatusNotFound, "unknown memory")
			return
		}
		d := MemoryDetail{Memory: *m, Sources: []SourceInfo{}}
		for _, ref := range m.SourceRefs {
			si := SourceInfo{SourceRef: ref}
			switch ref.Kind {
			case "event":
				var at, payload string
				var sid sql.NullString
				err := db.QueryRowContext(ctx, `SELECT event_type, occurred_at, payload_json, session_id FROM events WHERE event_id = ?`, ref.ID).
					Scan(&si.EventType, &at, &payload, &sid)
				if err != nil {
					si.Missing = true
					break
				}
				si.OccurredAt, _ = time.Parse(time.RFC3339Nano, at)
				si.Summary = describePayload(payload)
				si.SessionID = sid.String
			case "session":
				if s, err := sessions.Get(ctx, db, owner, ref.ID); err != nil || s == nil {
					si.Missing = true
				} else {
					si.SessionID, si.OccurredAt, si.Summary = s.SessionID, s.StartedAt, s.AgentType+" on "+s.DeviceName
				}
			}
			d.Sources = append(d.Sources, si)
		}
		writeJSON(w, http.StatusOK, d)
	}
}

// describePayload renders the gist of an event payload in one line.
func describePayload(raw string) string {
	var p map[string]any
	json.Unmarshal([]byte(raw), &p)
	input, _ := p["tool_input"].(map[string]any)
	for _, v := range []any{p["prompt"], p["last_assistant_message"], input["command"], p["path"], p["error"], p["tool_name"]} {
		if s, ok := v.(string); ok && s != "" {
			if len([]rune(s)) > 200 {
				s = string([]rune(s)[:200]) + "…"
			}
			return s
		}
	}
	return ""
}

// SyncReportHandler handles POST /api/v1/sync/report.
func SyncReportHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, _ := auth.DeviceIDFromContext(r.Context())
		var rep model.SyncReport
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&rep); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var oldest any
		if rep.OldestPending != nil {
			oldest = model.FormatTime(*rep.OldestPending)
		}
		rep.ClientVersion = clip(rep.ClientVersion, 64)
		rep.UpgradeTarget = clip(rep.UpgradeTarget, 64)
		rep.UpgradeError = clip(rep.UpgradeError, 500)
		if _, err := db.ExecContext(r.Context(), `
			UPDATE devices SET spool_pending = ?, spool_dead = ?, spool_dropped = ?, spool_oldest_pending_at = ?,
				spool_reported_at = ?, client_version = ?, upgrade_target = ?, upgrade_error = ? WHERE device_id = ?`,
			rep.Pending, rep.Dead, rep.Dropped, oldest, nowString(), rep.ClientVersion,
			rep.UpgradeTarget, rep.UpgradeError, deviceID); err != nil {
			writeError(w, http.StatusInternalServerError, "update failed")
			return
		}
		writeJSON(w, http.StatusOK, model.SyncReportResponse{ServerVersion: model.BuildVersion()})
	}
}

// LoginLinkResponse carries a new login code (shown once).
type LoginLinkResponse struct {
	Code      string    `json:"code"`
	Path      string    `json:"path"` // append to the server URL
	ExpiresAt time.Time `json:"expires_at"`
}

// LoginPath is the UI route that redeems a login code.
func LoginPath(code string) string { return "/#/login/" + code }

// CreateLoginLink handles POST /api/v1/ui/login-links (device token).
func CreateLoginLink(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, _ := auth.DeviceIDFromContext(r.Context())
		code, err := auth.CreateLoginCode(r.Context(), db, owner, deviceID, auth.RoleViewer)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create login link")
			return
		}
		writeJSON(w, http.StatusCreated, LoginLinkResponse{Code: code, Path: LoginPath(code), ExpiresAt: time.Now().Add(auth.LoginLinkTTL).UTC()})
	}
}

// UILogin handles POST /api/v1/ui/login: {"code": "..."} → session cookie.
func UILogin(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(auth.UIHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+auth.UIHeader+" header")
			return
		}
		var req struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Code == "" {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		secret, err := auth.RedeemLoginCode(r.Context(), db, req.Code, r.UserAgent())
		if errors.Is(err, auth.ErrInvalidLogin) {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "login failed")
			return
		}
		auth.SetUICookie(w, r, secret)
		w.WriteHeader(http.StatusNoContent)
	}
}

// UILogout handles POST /api/v1/ui/logout.
func UILogout(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(auth.UIHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+auth.UIHeader+" header")
			return
		}
		if s := auth.UISecret(r); s != "" {
			if err := auth.RevokeUISession(r.Context(), db, s); err != nil {
				writeError(w, http.StatusInternalServerError, "logout failed")
				return
			}
		}
		auth.SetUICookie(w, r, "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// clip shortens s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
