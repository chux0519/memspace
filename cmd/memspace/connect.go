package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/syncer"
)

// connectDevice registers this device with the server and saves its
// credentials: with an invite at once, otherwise by asking the
// administrator to approve a request (see approveDevice).
func connectDevice(ctx context.Context, cfg *config.Config, serverURL, name, invite string, wait time.Duration, out io.Writer) (*syncer.Credentials, error) {
	if name == "" {
		name, _ = os.Hostname()
	}
	c := client.New(serverURL, "")
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, err := c.Health(hctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("server not reachable: %w", err)
	}

	var deviceID, token string
	if invite != "" {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		deviceID, token, err = c.Register(rctx, name, runtime.GOOS, invite)
		cancel()
		if err != nil {
			return nil, err
		}
	} else {
		deviceID, token, name, err = approveDevice(ctx, cfg, c, name, wait, out)
		if err != nil {
			return nil, err
		}
	}
	creds := &syncer.Credentials{
		ServerURL: c.ServerURL, DeviceID: deviceID, DeviceName: name,
		Token: token, ConnectedAt: time.Now().UTC(),
	}
	if err := syncer.SaveCredentials(cfg.Client.CredentialsPath(), creds); err != nil {
		return nil, fmt.Errorf("save credentials: %w", err)
	}
	fmt.Fprintf(out, "Connected to %s as %q (device %s)\n", c.ServerURL, name, deviceID)
	fmt.Fprintf(out, "Credentials saved to %s\n", cfg.Client.CredentialsPath())
	return creds, nil
}

// pendingRequest is a device request waiting for approval. It is saved so
// that running the command again resumes it (same code) instead of asking
// anew: agents run `onboard --wait 0`, relay the code, and run it again
// once the user has approved.
type pendingRequest struct {
	ServerURL  string    `json:"server_url"`
	RequestID  string    `json:"request_id"`
	UserCode   string    `json:"user_code"`
	ClaimToken string    `json:"claim_token"`
	Name       string    `json:"name"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func pendingRequestPath(cfg *config.Config) string {
	return filepath.Join(cfg.Client.StateDir(), "device-request.json")
}

// errNotApproved means the request is still pending after waiting. The
// command exits with status 3 and no "error:" prefix: with --wait 0 this is
// the expected outcome, not a failure.
var errNotApproved = errors.New("not approved yet")

// approveDevice asks the administrator to approve this device and waits up
// to wait for the decision (0: check once). It returns the new device ID,
// token and name.
func approveDevice(ctx context.Context, cfg *config.Config, c *client.Client, name string, wait time.Duration, out io.Writer) (deviceID, token, finalName string, err error) {
	path := pendingRequestPath(cfg)
	var pr pendingRequest
	resumed := false
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &pr) == nil &&
		pr.ServerURL == c.ServerURL && time.Until(pr.ExpiresAt) > 5*time.Second {
		resumed = true
	} else {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		created, err := c.RequestDevice(rctx, name, runtime.GOOS, model.BuildVersion())
		cancel()
		if err != nil {
			return "", "", "", fmt.Errorf("request approval: %w", err)
		}
		pr = pendingRequest{
			ServerURL: c.ServerURL, RequestID: created.RequestID, UserCode: created.UserCode,
			ClaimToken: created.ClaimToken, Name: name, ExpiresAt: created.ExpiresAt,
		}
		if err := writePrivateJSON(path, pr); err != nil {
			return "", "", "", err
		}
	}

	if resumed {
		fmt.Fprintf(out, "Resuming the approval request for this device (code %s).\n", pr.UserCode)
	} else {
		fmt.Fprintf(out, "This device needs approval. Ask the MemSpace administrator to approve it:\n\n")
	}
	fmt.Fprintf(out, "  Code:    %s   (%s · %s, expires %s)\n", pr.UserCode, pr.Name, runtime.GOOS, pr.ExpiresAt.Local().Format("15:04"))
	fmt.Fprintf(out, "  Web UI:  %s/#/admin  (管理 → 待审批设备 → 批准)\n", c.ServerURL)
	fmt.Fprintf(out, "  Or on the server:  memspace server approve %s\n\n", pr.UserCode)

	deadline := time.Now().Add(wait)
	if wait > 0 {
		fmt.Fprintf(out, "Waiting for approval (up to %s; Ctrl-C to stop, run this command again to resume)...\n", wait.Round(time.Second))
	}
	for {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		st, err := c.ClaimDevice(cctx, pr.ClaimToken)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			return "", "", "", ctx.Err()
		case err != nil:
			// Transient (server restarting, network): keep waiting.
		case st.Status == "approved":
			os.Remove(path)
			fmt.Fprintf(out, "Approved.\n")
			return st.DeviceID, st.Token, pr.Name, nil
		case st.Status == "denied":
			os.Remove(path)
			return "", "", "", errors.New("the administrator denied this device")
		case st.Status != "pending":
			os.Remove(path)
			return "", "", "", errors.New("the approval request expired; run this command again for a new code")
		}
		if !time.Now().Before(deadline) {
			// Spelled out for agents running setup: they must relay the
			// code and wait for the user before running the command again.
			fmt.Fprintf(out, "Not approved yet. Next:\n")
			fmt.Fprintf(out, "  1. Show the user the code %s and how to approve it (above).\n", pr.UserCode)
			fmt.Fprintf(out, "  2. Wait until they say they have approved it (valid until %s).\n", pr.ExpiresAt.Local().Format("15:04"))
			fmt.Fprintf(out, "  3. Then run:  %s\n", rerunCommand())
			return "", "", "", errNotApproved
		}
		select {
		case <-ctx.Done():
			return "", "", "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// rerunCommand is this command line with a short wait, to finish setup
// after approval.
func rerunCommand() string {
	args := []string{"memspace"}
	set := false
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "--wait" && i+1 < len(os.Args):
			args = append(args, "--wait", "2m")
			i++
			set = true
		case strings.HasPrefix(a, "--wait="):
			args = append(args, "--wait=2m")
			set = true
		default:
			args = append(args, shellWord(a))
		}
	}
	if !set {
		args = append(args, "--wait", "2m")
	}
	return strings.Join(args, " ")
}

func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:=@,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func writePrivateJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
