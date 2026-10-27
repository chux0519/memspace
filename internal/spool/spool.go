// Package spool is the durable local event buffer between agent hooks and the
// server.
//
// Each event is one JSON file. Writers (short-lived hook processes, possibly
// many at once) create files in tmp/, fsync, and rename them into pending/.
// An uploader claims events by renaming pending/ → inflight/ (rename is
// atomic, so concurrent uploaders never claim the same file), deletes them
// after the server ACKs, and renames them back on failure. Files are only
// moved to dead/ when the server permanently rejects them; transient failures
// never drop events (at-least-once delivery).
//
// File names are "<occurred_at unix nanos>_<event_id>.json" so a directory
// listing is in event order, and re-spooling the same event overwrites itself.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/project"
)

const (
	dirTmp      = "tmp"
	dirPending  = "pending"
	dirInflight = "inflight"
	dirDead     = "dead"

	usageFile   = "usage.json"
	droppedFile = "dropped.log"

	// usageMaxAge bounds how stale the cached usage numbers used for the
	// capacity check may be. Recounting a large directory on every hook would
	// add latency to every tool call.
	usageMaxAge = 30 * time.Second
)

// ErrFull is returned by Write when the spool is at capacity. The event is
// counted as dropped so the loss is visible in `memspace client status`.
var ErrFull = errors.New("spool: capacity reached")

// Record is one spooled event plus the context needed to resolve it to server
// IDs at upload time.
type Record struct {
	EventID         string          `json:"event_id"`
	Agent           string          `json:"agent"`
	NativeSessionID string          `json:"native_session_id"`
	EventType       string          `json:"event_type"`
	OccurredAt      time.Time       `json:"occurred_at"`
	Project         project.Hint    `json:"project"`
	Payload         json.RawMessage `json:"payload"`
	Attempts        int             `json:"attempts,omitempty"`
	LastError       string          `json:"last_error,omitempty"`

	file string // base name in its current directory
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,128}$`)

// Limits bounds the spool size. Zero means unlimited.
type Limits struct {
	MaxEvents int
	MaxBytes  int64
}

// Spool manages the buffer rooted at a directory.
type Spool struct {
	dir    string
	limits Limits
}

// Open creates the spool directories if needed.
func Open(dir string, limits Limits) (*Spool, error) {
	for _, sub := range []string{dirTmp, dirPending, dirInflight, dirDead} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("spool: mkdir %s: %w", sub, err)
		}
	}
	return &Spool{dir: dir, limits: limits}, nil
}

// Dir returns the spool root.
func (s *Spool) Dir() string { return s.dir }

func fileName(r *Record) string {
	return fmt.Sprintf("%020d_%s.json", r.OccurredAt.UTC().UnixNano(), r.EventID)
}

// Write durably adds an event to pending/.
func (s *Spool) Write(r Record) error {
	if !validID.MatchString(r.EventID) {
		return fmt.Errorf("spool: invalid event id %q", r.EventID)
	}
	if r.EventType == "" || r.Agent == "" || r.NativeSessionID == "" {
		return fmt.Errorf("spool: event %s: agent, native_session_id and event_type are required", r.EventID)
	}
	if r.OccurredAt.IsZero() {
		r.OccurredAt = time.Now().UTC()
	}
	if len(r.Payload) == 0 {
		r.Payload = json.RawMessage("{}")
	}
	data, err := json.Marshal(&r)
	if err != nil {
		return fmt.Errorf("spool: marshal %s: %w", r.EventID, err)
	}
	if err := s.checkCapacity(int64(len(data))); err != nil {
		s.countDropped()
		return err
	}
	name := fileName(&r)
	if err := writeFileSync(filepath.Join(s.dir, dirTmp, name), data); err != nil {
		return fmt.Errorf("spool: write %s: %w", r.EventID, err)
	}
	if err := os.Rename(filepath.Join(s.dir, dirTmp, name), filepath.Join(s.dir, dirPending, name)); err != nil {
		os.Remove(filepath.Join(s.dir, dirTmp, name))
		return fmt.Errorf("spool: commit %s: %w", r.EventID, err)
	}
	if u, ok := s.cachedUsage(); ok {
		// Best effort: concurrent writers may lose increments; the periodic
		// recount corrects any drift.
		u.Events++
		u.Bytes += int64(len(data))
		s.saveUsage(u)
	}
	return nil
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type usage struct {
	Events    int       `json:"events"`
	Bytes     int64     `json:"bytes"`
	CountedAt time.Time `json:"counted_at"`
}

// checkCapacity uses usage numbers recounted at most usageMaxAge ago and
// incremented by each write since, so the cap is approximate under heavy
// concurrent writing.
func (s *Spool) checkCapacity(add int64) error {
	if s.limits.MaxEvents <= 0 && s.limits.MaxBytes <= 0 {
		return nil
	}
	u, ok := s.cachedUsage()
	if !ok {
		u = s.recountUsage()
	}
	if s.limits.MaxEvents > 0 && u.Events+1 > s.limits.MaxEvents {
		return ErrFull
	}
	if s.limits.MaxBytes > 0 && u.Bytes+add > s.limits.MaxBytes {
		return ErrFull
	}
	return nil
}

func (s *Spool) cachedUsage() (usage, bool) {
	var u usage
	b, err := os.ReadFile(filepath.Join(s.dir, usageFile))
	if err != nil || json.Unmarshal(b, &u) != nil || time.Since(u.CountedAt) > usageMaxAge {
		return usage{}, false
	}
	return u, true
}

func (s *Spool) saveUsage(u usage) {
	b, err := json.Marshal(u)
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, dirTmp, usageFile+"."+strconv.Itoa(os.Getpid()))
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(s.dir, usageFile))
	}
}

// recountUsage counts pending+inflight events (dead letters do not count
// against capacity: they are kept for inspection, not for upload).
func (s *Spool) recountUsage() usage {
	u := usage{CountedAt: time.Now()}
	for _, sub := range []string{dirPending, dirInflight} {
		n, b := countDir(filepath.Join(s.dir, sub))
		u.Events += n
		u.Bytes += b
	}
	s.saveUsage(u)
	return u
}

func countDir(dir string) (n int, bytes int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		n++
		if fi, err := e.Info(); err == nil {
			bytes += fi.Size()
		}
	}
	return n, bytes
}

// countDropped appends one byte to dropped.log; O_APPEND keeps concurrent
// hook processes from losing increments. The count is the file size.
func (s *Spool) countDropped() {
	f, err := os.OpenFile(filepath.Join(s.dir, droppedFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write([]byte{'x'})
	f.Close()
}

// Claim moves up to max pending events to inflight/ and returns them in
// event order. Events another process claimed first are skipped.
func (s *Spool) Claim(max int) ([]*Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, dirPending))
	if err != nil {
		return nil, fmt.Errorf("spool: list pending: %w", err)
	}
	var out []*Record
	now := time.Now()
	for _, e := range entries {
		if len(out) >= max {
			break
		}
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		src := filepath.Join(s.dir, dirPending, name)
		dst := filepath.Join(s.dir, dirInflight, name)
		if err := os.Rename(src, dst); err != nil {
			continue // claimed by someone else, or vanished
		}
		// Rename keeps the old mtime; stamp the claim time for stale recovery.
		os.Chtimes(dst, now, now)
		r, err := readRecord(dst)
		if err != nil {
			s.moveDead(name, dirInflight, nil, "unreadable spool file: "+err.Error())
			continue
		}
		r.file = name
		out = append(out, r)
	}
	return out, nil
}

func readRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Ack deletes events the server has durably accepted.
func (s *Spool) Ack(recs []*Record) error {
	var firstErr error
	for _, r := range recs {
		if err := os.Remove(filepath.Join(s.dir, dirInflight, r.file)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("spool: ack %s: %w", r.EventID, err)
		}
	}
	return firstErr
}

// Release returns claimed events to pending/ after a transient failure,
// recording the attempt. Nothing is ever dropped here.
func (s *Spool) Release(recs []*Record, reason string) error {
	var firstErr error
	for _, r := range recs {
		r.Attempts++
		r.LastError = reason
		path := filepath.Join(s.dir, dirInflight, r.file)
		if data, err := json.Marshal(r); err == nil {
			// Best effort: losing the attempt counter is harmless.
			writeFileSync(path, data)
		}
		if err := os.Rename(path, filepath.Join(s.dir, dirPending, r.file)); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("spool: release %s: %w", r.EventID, err)
		}
	}
	return firstErr
}

// Reject moves events the server permanently refused to dead/.
func (s *Spool) Reject(recs []*Record, reason string) error {
	var firstErr error
	for _, r := range recs {
		if err := s.moveDead(r.file, dirInflight, r, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Spool) moveDead(name, fromDir string, r *Record, reason string) error {
	src := filepath.Join(s.dir, fromDir, name)
	if r != nil {
		r.LastError = reason
		if data, err := json.Marshal(r); err == nil {
			writeFileSync(src, data)
		}
	}
	if err := os.Rename(src, filepath.Join(s.dir, dirDead, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("spool: reject %s: %w", name, err)
	}
	return nil
}

// RecoverStale returns inflight events claimed longer than maxAge ago to
// pending/ — the uploader that claimed them crashed or was killed.
func (s *Spool) RecoverStale(maxAge time.Duration) (int, error) {
	dir := filepath.Join(s.dir, dirInflight)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("spool: list inflight: %w", err)
	}
	n := 0
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil || e.IsDir() || time.Since(fi.ModTime()) < maxAge {
			continue
		}
		if os.Rename(filepath.Join(dir, e.Name()), filepath.Join(s.dir, dirPending, e.Name())) == nil {
			n++
		}
	}
	// Leftover tmp files are writes that crashed before commit; they were
	// never acknowledged to the hook, so removing them loses nothing durable.
	if tmp, err := os.ReadDir(filepath.Join(s.dir, dirTmp)); err == nil {
		for _, e := range tmp {
			if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > maxAge {
				os.Remove(filepath.Join(s.dir, dirTmp, e.Name()))
			}
		}
	}
	return n, nil
}

// RequeueDead moves all dead letters back to pending/ for another attempt.
func (s *Spool) RequeueDead() (int, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, dirDead))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if os.Rename(filepath.Join(s.dir, dirDead, e.Name()), filepath.Join(s.dir, dirPending, e.Name())) == nil {
			n++
		}
	}
	return n, nil
}

// ListDead returns the dead letters (for inspection).
func (s *Spool) ListDead() ([]*Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, dirDead))
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if r, err := readRecord(filepath.Join(s.dir, dirDead, e.Name())); err == nil {
			r.file = e.Name()
			out = append(out, r)
		}
	}
	return out, nil
}

// Stats summarizes the spool for status output.
type Stats struct {
	Pending, Inflight, Dead int
	PendingBytes            int64
	Dropped                 int64
	OldestPending           time.Time
}

// Stats counts events in each state. It also refreshes the cached usage used
// by the capacity check.
func (s *Spool) Stats() (Stats, error) {
	var st Stats
	var err error
	if _, err = os.Stat(s.dir); err != nil {
		return st, err
	}
	st.Pending, st.PendingBytes = countDir(filepath.Join(s.dir, dirPending))
	st.Inflight, _ = countDir(filepath.Join(s.dir, dirInflight))
	st.Dead, _ = countDir(filepath.Join(s.dir, dirDead))
	if fi, err := os.Stat(filepath.Join(s.dir, droppedFile)); err == nil {
		st.Dropped = fi.Size()
	}
	if entries, err := os.ReadDir(filepath.Join(s.dir, dirPending)); err == nil {
		for _, e := range entries {
			if ns, _, ok := strings.Cut(e.Name(), "_"); ok {
				if v, err := strconv.ParseInt(ns, 10, 64); err == nil {
					st.OldestPending = time.Unix(0, v)
					break
				}
			}
		}
	}
	s.recountUsage()
	return st, nil
}

// PendingCount is a cheap check used to decide whether another upload round
// is needed.
func (s *Spool) PendingCount() int {
	n, _ := countDir(filepath.Join(s.dir, dirPending))
	return n
}
