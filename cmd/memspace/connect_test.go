package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/syncer"
)

// The way agents run setup: `onboard --wait 0` shows the code and exits;
// running it again resumes the same request; after approval it connects.
func TestConnectByApproval(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	srv := httptest.NewServer(server.New(database, "owner"))
	defer srv.Close()

	cfg := config.Default()
	cfg.Client.Home = t.TempDir()
	ctx := context.Background()
	codeRE := regexp.MustCompile(`Code:\s+(\S+)`)

	var out bytes.Buffer
	_, err = connectDevice(ctx, cfg, srv.URL, "macmini", "", 0, &out)
	if !errors.Is(err, errNotApproved) {
		t.Fatalf("first run: %v\n%s", err, out.String())
	}
	m := codeRE.FindStringSubmatch(out.String())
	if m == nil || !strings.Contains(out.String(), srv.URL+"/#/admin") || !strings.Contains(out.String(), "memspace server approve "+m[1]) {
		t.Fatalf("first run output:\n%s", out.String())
	}
	code := m[1]
	if fi, err := os.Stat(pendingRequestPath(cfg)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved request: %v %v", fi, err)
	}

	out.Reset()
	if _, err = connectDevice(ctx, cfg, srv.URL, "macmini", "", 0, &out); !errors.Is(err, errNotApproved) {
		t.Fatalf("second run: %v", err)
	}
	if m := codeRE.FindStringSubmatch(out.String()); m == nil || m[1] != code || !strings.Contains(out.String(), "Resuming") {
		t.Fatalf("second run did not resume %s:\n%s", code, out.String())
	}

	if _, err := auth.DecideDeviceRequest(ctx, database, code, true); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	creds, err := connectDevice(ctx, cfg, srv.URL, "macmini", "", 0, &out)
	if err != nil {
		t.Fatalf("after approval: %v\n%s", err, out.String())
	}
	saved, err := syncer.LoadCredentials(cfg.Client.CredentialsPath())
	if err != nil || saved.Token != creds.Token || saved.DeviceName != "macmini" {
		t.Fatalf("credentials: %+v %v", saved, err)
	}
	if _, err := auth.VerifyToken(ctx, database, creds.Token); err != nil {
		t.Errorf("token not valid on the server: %v", err)
	}
	if _, err := os.Stat(pendingRequestPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("pending request not removed: %v", err)
	}
}

func TestConnectDenied(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	srv := httptest.NewServer(server.New(database, "owner"))
	defer srv.Close()
	cfg := config.Default()
	cfg.Client.Home = t.TempDir()
	ctx := context.Background()

	var out bytes.Buffer
	connectDevice(ctx, cfg, srv.URL, "intruder", "", 0, &out)
	code := regexp.MustCompile(`Code:\s+(\S+)`).FindStringSubmatch(out.String())[1]
	if _, err := auth.DecideDeviceRequest(ctx, database, code, false); err != nil {
		t.Fatal(err)
	}
	if _, err := connectDevice(ctx, cfg, srv.URL, "intruder", "", 0, &out); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("after deny: %v", err)
	}
	if _, err := syncer.LoadCredentials(cfg.Client.CredentialsPath()); !errors.Is(err, syncer.ErrNotConnected) {
		t.Errorf("denied device has credentials: %v", err)
	}
}
