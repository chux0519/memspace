package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/model"
)

// Administration from the web UI (admin sessions only, see
// auth.AdminMiddleware): invites, device revocation, browser sessions.

// Invite is an invite as listed to admins; the code itself is shown only
// when created.
type Invite struct {
	ID         string     `json:"id"` // hash of the code
	Note       string     `json:"note"`
	Status     string     `json:"status"` // pending | used | expired | revoked
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	UsedAt     *time.Time `json:"used_at,omitempty"`
	DeviceID   string     `json:"device_id,omitempty"`
	DeviceName string     `json:"device_name,omitempty"`
}

// ListInvites handles GET /api/v1/admin/invites.
func ListInvites(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `
			SELECT i.invite_hash, i.note, i.created_at, i.expires_at, i.used_at, i.revoked_at,
				COALESCE(i.used_by_device_id, ''), COALESCE(d.name, '')
			FROM invites i LEFT JOIN devices d ON d.device_id = i.used_by_device_id
			WHERE i.user_id = ? ORDER BY i.created_at DESC LIMIT 200`, owner)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []Invite{}
		now := time.Now()
		for rows.Next() {
			var inv Invite
			var created, expires string
			var used, revoked sql.NullString
			if err := rows.Scan(&inv.ID, &inv.Note, &created, &expires, &used, &revoked, &inv.DeviceID, &inv.DeviceName); err != nil {
				writeError(w, http.StatusInternalServerError, "query failed")
				return
			}
			inv.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
			inv.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
			inv.UsedAt = parseNullTime(used)
			switch {
			case used.Valid:
				inv.Status = "used"
			case revoked.Valid:
				inv.Status = "revoked"
			case inv.ExpiresAt.Before(now):
				inv.Status = "expired"
			default:
				inv.Status = "pending"
			}
			out = append(out, inv)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// CreatedInvite is the response to creating an invite (code shown once).
type CreatedInvite struct {
	Code      string    `json:"code"`
	Note      string    `json:"note"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CreateInvite handles POST /api/v1/admin/invites: {"note", "ttl_hours"}.
func CreateInvite(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Note     string  `json:"note"`
			TTLHours float64 `json:"ttl_hours"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.TTLHours <= 0 {
			req.TTLHours = 24
		}
		if req.TTLHours > 24*30 {
			writeError(w, http.StatusBadRequest, "ttl_hours must be at most 720")
			return
		}
		note := strings.TrimSpace(req.Note)
		if len([]rune(note)) > 80 {
			note = string([]rune(note)[:80])
		}
		ttl := time.Duration(req.TTLHours * float64(time.Hour))
		code, err := auth.CreateInviteWithNote(r.Context(), db, owner, ttl, note)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create invite")
			return
		}
		writeJSON(w, http.StatusCreated, CreatedInvite{Code: code, Note: note, ExpiresAt: time.Now().Add(ttl).UTC()})
	}
}

// RevokeInvite handles DELETE /api/v1/admin/invites/{id} (unused invites).
func RevokeInvite(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := db.ExecContext(r.Context(), `
			UPDATE invites SET revoked_at = ? WHERE invite_hash = ? AND user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
			nowString(), chi.URLParam(r, "id"), owner)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update failed")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeError(w, http.StatusNotFound, "no unused invite with this id")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// RevokeDeviceHandler handles POST /api/v1/admin/devices/{id}/revoke. The
// device's events stay; its token, MCP token and UI sessions stop working.
func RevokeDeviceHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var n int
		if err := db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM devices WHERE device_id = ? AND user_id = ?`, id, owner).Scan(&n); err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		if n == 0 {
			writeError(w, http.StatusNotFound, "unknown device")
			return
		}
		if err := auth.RevokeDevice(r.Context(), db, id); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// AdminLoginLink handles POST /api/v1/admin/login-links: {"role"} — a link
// for signing in another browser.
func AdminLoginLink(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Role string `json:"role"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req)
		if req.Role != auth.RoleAdmin {
			req.Role = auth.RoleViewer
		}
		code, err := auth.CreateLoginCode(r.Context(), db, owner, "", req.Role)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create login link")
			return
		}
		writeJSON(w, http.StatusCreated, LoginLinkResponse{Code: code, Path: LoginPath(code), ExpiresAt: time.Now().Add(auth.LoginLinkTTL).UTC()})
	}
}

// UISessionInfo is a browser session as listed to admins.
type UISessionInfo struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	UserAgent  string    `json:"user_agent"`
	DeviceName string    `json:"device_name,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// ListUISessions handles GET /api/v1/admin/ui-sessions (active ones).
func ListUISessions(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cur := auth.UISessionFromContext(r.Context())
		rows, err := db.QueryContext(r.Context(), `
			SELECT s.session_hash, s.role, s.user_agent, COALESCE(d.name, ''), s.created_at, s.last_seen_at, s.expires_at
			FROM ui_sessions s LEFT JOIN devices d ON d.device_id = s.device_id
			WHERE s.user_id = ? AND s.revoked_at IS NULL AND s.expires_at > ? AND d.revoked_at IS NULL
			ORDER BY s.last_seen_at DESC`, owner, nowString())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query failed")
			return
		}
		defer rows.Close()
		out := []UISessionInfo{}
		for rows.Next() {
			var s UISessionInfo
			var created, seen, expires string
			if err := rows.Scan(&s.ID, &s.Role, &s.UserAgent, &s.DeviceName, &created, &seen, &expires); err != nil {
				writeError(w, http.StatusInternalServerError, "query failed")
				return
			}
			s.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
			s.LastSeenAt, _ = time.Parse(time.RFC3339Nano, seen)
			s.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
			s.Current = cur != nil && cur.ID == s.ID
			out = append(out, s)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// RevokeUISessionHandler handles DELETE /api/v1/admin/ui-sessions/{id}.
func RevokeUISessionHandler(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := db.ExecContext(r.Context(), `
			UPDATE ui_sessions SET revoked_at = ? WHERE session_hash = ? AND user_id = ? AND revoked_at IS NULL`,
			nowString(), chi.URLParam(r, "id"), owner)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update failed")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			writeError(w, http.StatusNotFound, "unknown session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// SessionInfoHandler handles GET /api/v1/ui/session: who is signed in.
func SessionInfoHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := auth.RoleViewer
		if s := auth.UISessionFromContext(r.Context()); s != nil {
			role = s.Role
		}
		writeJSON(w, http.StatusOK, map[string]string{"role": role})
	}
}

// ClientDownload is a client binary offered for new devices.
type ClientDownload struct {
	Name string `json:"name"` // e.g. memspace-darwin-arm64
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Size int64  `json:"size"`
}

// clientNames are the files a downloads directory may serve.
func clientName(name string) (os, arch string, ok bool) {
	base := strings.TrimSuffix(name, ".exe")
	parts := strings.Split(base, "-")
	if len(parts) != 3 || parts[0] != "memspace" {
		return "", "", false
	}
	switch parts[1] {
	case "linux", "darwin", "windows":
	default:
		return "", "", false
	}
	if (parts[1] == "windows") != strings.HasSuffix(name, ".exe") {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// ListDownloads lists client binaries in dir (empty when dir is "").
func ListDownloads(dir string) []ClientDownload {
	out := []ClientDownload{}
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		goos, arch, ok := clientName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, ClientDownload{Name: e.Name(), OS: goos, Arch: arch, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DownloadsHandler handles GET /api/v1/admin/downloads.
func DownloadsHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ListDownloads(dir))
	}
}

// ServeDownload handles GET /downloads/{name}: client binaries, without
// authentication (they are the open-source client; a device still needs
// approval to register). Only names matching memspace-<os>-<arch> are
// served. A binary not in dir (or with no dir) redirects to the GitHub
// release of this server's version, so clients match the server; dir is a
// mirror for networks without access to GitHub.
func ServeDownload(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "name")
		if _, _, ok := clientName(name); !ok {
			http.NotFound(w, r)
			return
		}
		release := func() {
			http.Redirect(w, r, model.ReleaseDownloadURL(model.BuildVersion(), name), http.StatusFound)
		}
		if dir == "" {
			release()
			return
		}
		f, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			release()
			return
		}
		if err != nil {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeContent(w, r, name, st.ModTime(), f)
	}
}
