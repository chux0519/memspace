package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// Web UI authentication. A device token (`memspace ui`) or the
// server operator (`memspace server login-link`) creates a short-lived,
// single-use login code; the browser exchanges it for a session cookie.
//
// The code travels in the URL fragment (#/login/<code>), which browsers do
// not send to servers, so it does not end up in access logs or Referer
// headers. The cookie is HttpOnly and SameSite=Strict, and every
// cookie-authenticated request must also carry UIHeader, which a cross-site
// form or image cannot set.

const (
	// LoginPrefix marks login codes.
	LoginPrefix = "msl_"
	// UICookie is the session cookie name.
	UICookie = "memspace_ui"
	// UIHeader must be present (any value) on cookie-authenticated requests.
	UIHeader = "X-MemSpace-UI"

	LoginLinkTTL = 5 * time.Minute
	UISessionTTL = 14 * 24 * time.Hour

	// RoleAdmin sessions may manage invites, devices and browser sessions;
	// RoleViewer sessions only read.
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// ErrInvalidLogin means the login code is unknown, expired or used.
var ErrInvalidLogin = errors.New("auth: invalid, expired or already used login link")

func randomSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CreateLoginCode stores a single-use login code. deviceID is the device that
// asked for it ("" for the server operator or an admin session).
func CreateLoginCode(ctx context.Context, db *sql.DB, ownerUserID, deviceID, role string) (string, error) {
	if role != RoleAdmin {
		role = RoleViewer
	}
	code, err := randomSecret(LoginPrefix)
	if err != nil {
		return "", err
	}
	if err := EnsureOwner(ctx, db, ownerUserID); err != nil {
		return "", err
	}
	now := time.Now()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ui_login_links (link_hash, user_id, device_id, role, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		HashToken(code), ownerUserID, nullable(deviceID), role, model.FormatTime(now), model.FormatTime(now.Add(LoginLinkTTL))); err != nil {
		return "", fmt.Errorf("auth: store login link: %w", err)
	}
	return code, nil
}

// RedeemLoginCode consumes a login code and opens a UI session, returning
// the session secret for the cookie.
func RedeemLoginCode(ctx context.Context, db *sql.DB, code, userAgent string) (string, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := time.Now()
	var userID, role string
	var deviceID sql.NullString
	err = tx.QueryRowContext(ctx, `
		UPDATE ui_login_links SET used_at = ?
		WHERE link_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id, device_id, role`, model.FormatTime(now), HashToken(code), model.FormatTime(now)).Scan(&userID, &deviceID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidLogin
	}
	if err != nil {
		return "", fmt.Errorf("auth: redeem login link: %w", err)
	}
	if deviceID.Valid {
		var revoked sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM devices WHERE device_id = ?`, deviceID.String).Scan(&revoked); err != nil || revoked.Valid {
			return "", ErrInvalidLogin
		}
	}
	secret, err := randomSecret("")
	if err != nil {
		return "", err
	}
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO ui_sessions (session_hash, user_id, device_id, role, user_agent, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, HashToken(secret), userID, deviceID, role, userAgent,
		model.FormatTime(now), model.FormatTime(now), model.FormatTime(now.Add(UISessionTTL))); err != nil {
		return "", fmt.Errorf("auth: store ui session: %w", err)
	}
	return secret, tx.Commit()
}

// UISession is an authenticated browser session.
type UISession struct {
	ID   string // hash of the secret; safe to show and to revoke by
	Role string
}

// VerifyUISession checks a session secret: unexpired, not logged out, and
// its device (if any) not revoked.
func VerifyUISession(ctx context.Context, db *sql.DB, secret string) (*UISession, error) {
	var lastSeen string
	sess := &UISession{ID: HashToken(secret)}
	err := db.QueryRowContext(ctx, `
		SELECT s.last_seen_at, s.role FROM ui_sessions s LEFT JOIN devices d ON d.device_id = s.device_id
		WHERE s.session_hash = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND d.revoked_at IS NULL`,
		sess.ID, model.FormatTime(time.Now())).Scan(&lastSeen, &sess.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("auth: verify ui session: %w", err)
	}
	if t, _ := time.Parse(time.RFC3339Nano, lastSeen); time.Since(t) > time.Minute {
		db.ExecContext(ctx, `UPDATE ui_sessions SET last_seen_at = ? WHERE session_hash = ?`,
			model.FormatTime(time.Now()), sess.ID)
	}
	return sess, nil
}

const uiSessionKey contextKey = 1

// UISessionFromContext returns the browser session of a cookie-authenticated
// request (nil for device tokens).
func UISessionFromContext(ctx context.Context) *UISession {
	s, _ := ctx.Value(uiSessionKey).(*UISession)
	return s
}

// uiSession authenticates a cookie request (UIHeader required) and writes
// the error response itself when it fails.
func uiSession(w http.ResponseWriter, r *http.Request, db *sql.DB) (*UISession, bool) {
	secret := UISecret(r)
	if secret == "" {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return nil, false
	}
	if r.Header.Get(UIHeader) == "" {
		writeJSONError(w, http.StatusForbidden, "missing "+UIHeader+" header")
		return nil, false
	}
	sess, err := VerifyUISession(r.Context(), db, secret)
	switch {
	case errors.Is(err, ErrInvalidToken):
		writeJSONError(w, http.StatusUnauthorized, "session expired; open a new login link")
		return nil, false
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "authentication unavailable")
		return nil, false
	}
	return sess, true
}

// AdminMiddleware admits only admin browser sessions.
func AdminMiddleware(db *sql.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := uiSession(w, r, db)
			if !ok {
				return
			}
			if sess.Role != RoleAdmin {
				writeJSONError(w, http.StatusForbidden, "admin session required (memspace server login-link)")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), uiSessionKey, sess)))
		})
	}
}

// RevokeUISession logs a session out.
func RevokeUISession(ctx context.Context, db *sql.DB, secret string) error {
	_, err := db.ExecContext(ctx, `UPDATE ui_sessions SET revoked_at = ? WHERE session_hash = ? AND revoked_at IS NULL`,
		model.FormatTime(time.Now()), HashToken(secret))
	return err
}

// SetUICookie sets (secret != "") or clears the session cookie.
func SetUICookie(w http.ResponseWriter, r *http.Request, secret string) {
	c := &http.Cookie{
		Name: UICookie, Value: secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge: int(UISessionTTL / time.Second),
	}
	if secret == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// UISecret returns the session secret from a request's cookie.
func UISecret(r *http.Request) string {
	c, err := r.Cookie(UICookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// ReadOrUIMiddleware authenticates read endpoints shared by agents and the
// web UI: a device token (Authorization: Bearer), or a UI session cookie
// together with UIHeader. Cookie requests carry no device id.
func ReadOrUIMiddleware(db *sql.DB) func(http.Handler) http.Handler {
	bearer := Middleware(db)
	return func(next http.Handler) http.Handler {
		withToken := bearer(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secret := UISecret(r)
			if r.Header.Get("Authorization") != "" || secret == "" {
				withToken.ServeHTTP(w, r)
				return
			}
			sess, ok := uiSession(w, r, db)
			if !ok {
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), uiSessionKey, sess)))
		})
	}
}
