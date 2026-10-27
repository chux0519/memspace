package model

import (
	"runtime/debug"
	"time"
)

// SyncReport is what a client reports about its local spool after a sync
// (POST /api/v1/sync/report), so the server can show unsent backlogs.
type SyncReport struct {
	Pending       int64      `json:"pending"`
	Dead          int64      `json:"dead"`
	Dropped       int64      `json:"dropped"`
	OldestPending *time.Time `json:"oldest_pending_at,omitempty"`
	ClientVersion string     `json:"client_version,omitempty"`
}

// BuildVersion describes the running binary: its module version, or the VCS
// revision for development builds.
func BuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "devel"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "-dirty"
	}
	return "devel-" + rev
}
