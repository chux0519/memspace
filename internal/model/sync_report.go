package model

import (
	"regexp"
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
	// UpgradeTarget and UpgradeError describe a failed automatic upgrade
	// to the server's version (empty when there is none).
	UpgradeTarget string `json:"upgrade_target,omitempty"`
	UpgradeError  string `json:"upgrade_error,omitempty"`
}

// SyncReportResponse answers a sync report. Servers before v0.1.3 answer
// 204 without a body.
type SyncReportResponse struct {
	ServerVersion string `json:"server_version"`
}

// ReleaseRepo is the GitHub repository that publishes release binaries.
const ReleaseRepo = "chux0519/memspace"

var releaseRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
var pseudoRE = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)

// IsRelease reports whether v is a tagged release version (v1.2.3 or
// v1.2.3-rc.1), not a development build or a Go pseudo-version.
func IsRelease(v string) bool { return releaseRE.MatchString(v) && !pseudoRE.MatchString(v) }

// ReleaseDownloadURL is where a release asset is published: for version v
// when it is a release, else the latest release.
func ReleaseDownloadURL(v, asset string) string {
	if IsRelease(v) {
		return "https://github.com/" + ReleaseRepo + "/releases/download/" + v + "/" + asset
	}
	return "https://github.com/" + ReleaseRepo + "/releases/latest/download/" + asset
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
