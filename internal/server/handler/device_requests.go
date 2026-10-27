// Package handler — device requests: join by administrator approval
// instead of an invite.
package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/model"
)

// CreateDeviceRequest handles POST /api/v1/device-requests (no auth). It
// only records a pending request; the claim secret in the response is the
// one credential that can later fetch the token.
func CreateDeviceRequest(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.DeviceRequestCreate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		switch {
		case req.Name == "" || req.Platform == "":
			writeError(w, http.StatusBadRequest, "name and platform are required")
			return
		case utf8.RuneCountInString(req.Name) > maxDeviceFieldLen || utf8.RuneCountInString(req.Platform) > maxDeviceFieldLen ||
			utf8.RuneCountInString(req.ClientVersion) > maxDeviceFieldLen:
			writeError(w, http.StatusBadRequest, "fields must be at most 128 characters")
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		dr, claim, err := auth.CreateDeviceRequest(r.Context(), db, req.Name, req.Platform, host, req.ClientVersion)
		switch {
		case errors.Is(err, auth.ErrTooManyRequests):
			writeError(w, http.StatusTooManyRequests, "too many pending device requests; approve or deny them, or wait for them to expire")
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, "failed to create request")
			return
		}
		writeJSON(w, http.StatusCreated, model.DeviceRequestCreated{
			RequestID: dr.ID, UserCode: dr.UserCode, ClaimToken: claim, ExpiresAt: dr.ExpiresAt,
		})
	}
}

// ClaimDeviceRequest handles POST /api/v1/device-requests/claim (no auth;
// the claim secret is the credential). Pending: 202. Approved: 200 with the
// device token, once. Denied: 403. Expired or already claimed: 410.
func ClaimDeviceRequest(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.DeviceRequestClaim
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.ClaimToken == "" {
			writeError(w, http.StatusBadRequest, "claim_token is required")
			return
		}
		res, err := auth.ClaimDeviceRequest(r.Context(), db, ownerUserID, req.ClaimToken)
		switch {
		case errors.Is(err, auth.ErrUnknownClaim):
			writeError(w, http.StatusNotFound, "unknown device request")
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, "failed to check request")
			return
		}
		out := model.DeviceRequestStatus{Status: res.Status, ExpiresAt: res.ExpiresAt}
		status := http.StatusAccepted
		switch res.Status {
		case auth.RequestApproved:
			out.DeviceID, out.Token, out.Name = res.DeviceID, res.Token, res.Name
			status = http.StatusOK
		case auth.RequestDenied:
			status = http.StatusForbidden
		case auth.RequestExpired, auth.RequestClaimed:
			status = http.StatusGone
		}
		writeJSON(w, status, out)
	}
}

// ListDeviceRequests handles GET /api/v1/admin/device-requests.
func ListDeviceRequests(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqs, err := auth.ListDeviceRequests(r.Context(), db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"requests": reqs, "now": time.Now().UTC()})
	}
}

// DecideDeviceRequest handles POST /api/v1/admin/device-requests/{id}/approve
// and /deny.
func DecideDeviceRequest(db *sql.DB, approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dr, err := auth.DecideDeviceRequest(r.Context(), db, chi.URLParam(r, "id"), approve)
		switch {
		case errors.Is(err, auth.ErrUnknownRequest):
			writeError(w, http.StatusNotFound, "no pending request with that id (expired or already decided)")
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, "failed to decide request")
			return
		}
		writeJSON(w, http.StatusOK, dr)
	}
}
