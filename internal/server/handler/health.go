// Package handler — health check endpoint.
package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

type healthResponse struct {
	Status string `json:"status"`
	Time   string `json:"time"`
	DBOk   bool   `json:"db_ok"`
	// Version of the server binary; clients compare it with their own.
	Version string `json:"version"`
	// LatestVersion is the latest release on GitHub, when the server
	// checks (server.update_check).
	LatestVersion string `json:"latest_version,omitempty"`
}

// Health handles GET /api/v1/health — no auth required. latest returns the
// latest release seen ("" when unknown).
func Health(db *sql.DB, latest func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbOk := db.PingContext(r.Context()) == nil
		resp := healthResponse{
			Status:  "ok",
			Time:    time.Now().UTC().Format(time.RFC3339),
			DBOk:    dbOk,
			Version: model.BuildVersion(),
		}
		if latest != nil {
			resp.LatestVersion = latest()
		}
		if !dbOk {
			resp.Status = "degraded"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}
