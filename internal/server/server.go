// Package server wires up the chi router and all route handlers.
package server

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/mcpserver"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/web"
)

// Options are optional server components.
type Options struct {
	ContextPolicy  admission.Policy
	EvidencePolicy evidence.Policy
	// Embedding enables semantic search status reporting (nil = disabled).
	Embedding *embedding.Service
	// DownloadsDir serves client binaries at /downloads/ ("" = none).
	DownloadsDir string
	// LatestRelease returns the latest release on GitHub ("" when unknown).
	LatestRelease func() string
}

// New returns an http.Handler with all routes registered.
func New(db *sql.DB, ownerUserID string) http.Handler {
	return NewWithOptions(db, ownerUserID, Options{})
}

// NewWithOptions is New with optional components.
func NewWithOptions(db *sql.DB, ownerUserID string, opts Options) http.Handler {
	r := chi.NewRouter()

	// No middleware.RealIP: it trusts X-Forwarded-For/X-Real-IP from any
	// client. Add it back only behind a reverse proxy that sets them.
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := admission.WithPolicy(req.Context(), opts.ContextPolicy)
			next.ServeHTTP(w, req.WithContext(evidence.WithPolicy(ctx, opts.EvidencePolicy)))
		})
	})

	r.Get("/api/v1/health", handler.Health(db, opts.LatestRelease))

	// Device registration — no token, but a one-time invite is required.
	r.Post("/api/v1/devices", handler.RegisterDevice(db, ownerUserID))
	// Join by approval: anyone can ask (capped, expiring); only the
	// holder of the claim secret gets the token, after an admin approves.
	r.Post("/api/v1/device-requests", handler.CreateDeviceRequest(db))
	r.Post("/api/v1/device-requests/claim", handler.ClaimDeviceRequest(db, ownerUserID))

	// MCP (streamable HTTP); accepts the device token or its read-only MCP token.
	r.Handle("/mcp", mcpserver.Handler(db, ownerUserID))

	// Web UI login: exchanges a one-time login code for a session cookie.
	r.Post("/api/v1/ui/login", handler.UILogin(db))
	r.Post("/api/v1/ui/logout", handler.UILogout(db))

	// Writes and device-specific routes require a valid device token.
	r.Group(func(r chi.Router) {
		r.Use(auth.Middleware(db))

		r.Post("/api/v1/projects", handler.EnsureProject(db, ownerUserID))
		r.Post("/api/v1/sessions", handler.EnsureSession(db, ownerUserID))
		r.Post("/api/v1/events/batch", handler.BatchEvents(db))
		r.Get("/api/v1/sync/status", handler.SyncStatus(db))
		r.Post("/api/v1/sync/report", handler.SyncReportHandler(db))
		r.Post("/api/v1/context", handler.Context(db, ownerUserID))
		r.Post("/api/v1/devices/mcp-token", handler.RotateMCPToken(db))
		r.Post("/api/v1/ui/login-links", handler.CreateLoginLink(db, ownerUserID))
	})

	// Reads: a device token, or a web UI session.
	r.Group(func(r chi.Router) {
		r.Use(auth.ReadOrUIMiddleware(db))

		r.Get("/api/v1/ui/session", handler.SessionInfoHandler())
		r.Get("/api/v1/overview", handler.OverviewHandler(db, ownerUserID, opts.Embedding))
		r.Get("/api/v1/devices", handler.ListDevices(db, ownerUserID))
		r.Get("/api/v1/projects", handler.ListProjects(db, ownerUserID))
		r.Get("/api/v1/projects/{id}", handler.ProjectHandler(db, ownerUserID))
		r.Get("/api/v1/sessions", handler.ListSessionsHandler(db, ownerUserID))
		r.Get("/api/v1/sessions/{id}", handler.GetSessionHandler(db, ownerUserID))
		r.Get("/api/v1/sessions/{id}/events", handler.SessionEventsHandler(db, ownerUserID))
		r.Get("/api/v1/events/{id}", handler.EventHandler(db, ownerUserID))
		r.Get("/api/v1/memories", handler.MemoriesHandler(db, ownerUserID))
		r.Get("/api/v1/memories/{id}", handler.MemoryHandler(db, ownerUserID))
		r.Post("/api/v1/search", handler.Search(db, ownerUserID))
		r.Get("/api/v1/embedding/status", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(opts.Embedding.Status(r.Context()))
		})
	})

	// Administration: admin browser sessions only (memspace server login-link).
	r.Group(func(r chi.Router) {
		r.Use(auth.AdminMiddleware(db))

		r.Get("/api/v1/admin/invites", handler.ListInvites(db, ownerUserID))
		r.Post("/api/v1/admin/invites", handler.CreateInvite(db, ownerUserID))
		r.Delete("/api/v1/admin/invites/{id}", handler.RevokeInvite(db, ownerUserID))
		r.Post("/api/v1/admin/devices/{id}/revoke", handler.RevokeDeviceHandler(db, ownerUserID))
		r.Post("/api/v1/admin/login-links", handler.AdminLoginLink(db, ownerUserID))
		r.Get("/api/v1/admin/ui-sessions", handler.ListUISessions(db, ownerUserID))
		r.Get("/api/v1/admin/device-requests", handler.ListDeviceRequests(db))
		r.Post("/api/v1/admin/device-requests/{id}/approve", handler.DecideDeviceRequest(db, true))
		r.Post("/api/v1/admin/device-requests/{id}/deny", handler.DecideDeviceRequest(db, false))
		r.Delete("/api/v1/admin/ui-sessions/{id}", handler.RevokeUISessionHandler(db, ownerUserID))
		r.Get("/api/v1/admin/downloads", handler.DownloadsHandler(opts.DownloadsDir))
	})

	r.Get("/downloads/{name}", handler.ServeDownload(opts.DownloadsDir))

	r.Handle("/api/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"not found"}`))
	}))
	r.Handle("/*", staticUI(web.Dist()))

	return r
}
