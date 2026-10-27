package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// appServer is a minimal JSON-RPC client for `codex app-server` (stdio).
// MemSpace uses it for two things Codex owns: listing hooks with their trust
// status, and recording trust for MemSpace's own hooks when asked to.
type appServer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte
	nextID int
}

func startAppServer(ctx context.Context, codexBin, codexHome string) (*appServer, error) {
	cmd := exec.CommandContext(ctx, codexBin, "app-server")
	cmd.Env = os.Environ()
	if codexHome != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+codexHome)
	}
	cmd.Dir = os.TempDir()
	setProcessGroup(cmd)
	// CommandContext's default cancel kills only the wrapper.
	cmd.Cancel = func() error { killProcessGroup(cmd); return nil }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &appServer{cmd: cmd, stdin: stdin, lines: make(chan []byte, 64), nextID: 1}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			s.lines <- append([]byte(nil), sc.Bytes()...)
		}
		close(s.lines)
	}()
	if _, err := s.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "memspace", "version": "0.1"},
	}); err != nil {
		s.close()
		return nil, fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := s.send(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *appServer) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *appServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID
	s.nextID++
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("%s: timed out", method)
		case line, ok := <-s.lines:
			if !ok {
				return nil, fmt.Errorf("%s: app-server exited", method)
			}
			var resp struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(line, &resp) != nil || resp.ID == nil || *resp.ID != id {
				continue // notification or unrelated message
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

func (s *appServer) close() {
	s.stdin.Close()
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		killProcessGroup(s.cmd)
		<-done
	}
}

// hookInfo is one entry of hooks/list.
type hookInfo struct {
	Key         string `json:"key"`
	EventName   string `json:"eventName"`
	Command     string `json:"command"`
	SourcePath  string `json:"sourcePath"`
	Enabled     bool   `json:"enabled"`
	CurrentHash string `json:"currentHash"`
	TrustStatus string `json:"trustStatus"`
}

func (s *appServer) listHooks(ctx context.Context, cwd string) ([]hookInfo, error) {
	raw, err := s.call(ctx, "hooks/list", map[string]any{"cwds": []string{cwd}})
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			Hooks []hookInfo `json:"hooks"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("hooks/list: %w", err)
	}
	var out []hookInfo
	for _, d := range res.Data {
		out = append(out, d.Hooks...)
	}
	return out, nil
}

// trustHooks records the current hash of each hook as trusted, exactly as
// approving it in Codex's hook review would.
func (s *appServer) trustHooks(ctx context.Context, hooks []hookInfo) error {
	state := map[string]any{}
	for _, h := range hooks {
		state[h.Key] = map[string]any{"trusted_hash": h.CurrentHash}
	}
	if len(state) == 0 {
		return nil
	}
	_, err := s.call(ctx, "config/value/write", map[string]any{
		"keyPath": "hooks.state", "value": state, "mergeStrategy": "upsert",
	})
	return err
}
