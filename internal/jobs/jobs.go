// Package jobs is the server's durable background job queue:
// SQLite-backed, atomic claims with leases, retries with exponential
// backoff, and a bounded worker pool. A job whose worker dies is reclaimed
// when its lease expires.
package jobs

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// Execer is satisfied by *sql.DB and *sql.Tx.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Enqueue schedules a job, deduplicated by key. Re-enqueueing a pending job
// moves its run time (debouncing) but never past queued_at+maxDelay, so a
// steady stream of triggers cannot postpone it forever. Re-enqueueing a
// running job makes it run again after the current run.
func Enqueue(ctx context.Context, x Execer, kind, key, payload string, runAfter time.Time, maxDelay time.Duration) error {
	now := time.Now().UTC()
	var status, queuedAt string
	err := x.QueryRowContext(ctx, `SELECT status, queued_at FROM jobs WHERE dedup_key = ?`, key).Scan(&status, &queuedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = x.ExecContext(ctx, `
			INSERT INTO jobs (kind, dedup_key, payload, status, run_after, queued_at, created_at, updated_at)
			VALUES (?, ?, ?, 'queued', ?, ?, ?, ?)`,
			kind, key, payload, model.FormatTime(runAfter), model.FormatTime(now), model.FormatTime(now), model.FormatTime(now))
		return err
	case err != nil:
		return err
	}
	first := now
	if status == "queued" {
		if t, err := time.Parse(time.RFC3339Nano, queuedAt); err == nil {
			first = t
		}
	}
	if deadline := first.Add(maxDelay); runAfter.After(deadline) {
		runAfter = deadline
	}
	// A running job keeps its lease; setting status back to queued makes
	// the runner's completion a no-op, so the job runs once more.
	_, err = x.ExecContext(ctx, `
		UPDATE jobs SET status = 'queued', payload = ?, run_after = ?, queued_at = ?, attempts = 0,
			last_error = NULL, updated_at = ?
		WHERE dedup_key = ?`,
		payload, model.FormatTime(runAfter), model.FormatTime(first), model.FormatTime(now), key)
	return err
}

// Job is a claimed job.
type Job struct {
	ID       int64
	Kind     string
	Payload  string
	Attempts int
	token    string
}

// Handler runs one job. Returning an error schedules a retry.
type Handler func(ctx context.Context, payload string) error

// Runner executes queued jobs.
type Runner struct {
	DB          *sql.DB
	Handlers    map[string]Handler
	Workers     int
	Poll        time.Duration
	Lease       time.Duration
	MaxAttempts int
	Logf        func(format string, args ...any)
}

func (r *Runner) defaults() {
	if r.Workers <= 0 {
		r.Workers = 2
	}
	if r.Poll <= 0 {
		r.Poll = time.Second
	}
	if r.Lease <= 0 {
		r.Lease = 2 * time.Minute
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 5
	}
	if r.Logf == nil {
		r.Logf = log.Printf
	}
}

// Run processes jobs until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	r.defaults()
	var wg sync.WaitGroup
	for i := 0; i < r.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, err := r.RunOnce(ctx)
				if err != nil && ctx.Err() == nil {
					r.Logf("jobs: %v", err)
				}
				if !worked {
					select {
					case <-ctx.Done():
					case <-time.After(r.Poll):
					}
				}
			}
		}()
	}
	wg.Wait()
}

// RunOnce claims and runs at most one job. It reports whether one ran.
func (r *Runner) RunOnce(ctx context.Context) (bool, error) {
	r.defaults()
	job, err := r.claim(ctx)
	if err != nil || job == nil {
		return false, err
	}
	h, ok := r.Handlers[job.Kind]
	var runErr error
	if !ok {
		runErr = fmt.Errorf("no handler for job kind %q", job.Kind)
	} else {
		runErr = safeRun(ctx, h, job.Payload)
	}
	return true, r.finish(context.WithoutCancel(ctx), job, runErr)
}

func safeRun(ctx context.Context, h Handler, payload string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return h(ctx, payload)
}

func (r *Runner) claim(ctx context.Context) (*Job, error) {
	r.defaults()
	now := time.Now().UTC()
	b := make([]byte, 8)
	rand.Read(b)
	token := hex.EncodeToString(b)
	// One statement, so the claim is atomic: a job is claimable when it is
	// due, or when a previous claim's lease expired (crashed worker).
	row := r.DB.QueryRowContext(ctx, `
		UPDATE jobs SET status = 'running', lease_until = ?, lease_token = ?, attempts = attempts + 1, updated_at = ?
		WHERE job_id = (
			SELECT job_id FROM jobs
			WHERE (status = 'queued' AND run_after <= ?) OR (status = 'running' AND lease_until < ?)
			ORDER BY run_after LIMIT 1)
		RETURNING job_id, kind, payload, attempts`,
		model.FormatTime(now.Add(r.Lease)), token, model.FormatTime(now), model.FormatTime(now), model.FormatTime(now))
	j := &Job{token: token}
	if err := row.Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim: %w", err)
	}
	return j, nil
}

// finish records the outcome, unless the job was re-enqueued meanwhile (its
// lease token no longer matches), in which case it simply runs again.
func (r *Runner) finish(ctx context.Context, j *Job, runErr error) error {
	now := time.Now().UTC()
	if runErr == nil {
		_, err := r.DB.ExecContext(ctx, `
			UPDATE jobs SET status = 'done', lease_until = NULL, last_error = NULL, updated_at = ?
			WHERE job_id = ? AND lease_token = ? AND status = 'running'`,
			model.FormatTime(now), j.ID, j.token)
		return err
	}
	r.Logf("jobs: %s #%d attempt %d: %v", j.Kind, j.ID, j.Attempts, runErr)
	status, runAfter := "queued", now.Add(Backoff(j.Attempts))
	if j.Attempts >= r.MaxAttempts {
		status = "failed"
	}
	_, err := r.DB.ExecContext(ctx, `
		UPDATE jobs SET status = ?, run_after = ?, lease_until = NULL, last_error = ?, updated_at = ?
		WHERE job_id = ? AND lease_token = ? AND status = 'running'`,
		status, model.FormatTime(runAfter), runErr.Error(), model.FormatTime(now), j.ID, j.token)
	return err
}

// Backoff is the retry delay after n failed attempts: 10s·2^(n-1), at most 10m.
func Backoff(n int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < n && d < 10*time.Minute; i++ {
		d *= 2
	}
	return min(d, 10*time.Minute)
}
