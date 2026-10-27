package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/spool"
)

// Normalizer converts one raw hook invocation into spool records.
type Normalizer interface {
	Normalize(raw []byte, now time.Time) ([]spool.Record, error)
}

// DryRunEnv makes `memspace hook` print normalized events instead of
// spooling them; doctor uses it to exercise the installed command.
const DryRunEnv = "MEMSPACE_HOOK_DRY_RUN"

// HookChecks verifies that every spec has a MemSpace handler, and that it is
// the command this binary would install.
func HookChecks(doc map[string]any, agent, want string, specs []HookSpec) Check {
	installed := InstalledHooks(doc, agent)
	var missing, stale []string
	for _, s := range specs {
		cmds := installed[s.Event]
		switch {
		case len(cmds) == 0:
			missing = append(missing, s.Event)
		case want != "" && cmds[0] != want:
			stale = append(stale, s.Event)
		}
	}
	sort.Strings(missing)
	switch {
	case len(missing) == len(specs):
		return Check{Name: "hooks", Status: Fail, Detail: "not installed (run 'memspace agent install " + agent + "')"}
	case len(missing) > 0:
		return Check{Name: "hooks", Status: Fail, Detail: "missing for " + strings.Join(missing, ", ") + " (re-run install)"}
	case len(stale) > 0:
		return Check{Name: "hooks", Status: Warn, Detail: "installed command differs from this binary's (re-run install to update)"}
	default:
		return Check{Name: "hooks", Status: OK, Detail: fmt.Sprintf("%d events", len(specs))}
	}
}

// SelfTest runs the installed hook command the way the agent would (through
// a shell, with a sample payload on stdin) in dry-run mode, and checks that
// it produced a normalized event.
func SelfTest(ctx context.Context, agent, command string, sample []byte) Check {
	name := "hook self-test"
	if command == "" {
		return Check{Name: name, Status: Fail, Detail: "no installed hook command to run"}
	}
	shell, flag := "sh", "-c"
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("bash"); err != nil {
			return Check{Name: name, Status: Warn, Detail: "skipped: bash not found"}
		}
		shell = "bash"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, flag, command)
	cmd.Stdin = bytes.NewReader(sample)
	cmd.Env = append(os.Environ(), DryRunEnv+"=1")
	cmd.Dir = os.TempDir()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	if err := cmd.Run(); err != nil {
		return Check{Name: name, Status: Fail, Detail: fmt.Sprintf("%v: %s", err, strings.TrimSpace(stderr.String()))}
	}
	if !strings.Contains(stdout.String(), `"event_type"`) {
		return Check{Name: name, Status: Fail, Detail: "hook ran but produced no event: " + strings.TrimSpace(stderr.String())}
	}
	return Check{Name: name, Status: OK, Detail: fmt.Sprintf("installed command ran in %s", time.Since(start).Round(time.Millisecond))}
}
