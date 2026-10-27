// Package handler — device registration and listing.
package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/model"
)

const maxDeviceFieldLen = 128

// RegisterDevice handles POST /api/v1/devices.
//
// It needs no token, but it does need a single-use invite created by the
// server operator: the returned token grants full access to the owner's
// memory. The invite is consumed in the same transaction that creates the
// device, so one invite can never register two devices.
func RegisterDevice(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req model.RegisterDeviceRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		switch {
		case req.Name == "" || req.Platform == "":
			writeError(w, http.StatusBadRequest, "name and platform are required")
			return
		case utf8.RuneCountInString(req.Name) > maxDeviceFieldLen || utf8.RuneCountInString(req.Platform) > maxDeviceFieldLen:
			writeError(w, http.StatusBadRequest, "name and platform must be at most 128 characters")
			return
		case req.InviteCode == "":
			writeError(w, http.StatusForbidden, "an invite code is required (create one with 'memspace server invite')")
			return
		}

		ctx := r.Context()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db begin failed")
			return
		}
		defer tx.Rollback()

		deviceID, plaintext, err := auth.CreateDevice(ctx, tx, ownerUserID, req.Name, req.Platform)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to register device")
			return
		}
		if err := auth.ConsumeInvite(ctx, tx, req.InviteCode, deviceID); err != nil {
			if errors.Is(err, auth.ErrInvalidInvite) {
				writeError(w, http.StatusForbidden, "invalid, expired or already used invite")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to check invite")
			return
		}
		if err := tx.Commit(); err != nil {
			writeError(w, http.StatusInternalServerError, "commit failed")
			return
		}

		writeJSON(w, http.StatusCreated, model.RegisterDeviceResponse{DeviceID: deviceID, Token: plaintext})
	}
}

// ListDevices handles GET /api/v1/devices — requires auth.
func ListDevices(db *sql.DB, ownerUserID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		devices, err := QueryDevices(r.Context(), db, ownerUserID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		writeJSON(w, http.StatusOK, devices)
	}
}

// QueryDevices lists the owner's devices, newest first.
func QueryDevices(ctx context.Context, db *sql.DB, ownerUserID string) ([]model.Device, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT device_id, user_id, name, platform, created_at, last_seen_at, revoked_at
		FROM devices WHERE user_id = ? ORDER BY created_at DESC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := []model.Device{}
	for rows.Next() {
		var d model.Device
		var createdAt string
		var lastSeen, revokedAt sql.NullString
		if err := rows.Scan(&d.DeviceID, &d.UserID, &d.Name, &d.Platform,
			&createdAt, &lastSeen, &revokedAt); err != nil {
			return nil, err
		}
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		d.LastSeenAt = parseNullTime(lastSeen)
		d.RevokedAt = parseNullTime(revokedAt)
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// writeError writes a simple JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
