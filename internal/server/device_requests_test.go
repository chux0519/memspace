package server_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/model"
)

func requestDevice(t *testing.T, srv *httptest.Server, name string) model.DeviceRequestCreated {
	t.Helper()
	var out model.DeviceRequestCreated
	code := postJSON(t, srv, "", "/api/v1/device-requests",
		model.DeviceRequestCreate{Name: name, Platform: "darwin", ClientVersion: "test"}, &out)
	if code != http.StatusCreated {
		t.Fatalf("create request: %d", code)
	}
	return out
}

func claim(t *testing.T, srv *httptest.Server, secret string) (int, model.DeviceRequestStatus) {
	t.Helper()
	var out model.DeviceRequestStatus
	code := postJSON(t, srv, "", "/api/v1/device-requests/claim", model.DeviceRequestClaim{ClaimToken: secret}, &out)
	return code, out
}

func TestDeviceRequestApproval(t *testing.T) {
	srv := newTestServer(t)
	v, _ := testDBs.Load(srv)
	database := v.(*sql.DB)

	req := requestDevice(t, srv, "macmini")
	if !regexp.MustCompile(`^[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}$`).MatchString(req.UserCode) {
		t.Errorf("user code %q", req.UserCode)
	}
	if !strings.HasPrefix(req.ClaimToken, auth.ClaimPrefix) {
		t.Errorf("claim token %q", req.ClaimToken)
	}
	if code, st := claim(t, srv, req.ClaimToken); code != http.StatusAccepted || st.Status != "pending" || st.Token != "" {
		t.Fatalf("claim while pending: %d %+v", code, st)
	}

	// Only admin sessions see and decide requests.
	viewer := login(t, srv.URL, database, "", auth.RoleViewer)
	if code := viewer.json("GET", "/api/v1/admin/device-requests", "", nil); code != http.StatusForbidden {
		t.Errorf("viewer list: %d", code)
	}
	if code := viewer.json("POST", "/api/v1/admin/device-requests/"+req.RequestID+"/approve", "", nil); code != http.StatusForbidden {
		t.Errorf("viewer approve: %d", code)
	}
	admin := login(t, srv.URL, database, "", auth.RoleAdmin)
	var list struct {
		Requests []auth.DeviceRequest `json:"requests"`
	}
	admin.json("GET", "/api/v1/admin/device-requests", "", &list)
	if len(list.Requests) != 1 || list.Requests[0].UserCode != req.UserCode || list.Requests[0].Name != "macmini" ||
		list.Requests[0].RemoteAddr != "127.0.0.1" || list.Requests[0].ClientVersion != "test" {
		t.Fatalf("admin list: %+v", list.Requests)
	}

	if code := admin.json("POST", "/api/v1/admin/device-requests/"+req.RequestID+"/approve", "", nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	if code := admin.json("POST", "/api/v1/admin/device-requests/"+req.RequestID+"/deny", "", nil); code != http.StatusNotFound {
		t.Errorf("deny after approve: %d", code)
	}
	code, st := claim(t, srv, req.ClaimToken)
	if code != http.StatusOK || st.Status != "approved" || st.Token == "" || st.DeviceID == "" || st.Name != "macmini" {
		t.Fatalf("claim after approval: %d %+v", code, st)
	}
	var sync model.SyncStatusResponse
	if code := getJSON(t, srv, st.Token, "/api/v1/sync/status", &sync); code != http.StatusOK || sync.DeviceID != st.DeviceID {
		t.Errorf("new token: %d %+v", code, sync)
	}
	// The token is handed out once.
	if code, again := claim(t, srv, req.ClaimToken); code != http.StatusGone || again.Token != "" {
		t.Errorf("second claim: %d %+v", code, again)
	}
	admin.json("GET", "/api/v1/admin/device-requests", "", &list)
	if len(list.Requests) != 0 {
		t.Errorf("claimed request still listed: %+v", list.Requests)
	}

	// Deny, by code typed loosely (as `memspace server approve` accepts it).
	req2 := requestDevice(t, srv, "laptop")
	loose := strings.ToLower(strings.ReplaceAll(req2.UserCode, "-", " "))
	if _, err := auth.DecideDeviceRequest(context.Background(), database, loose, false); err != nil {
		t.Fatalf("deny by code: %v", err)
	}
	if code, st := claim(t, srv, req2.ClaimToken); code != http.StatusForbidden || st.Status != "denied" {
		t.Errorf("claim after deny: %d %+v", code, st)
	}

	// Expired requests can be neither decided nor claimed.
	req3 := requestDevice(t, srv, "old")
	database.Exec(`UPDATE device_requests SET expires_at = ? WHERE request_id = ?`,
		model.FormatTime(time.Now().Add(-time.Minute)), req3.RequestID)
	if _, err := auth.DecideDeviceRequest(context.Background(), database, req3.UserCode, true); err != auth.ErrUnknownRequest {
		t.Errorf("approve expired: %v", err)
	}
	if code, st := claim(t, srv, req3.ClaimToken); code != http.StatusGone || st.Status != "expired" {
		t.Errorf("claim expired: %d %+v", code, st)
	}

	if code, _ := claim(t, srv, "msr_unknown"); code != http.StatusNotFound {
		t.Errorf("unknown claim: %d", code)
	}
	if code := postJSON(t, srv, "", "/api/v1/device-requests", model.DeviceRequestCreate{Name: "x"}, nil); code != http.StatusBadRequest {
		t.Errorf("missing platform: %d", code)
	}
}

// Anyone can create requests, so open ones are capped.
func TestDeviceRequestCap(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < auth.MaxOpenRequests; i++ {
		requestDevice(t, srv, "d")
	}
	if code := postJSON(t, srv, "", "/api/v1/device-requests",
		model.DeviceRequestCreate{Name: "one-too-many", Platform: "linux"}, nil); code != http.StatusTooManyRequests {
		t.Errorf("over the cap: %d", code)
	}
}
