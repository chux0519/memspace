// Package handler — session HTTP endpoints (logic lives in package sessions).
package handler

import (
	"database/sql"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/sessions"
	"github.com/chux0519/memspace/internal/summary"
)

// SessionDetail is the body of GET /api/v1/sessions/{id}.
type SessionDetail struct {
	Session model.SessionInfo `json:"session"`
	Summary *summary.Summary  `json:"summary,omitempty"`
}

// ListSessionsHandler handles GET /api/v1/sessions.
func ListSessionsHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		q := sessions.Query{AgentType: v.Get("agent_type"), DeviceID: v.Get("device_id"), Status: v.Get("status")}
		q.Limit, _ = strconv.Atoi(v.Get("limit"))
		if pid := v.Get("project_id"); pid != "" || v.Get("git_identity") != "" || v.Get("project_name") != "" {
			id, err := search.ResolveProject(r.Context(), db, owner, pid, v.Get("git_identity"), v.Get("project_name"))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "query failed")
				return
			}
			if id == "" {
				writeJSON(w, http.StatusOK, []model.SessionInfo{})
				return
			}
			q.ProjectIDs = []string{id}
		}
		if s := v.Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				writeError(w, http.StatusBadRequest, "since must be RFC 3339")
				return
			}
			q.Since = &t
		}
		if b := v.Get("before"); b != "" {
			t, err := time.Parse(time.RFC3339Nano, b)
			if err != nil {
				writeError(w, http.StatusBadRequest, "before must be RFC 3339")
				return
			}
			q.Before, q.BeforeID = &t, v.Get("before_id")
		}
		out, err := sessions.List(r.Context(), db, owner, q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// GetSessionHandler handles GET /api/v1/sessions/{id}.
func GetSessionHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		si, err := sessions.Get(r.Context(), db, owner, chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if si == nil {
			writeError(w, http.StatusNotFound, "unknown session")
			return
		}
		s, err := sessions.Summary(r.Context(), db, si)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "summary failed")
			return
		}
		writeJSON(w, http.StatusOK, SessionDetail{Session: *si, Summary: s})
	}
}

// SessionEventsHandler handles GET /api/v1/sessions/{id}/events, paginated
// by an opaque cursor over (occurred_at, event_id).
func SessionEventsHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		si, err := sessions.Get(r.Context(), db, owner, id)
		if err != nil || si == nil {
			if err != nil {
				writeError(w, http.StatusInternalServerError, "query failed")
			} else {
				writeError(w, http.StatusNotFound, "unknown session")
			}
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		afterAt, afterID := "", ""
		if c := r.URL.Query().Get("cursor"); c != "" {
			b, err := base64.RawURLEncoding.DecodeString(c)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad cursor")
				return
			}
			afterAt, afterID, _ = strings.Cut(string(b), "|")
		}
		page, err := sessions.Events(r.Context(), db, id, afterAt, afterID, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}
