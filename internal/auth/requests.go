package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/chux0519/memspace/internal/model"
)

// ---------------------------------------------------------------------------
// Device requests (approval instead of an invite)
//
// A device asks to join and shows a short code; an administrator compares
// the code and approves the request; the device then claims its token with
// a secret only it holds. Creating a request needs no credentials, so it
// only ever yields a pending record: the number of open requests is capped
// and they expire.

const (
	// RequestTTL is how long a request waits for a decision, and how long
	// an approved request waits to be claimed.
	RequestTTL = 10 * time.Minute
	// MaxOpenRequests caps pending requests, since anyone who can reach
	// the server can create one.
	MaxOpenRequests = 20
	// ClaimPrefix marks claim secrets.
	ClaimPrefix = "msr_"
)

// Request statuses.
const (
	RequestPending  = "pending"
	RequestApproved = "approved"
	RequestDenied   = "denied"
	RequestClaimed  = "claimed"
	RequestExpired  = "expired" // derived: pending or approved past expires_at
)

var (
	// ErrTooManyRequests means MaxOpenRequests are already pending.
	ErrTooManyRequests = errors.New("auth: too many pending device requests")
	// ErrUnknownRequest means no open request matches.
	ErrUnknownRequest = errors.New("auth: no pending device request with that code")
)

// DeviceRequest is a request as shown to administrators.
type DeviceRequest struct {
	ID            string     `json:"request_id"`
	UserCode      string     `json:"user_code"`
	Name          string     `json:"name"`
	Platform      string     `json:"platform"`
	RemoteAddr    string     `json:"remote_addr"`
	ClientVersion string     `json:"client_version"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
}

// codeAlphabet has no look-alikes (0/O, 1/I/L).
const codeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func newUserCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, c := range b {
		if i == 4 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return sb.String(), nil
}

// NormalizeUserCode accepts codes typed with lower case, spaces or no dash.
func NormalizeUserCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		if strings.ContainsRune(codeAlphabet, r) {
			sb.WriteRune(r)
		}
	}
	s := sb.String()
	if len(s) == 8 {
		return s[:4] + "-" + s[4:]
	}
	return s
}

// CreateDeviceRequest stores a pending request and returns it with the
// plaintext claim secret (given to the requesting device only).
func CreateDeviceRequest(ctx context.Context, db *sql.DB, name, platform, remoteAddr, clientVersion string) (*DeviceRequest, string, error) {
	now := time.Now()
	nowS := model.FormatTime(now)
	var open int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_requests WHERE status = ? AND expires_at > ?`,
		RequestPending, nowS).Scan(&open); err != nil {
		return nil, "", fmt.Errorf("auth: count requests: %w", err)
	}
	if open >= MaxOpenRequests {
		return nil, "", ErrTooManyRequests
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, "", fmt.Errorf("auth: generate claim secret: %w", err)
	}
	claim := ClaimPrefix + hex.EncodeToString(secret)
	req := &DeviceRequest{
		ID: uuid.New().String(), Name: name, Platform: platform, RemoteAddr: remoteAddr,
		ClientVersion: clientVersion, Status: RequestPending, CreatedAt: now, ExpiresAt: now.Add(RequestTTL),
	}
	// A code must identify one open request: retry the rare collision.
	for attempt := 0; ; attempt++ {
		code, err := newUserCode()
		if err != nil {
			return nil, "", fmt.Errorf("auth: generate code: %w", err)
		}
		var taken int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_requests WHERE user_code = ? AND status IN (?, ?) AND expires_at > ?`,
			code, RequestPending, RequestApproved, nowS).Scan(&taken); err != nil {
			return nil, "", fmt.Errorf("auth: check code: %w", err)
		}
		if taken == 0 {
			req.UserCode = code
			break
		}
		if attempt >= 5 {
			return nil, "", errors.New("auth: could not allocate a request code")
		}
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO device_requests (request_id, user_code, claim_hash, name, platform, remote_addr, client_version, created_at, expires_at, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.ID, req.UserCode, HashToken(claim), name, platform, remoteAddr, clientVersion,
		nowS, model.FormatTime(req.ExpiresAt), RequestPending)
	if err != nil {
		return nil, "", fmt.Errorf("auth: store request: %w", err)
	}
	return req, claim, nil
}

// ListDeviceRequests returns open requests: pending, or approved and not
// yet claimed, and not expired. Oldest first.
func ListDeviceRequests(ctx context.Context, db *sql.DB) ([]DeviceRequest, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT request_id, user_code, name, platform, remote_addr, client_version, status, created_at, expires_at, decided_at
		FROM device_requests WHERE status IN (?, ?) AND expires_at > ?
		ORDER BY created_at`, RequestPending, RequestApproved, model.FormatTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceRequest{}
	for rows.Next() {
		var r DeviceRequest
		var created, expires string
		var decided sql.NullString
		if err := rows.Scan(&r.ID, &r.UserCode, &r.Name, &r.Platform, &r.RemoteAddr, &r.ClientVersion,
			&r.Status, &created, &expires, &decided); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		if decided.Valid {
			t, _ := time.Parse(time.RFC3339Nano, decided.String)
			r.DecidedAt = &t
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideDeviceRequest approves or denies the pending request with the
// given ID or user code. An approved request can be claimed for RequestTTL.
func DecideDeviceRequest(ctx context.Context, db *sql.DB, ref string, approve bool) (*DeviceRequest, error) {
	now := time.Now()
	status, expires := RequestDenied, now
	if approve {
		status, expires = RequestApproved, now.Add(RequestTTL)
	}
	nowS := model.FormatTime(now)
	var id string
	err := db.QueryRowContext(ctx, `
		UPDATE device_requests SET status = ?, decided_at = ?, expires_at = ?
		WHERE (request_id = ? OR user_code = ?) AND status = ? AND expires_at > ?
		RETURNING request_id`,
		status, nowS, model.FormatTime(expires), ref, NormalizeUserCode(ref), RequestPending, nowS).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownRequest
	}
	if err != nil {
		return nil, fmt.Errorf("auth: decide request: %w", err)
	}
	var r DeviceRequest
	var created string
	if err := db.QueryRowContext(ctx, `SELECT request_id, user_code, name, platform, remote_addr, created_at FROM device_requests WHERE request_id = ?`, id).
		Scan(&r.ID, &r.UserCode, &r.Name, &r.Platform, &r.RemoteAddr, &created); err != nil {
		return nil, fmt.Errorf("auth: read request: %w", err)
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	r.Status, r.ExpiresAt, r.DecidedAt = status, expires, &now
	return &r, nil
}

// ClaimResult is what a device learns when it polls with its claim secret.
type ClaimResult struct {
	Status    string // pending | approved (token issued) | denied | expired | claimed
	ExpiresAt time.Time
	DeviceID  string
	Token     string // plaintext, only when Status is approved
	Name      string
}

// ErrUnknownClaim means the claim secret matches no request.
var ErrUnknownClaim = errors.New("auth: unknown device request")

// ClaimDeviceRequest reports a request's state to the device holding its
// claim secret and, once approved, registers the device and returns its
// token, exactly once.
func ClaimDeviceRequest(ctx context.Context, db *sql.DB, ownerUserID, claim string) (*ClaimResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id, status, expires, name, platform string
	err = tx.QueryRowContext(ctx, `SELECT request_id, status, expires_at, name, platform FROM device_requests WHERE claim_hash = ?`,
		HashToken(claim)).Scan(&id, &status, &expires, &name, &platform)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownClaim
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read request: %w", err)
	}
	res := &ClaimResult{Status: status, Name: name}
	res.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	if (status == RequestPending || status == RequestApproved) && !res.ExpiresAt.After(time.Now()) {
		res.Status = RequestExpired
		return res, nil
	}
	if status != RequestApproved {
		return res, nil
	}
	deviceID, token, err := CreateDevice(ctx, tx, ownerUserID, name, platform)
	if err != nil {
		return nil, err
	}
	upd, err := tx.ExecContext(ctx, `UPDATE device_requests SET status = ?, device_id = ? WHERE request_id = ? AND status = ?`,
		RequestClaimed, deviceID, id, RequestApproved)
	if err != nil {
		return nil, fmt.Errorf("auth: claim request: %w", err)
	}
	if n, _ := upd.RowsAffected(); n != 1 {
		return &ClaimResult{Status: RequestClaimed, Name: name}, nil // claimed concurrently
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("auth: commit claim: %w", err)
	}
	res.DeviceID, res.Token = deviceID, token
	return res, nil
}

// CreateDevice registers a device inside tx and returns its ID and plaintext
// token (only the hash is stored).
func CreateDevice(ctx context.Context, tx *sql.Tx, ownerUserID, name, platform string) (deviceID, token string, err error) {
	token, hash, err := GenerateToken()
	if err != nil {
		return "", "", err
	}
	if err := EnsureOwner(ctx, tx, ownerUserID); err != nil {
		return "", "", err
	}
	deviceID = uuid.New().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO devices (device_id, user_id, name, platform, token_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		deviceID, ownerUserID, name, platform, hash, model.FormatTime(time.Now())); err != nil {
		return "", "", fmt.Errorf("auth: register device: %w", err)
	}
	return deviceID, token, nil
}
