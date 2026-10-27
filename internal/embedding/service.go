package embedding

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/chux0519/memspace/internal/jobs"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
)

// Circuit breaker for query embeddings.
const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
)

// JobKind is the job queue kind of the embedding sweep.
const JobKind = "embedding.sweep"

// Sweep scheduling: soon after new documents arrive, at most every
// maxSweepDelay while ingest keeps going.
var (
	sweepDelay    = 3 * time.Second
	maxSweepDelay = 20 * time.Second
)

// Enqueue schedules an embedding sweep (deduplicated).
func Enqueue(ctx context.Context, x jobs.Execer) error {
	return jobs.Enqueue(ctx, x, JobKind, JobKind, "{}", time.Now().UTC().Add(sweepDelay), maxSweepDelay)
}

// Config configures the service (from config.EmbeddingConfig).
type Config struct {
	URL          string
	Model        string
	APIKey       string
	Dimensions   int
	QueryPrefix  string // "" = automatic ("query: " for Snowflake arctic-embed)
	BatchSize    int
	QueryTimeout time.Duration
	// MinSimilarity and SearchMinSimilarity override the model's calibrated
	// thresholds (0 = calibrated default; see ThresholdsFor).
	MinSimilarity       float64
	SearchMinSimilarity float64
}

// Service embeds documents in the background and answers semantic queries.
// A nil *Service means semantic search is disabled.
type Service struct {
	db     *sql.DB
	cfg    Config
	client *Client
	logf   func(string, ...any)

	mu           sync.Mutex
	info         *ServiceInfo // current service identity
	infoAt       time.Time
	lastErr      string
	lastErrAt    time.Time
	breakerUntil time.Time
	queryFails   int // consecutive query failures
	enqueue      func(ctx context.Context) error
}

// New returns nil when cfg.URL is empty.
func New(db *sql.DB, cfg Config) *Service {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	if cfg.BatchSize <= 0 {
		// Small batches: services like embeddings_server run one request at
		// a time, and a query must not wait long behind a background batch.
		cfg.BatchSize = 8
	}
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = time.Second
	}
	s := &Service{
		db:     db,
		cfg:    cfg,
		client: &Client{URL: cfg.URL, Model: cfg.Model, APIKey: cfg.APIKey, Dimensions: cfg.Dimensions},
		logf:   log.Printf,
	}
	s.enqueue = func(ctx context.Context) error { return Enqueue(ctx, db) }
	return s
}

func (s *Service) fail(err error) error {
	s.mu.Lock()
	s.lastErr, s.lastErrAt = err.Error(), time.Now()
	s.mu.Unlock()
	return err
}

// describe returns the service identity, refreshed every 5 minutes.
func (s *Service) describe(ctx context.Context, force bool) (*ServiceInfo, error) {
	s.mu.Lock()
	if s.info != nil && !force && time.Since(s.infoAt) < 5*time.Minute {
		info := *s.info
		s.mu.Unlock()
		return &info, nil
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	info, err := s.client.Describe(ctx, s.cfg.QueryPrefix)
	if err != nil {
		return nil, s.fail(err)
	}
	if info.QueryPrefix == "" && strings.Contains(strings.ToLower(info.Model), "arctic-embed") {
		info.QueryPrefix = "query: " // Snowflake arctic-embed is trained with this query prefix
	}
	s.mu.Lock()
	s.info, s.infoAt = &info, time.Now()
	s.mu.Unlock()
	return &info, nil
}

// Handler is the jobs.Handler for JobKind.
func (s *Service) Handler() jobs.Handler {
	return func(ctx context.Context, _ string) error {
		more, err := s.Sweep(ctx, 512)
		if err != nil {
			return err
		}
		if more {
			// Re-enqueueing the running job makes it run again right after.
			return jobs.Enqueue(ctx, s.db, JobKind, JobKind, "{}", time.Now().UTC(), 0)
		}
		return nil
	}
}

// Sweep embeds up to maxDocs pending documents into the current service's
// space and activates it once complete. It reports whether work remains.
func (s *Service) Sweep(ctx context.Context, maxDocs int) (bool, error) {
	info, err := s.describe(ctx, false)
	if err != nil {
		return false, err
	}
	space, err := EnsureSpace(ctx, s.db, *info)
	if err != nil {
		return false, s.fail(err)
	}
	if _, err := CleanOrphans(ctx, s.db); err != nil {
		return false, s.fail(err)
	}
	done := 0
	for done < maxDocs {
		docs, err := PendingDocs(ctx, s.db, space, min(s.cfg.BatchSize, maxDocs-done))
		if err != nil {
			return false, s.fail(err)
		}
		if len(docs) == 0 {
			break
		}
		if err := s.embedDocs(ctx, space, docs); err != nil {
			return false, s.fail(err)
		}
		done += len(docs)
	}
	if _, err := Activate(ctx, s.db, space); err != nil {
		return false, s.fail(err)
	}
	pending, _, err := CountPending(ctx, s.db, space)
	if err != nil {
		return false, err
	}
	return pending > 0, nil
}

func (s *Service) embedDocs(ctx context.Context, space *Space, docs []PendingDoc) error {
	var texts []string
	var items []Embedded
	byDoc := map[int64]string{}
	for _, d := range docs {
		byDoc[d.DocID] = d.Text
		for i, c := range Chunk(d.Text) {
			items = append(items, Embedded{DocID: d.DocID, ProjectID: d.ProjectID, ChunkNo: i, Text: c})
			texts = append(texts, c)
		}
	}
	for start := 0; start < len(texts); start += s.cfg.BatchSize {
		end := min(start+s.cfg.BatchSize, len(texts))
		cctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
		vecs, err := s.client.Embed(cctx, texts[start:end])
		cancel()
		if err != nil {
			return err
		}
		for i, v := range vecs {
			items[start+i].Vector = v
		}
	}
	return StoreChunks(ctx, s.db, space, items, byDoc)
}

// Similar embeds query and returns the nearest documents in the active
// space. It returns nothing (and no error) when semantic search cannot be
// used right now: no active space yet, the service changed and the new space
// is still building, or the service recently failed (circuit breaker).
func (s *Service) Similar(ctx context.Context, query string, projectIDs []string, k int) ([]Hit, error) {
	s.mu.Lock()
	open := time.Now().Before(s.breakerUntil)
	info := s.info
	s.mu.Unlock()
	if open || info == nil {
		return nil, nil
	}
	space, err := ActiveSpace(ctx, s.db)
	if err != nil || space == nil || space.Fingerprint != info.Fingerprint() {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()
	vecs, err := s.client.Embed(qctx, []string{info.QueryPrefix + query})
	if err != nil {
		// A single slow answer (e.g. queued behind a background batch) just
		// skips semantic results for this query; repeated failures mean the
		// service is down, so stop waiting on it for a while.
		s.mu.Lock()
		s.queryFails++
		if s.queryFails >= breakerThreshold {
			s.breakerUntil = time.Now().Add(breakerCooldown)
		}
		s.mu.Unlock()
		s.fail(fmt.Errorf("query embedding: %w", err))
		return nil, nil // degrade to keyword search
	}
	s.mu.Lock()
	s.queryFails = 0
	s.mu.Unlock()
	return KNN(ctx, s.db, space, vecs[0], projectIDs, k)
}

// Start describes the service (so queries can be served) and schedules a
// sweep; errors are recorded and retried by the job queue.
func (s *Service) Start(ctx context.Context) {
	if _, err := s.describe(ctx, true); err != nil {
		s.logf("embedding: service unavailable: %v", err)
	}
	if err := s.enqueue(ctx); err != nil {
		s.logf("embedding: schedule sweep: %v", err)
	}
}

// Status describes the semantic index.
type Status struct {
	Enabled      bool         `json:"enabled"`
	URL          string       `json:"url,omitempty"`
	Service      *ServiceInfo `json:"service,omitempty"`
	Active       *SpaceInfo   `json:"active_space,omitempty"`
	Building     *SpaceInfo   `json:"building_space,omitempty"`
	Thresholds   *Thresholds  `json:"thresholds,omitempty"`
	LastError    string       `json:"last_error,omitempty"`
	LastErrorAt  *time.Time   `json:"last_error_at,omitempty"`
	BreakerUntil *time.Time   `json:"breaker_until,omitempty"`
}

// SpaceInfo summarizes a space's coverage.
type SpaceInfo struct {
	Model       string `json:"model"`
	Dimensions  int    `json:"dimensions"`
	Fingerprint string `json:"fingerprint"`
	Docs        int    `json:"docs_embedded"`
	Pending     int    `json:"docs_pending"`
}

// Status reports the service and index state.
func (s *Service) Status(ctx context.Context) Status {
	if s == nil {
		return Status{}
	}
	st := Status{Enabled: true, URL: s.cfg.URL}
	t := s.Thresholds()
	st.Thresholds = &t
	s.mu.Lock()
	if s.info != nil {
		info := *s.info
		st.Service = &info
	}
	if s.lastErr != "" {
		st.LastError = redact.String(s.lastErr)
		t := s.lastErrAt
		st.LastErrorAt = &t
	}
	if time.Now().Before(s.breakerUntil) {
		t := s.breakerUntil
		st.BreakerUntil = &t
	}
	s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT `+spaceCols+` FROM embedding_spaces WHERE status <> 'retired' ORDER BY space_id`)
	if err != nil {
		return st
	}
	var spaces []*Space
	for rows.Next() {
		if sp, err := scanSpace(rows); err == nil {
			spaces = append(spaces, sp)
		}
	}
	rows.Close()
	for _, sp := range spaces {
		pending, total, _ := CountPending(ctx, s.db, sp)
		si := &SpaceInfo{Model: sp.Model, Dimensions: sp.Dimensions, Fingerprint: sp.Fingerprint, Docs: total - pending, Pending: pending}
		if sp.Status == "active" {
			st.Active = si
		} else {
			st.Building = si
		}
	}
	return st
}

// Thresholds returns the similarity thresholds for the current service's
// model. Before the service has been described, the configured model name
// (if any) decides.
func (s *Service) Thresholds() Thresholds {
	model := s.cfg.Model
	s.mu.Lock()
	if s.info != nil {
		model = s.info.Model
	}
	s.mu.Unlock()
	return ThresholdsFor(model, s.cfg.MinSimilarity, s.cfg.SearchMinSimilarity)
}

// MinSimilarities implements search.Semantic.
func (s *Service) MinSimilarities() (inject, searchFloor float64) {
	t := s.Thresholds()
	return t.Inject, t.SearchFloor
}

// SemanticSearch implements search.Semantic.
func (s *Service) SemanticSearch(ctx context.Context, query string, projectIDs []string, k int) ([]search.SemanticHit, error) {
	hits, err := s.Similar(ctx, query, projectIDs, k)
	if err != nil {
		return nil, err
	}
	out := make([]search.SemanticHit, len(hits))
	for i, h := range hits {
		out[i] = search.SemanticHit{DocID: h.DocID, Text: h.ChunkText, Similarity: h.Similarity}
	}
	return out, nil
}

// DisabledHandler is the job handler when semantic search is off: sweeps
// enqueued by ingest complete immediately.
func DisabledHandler(context.Context, string) error { return nil }
