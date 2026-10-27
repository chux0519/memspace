// Package handler — event batch ingestion and project/session management.
package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/summary"
	"github.com/google/uuid"
)

const (
	// MaxBatchEvents bounds events per batch request.
	MaxBatchEvents = 1000
	// MaxBatchBytes bounds the batch request body.
	MaxBatchBytes = 16 << 20
	// maxPayloadBytes bounds a single event payload after server redaction.
	maxPayloadBytes = 256 << 10
)

var (
	validEventID   = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,128}$`)
	validEventType = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
)

func nowString() string { return model.FormatTime(time.Now()) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// EnsureProject handles POST /api/v1/projects.
//
// Idempotent: a request with a canonical Git identity matches an existing
// project with that identity; otherwise it matches by name among projects
// without one. Projects are never merged automatically.
func EnsureProject(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.EnsureProjectRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if req.CanonicalGitIdentity != nil && *req.CanonicalGitIdentity == "" {
			req.CanonicalGitIdentity = nil
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db begin failed")
			return
		}
		defer tx.Rollback()

		var projectID string
		if req.CanonicalGitIdentity != nil {
			err = tx.QueryRowContext(r.Context(),
				`SELECT project_id FROM projects WHERE user_id = ? AND canonical_git_identity = ?
				 ORDER BY created_at LIMIT 1`,
				ownerUserID, *req.CanonicalGitIdentity).Scan(&projectID)
		} else {
			err = tx.QueryRowContext(r.Context(),
				`SELECT project_id FROM projects WHERE user_id = ? AND name = ? AND canonical_git_identity IS NULL
				 ORDER BY created_at LIMIT 1`,
				ownerUserID, req.Name).Scan(&projectID)
		}
		if err == nil {
			writeJSON(w, http.StatusOK, model.EnsureProjectResponse{ProjectID: projectID, Created: false})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "db query failed")
			return
		}

		projectID = uuid.New().String()
		now := nowString()
		// ON CONFLICT DO NOTHING + re-read: a concurrent request that created
		// the same Git identity first wins (unique index, migration 003).
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO projects (project_id, user_id, name, canonical_git_identity, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
			projectID, ownerUserID, req.Name, req.CanonicalGitIdentity, now, now,
		)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create project")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if err := tx.QueryRowContext(r.Context(),
				`SELECT project_id FROM projects WHERE user_id = ? AND canonical_git_identity = ?`,
				ownerUserID, req.CanonicalGitIdentity).Scan(&projectID); err != nil {
				writeError(w, http.StatusInternalServerError, "db query failed")
				return
			}
			writeJSON(w, http.StatusOK, model.EnsureProjectResponse{ProjectID: projectID, Created: false})
			return
		}
		if err := tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, "commit failed")
			return
		}
		writeJSON(w, http.StatusCreated, model.EnsureProjectResponse{ProjectID: projectID, Created: true})
	}
}

// ListProjects handles GET /api/v1/projects.
func ListProjects(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT project_id, user_id, name, canonical_git_identity, created_at, updated_at
			FROM projects WHERE user_id = ? ORDER BY updated_at DESC`, ownerUserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()

		projects := []model.Project{}
		for rows.Next() {
			var p model.Project
			var git sql.NullString
			var createdAt, updatedAt string
			if err := rows.Scan(&p.ProjectID, &p.UserID, &p.Name, &git,
				&createdAt, &updatedAt); err != nil {
				writeError(w, http.StatusInternalServerError, "scan failed")
				return
			}
			p.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
			p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
			if git.Valid {
				p.CanonicalGitIdentity = &git.String
			}
			projects = append(projects, p)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, projects)
	}
}

// EnsureSession handles POST /api/v1/sessions — idempotent by
// (device_id, agent_type, native_session_id). It also records the project
// location on this device when one is given.
func EnsureSession(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, ok := auth.DeviceIDFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "device identity missing")
			return
		}

		var req model.EnsureSessionRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.ProjectID == "" || req.AgentType == "" || req.NativeSessionID == "" {
			writeError(w, http.StatusBadRequest, "project_id, agent_type and native_session_id are required")
			return
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db begin failed")
			return
		}
		defer tx.Rollback()

		var exists int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM projects WHERE project_id = ? AND user_id = ?`,
			req.ProjectID, ownerUserID).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "db query failed")
			return
		}
		if exists == 0 {
			writeError(w, http.StatusNotFound, "unknown project_id")
			return
		}

		now := nowString()
		var locationID *string
		if req.Location != nil && req.Location.AbsolutePath != "" {
			id, err := upsertLocation(r, tx, req.ProjectID, deviceID, req.Location, now)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to record location")
				return
			}
			locationID = &id
		}

		var sessionID, sessionProject string
		err = tx.QueryRowContext(r.Context(), `
			SELECT session_id, project_id FROM sessions
			WHERE device_id = ? AND agent_type = ? AND native_session_id = ?`,
			deviceID, req.AgentType, req.NativeSessionID,
		).Scan(&sessionID, &sessionProject)
		if err == nil {
			if err := tx.Commit(); err != nil {
				writeError(w, http.StatusInternalServerError, "commit failed")
				return
			}
			writeJSON(w, http.StatusOK, model.EnsureSessionResponse{
				SessionID: sessionID, ProjectID: sessionProject, LocationID: locationID, Created: false,
			})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "db query failed")
			return
		}

		startedAt := now
		if req.StartedAt != nil && !req.StartedAt.IsZero() {
			startedAt = model.FormatTime(*req.StartedAt)
		}
		sessionID = uuid.New().String()
		if _, err = tx.ExecContext(r.Context(), `
			INSERT INTO sessions
				(session_id, project_id, location_id, device_id, agent_type, native_session_id, started_at, status)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'active')`,
			sessionID, req.ProjectID, locationID, deviceID,
			req.AgentType, req.NativeSessionID, startedAt,
		); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE projects SET updated_at = ? WHERE project_id = ?`, now, req.ProjectID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to touch project")
			return
		}
		if err := tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, "commit failed")
			return
		}
		writeJSON(w, http.StatusCreated, model.EnsureSessionResponse{
			SessionID: sessionID, ProjectID: req.ProjectID, LocationID: locationID, Created: true,
		})
	}
}

func upsertLocation(r *http.Request, tx *sql.Tx, projectID, deviceID string, loc *model.LocationInput, now string) (string, error) {
	var id string
	err := tx.QueryRowContext(r.Context(),
		`SELECT location_id FROM project_locations WHERE device_id = ? AND absolute_path = ?`,
		deviceID, loc.AbsolutePath).Scan(&id)
	switch {
	case err == nil:
		_, err = tx.ExecContext(r.Context(), `
			UPDATE project_locations
			SET project_id = ?, git_branch = COALESCE(?, git_branch), git_head = COALESCE(?, git_head), last_observed_at = ?
			WHERE location_id = ?`,
			projectID, loc.GitBranch, loc.GitHead, now, id)
		return id, err
	case errors.Is(err, sql.ErrNoRows):
		id = uuid.New().String()
		_, err = tx.ExecContext(r.Context(), `
			INSERT INTO project_locations (location_id, project_id, device_id, absolute_path, git_branch, git_head, last_observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, projectID, deviceID, loc.AbsolutePath, loc.GitBranch, loc.GitHead, now)
		return id, err
	default:
		return "", err
	}
}

// BatchEvents handles POST /api/v1/events/batch — idempotent by event_id.
//
// The whole batch is written in one transaction and the response (the ACK)
// is only sent after it commits, so an acknowledged event survives a server
// restart. Per-event problems are reported in receipts and do not fail the
// rest of the batch.
func BatchEvents(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, ok := auth.DeviceIDFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "device identity missing")
			return
		}

		var req model.BatchEventsRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBatchBytes)).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "batch too large")
				return
			}
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.ProjectID == "" {
			writeError(w, http.StatusBadRequest, "project_id is required")
			return
		}
		if len(req.Events) == 0 {
			writeError(w, http.StatusBadRequest, "events must not be empty")
			return
		}
		if len(req.Events) > MaxBatchEvents {
			writeError(w, http.StatusRequestEntityTooLarge, "too many events in batch")
			return
		}

		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db begin failed")
			return
		}
		defer tx.Rollback()

		var projectExists int
		if err := tx.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM projects WHERE project_id = ?`, req.ProjectID).Scan(&projectExists); err != nil {
			writeError(w, http.StatusInternalServerError, "db query failed")
			return
		}
		if projectExists == 0 {
			writeError(w, http.StatusNotFound, "unknown project_id")
			return
		}

		now := nowString()
		resp := model.BatchEventsResponse{Receipts: make([]model.EventReceipt, 0, len(req.Events))}
		reject := func(id, code, msg string) {
			resp.Errors++
			resp.Receipts = append(resp.Receipts, model.EventReceipt{
				EventID: id, Status: model.ReceiptError, Code: code, Error: msg,
			})
		}
		// Sessions verified in this batch: session_id → agent_type ("" = invalid).
		sessions := map[string]string{}
		// Sessions that received new events → whether one ended them.
		touched := map[string]bool{}

		for _, ev := range req.Events {
			if !validEventID.MatchString(ev.EventID) {
				reject(ev.EventID, model.CodeInvalid, "event_id must be 8-128 chars of [A-Za-z0-9_-]")
				continue
			}
			if !validEventType.MatchString(ev.EventType) {
				reject(ev.EventID, model.CodeInvalid, "event_type must look like 'tool.completed'")
				continue
			}
			if ev.PayloadJSON == "" {
				ev.PayloadJSON = "{}"
			}
			if !json.Valid([]byte(ev.PayloadJSON)) {
				reject(ev.EventID, model.CodeInvalid, "payload_json is not valid JSON")
				continue
			}
			if ev.SessionID != nil {
				sid := *ev.SessionID
				agent, seen := sessions[sid]
				if !seen {
					err := tx.QueryRowContext(r.Context(),
						`SELECT agent_type FROM sessions WHERE session_id = ? AND device_id = ? AND project_id = ?`,
						sid, deviceID, req.ProjectID).Scan(&agent)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						reject(ev.EventID, model.CodeInternal, "db error")
						continue
					}
					sessions[sid] = agent
				}
				if agent == "" {
					reject(ev.EventID, model.CodeUnknownSession, "session_id does not belong to this device and project")
					continue
				}
			}

			// Defence in depth: the client already redacted, redact again.
			payload := redact.JSON([]byte(ev.PayloadJSON))
			if len(payload) > maxPayloadBytes {
				reject(ev.EventID, model.CodeInvalid, "payload too large")
				continue
			}

			occurredAt := ev.OccurredAt
			if occurredAt.IsZero() {
				occurredAt = time.Now().UTC()
			}
			schema := ev.SchemaVersion
			if schema == 0 {
				schema = 1
			}

			res, err := tx.ExecContext(r.Context(), `
				INSERT OR IGNORE INTO events
					(event_id, session_id, project_id, device_id, event_type,
					 occurred_at, received_at, sequence_number, payload_json, content_hash, schema_version)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ev.EventID, ev.SessionID, req.ProjectID, deviceID, ev.EventType,
				model.FormatTime(occurredAt), now,
				ev.SequenceNumber, string(payload), ev.ContentHash, schema,
			)
			if err != nil {
				reject(ev.EventID, model.CodeInternal, "db error")
				continue
			}
			if n, _ := res.RowsAffected(); n == 0 {
				resp.Duplicate++
				resp.Receipts = append(resp.Receipts, model.EventReceipt{EventID: ev.EventID, Status: model.ReceiptDuplicate})
				continue
			}
			resp.Accepted++
			resp.Receipts = append(resp.Receipts, model.EventReceipt{EventID: ev.EventID, Status: model.ReceiptAccepted})

			sid := ""
			if ev.SessionID != nil {
				sid = *ev.SessionID
			}
			if err := search.IndexEvent(r.Context(), tx, search.Event{
				EventID: ev.EventID, EventType: ev.EventType, ProjectID: req.ProjectID,
				SessionID: sid, DeviceID: deviceID, AgentType: sessions[sid],
				OccurredAt: occurredAt, Payload: payload,
			}); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to index event")
				return
			}

			if sid != "" {
				touched[sid] = touched[sid] || ev.EventType == "session.ended"
			}
			if ev.EventType == "session.ended" && ev.SessionID != nil {
				if _, err := tx.ExecContext(r.Context(), `
					UPDATE sessions SET status = 'ended', ended_at = ?
					WHERE session_id = ? AND (ended_at IS NULL OR ended_at < ?)`,
					model.FormatTime(occurredAt), *ev.SessionID,
					model.FormatTime(occurredAt)); err != nil {
					writeError(w, http.StatusInternalServerError, "failed to end session")
					return
				}
			}
		}

		for sid, ended := range touched {
			if err := summary.Enqueue(r.Context(), tx, sid, ended); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to schedule summary")
				return
			}
		}
		if resp.Accepted > 0 {
			if err := embedding.Enqueue(r.Context(), tx); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to schedule embedding")
				return
			}
		}

		if _, err := tx.ExecContext(r.Context(),
			`UPDATE devices SET last_seen_at = ? WHERE device_id = ?`, now, deviceID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to touch device")
			return
		}
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE projects SET updated_at = ? WHERE project_id = ?`, now, req.ProjectID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to touch project")
			return
		}
		if err := tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, "commit failed")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// SyncStatus handles GET /api/v1/sync/status for the calling device.
func SyncStatus(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, ok := auth.DeviceIDFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "device identity missing")
			return
		}
		resp := model.SyncStatusResponse{DeviceID: deviceID}
		var lastSeen, lastRecv, lastOcc sql.NullString
		err := db.QueryRowContext(r.Context(), `
			SELECT d.name, d.last_seen_at,
				(SELECT COUNT(*) FROM events WHERE device_id = d.device_id),
				(SELECT MAX(received_at) FROM events WHERE device_id = d.device_id),
				(SELECT MAX(occurred_at) FROM events WHERE device_id = d.device_id),
				(SELECT COUNT(*) FROM sessions WHERE device_id = d.device_id)
			FROM devices d WHERE d.device_id = ?`, deviceID,
		).Scan(&resp.DeviceName, &lastSeen, &resp.EventsTotal, &lastRecv, &lastOcc, &resp.SessionsTotal)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		resp.LastSeenAt = parseNullTime(lastSeen)
		resp.LastEventReceivedAt = parseNullTime(lastRecv)
		resp.LastEventOccurredAt = parseNullTime(lastOcc)
		writeJSON(w, http.StatusOK, resp)
	}
}

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

// Search handles POST /api/v1/search.
func Search(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.SearchRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		resp, err := search.Search(r.Context(), db, ownerUserID, req)
		if errors.Is(err, search.ErrBadRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search failed")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
