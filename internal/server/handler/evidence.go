package handler

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/chux0519/memspace/internal/evidence"
	"github.com/go-chi/chi/v5"
)

// EvidenceHandler exposes retained versions and direct relation state. It does
// not fetch remote articles or follow links into another project.
func EvidenceHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project_id")
		if project == "" {
			writeError(w, http.StatusBadRequest, "project_id required")
			return
		}
		limit := 24
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || n > 512 {
				writeError(w, http.StatusBadRequest, "limit must be 1..512")
				return
			}
			limit = n
		}
		detail, e := evidence.Inspect(r.Context(), db, owner, project, chi.URLParam(r, "id"), limit)
		if e == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "evidence not found")
			return
		}
		if e != nil {
			writeError(w, http.StatusInternalServerError, "evidence lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	}
}

func EvidenceAssertionHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		project := r.URL.Query().Get("project_id")
		if project == "" {
			writeError(w, 400, "project_id required")
			return
		}
		var id, status, diagnostic, link string
		e := db.QueryRowContext(r.Context(), `SELECT i.event_id,i.status,i.diagnostic,i.link_id FROM evidence_inputs i JOIN projects p ON p.project_id=i.project_id WHERE i.event_id=? AND i.project_id=? AND p.user_id=?`, chi.URLParam(r, "id"), project, owner).Scan(&id, &status, &diagnostic, &link)
		if e == sql.ErrNoRows {
			writeError(w, 404, "assertion not found")
			return
		}
		if e != nil {
			writeError(w, 500, "assertion lookup failed")
			return
		}
		writeJSON(w, 200, map[string]string{"event_id": id, "project_id": project, "status": status, "diagnostic": diagnostic, "link_id": link})
	}
}
