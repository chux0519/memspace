package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/server/handler"
)

func (b *uiBrowser) json(method, path, body string, out any) int {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.srv+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(auth.UIHeader, "1")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func login(t *testing.T, srvURL string, database *sql.DB, deviceID, role string) *uiBrowser {
	t.Helper()
	code, err := auth.CreateLoginCode(context.Background(), database, ownerUserID, deviceID, role)
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, srvURL)
	if got := b.do("POST", "/api/v1/ui/login", `{"code":"`+code+`"}`, true); got != http.StatusNoContent {
		t.Fatalf("login as %s: %d", role, got)
	}
	return b
}

func TestAdmin(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	downloads := t.TempDir()
	os.WriteFile(filepath.Join(downloads, "memspace-darwin-arm64"), []byte("binary"), 0o755)
	os.WriteFile(filepath.Join(downloads, "secret.txt"), []byte("no"), 0o644)
	srv := httptest.NewServer(server.NewWithOptions(database, ownerUserID, server.Options{DownloadsDir: downloads}))
	t.Cleanup(srv.Close)
	testDBs.Store(srv, database)

	admin := login(t, srv.URL, database, "", auth.RoleAdmin)
	var who map[string]string
	if admin.json("GET", "/api/v1/ui/session", "", &who); who["role"] != "admin" {
		t.Errorf("session role = %v", who)
	}

	// Create an invite in the UI and register a device with it.
	var inv handler.CreatedInvite
	if code := admin.json("POST", "/api/v1/admin/invites", `{"note":"mac mini","ttl_hours":2}`, &inv); code != http.StatusCreated || !strings.HasPrefix(inv.Code, "msi_") {
		t.Fatalf("create invite %d: %+v", code, inv)
	}
	resp := postRegister(t, srv, model.RegisterDeviceRequest{Name: "macmini", Platform: "darwin", InviteCode: inv.Code})
	var reg model.RegisterDeviceResponse
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register with UI invite: %d", resp.StatusCode)
	}

	// A second invite is withdrawn before use.
	var inv2 handler.CreatedInvite
	admin.json("POST", "/api/v1/admin/invites", `{"note":"spare"}`, &inv2)
	var list []handler.Invite
	admin.json("GET", "/api/v1/admin/invites", "", &list)
	if len(list) != 2 || list[1].Status != "used" || list[1].DeviceName != "macmini" || list[1].Note != "mac mini" || list[0].Status != "pending" {
		t.Fatalf("invites = %+v", list)
	}
	if code := admin.json("DELETE", "/api/v1/admin/invites/"+list[0].ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke invite: %d", code)
	}
	if code := admin.json("DELETE", "/api/v1/admin/invites/"+list[1].ID, "", nil); code != http.StatusNotFound {
		t.Errorf("revoking a used invite: %d", code)
	}
	resp = postRegister(t, srv, model.RegisterDeviceRequest{Name: "x", Platform: "linux", InviteCode: inv2.Code})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("register with revoked invite: %d", resp.StatusCode)
	}

	// Viewer sessions (memspace ui) and device tokens cannot administer.
	viewer := login(t, srv.URL, database, reg.DeviceID, auth.RoleViewer)
	if code := viewer.json("POST", "/api/v1/admin/invites", `{}`, nil); code != http.StatusForbidden {
		t.Errorf("viewer creating invite: %d", code)
	}
	if code := postJSON(t, srv, reg.Token, "/api/v1/admin/invites", struct{}{}, nil); code != http.StatusUnauthorized {
		t.Errorf("device token creating invite: %d", code)
	}
	if code := newBrowser(t, srv.URL).do("GET", "/api/v1/admin/invites", "", true); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}
	// The cookie alone is not enough (cross-site requests cannot add the header).
	if code := admin.do("POST", "/api/v1/admin/invites", `{}`, false); code != http.StatusForbidden {
		t.Errorf("admin without UI header: %d", code)
	}

	// Browser sessions: list (current marked), and revoke the viewer's.
	var sessions []handler.UISessionInfo
	admin.json("GET", "/api/v1/admin/ui-sessions", "", &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}
	var viewerID string
	current := 0
	for _, s := range sessions {
		if s.Current {
			current++
		}
		if s.Role == auth.RoleViewer {
			viewerID = s.ID
			if s.DeviceName != "macmini" {
				t.Errorf("viewer session device = %q", s.DeviceName)
			}
		}
	}
	if current != 1 || viewerID == "" {
		t.Fatalf("sessions = %+v", sessions)
	}
	if code := admin.json("DELETE", "/api/v1/admin/ui-sessions/"+viewerID, "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke session: %d", code)
	}
	if code := viewer.do("GET", "/api/v1/overview", "", true); code != http.StatusUnauthorized {
		t.Errorf("revoked viewer session: %d", code)
	}

	// Login links for other browsers.
	var link handler.LoginLinkResponse
	admin.json("POST", "/api/v1/admin/login-links", `{"role":"viewer"}`, &link)
	b := newBrowser(t, srv.URL)
	if code := b.do("POST", "/api/v1/ui/login", `{"code":"`+link.Code+`"}`, true); code != http.StatusNoContent {
		t.Fatalf("login with admin-made link: %d", code)
	}
	if code := b.json("GET", "/api/v1/admin/invites", "", nil); code != http.StatusForbidden {
		t.Errorf("viewer link grants admin: %d", code)
	}

	// Revoking a device from the UI.
	if code := admin.json("POST", "/api/v1/admin/devices/"+reg.DeviceID+"/revoke", "", nil); code != http.StatusNoContent {
		t.Fatalf("revoke device: %d", code)
	}
	if code := getJSON(t, srv, reg.Token, "/api/v1/projects", nil); code != http.StatusUnauthorized {
		t.Errorf("revoked device token: %d", code)
	}
	if code := admin.json("POST", "/api/v1/admin/devices/nope/revoke", "", nil); code != http.StatusNotFound {
		t.Errorf("unknown device: %d", code)
	}

	// Client downloads: listed for admins, served without auth, only client names.
	var dl []handler.ClientDownload
	admin.json("GET", "/api/v1/admin/downloads", "", &dl)
	if len(dl) != 1 || dl[0].OS != "darwin" || dl[0].Arch != "arm64" {
		t.Errorf("downloads = %+v", dl)
	}
	r, _ := http.Get(srv.URL + "/downloads/memspace-darwin-arm64")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != http.StatusOK || string(body) != "binary" {
		t.Errorf("download: %d %q", r.StatusCode, body)
	}
	for _, p := range []string{"/downloads/secret.txt", "/downloads/..%2Fsecret.txt", "/downloads/memspace-windows-amd64"} {
		r, _ := http.Get(srv.URL + p)
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.StatusCode)
		}
	}
	// A client that is not mirrored here comes from the GitHub release.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, _ = noRedirect.Get(srv.URL + "/downloads/memspace-linux-amd64")
	r.Body.Close()
	if loc := r.Header.Get("Location"); r.StatusCode != http.StatusFound ||
		loc != model.ReleaseDownloadURL(model.BuildVersion(), "memspace-linux-amd64") ||
		!strings.HasPrefix(loc, "https://github.com/chux0519/memspace/releases/") {
		t.Errorf("unmirrored client: %d %q", r.StatusCode, loc)
	}
}

func TestReleaseVersions(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0": true, "v1.2.3-rc.1": true,
		"v0.0.0-20261027152831-d0a1f459ca81":   false,
		"v0.1.1-0.20261027152831-d0a1f459ca81": false,
		"v0.1.0+dirty":                         false,
		"devel-d0a1f459ca81":                   false,
		"":                                     false,
	} {
		if got := model.IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v", v, got)
		}
	}
	if got := model.ReleaseDownloadURL("v0.1.0", "memspace-linux-amd64"); got != "https://github.com/chux0519/memspace/releases/download/v0.1.0/memspace-linux-amd64" {
		t.Errorf("release URL %s", got)
	}
	if got := model.ReleaseDownloadURL("devel-abc", "memspace-linux-amd64"); got != "https://github.com/chux0519/memspace/releases/latest/download/memspace-linux-amd64" {
		t.Errorf("devel URL %s", got)
	}
}
