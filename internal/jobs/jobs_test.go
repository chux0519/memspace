package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func status(t *testing.T, d *sql.DB, key string) (st string, attempts int, runAfter time.Time) {
	t.Helper()
	var ra string
	if err := d.QueryRow(`SELECT status, attempts, run_after FROM jobs WHERE dedup_key = ?`, key).Scan(&st, &attempts, &ra); err != nil {
		t.Fatal(err)
	}
	runAfter, _ = time.Parse(time.RFC3339Nano, ra)
	return
}

func quiet(string, ...any) {}

func TestRunAndDedup(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	Enqueue(ctx, d, "k", "key1", `{"n":1}`, now, time.Hour)
	Enqueue(ctx, d, "k", "key1", `{"n":2}`, now, time.Hour)
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&n)
	if n != 1 {
		t.Fatalf("dedup: %d jobs", n)
	}
	var got []string
	r := &Runner{DB: d, Logf: quiet, Handlers: map[string]Handler{"k": func(_ context.Context, p string) error {
		got = append(got, p)
		return nil
	}}}
	if ran, err := r.RunOnce(ctx); !ran || err != nil {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if ran, _ := r.RunOnce(ctx); ran {
		t.Fatal("done job ran again")
	}
	if len(got) != 1 || got[0] != `{"n":2}` {
		t.Errorf("payloads = %v (latest payload should win)", got)
	}
	if st, _, _ := status(t, d, "key1"); st != "done" {
		t.Errorf("status = %s", st)
	}
	// Re-enqueueing a finished job schedules it again.
	Enqueue(ctx, d, "k", "key1", `{"n":3}`, now, time.Hour)
	if ran, _ := r.RunOnce(ctx); !ran || got[1] != `{"n":3}` {
		t.Errorf("re-enqueued job: %v", got)
	}
}

func TestDebounceIsBounded(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	Enqueue(ctx, d, "k", "key", "{}", now.Add(3*time.Minute), 10*time.Minute)
	for i := 0; i < 20; i++ { // a busy session keeps pushing the run time out
		Enqueue(ctx, d, "k", "key", "{}", now.Add(time.Duration(i+4)*time.Minute), 10*time.Minute)
	}
	_, _, runAfter := status(t, d, "key")
	if runAfter.After(now.Add(10*time.Minute + time.Second)) {
		t.Errorf("debounced past the cap: %v", runAfter.Sub(now))
	}
}

func TestRetryThenFail(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	Enqueue(ctx, d, "k", "key", "{}", time.Now().UTC(), time.Hour)
	r := &Runner{DB: d, Logf: quiet, MaxAttempts: 3, Handlers: map[string]Handler{"k": func(context.Context, string) error {
		return errors.New("boom")
	}}}
	for attempt := 1; attempt <= 3; attempt++ {
		if ran, _ := r.RunOnce(ctx); !ran {
			t.Fatalf("attempt %d did not run", attempt)
		}
		st, n, runAfter := status(t, d, "key")
		if n != attempt {
			t.Errorf("attempts = %d", n)
		}
		if attempt < 3 {
			if st != "queued" || runAfter.Before(time.Now().Add(Backoff(attempt)-time.Second)) {
				t.Errorf("attempt %d: %s, retry in %v", attempt, st, time.Until(runAfter))
			}
			d.Exec(`UPDATE jobs SET run_after = ?`, model.FormatTime(time.Now().UTC())) // skip the wait
		} else if st != "failed" {
			t.Errorf("final status = %s", st)
		}
	}
	// Panics are failures, not crashes.
	Enqueue(ctx, d, "p", "panicky", "{}", time.Now().UTC(), time.Hour)
	r.Handlers["p"] = func(context.Context, string) error { panic("oops") }
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := status(t, d, "panicky"); st != "queued" {
		t.Errorf("panicking job status = %s", st)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	Enqueue(ctx, d, "k", "key", "{}", time.Now().UTC(), time.Hour)
	r := &Runner{DB: d, Logf: quiet, Handlers: map[string]Handler{"k": func(context.Context, string) error { return nil }}}
	if j, err := r.claim(ctx); err != nil || j == nil {
		t.Fatal("claim failed") // a worker claims and then dies
	}
	if ran, _ := r.RunOnce(ctx); ran {
		t.Fatal("live lease was stolen")
	}
	d.Exec(`UPDATE jobs SET lease_until = ?`, model.FormatTime(time.Now().UTC().Add(-time.Second)))
	if ran, _ := r.RunOnce(ctx); !ran {
		t.Fatal("expired lease not reclaimed")
	}
	if st, n, _ := status(t, d, "key"); st != "done" || n != 2 {
		t.Errorf("status=%s attempts=%d", st, n)
	}
}

func TestReenqueueWhileRunningRunsAgain(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	Enqueue(ctx, d, "k", "key", "{}", time.Now().UTC(), time.Hour)
	runs := 0
	var r *Runner
	r = &Runner{DB: d, Logf: quiet, Handlers: map[string]Handler{"k": func(context.Context, string) error {
		runs++
		if runs == 1 { // new events arrive while summarizing
			Enqueue(ctx, d, "k", "key", "{}", time.Now().UTC(), time.Hour)
		}
		return nil
	}}}
	r.RunOnce(ctx)
	if st, _, _ := status(t, d, "key"); st != "queued" {
		t.Fatalf("status after concurrent enqueue = %s", st)
	}
	r.RunOnce(ctx)
	if runs != 2 {
		t.Errorf("runs = %d", runs)
	}
}

func TestConcurrentRunnersDoNotDoubleRun(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		Enqueue(ctx, d, "k", "key"+string(rune('A'+i)), "{}", time.Now().UTC(), time.Hour)
	}
	var count atomic.Int32
	r := &Runner{DB: d, Logf: quiet, Workers: 4, Poll: 10 * time.Millisecond, Handlers: map[string]Handler{"k": func(_ context.Context, p string) error {
		count.Add(1)
		return nil
	}}}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(runCtx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		d.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status = 'done'`).Scan(&n)
		if n == 30 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if count.Load() != 30 {
		t.Errorf("handler ran %d times for 30 jobs", count.Load())
	}
}
