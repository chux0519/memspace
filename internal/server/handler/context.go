// Package handler — context packs and MCP token management.
package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/search"
)

// Context handles POST /api/v1/context: the bounded context
// agents receive at session start and on prompts.
func Context(db *sql.DB, owner string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, _ := auth.DeviceIDFromContext(r.Context())
		var req contextpack.Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		pack, err := contextpack.Build(r.Context(), db, owner, deviceID, req)
		if errors.Is(err, search.ErrBadRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "context build failed")
			return
		}
		writeJSON(w, http.StatusOK, pack)
	}
}

// MCPTokenResponse returns a newly issued read-only MCP token (shown once).
type MCPTokenResponse struct {
	Token string `json:"token"`
}

// RotateMCPToken handles POST /api/v1/devices/mcp-token.
func RotateMCPToken(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID, _ := auth.DeviceIDFromContext(r.Context())
		tok, err := auth.RotateMCPToken(r.Context(), db, deviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to issue token")
			return
		}
		writeJSON(w, http.StatusCreated, MCPTokenResponse{Token: tok})
	}
}
