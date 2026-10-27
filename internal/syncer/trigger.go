package syncer

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ShouldTrigger reports whether a hook should start a background sync: not
// when one is already running, and not while backing off after a failure.
func ShouldTrigger(stateDir string) bool {
	if lockHeld(stateDir) {
		return false
	}
	return !time.Now().Before(LoadState(stateDir).NextAttemptAt)
}

// SpawnDetached starts `exe args...` in its own session/process group so it
// outlives the hook process (agents may kill a hook's process group when it
// exits or times out). Output is appended to logPath.
func SpawnDetached(exe string, args []string, logPath string) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err == nil {
			rotateIfLarge(logPath, 4<<20)
			if f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err == nil {
				defer f.Close()
				cmd.Stdout = f
				cmd.Stderr = f
			}
		}
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// rotateIfLarge keeps one previous generation of a log file.
func rotateIfLarge(path string, max int64) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > max {
		os.Rename(path, path+".1")
	}
}
