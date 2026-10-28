package syncer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/project"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/internal/spool"
)

// gate simulates the network between client and server.
type gate struct {
	mode    atomic.Int32 // gateUp, gateDown, gateLoseAck
	handler http.Handler
}

const (
	gateUp int32 = iota
	gateDown
	gateLoseAck
)

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch g.mode.Load() {
	case gateDown:
		hijackClose(w)
	case gateLoseAck:
		// The server processes and commits the request, but the response
		// never reaches the client.
		g.handler.ServeHTTP(httptest.NewRecorder(), r)
		hijackClose(w)
	default:
		g.handler.ServeHTTP(w, r)
	}
}

func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
		}
	}
}

type harness struct {
	t     *testing.T
	db    *sql.DB
	gate  *gate
	url   string
	dbDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	g := &gate{handler: server.New(database, "owner")}
	ts := httptest.NewServer(g)
	t.Cleanup(ts.Close)
	return &harness{t: t, db: database, gate: g, url: ts.URL, dbDir: dir}
}

type device struct {
	s    *Syncer
	sp   *spool.Spool
	name string
}

func (h *harness) device(name string) *device {
	h.t.Helper()
	ctx := context.Background()
	code, err := auth.CreateInvite(ctx, h.db, "owner", time.Hour)
	if err != nil {
		h.t.Fatal(err)
	}
	id, tok, err := client.New(h.url, "").Register(ctx, name, "linux", code)
	if err != nil {
		h.t.Fatal(err)
	}
	cfg := config.Default().Client
	cfg.Home = h.t.TempDir()
	cfg.BackoffBase = config.Duration{Duration: 200 * time.Millisecond}
	cfg.BackoffMax = config.Duration{Duration: 400 * time.Millisecond}
	cfg.BatchSize = 25
	sp, err := spool.Open(cfg.SpoolDir(), spool.Limits{})
	if err != nil {
		h.t.Fatal(err)
	}
	creds := &Credentials{ServerURL: h.url, DeviceID: id, DeviceName: name, Token: tok}
	return &device{s: New(cfg, sp, creds), sp: sp, name: name}
}

var repo = project.Hint{Root: "/src/app", Name: "app", GitIdentity: "github.com/me/app", GitBranch: "main", GitHead: "abc"}

func (d *device) emit(t *testing.T, session string, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("evt_%s_%s_%04d", d.name, session, i)
		err := d.sp.Write(spool.Record{
			EventID: id, Agent: "claude-code", NativeSessionID: session,
			EventType: "tool.completed", OccurredAt: time.Now(), Project: repo,
			Payload: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func (h *harness) count(q string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.db.QueryRow(q, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (d *device) state() State { return LoadState(d.s.Cfg.StateDir()) }

// M2 acceptance: events reach the server while online; while offline they
// stay in the spool, the client reports being offline and backs off; after
// the network recovers every event is delivered exactly once.
func TestOfflineBufferAndReplay(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	ctx := context.Background()

	d.emit(t, "s1", 30)
	if res, err := d.s.Run(ctx, false); err != nil || res.Uploaded != 30 {
		t.Fatalf("online sync: %+v %v", res, err)
	}

	// Network goes down while the agent keeps working.
	h.gate.mode.Store(gateDown)
	d.emit(t, "s2", 40)
	if _, err := d.s.Run(ctx, false); err == nil {
		t.Fatal("sync succeeded with the network down")
	}
	st := d.state()
	if !st.Offline() || st.LastError == "" || st.NextAttemptAt.IsZero() {
		t.Errorf("offline not visible in state: %+v", st)
	}
	if _, err := d.s.Run(ctx, false); !errors.Is(err, ErrBackoff) {
		t.Errorf("expected backoff, got %v", err)
	}
	if ss, _ := d.sp.Stats(); ss.Pending != 40 || ss.Inflight != 0 || ss.Dead != 0 {
		t.Errorf("events not kept while offline: %+v", ss)
	}

	// Repeated failures grow the backoff, never drop events.
	for i := 0; i < 3; i++ {
		time.Sleep(500 * time.Millisecond)
		d.s.Run(ctx, false)
	}
	if st := d.state(); st.ConsecutiveFailures < 2 {
		t.Errorf("failures = %d", st.ConsecutiveFailures)
	}

	// Network recovers.
	h.gate.mode.Store(gateUp)
	time.Sleep(500 * time.Millisecond)
	if res, err := d.s.Run(ctx, false); err != nil || res.Uploaded != 40 {
		t.Fatalf("replay: %+v %v", res, err)
	}
	if st := d.state(); st.Offline() || st.LastSuccessAt.IsZero() {
		t.Errorf("state after recovery: %+v", st)
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 70 {
		t.Errorf("server has %d events, want 70", n)
	}
	if ss, _ := d.sp.Stats(); ss.Pending+ss.Inflight+ss.Dead != 0 {
		t.Errorf("spool not drained: %+v", ss)
	}
	// Sessions and location were resolved from the hints.
	if n := h.count(`SELECT COUNT(*) FROM sessions WHERE agent_type = 'claude-code'`); n != 2 {
		t.Errorf("sessions = %d", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM projects WHERE canonical_git_identity = 'github.com/me/app'`); n != 1 {
		t.Errorf("projects = %d", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM project_locations WHERE absolute_path = '/src/app' AND git_branch = 'main'`); n != 1 {
		t.Errorf("locations = %d", n)
	}
}

// A lost ACK (server committed, client never heard back) must not lose or
// duplicate events: the client retries and the server reports duplicates.
func TestLostAckIsRetriedAsDuplicate(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	ctx := context.Background()
	d.emit(t, "s1", 1) // resolve the session while the network is fine
	d.s.Run(ctx, false)

	d.emit(t, "s1", 20) // ids 0..19; id 0 was already uploaded
	h.gate.mode.Store(gateLoseAck)
	if _, err := d.s.Run(ctx, true); err == nil {
		t.Fatal("expected failure on lost ACK")
	}
	if ss, _ := d.sp.Stats(); ss.Pending != 20 {
		t.Fatalf("pending after lost ACK = %d", ss.Pending)
	}
	h.gate.mode.Store(gateUp)
	res, err := d.s.Run(ctx, true)
	if err != nil || res.Duplicates != 20 || res.Uploaded != 0 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 20 {
		t.Errorf("server has %d events, want 20", n)
	}
}

// A sync killed mid-upload leaves events in inflight/; the next sync
// recovers them once they are stale.
func TestCrashedUploadRecovered(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	d.emit(t, "s1", 5)
	claimed, _ := d.sp.Claim(5) // simulate a sync that claimed and died
	old := time.Now().Add(-time.Hour)
	for _, r := range claimed {
		matches, _ := filepath.Glob(filepath.Join(d.sp.Dir(), "inflight", "*"+r.EventID+".json"))
		for _, m := range matches {
			os.Chtimes(m, old, old)
		}
	}
	if res, err := d.s.Run(context.Background(), false); err != nil || res.Uploaded != 5 {
		t.Fatalf("%+v %v", res, err)
	}
}

// A poison event is isolated and dead-lettered; its neighbours are uploaded.
func TestInvalidEventDeadLettered(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	d.emit(t, "s1", 3)
	d.sp.Write(spool.Record{
		EventID: "evt_bad_type_0001", Agent: "claude-code", NativeSessionID: "s1",
		EventType: "NotAType", OccurredAt: time.Now(), Project: repo, Payload: json.RawMessage(`{}`),
	})
	res, err := d.s.Run(context.Background(), false)
	if err != nil || res.Uploaded != 3 || res.Rejected != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	dead, _ := d.sp.ListDead()
	if len(dead) != 1 || dead[0].EventID != "evt_bad_type_0001" || dead[0].LastError == "" {
		t.Errorf("dead = %+v", dead)
	}
}

// If the server forgets a session (e.g. restored from an older backup), the
// client drops its cached IDs and re-resolves instead of failing forever.
func TestStaleSessionCacheReresolved(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	ctx := context.Background()
	d.emit(t, "s1", 2)
	d.s.Run(ctx, false)

	h.db.Exec(`DELETE FROM events`)
	h.db.Exec(`DELETE FROM sessions`)

	d.emit(t, "s1", 3) // same ids 0,1 are re-sent plus a new one
	if _, err := d.s.Run(ctx, false); err == nil {
		t.Fatal("expected first attempt to fail on unknown session")
	}
	res, err := d.s.Run(ctx, true)
	if err != nil || res.Uploaded != 3 {
		t.Fatalf("after re-resolve: %+v %v", res, err)
	}
}

// Two devices sync concurrently against one server; each device's events
// land exactly once and stay attributed to it, in one shared project.
func TestTwoDevicesConcurrently(t *testing.T) {
	h := newHarness(t)
	a, b := h.device("linux"), h.device("mac")
	a.emit(t, "same-native-id", 120)
	b.emit(t, "same-native-id", 80)
	var wg sync.WaitGroup
	for _, d := range []*device{a, b} {
		wg.Add(1)
		go func(d *device) {
			defer wg.Done()
			if _, err := d.s.Run(context.Background(), false); err != nil {
				t.Error(err)
			}
		}(d)
	}
	wg.Wait()
	if n := h.count(`SELECT COUNT(*) FROM events`); n != 200 {
		t.Errorf("events = %d", n)
	}
	if n := h.count(`SELECT COUNT(DISTINCT device_id) FROM sessions WHERE native_session_id = 'same-native-id'`); n != 2 {
		t.Errorf("native session ids must be scoped per device, got %d sessions", n)
	}
	if n := h.count(`SELECT COUNT(*) FROM projects`); n != 1 {
		t.Errorf("projects = %d, want one shared project", n)
	}
}

// Only one sync runs at a time per device.
func TestLockExcludesConcurrentSync(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	lk, err := acquireLock(d.s.Cfg.StateDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.s.Run(context.Background(), true); !errors.Is(err, ErrLocked) {
		t.Errorf("got %v, want ErrLocked", err)
	}
	if ShouldTrigger(d.s.Cfg.StateDir()) {
		t.Error("hooks should not spawn a sync while one runs")
	}
	lk.release()
	if !ShouldTrigger(d.s.Cfg.StateDir()) {
		t.Error("hooks should spawn a sync when idle")
	}
	// An abandoned lock (crashed sync) is taken over.
	lk, _ = acquireLock(d.s.Cfg.StateDir())
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lk.path, old, old)
	if _, err := d.s.Run(context.Background(), true); err != nil {
		t.Errorf("stale lock not taken over: %v", err)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	base, max := time.Second, 30*time.Second
	prev := time.Duration(0)
	for n := 1; n <= 4; n++ {
		d := backoff(n, base, max)
		if d < prev {
			t.Errorf("backoff(%d) = %v < %v", n, d, prev)
		}
		prev = d
	}
	if d := backoff(20, base, max); d > max*12/10 {
		t.Errorf("backoff not capped: %v", d)
	}
}

// After a sync that reached the server, AfterSync gets the server's version
// and a failed upgrade is reported with the spool state; a failed sync does
// not run it.
func TestAfterSyncAndUpgradeReport(t *testing.T) {
	h := newHarness(t)
	d := h.device("laptop")
	var got []string
	d.s.AfterSync = func(_ context.Context, v string) { got = append(got, v) }
	d.s.UpgradeStatus = func() (string, string) { return "v9.0.0", "download: HTTP 404" }
	d.emit(t, "s1", 3)
	if _, err := d.s.Run(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != model.BuildVersion() {
		t.Fatalf("AfterSync calls = %q", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM devices WHERE upgrade_target = 'v9.0.0' AND upgrade_error = 'download: HTTP 404'`); n != 1 {
		t.Errorf("upgrade failure not reported")
	}

	h.gate.mode.Store(gateDown)
	d.emit(t, "s1", 1)
	d.s.Run(context.Background(), true)
	if len(got) != 1 {
		t.Errorf("AfterSync ran after a failed sync: %q", got)
	}
}
