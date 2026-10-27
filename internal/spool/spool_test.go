package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func rec(i int) Record {
	return Record{
		EventID:         fmt.Sprintf("evt_%08d", i),
		Agent:           "claude-code",
		NativeSessionID: "s1",
		EventType:       "tool.completed",
		OccurredAt:      time.Unix(1_700_000_000, int64(i)),
		Payload:         json.RawMessage(`{"i":1}`),
	}
}

func open(t *testing.T, l Limits) *Spool {
	t.Helper()
	s, err := Open(t.TempDir(), l)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriteClaimAckOrder(t *testing.T) {
	s := open(t, Limits{})
	for _, i := range []int{3, 1, 2} {
		if err := s.Write(rec(i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Claim(10)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim: %v %d", err, len(got))
	}
	for i, r := range got {
		if r.EventID != rec(i+1).EventID {
			t.Errorf("order[%d] = %s", i, r.EventID)
		}
	}
	if st, _ := s.Stats(); st.Pending != 0 || st.Inflight != 3 {
		t.Errorf("stats after claim: %+v", st)
	}
	if err := s.Ack(got); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Stats(); st.Pending+st.Inflight+st.Dead != 0 {
		t.Errorf("stats after ack: %+v", st)
	}
}

func TestReleaseKeepsEventsAndCountsAttempts(t *testing.T) {
	s := open(t, Limits{})
	s.Write(rec(1))
	for attempt := 1; attempt <= 5; attempt++ {
		got, _ := s.Claim(10)
		if len(got) != 1 {
			t.Fatalf("attempt %d: event lost", attempt)
		}
		if got[0].Attempts != attempt-1 {
			t.Errorf("attempts = %d, want %d", got[0].Attempts, attempt-1)
		}
		s.Release(got, "connection refused")
	}
	if st, _ := s.Stats(); st.Pending != 1 || st.Dead != 0 {
		t.Errorf("transient failures must never dead-letter: %+v", st)
	}
}

func TestRejectAndRequeue(t *testing.T) {
	s := open(t, Limits{})
	s.Write(rec(1))
	got, _ := s.Claim(1)
	s.Reject(got, "invalid event_type")
	dead, _ := s.ListDead()
	if len(dead) != 1 || dead[0].LastError != "invalid event_type" {
		t.Fatalf("dead = %+v", dead)
	}
	if n, _ := s.RequeueDead(); n != 1 {
		t.Fatalf("requeued %d", n)
	}
	if s.PendingCount() != 1 {
		t.Errorf("requeued event not pending")
	}
}

func TestRewriteSameEventIsIdempotent(t *testing.T) {
	s := open(t, Limits{})
	s.Write(rec(1))
	s.Write(rec(1))
	if n := s.PendingCount(); n != 1 {
		t.Errorf("pending = %d, want 1", n)
	}
}

func TestConcurrentWritersAndClaimers(t *testing.T) {
	s := open(t, Limits{})
	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := s.Write(rec(w*perWriter + i)); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()

	var mu sync.Mutex
	seen := map[string]int{}
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := s.Claim(7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, r := range got {
					seen[r.EventID]++
				}
				mu.Unlock()
				s.Ack(got)
			}
		}()
	}
	wg.Wait()
	if len(seen) != writers*perWriter {
		t.Errorf("claimed %d distinct events, want %d", len(seen), writers*perWriter)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s claimed %d times", id, n)
		}
	}
}

func TestCapacityDropsAndCounts(t *testing.T) {
	s := open(t, Limits{MaxEvents: 3})
	for i := 0; i < 5; i++ {
		err := s.Write(rec(i))
		if i < 3 && err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if i >= 3 && !errors.Is(err, ErrFull) {
			t.Fatalf("write %d over capacity: %v", i, err)
		}
	}
	st, _ := s.Stats()
	if st.Pending != 3 || st.Dropped == 0 {
		t.Errorf("stats = %+v", st)
	}
	// Draining frees capacity again.
	got, _ := s.Claim(3)
	s.Ack(got)
	s.Stats()
	if err := s.Write(rec(50)); err != nil {
		t.Errorf("write after drain: %v", err)
	}
}

func TestRecoverStale(t *testing.T) {
	s := open(t, Limits{})
	s.Write(rec(1))
	got, _ := s.Claim(1)
	if n, _ := s.RecoverStale(time.Hour); n != 0 {
		t.Fatalf("recovered a fresh claim")
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(s.Dir(), dirInflight, got[0].file), old, old)
	if n, _ := s.RecoverStale(time.Hour); n != 1 {
		t.Fatalf("stale claim not recovered")
	}
	if s.PendingCount() != 1 {
		t.Errorf("recovered event not pending")
	}
}

func TestWriteValidates(t *testing.T) {
	s := open(t, Limits{})
	bad := rec(1)
	bad.EventID = "../../etc/passwd"
	if err := s.Write(bad); err == nil {
		t.Error("path-like event id accepted")
	}
	bad = rec(1)
	bad.NativeSessionID = ""
	if err := s.Write(bad); err == nil {
		t.Error("missing session accepted")
	}
}
