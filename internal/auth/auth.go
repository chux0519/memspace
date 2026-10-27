// Package auth provides device tokens, registration invites and the
// authentication middleware.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// GenerateToken creates a cryptographically random 32-byte token and returns
// both the plaintext (to give the client once) and the SHA-256 hash (to store).
func GenerateToken() (plaintext, hash string, err error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", fmt.Errorf("auth: generate token: %w", err)
	}
	plaintext = hex.EncodeToString(bytes)
	return plaintext, HashToken(plaintext), nil
}

// HashToken returns the SHA-256 hash of a plaintext secret for storage and
// lookup. Secrets are high-entropy random values, so an unsalted hash is
// sufficient and allows indexed lookup.
func HashToken(plaintext string) string {
	h := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(h[:])
}

// ErrInvalidToken means the token is unknown or its device was revoked.
var ErrInvalidToken = errors.New("auth: invalid or revoked token")

// VerifyToken returns the device_id for a valid, unrevoked token.
func VerifyToken(ctx context.Context, db *sql.DB, token string) (deviceID string, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT device_id FROM devices
		WHERE token_hash = ? AND revoked_at IS NULL
	`, HashToken(token)).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("auth: verify token: %w", err)
	}
	return deviceID, nil
}

// VerifyReadToken accepts either the device token or the device's read-only
// MCP token, for read-only endpoints such as MCP.
func VerifyReadToken(ctx context.Context, db *sql.DB, token string) (deviceID string, err error) {
	h := HashToken(token)
	err = db.QueryRowContext(ctx, `
		SELECT device_id FROM devices
		WHERE (token_hash = ? OR mcp_token_hash = ?) AND revoked_at IS NULL
	`, h, h).Scan(&deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	if err != nil {
		return "", fmt.Errorf("auth: verify token: %w", err)
	}
	return deviceID, nil
}

// RotateMCPToken issues a new read-only MCP token for a device, invalidating
// the previous one.
func RotateMCPToken(ctx context.Context, db *sql.DB, deviceID string) (string, error) {
	plaintext, hash, err := GenerateToken()
	if err != nil {
		return "", err
	}
	plaintext = "msm_" + plaintext
	hash = HashToken(plaintext)
	if _, err := db.ExecContext(ctx, `UPDATE devices SET mcp_token_hash = ? WHERE device_id = ? AND revoked_at IS NULL`, hash, deviceID); err != nil {
		return "", fmt.Errorf("auth: store mcp token: %w", err)
	}
	return plaintext, nil
}

// ---------------------------------------------------------------------------
// Registration invites
//
// Registering a device yields a token with full access to the owner's
// memory, so registration requires a one-time invite created by whoever
// operates the server (`memspace server invite`).

// InvitePrefix marks invite codes so they are recognizable in config/logs.
const InvitePrefix = "msi_"

// ErrInvalidInvite means the invite is unknown, expired or already used.
var ErrInvalidInvite = errors.New("auth: invalid, expired or already used invite")

// EnsureOwner creates the owner user row if missing.
func EnsureOwner(ctx context.Context, execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, ownerUserID string) error {
	_, err := execer.ExecContext(ctx, `INSERT OR IGNORE INTO users (user_id, created_at) VALUES (?, ?)`,
		ownerUserID, model.FormatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("auth: ensure owner: %w", err)
	}
	return nil
}

// CreateInvite stores a new single-use invite valid for ttl and returns its
// plaintext code (shown once).
func CreateInvite(ctx context.Context, db *sql.DB, ownerUserID string, ttl time.Duration) (string, error) {
	return CreateInviteWithNote(ctx, db, ownerUserID, ttl, "")
}

// CreateInviteWithNote is CreateInvite with a note shown in the admin UI
// (e.g. the device the invite is meant for).
func CreateInviteWithNote(ctx context.Context, db *sql.DB, ownerUserID string, ttl time.Duration, note string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate invite: %w", err)
	}
	code := InvitePrefix + hex.EncodeToString(b)
	if err := EnsureOwner(ctx, db, ownerUserID); err != nil {
		return "", err
	}
	now := time.Now()
	_, err := db.ExecContext(ctx, `
		INSERT INTO invites (invite_hash, user_id, created_at, expires_at, note) VALUES (?, ?, ?, ?, ?)`,
		HashToken(code), ownerUserID, model.FormatTime(now), model.FormatTime(now.Add(ttl)), note)
	if err != nil {
		return "", fmt.Errorf("auth: store invite: %w", err)
	}
	return code, nil
}

// ConsumeInvite marks an invite as used by deviceID inside tx. It fails with
// ErrInvalidInvite unless the invite exists, is unexpired and unused.
func ConsumeInvite(ctx context.Context, tx *sql.Tx, code, deviceID string) error {
	now := model.FormatTime(time.Now())
	res, err := tx.ExecContext(ctx, `
		UPDATE invites SET used_at = ?, used_by_device_id = ?
		WHERE invite_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		now, deviceID, HashToken(code), now)
	if err != nil {
		return fmt.Errorf("auth: consume invite: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("auth: consume invite: %w", err)
	} else if n == 0 {
		return ErrInvalidInvite
	}
	return nil
}

// ErrUnknownDevice means no device has the given ID.
var ErrUnknownDevice = errors.New("auth: unknown device")

// RevokeDevice permanently invalidates a device's token. Its events stay.
func RevokeDevice(ctx context.Context, db *sql.DB, deviceID string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE devices SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`,
		model.FormatTime(time.Now()), deviceID)
	if err != nil {
		return fmt.Errorf("auth: revoke device: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE device_id = ?`, deviceID).Scan(&exists); err != nil {
			return fmt.Errorf("auth: revoke device: %w", err)
		}
		if exists == 0 {
			return ErrUnknownDevice
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Middleware

type contextKey int

const deviceIDKey contextKey = 0

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Middleware checks Authorization: Bearer <device token> and attaches the
// device_id to the request context, or aborts with 401.
func Middleware(db *sql.DB) func(http.Handler) http.Handler {
	return middleware(func(ctx context.Context, token string) (string, error) { return VerifyToken(ctx, db, token) })
}

// ReadMiddleware is Middleware for read-only endpoints: it also accepts the
// device's MCP token.
func ReadMiddleware(db *sql.DB) func(http.Handler) http.Handler {
	return middleware(func(ctx context.Context, token string) (string, error) { return VerifyReadToken(ctx, db, token) })
}

func middleware(verify func(context.Context, string) (string, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing Authorization header")
				return
			}
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || token == "" {
				writeJSONError(w, http.StatusUnauthorized, "invalid Authorization format")
				return
			}
			deviceID, err := verify(r.Context(), token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				writeJSONError(w, http.StatusUnauthorized, "invalid or revoked token")
				return
			case err != nil:
				// A database failure is not the client's fault: 500 lets the
				// client retry instead of treating its token as revoked.
				writeJSONError(w, http.StatusInternalServerError, "authentication unavailable")
				return
			}
			ctx := context.WithValue(r.Context(), deviceIDKey, deviceID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// DeviceIDFromContext retrieves the device_id set by the Middleware.
func DeviceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(deviceIDKey).(string)
	return id, ok
}
