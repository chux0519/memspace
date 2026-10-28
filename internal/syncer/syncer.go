// Package syncer uploads spooled events to the server: it resolves local
// project/session hints to server IDs, uploads in batches, deletes events
// only after the server ACKs them, and backs off exponentially while the
// server is unreachable.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/chux0519/memspace/internal/client"
	"github.com/chux0519/memspace/internal/config"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/spool"
)

// Importer pulls events from an agent's own records (e.g. Codex rollout
// files) into the spool. heartbeat must be called periodically during long
// imports to keep the sync lock alive.
type Importer interface {
	Name() string
	Import(ctx context.Context, sp *spool.Spool, heartbeat func()) (int, error)
}

// Syncer runs one device's upload loop.
type Syncer struct {
	Cfg       config.ClientConfig
	Spool     *spool.Spool
	Creds     *Credentials
	Client    *client.Client
	Importers []Importer
	Log       io.Writer
	// AfterSync, when set, runs after a sync that reached the server, with
	// the server's version ("" from servers that do not report it); the
	// client upgrades itself there. It runs outside the sync lock.
	AfterSync func(ctx context.Context, serverVersion string)
	// UpgradeStatus, when set, returns a failed automatic upgrade (target
	// version and error) to report to the server.
	UpgradeStatus func() (target, err string)

	serverVersion string
	reported      bool
}

// New builds a Syncer from config and saved credentials.
func New(cfg config.ClientConfig, sp *spool.Spool, creds *Credentials, importers ...Importer) *Syncer {
	url := creds.ServerURL
	if cfg.ServerURL != "" {
		url = cfg.ServerURL
	}
	return &Syncer{
		Cfg:       cfg,
		Spool:     sp,
		Creds:     creds,
		Client:    client.New(url, creds.Token),
		Importers: importers,
		Log:       io.Discard,
	}
}

// ErrBackoff means the previous attempt failed and the retry delay has not
// elapsed. Use force to retry immediately.
var ErrBackoff = errors.New("in backoff after a failed sync")

// Result summarizes one Run.
type Result struct {
	Imported   int
	Uploaded   int
	Duplicates int
	Rejected   int
	Retried    int
}

// Run imports and uploads until the spool is empty or an attempt fails.
// Failures are recorded in the persisted state and schedule the next attempt.
func (s *Syncer) Run(ctx context.Context, force bool) (Result, error) {
	s.reported = false
	res, err := s.run(ctx, force)
	if s.reported && s.AfterSync != nil {
		s.AfterSync(ctx, s.serverVersion)
	}
	return res, err
}

func (s *Syncer) run(ctx context.Context, force bool) (Result, error) {
	var total Result
	for round := 0; ; round++ {
		res, err := s.runLocked(ctx, force || round > 0)
		total.add(res)
		if err != nil {
			return total, err
		}
		// Events written after our last claim but before we released the
		// lock would otherwise wait for the next trigger: their hooks saw
		// the lock held and did not start a sync. Check again after release.
		if s.Spool.PendingCount() == 0 || round >= 10 {
			return total, nil
		}
	}
}

func (r *Result) add(o Result) {
	r.Imported += o.Imported
	r.Uploaded += o.Uploaded
	r.Duplicates += o.Duplicates
	r.Rejected += o.Rejected
	r.Retried += o.Retried
}

func (s *Syncer) runLocked(ctx context.Context, force bool) (Result, error) {
	var res Result
	stateDir := s.Cfg.StateDir()
	lk, err := acquireLock(stateDir)
	if err != nil {
		return res, err
	}
	defer lk.release()

	st := LoadState(stateDir)
	if !force && time.Now().Before(st.NextAttemptAt) {
		return res, fmt.Errorf("%w (next attempt %s)", ErrBackoff, st.NextAttemptAt.Local().Format(time.TimeOnly))
	}

	if n, _ := s.Spool.RecoverStale(10 * time.Minute); n > 0 {
		fmt.Fprintf(s.Log, "recovered %d events from an interrupted upload\n", n)
	}

	cache := loadCache(stateDir, s.Client.ServerURL)
	defer cache.save(stateDir, s.Client.ServerURL)

	st.LastAttemptAt = time.Now().UTC()
	runErr := s.loop(ctx, lk, cache, &res)

	if runErr != nil {
		st.ConsecutiveFailures++
		st.LastError = runErr.Error()
		st.NextAttemptAt = time.Now().UTC().Add(backoff(st.ConsecutiveFailures, s.Cfg.BackoffBase.Duration, s.Cfg.BackoffMax.Duration))
	} else {
		st.ConsecutiveFailures = 0
		st.LastError = ""
		st.NextAttemptAt = time.Time{}
		st.LastSuccessAt = time.Now().UTC()
	}
	if runErr == nil {
		s.report(ctx)
	}
	st.TotalUploaded += int64(res.Uploaded)
	st.TotalDuplicates += int64(res.Duplicates)
	st.TotalRejected += int64(res.Rejected)
	if err := saveState(stateDir, st); err != nil && runErr == nil {
		runErr = fmt.Errorf("save sync state: %w", err)
	}
	return res, runErr
}

func (s *Syncer) loop(ctx context.Context, lk *lock, cache *resolveCache, res *Result) error {
	batch := s.Cfg.BatchSize
	if batch <= 0 {
		batch = 200
	}
	for {
		// Import first so a full spool drains before the next import fills it.
		for _, imp := range s.Importers {
			n, err := imp.Import(ctx, s.Spool, lk.refresh)
			res.Imported += n
			if err != nil {
				fmt.Fprintf(s.Log, "import %s: %v\n", imp.Name(), err)
			}
		}
		recs, err := s.Spool.Claim(batch)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			return nil
		}
		lk.refresh()
		if err := s.upload(ctx, cache, recs, res); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// report tells the server what is still waiting locally. Best effort: an
// older server without the endpoint, or a failure here, must not fail a sync.
func (s *Syncer) report(ctx context.Context) {
	st, err := s.Spool.Stats()
	if err != nil {
		return
	}
	rep := model.SyncReport{
		Pending: int64(st.Pending + st.Inflight), Dead: int64(st.Dead), Dropped: st.Dropped,
		ClientVersion: model.BuildVersion(),
	}
	if s.UpgradeStatus != nil {
		rep.UpgradeTarget, rep.UpgradeError = s.UpgradeStatus()
	}
	if !st.OldestPending.IsZero() {
		t := st.OldestPending.UTC()
		rep.OldestPending = &t
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := s.Client.ReportSync(rctx, rep)
	if err != nil {
		fmt.Fprintf(s.Log, "report spool state: %v\n", err)
		return
	}
	s.serverVersion, s.reported = v, true
}

func sessionKey(r *spool.Record) string { return r.Agent + "|" + r.NativeSessionID }

// upload sends one claimed batch. Every claimed record ends in exactly one
// of Ack, Release or Reject before it returns.
func (s *Syncer) upload(ctx context.Context, cache *resolveCache, recs []*spool.Record, res *Result) error {
	// Group by session, preserving event order inside each group.
	bySession := map[string][]*spool.Record{}
	var order []string
	for _, r := range recs {
		k := sessionKey(r)
		if _, ok := bySession[k]; !ok {
			order = append(order, k)
		}
		bySession[k] = append(bySession[k], r)
	}

	// Resolve sessions; the first failure aborts the batch.
	resolved := map[string]resolvedEntry{}
	for i, k := range order {
		e, err := s.resolveSession(ctx, cache, bySession[k])
		if err != nil {
			for _, k2 := range order[i:] {
				s.releaseOrReject(bySession[k2], err, res)
			}
			// Sessions resolved before the failure are still claimed.
			for _, k2 := range order[:i] {
				s.Spool.Release(bySession[k2], "aborted: "+err.Error())
				res.Retried += len(bySession[k2])
			}
			return err
		}
		resolved[k] = e
	}

	// One batch request per project.
	byProject := map[string][]*spool.Record{}
	var projects []string
	for _, k := range order {
		pid := resolved[k].ProjectID
		if _, ok := byProject[pid]; !ok {
			projects = append(projects, pid)
		}
		byProject[pid] = append(byProject[pid], bySession[k]...)
	}
	sort.Strings(projects)

	var firstErr error
	for _, pid := range projects {
		group := byProject[pid]
		if firstErr != nil {
			s.Spool.Release(group, "aborted: "+firstErr.Error())
			res.Retried += len(group)
			continue
		}
		if err := s.send(ctx, cache, pid, group, resolved, res); err != nil {
			firstErr = err
		}
	}
	return firstErr
}

// releaseOrReject handles records whose session could not be resolved.
func (s *Syncer) releaseOrReject(recs []*spool.Record, err error, res *Result) {
	if client.Retryable(err) || client.IsStatus(err, http.StatusNotFound) {
		s.Spool.Release(recs, err.Error())
		res.Retried += len(recs)
		return
	}
	s.Spool.Reject(recs, err.Error())
	res.Rejected += len(recs)
}

func (s *Syncer) resolveSession(ctx context.Context, cache *resolveCache, recs []*spool.Record) (resolvedEntry, error) {
	first := recs[0]
	k := sessionKey(first)
	if e, ok := cache.Sessions[k]; ok {
		e.UsedAt = time.Now().UTC()
		cache.Sessions[k] = e
		return e, nil
	}
	hint := first.Project
	pid, ok := cache.Projects[hint.Key()]
	if !ok {
		var err error
		pid, err = s.Client.EnsureProject(ctx, hint.Name, hint.GitIdentity)
		if err != nil {
			return resolvedEntry{}, fmt.Errorf("ensure project %q: %w", hint.Name, err)
		}
		cache.Projects[hint.Key()] = pid
	}
	startedAt := first.OccurredAt
	for _, r := range recs {
		if r.OccurredAt.Before(startedAt) {
			startedAt = r.OccurredAt
		}
	}
	req := model.EnsureSessionRequest{
		ProjectID:       pid,
		AgentType:       first.Agent,
		NativeSessionID: first.NativeSessionID,
		StartedAt:       &startedAt,
	}
	if hint.Root != "" {
		req.Location = &model.LocationInput{AbsolutePath: hint.Root}
		if hint.GitBranch != "" {
			req.Location.GitBranch = &hint.GitBranch
		}
		if hint.GitHead != "" {
			req.Location.GitHead = &hint.GitHead
		}
	}
	resp, err := s.Client.EnsureSession(ctx, req)
	if err != nil {
		if client.IsStatus(err, http.StatusNotFound) {
			delete(cache.Projects, hint.Key()) // server lost the project; re-resolve next time
		}
		return resolvedEntry{}, fmt.Errorf("ensure session: %w", err)
	}
	e := resolvedEntry{SessionID: resp.SessionID, ProjectID: resp.ProjectID, UsedAt: time.Now().UTC()}
	cache.Sessions[k] = e
	return e, nil
}

func (s *Syncer) send(ctx context.Context, cache *resolveCache, projectID string, recs []*spool.Record, resolved map[string]resolvedEntry, res *Result) error {
	events := make([]model.Event, len(recs))
	for i, r := range recs {
		sid := resolved[sessionKey(r)].SessionID
		events[i] = model.Event{
			EventID:       r.EventID,
			SessionID:     &sid,
			EventType:     r.EventType,
			OccurredAt:    r.OccurredAt,
			PayloadJSON:   string(r.Payload),
			SchemaVersion: 1,
		}
	}

	resp, err := s.Client.BatchEvents(ctx, projectID, events)
	if err != nil {
		switch {
		case client.IsStatus(err, http.StatusNotFound):
			s.forgetProject(cache, projectID)
			s.Spool.Release(recs, err.Error())
			res.Retried += len(recs)
			return err
		case client.Retryable(err):
			s.Spool.Release(recs, err.Error())
			res.Retried += len(recs)
			return err
		case len(recs) > 1:
			// A permanent whole-batch rejection: split to isolate the bad
			// event instead of dead-lettering its neighbours.
			mid := len(recs) / 2
			if err := s.send(ctx, cache, projectID, recs[:mid], resolved, res); err != nil {
				s.Spool.Release(recs[mid:], "aborted: "+err.Error())
				res.Retried += len(recs) - mid
				return err
			}
			return s.send(ctx, cache, projectID, recs[mid:], resolved, res)
		default:
			s.Spool.Reject(recs, err.Error())
			res.Rejected += len(recs)
			return nil
		}
	}

	byID := make(map[string]*spool.Record, len(recs))
	for _, r := range recs {
		byID[r.EventID] = r
	}
	var ack, retry, reject []*spool.Record
	var retryErr string
	for _, rc := range resp.Receipts {
		r, ok := byID[rc.EventID]
		if !ok {
			continue
		}
		delete(byID, rc.EventID)
		switch {
		case rc.Status == model.ReceiptAccepted:
			ack = append(ack, r)
			res.Uploaded++
		case rc.Status == model.ReceiptDuplicate:
			ack = append(ack, r)
			res.Duplicates++
		case rc.Code == model.CodeInvalid:
			r.LastError = rc.Error
			reject = append(reject, r)
		default:
			if rc.Code == model.CodeUnknownSession {
				delete(cache.Sessions, sessionKey(r))
			}
			retryErr = rc.Code + ": " + rc.Error
			retry = append(retry, r)
		}
	}
	for _, r := range byID { // no receipt: treat as not stored
		retry = append(retry, r)
		retryErr = "missing receipt"
	}
	if err := s.Spool.Ack(ack); err != nil {
		fmt.Fprintf(s.Log, "%v\n", err)
	}
	for _, r := range reject {
		s.Spool.Reject([]*spool.Record{r}, r.LastError)
		res.Rejected++
	}
	if len(retry) > 0 {
		s.Spool.Release(retry, retryErr)
		res.Retried += len(retry)
		return fmt.Errorf("%d events not stored: %s", len(retry), retryErr)
	}
	return nil
}

func (s *Syncer) forgetProject(cache *resolveCache, projectID string) {
	for k, v := range cache.Projects {
		if v == projectID {
			delete(cache.Projects, k)
		}
	}
	for k, v := range cache.Sessions {
		if v.ProjectID == projectID {
			delete(cache.Sessions, k)
		}
	}
}
