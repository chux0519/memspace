// Package model defines the core data structures shared across the server.
package model

import (
	"encoding/json"
	"time"
)

// TimeFormat is how timestamps are stored in SQLite: UTC with a fixed-width
// fraction, so that text comparison and ORDER BY match chronological order.
// (time.RFC3339Nano trims trailing zeros and would sort "…:00Z" after
// "…:00.1Z".) time.Parse(time.RFC3339Nano, …) reads it back.
const TimeFormat = "2006-01-02T15:04:05.000000000Z"

// FormatTime renders t in TimeFormat.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// User is the single owner for v0.1; user_id is kept for future multi-user support.
type User struct {
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Device represents a registered client machine.
type Device struct {
	DeviceID   string     `json:"device_id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Project is identity independent of absolute path.
type Project struct {
	ProjectID            string    `json:"project_id"`
	UserID               string    `json:"user_id"`
	Name                 string    `json:"name"`
	CanonicalGitIdentity *string   `json:"canonical_git_identity,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// ProjectLocation records where a project lives on a specific device.
type ProjectLocation struct {
	LocationID     string    `json:"location_id"`
	ProjectID      string    `json:"project_id"`
	DeviceID       string    `json:"device_id"`
	AbsolutePath   string    `json:"absolute_path"`
	GitBranch      *string   `json:"git_branch,omitempty"`
	GitHead        *string   `json:"git_head,omitempty"`
	LastObservedAt time.Time `json:"last_observed_at"`
}

// Session is one agent run scoped to a project + device + location.
type Session struct {
	SessionID       string     `json:"session_id"`
	ProjectID       string     `json:"project_id"`
	LocationID      *string    `json:"location_id,omitempty"`
	DeviceID        string     `json:"device_id"`
	AgentType       string     `json:"agent_type"`
	NativeSessionID string     `json:"native_session_id"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	Status          string     `json:"status"`
}

// Event is the append-only raw record of something an agent did.
type Event struct {
	EventID        string    `json:"event_id"`
	SessionID      *string   `json:"session_id,omitempty"`
	ProjectID      string    `json:"project_id"`
	DeviceID       string    `json:"device_id"`
	EventType      string    `json:"event_type"`
	OccurredAt     time.Time `json:"occurred_at"`
	ReceivedAt     time.Time `json:"received_at"`
	SequenceNumber *int64    `json:"sequence_number,omitempty"`
	PayloadJSON    string    `json:"payload_json"`
	ContentHash    *string   `json:"content_hash,omitempty"`
	SchemaVersion  int       `json:"schema_version"`
}

// RegisterDeviceRequest is the body for POST /api/v1/devices.
type RegisterDeviceRequest struct {
	Name       string `json:"name"`
	Platform   string `json:"platform"`
	InviteCode string `json:"invite_code"`
}

// RegisterDeviceResponse returns the plain token once; the client must store it.
type RegisterDeviceResponse struct {
	DeviceID string `json:"device_id"`
	Token    string `json:"token"` // plaintext, shown only at registration
}

// BatchEventsRequest is the body for POST /api/v1/events/batch.
type BatchEventsRequest struct {
	ProjectID string  `json:"project_id"`
	Events    []Event `json:"events"`
}

// Receipt statuses.
const (
	ReceiptAccepted  = "accepted"
	ReceiptDuplicate = "duplicate"
	ReceiptError     = "error"
)

// Receipt error codes tell the client whether retrying can help.
const (
	// CodeInvalid: the event itself is malformed; retrying cannot succeed.
	CodeInvalid = "invalid"
	// CodeUnknownSession: session_id does not exist for this device/project.
	// The client should re-resolve the session and retry.
	CodeUnknownSession = "unknown_session"
	// CodeInternal: a server-side failure; retry later.
	CodeInternal = "internal"
)

// EventReceipt is the per-event result of a batch upload.
type EventReceipt struct {
	EventID string `json:"event_id"`
	Status  string `json:"status"` // accepted | duplicate | error
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

// BatchEventsResponse summarizes a batch upload.
type BatchEventsResponse struct {
	Accepted  int            `json:"accepted"`
	Duplicate int            `json:"duplicate"`
	Errors    int            `json:"errors"`
	Receipts  []EventReceipt `json:"receipts"`
}

// EnsureProjectRequest lets a device ensure a project exists before posting events.
type EnsureProjectRequest struct {
	Name                 string  `json:"name"`
	CanonicalGitIdentity *string `json:"canonical_git_identity,omitempty"`
}

// EnsureProjectResponse returns the existing or newly created project id.
type EnsureProjectResponse struct {
	ProjectID string `json:"project_id"`
	Created   bool   `json:"created"`
}

// LocationInput describes where on the calling device the project lives.
type LocationInput struct {
	AbsolutePath string  `json:"absolute_path"`
	GitBranch    *string `json:"git_branch,omitempty"`
	GitHead      *string `json:"git_head,omitempty"`
}

// EnsureSessionRequest lets a device ensure a session exists before posting events.
// Idempotent by (device, agent_type, native_session_id).
type EnsureSessionRequest struct {
	ProjectID       string         `json:"project_id"`
	AgentType       string         `json:"agent_type"`
	NativeSessionID string         `json:"native_session_id"`
	StartedAt       *time.Time     `json:"started_at,omitempty"`
	Location        *LocationInput `json:"location,omitempty"`
}

// EnsureSessionResponse returns the existing or newly created session id.
// ProjectID is the project the session belongs to, which wins over the
// requested one if the session already existed.
type EnsureSessionResponse struct {
	SessionID  string  `json:"session_id"`
	ProjectID  string  `json:"project_id"`
	LocationID *string `json:"location_id,omitempty"`
	Created    bool    `json:"created"`
}

// SyncStatusResponse is the server's view of the calling device's uploads.
type SyncStatusResponse struct {
	DeviceID            string     `json:"device_id"`
	DeviceName          string     `json:"device_name"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	EventsTotal         int64      `json:"events_total"`
	LastEventReceivedAt *time.Time `json:"last_event_received_at,omitempty"`
	LastEventOccurredAt *time.Time `json:"last_event_occurred_at,omitempty"`
	SessionsTotal       int64      `json:"sessions_total"`
}

// Search scopes.
const (
	ScopeCurrentProject   = "current_project"
	ScopeSelectedProjects = "selected_projects"
	ScopeAllProjects      = "all_projects"
)

// SearchRequest is the body of POST /api/v1/search.
//
// With scope current_project (the default), the project is identified by
// project_id, or by git_identity / project_name as a client derives them
// from its working directory.
type SearchRequest struct {
	Query       string     `json:"query"`
	Scope       string     `json:"scope,omitempty"`
	ProjectID   string     `json:"project_id,omitempty"`
	GitIdentity string     `json:"git_identity,omitempty"`
	ProjectName string     `json:"project_name,omitempty"`
	ProjectIDs  []string   `json:"project_ids,omitempty"`
	SessionID   string     `json:"session_id,omitempty"`
	Kinds       []string   `json:"kinds,omitempty"`
	AgentTypes  []string   `json:"agent_types,omitempty"`
	DeviceIDs   []string   `json:"device_ids,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	Limit       int        `json:"limit,omitempty"`
	// Mode: "" (hybrid when semantic search is available), "keyword" or
	// "semantic".
	Mode string `json:"mode,omitempty"`
	// Highlight selects snippet match markers: "" for [brackets], or
	// HighlightMarks for U+E000…U+E001 (used by the web UI).
	Highlight string `json:"highlight,omitempty"`
}

// HighlightMarks requests private-use snippet markers.
const HighlightMarks = "marks"

// SearchResult is one hit, traceable to its source (kind + ref_id).
type SearchResult struct {
	DocID       int64     `json:"doc_id"`
	Kind        string    `json:"kind"`
	RefID       string    `json:"ref_id"`
	ProjectID   string    `json:"project_id"`
	ProjectName string    `json:"project_name"`
	SessionID   string    `json:"session_id,omitempty"`
	AgentType   string    `json:"agent_type,omitempty"`
	DeviceID    string    `json:"device_id,omitempty"`
	DeviceName  string    `json:"device_name,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	Title       string    `json:"title,omitempty"`
	Snippet     string    `json:"snippet"`
	Score       float64   `json:"score"`
	// Match is "all" when every query term matched, "any" for partial matches.
	Match string `json:"match"`
	// MatchedTerms of TotalTerms query terms occur in the document.
	MatchedTerms int `json:"matched_terms"`
	TotalTerms   int `json:"total_terms"`
	// Similarity is the cosine similarity to the query when the document
	// was found by semantic search (0 otherwise).
	Similarity float64 `json:"similarity,omitempty"`
}

// SearchResponse lists hits, best first.
type SearchResponse struct {
	Results    []SearchResult `json:"results"`
	Scope      string         `json:"scope"`
	ProjectIDs []string       `json:"project_ids,omitempty"`
	Note       string         `json:"note,omitempty"`
}

// SessionInfo describes a session for listings.
type SessionInfo struct {
	SessionID       string     `json:"session_id"`
	ProjectID       string     `json:"project_id"`
	ProjectName     string     `json:"project_name"`
	DeviceID        string     `json:"device_id"`
	DeviceName      string     `json:"device_name"`
	AgentType       string     `json:"agent_type"`
	NativeSessionID string     `json:"native_session_id"`
	Status          string     `json:"status"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
	LastEventAt     *time.Time `json:"last_event_at,omitempty"`
	EventCount      int        `json:"event_count"`
	Objective       string     `json:"objective,omitempty"`
	LatestRequest   string     `json:"latest_request,omitempty"`
}

// EventRecord is one stored event, for timelines.
type EventRecord struct {
	EventID    string          `json:"event_id"`
	EventType  string          `json:"event_type"`
	OccurredAt time.Time       `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// EventPage is a page of a session's event timeline.
type EventPage struct {
	Events []EventRecord `json:"events"`
	// Next is the cursor for the following page; empty at the end.
	Next string `json:"next,omitempty"`
}
