package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// uiCSP allows only same-origin resources: no inline scripts, no third
// parties. Captured content is rendered as text by the UI, never as HTML.
const uiCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// securityHeaders applies to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/mcp" {
			h.Set("Content-Security-Policy", uiCSP)
		}
		next.ServeHTTP(w, r)
	})
}

// staticUI serves the embedded single-page app. Routing happens in the URL
// fragment, so unknown paths are 404s rather than index.html fallbacks.
func staticUI(files fs.FS) http.Handler {
	fileServer := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)
		if p == "/" || p == "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(p, "/assets/") {
			// Vite puts content hashes in asset names.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
}
