// Command retrieval benchmarks production search, context packing and hook injection
// against a fixed, synthetic corpus in a disposable database.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/inject"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/summary"
)

const owner = "benchmark-owner"
const device = "benchmark-device"

type document struct {
	ID       string `json:"id"`
	Project  string `json:"project"`
	Session  string `json:"session"`
	Text     string `json:"text"`
	Evidence string `json:"evidence"`
}
type question struct {
	ID           string     `json:"id"`
	Split        string     `json:"split"`
	Category     string     `json:"category"`
	Query        string     `json:"query"`
	Project      string     `json:"project"`
	Scope        string     `json:"scope"`
	Selected     []string   `json:"selected"`
	Groups       [][]string `json:"evidence_groups"`
	Budget       int        `json:"budget"`
	RelevantRefs []string   `json:"relevant_refs,omitempty"`
}
type corpus struct {
	Version   string     `json:"version"`
	Projects  []string   `json:"projects"`
	Documents []document `json:"documents"`
	Questions []question `json:"questions"`
}

type evidenceScore struct {
	AnyHit           bool    `json:"any_hit"`
	AllHit           bool    `json:"all_hit"`
	Recall           float64 `json:"evidence_recall"`
	MRR              float64 `json:"mrr"`
	Negative         bool    `json:"negative"`
	FalseInjection   bool    `json:"false_injection"`
	OutOfScope       int     `json:"out_of_scope"`
	Returned         int     `json:"returned"`
	ProvenanceErrors int     `json:"provenance_errors"`
	JudgedReturned   *int    `json:"judged_returned,omitempty"`
	JudgedIrrelevant *int    `json:"judged_irrelevant,omitempty"`
}

type semanticStats struct {
	Calls     int    `json:"calls"`
	Empty     int    `json:"empty"`
	Degraded  int    `json:"degraded"`
	LastError string `json:"last_error,omitempty"`
}
type observedSemantic struct {
	svc            *embedding.Service
	stats          semanticStats
	backend        search.Semantic
	candidateFloor *float64
}

func (o *observedSemantic) MinSimilarities() (float64, float64) {
	inject, floor := o.svc.MinSimilarities()
	if o.candidateFloor != nil {
		floor = *o.candidateFloor
	}
	return inject, floor
}
func (o *observedSemantic) SemanticSearch(ctx context.Context, q string, p []string, k int) ([]search.SemanticHit, error) {
	o.stats.Calls++
	backend := o.backend
	if backend == nil {
		backend = o.svc
	}
	h, err := backend.SemanticSearch(ctx, q, p, k)
	if len(h) == 0 {
		o.stats.Empty++
	}
	if err != nil {
		o.stats.Degraded++
	}
	return h, err
}

type trackedTransport struct {
	base http.RoundTripper
	err  error
}

func (t *trackedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	t.err = err
	return resp, err
}

type receipt struct {
	Question   string               `json:"question_id"`
	Split      string               `json:"split"`
	Category   string               `json:"category"`
	Layer      string               `json:"layer"`
	Mode       string               `json:"mode"`
	Repetition int                  `json:"repetition"`
	Query      string               `json:"query"`
	DurationMS float64              `json:"duration_ms"`
	Text       string               `json:"text"`
	Characters int                  `json:"characters"`
	Bytes      int                  `json:"bytes"`
	Results    []model.SearchResult `json:"results,omitempty"`
	Pack       *contextpack.Pack    `json:"pack,omitempty"`
	Score      evidenceScore        `json:"score"`
	Semantic   semanticStats        `json:"semantic"`
	Error      string               `json:"error,omitempty"`
	Timeout    bool                 `json:"timeout"`
}

func canonical(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.NewReplacer("[", "", "]", "", "\ue000", "", "\ue001", "").Replace(s)), " "))
}

// Evidence groups are AND across groups and OR within each group. A reference
// match establishes retrieval; injection additionally requires visible evidence.
func score(q question, refs []string, text string, visible bool, docs map[string]document) evidenceScore {
	s := evidenceScore{Negative: len(q.Groups) == 0, Returned: len(refs)}
	if q.RelevantRefs != nil {
		allowed := map[string]bool{}
		for _, ref := range q.RelevantRefs {
			allowed[ref] = true
		}
		seen := map[string]bool{}
		total, bad := 0, 0
		for _, ref := range refs {
			if !seen[ref] {
				seen[ref] = true
				total++
				if !allowed[ref] {
					bad++
				}
			}
		}
		s.JudgedReturned, s.JudgedIrrelevant = &total, &bad
	}
	ranks := map[string]int{}
	for i, ref := range refs {
		d, ok := docs[ref]
		if !ok {
			continue
		}
		allowed := q.Scope == model.ScopeAllProjects || (q.Scope == model.ScopeCurrentProject && d.Project == q.Project)
		if q.Scope == model.ScopeSelectedProjects {
			for _, p := range q.Selected {
				if p == d.Project {
					allowed = true
				}
			}
		}
		if !allowed {
			s.OutOfScope++
		}
		if _, ok := ranks[ref]; !ok {
			ranks[ref] = i + 1
		}
	}
	if s.Negative {
		s.FalseInjection = strings.TrimSpace(text) != ""
		return s
	}
	hits, best := 0, 0
	for _, group := range q.Groups {
		rank := 0
		for _, ref := range group {
			r := ranks[ref]
			if r == 0 {
				continue
			}
			if visible && !strings.Contains(canonical(text), canonical(docs[ref].Evidence)) {
				continue
			}
			if rank == 0 || r < rank {
				rank = r
			}
		}
		if rank > 0 {
			hits++
			if best == 0 || rank < best {
				best = rank
			}
		}
	}
	s.AnyHit = hits > 0
	s.AllHit = hits == len(q.Groups)
	s.Recall = float64(hits) / float64(len(q.Groups))
	if best > 0 {
		s.MRR = 1 / float64(best)
	}
	return s
}

func allowedProject(q question, project string) bool {
	if q.Scope == model.ScopeAllProjects {
		return true
	}
	if q.Scope == model.ScopeCurrentProject {
		return project == q.Project
	}
	for _, p := range q.Selected {
		if p == project {
			return true
		}
	}
	return false
}
func auditProvenance(r *receipt, q question, docs map[string]document) {
	r.Score.OutOfScope = 0
	audit := func(ref, project, name string) {
		if !allowedProject(q, project) {
			r.Score.OutOfScope++
		}
		d, ok := docs[ref]
		if !ok {
			for _, candidate := range docs {
				if candidate.Session == ref {
					d = candidate
					ok = true
					break
				}
			}
		}
		if !ok || d.Project != project || (name != "" && name != project) {
			r.Score.ProvenanceErrors++
		}
	}
	for _, h := range r.Results {
		audit(h.RefID, h.ProjectID, h.ProjectName)
	}
	if r.Pack != nil && r.Text != "" {
		for _, it := range r.Pack.Items {
			audit(it.Ref, it.ProjectID, "")
		}
	}
}

func validate(c corpus) error {
	if len(c.Questions) < 1 || len(c.Questions) > 1000 {
		return fmt.Errorf("expected 1–1000 questions, got %d", len(c.Questions))
	}
	projects := map[string]bool{}
	for _, p := range c.Projects {
		if projects[p] {
			return fmt.Errorf("duplicate project %s", p)
		}
		projects[p] = true
	}
	docs := map[string]document{}
	sessions := map[string]string{}
	for _, d := range c.Documents {
		if d.ID == "" || d.Session == "" || docs[d.ID].ID != "" || (sessions[d.Session] != "" && sessions[d.Session] != d.Project) || !projects[d.Project] || d.Evidence == "" || !strings.Contains(d.Text, d.Evidence) {
			return fmt.Errorf("invalid document %q", d.ID)
		}
		docs[d.ID] = d
		sessions[d.Session] = d.Project
	}
	ids := map[string]bool{}
	// Related evidence labels may not occur in both dev and test.
	splits := map[string]string{}
	for _, q := range c.Questions {
		if q.ID == "" || ids[q.ID] || q.Query == "" || (q.Split != "dev" && q.Split != "test") || q.Budget < 0 {
			return fmt.Errorf("invalid question %q", q.ID)
		}
		ids[q.ID] = true
		if q.RelevantRefs != nil {
			allowed := map[string]bool{}
			for _, ref := range q.RelevantRefs {
				d, ok := docs[ref]
				if !ok || !allowedProject(q, d.Project) {
					return fmt.Errorf("invalid relevant source %s/%s", q.ID, ref)
				}
				allowed[ref] = true
			}
			for _, group := range q.Groups {
				for _, ref := range group {
					if !allowed[ref] {
						return fmt.Errorf("required source missing from relevance labels %s/%s", q.ID, ref)
					}
				}
			}
		}
		if q.Scope != model.ScopeCurrentProject && q.Scope != model.ScopeAllProjects && q.Scope != model.ScopeSelectedProjects {
			return fmt.Errorf("invalid scope %s", q.ID)
		}
		if q.Scope == model.ScopeSelectedProjects && len(q.Selected) == 0 {
			return fmt.Errorf("missing selected projects %s", q.ID)
		}
		for _, g := range q.Groups {
			if len(g) == 0 {
				return fmt.Errorf("empty evidence group %s", q.ID)
			}
			for _, ref := range g {
				d, ok := docs[ref]
				if !ok {
					return fmt.Errorf("unknown evidence %s", ref)
				}
				if s := splits[ref]; s != "" && s != q.Split {
					return fmt.Errorf("evidence %s leaks between splits", ref)
				}
				splits[ref] = q.Split
				if q.Scope == model.ScopeCurrentProject && d.Project != q.Project {
					return fmt.Errorf("out-of-scope label %s", q.ID)
				}
				if q.Scope == model.ScopeSelectedProjects {
					allowed := false
					for _, p := range q.Selected {
						allowed = allowed || p == d.Project
					}
					if !allowed {
						return fmt.Errorf("out-of-scope label %s", q.ID)
					}
				}
			}
		}
	}
	return nil
}

func seed(ctx context.Context, d *sql.DB, c corpus, work string) (map[string]document, map[string]string, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	anchor := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	now := model.FormatTime(anchor)
	run := func(q string, args ...any) error { _, e := tx.ExecContext(ctx, q, args...); return e }
	if err = run(`INSERT INTO users(user_id,created_at) VALUES (?,?)`, owner, now); err != nil {
		return nil, nil, err
	}
	if err = run(`INSERT INTO devices(device_id,user_id,name,platform,token_hash,created_at) VALUES (?,?,?,'linux','bench',?)`, device, owner, device, now); err != nil {
		return nil, nil, err
	}
	paths := map[string]string{}
	for _, p := range append(append([]string{}, c.Projects...), "unknown") {
		path := filepath.Join(work, "repos", p)
		if err = os.MkdirAll(filepath.Join(path, ".git"), 0700); err != nil {
			return nil, nil, err
		}
		git := "github.com/memspace-benchmark/" + p
		if err = os.WriteFile(filepath.Join(path, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://"+git+".git\n"), 0600); err != nil {
			return nil, nil, err
		}
		paths[p] = path
		if p == "unknown" {
			continue
		}
		if err = run(`INSERT INTO projects(project_id,user_id,name,canonical_git_identity,created_at,updated_at) VALUES (?,?,?,?,?,?)`, p, owner, p, git, now, now); err != nil {
			return nil, nil, err
		}
	}
	docs := map[string]document{}
	for i, doc := range c.Documents {
		dev, agent := device, "codex"
		if i%2 == 1 {
			agent = "claude-code"
		}
		if err = run(`INSERT INTO sessions(session_id,project_id,device_id,agent_type,native_session_id,started_at,status) VALUES (?,?,?,?,?,?,'ended') ON CONFLICT(session_id) DO NOTHING`, doc.Session, doc.Project, dev, agent, doc.Session, now); err != nil {
			return nil, nil, err
		}
		if err = tx.QueryRowContext(ctx, `SELECT agent_type FROM sessions WHERE session_id=?`, doc.Session).Scan(&agent); err != nil {
			return nil, nil, err
		}
		payload, _ := json.Marshal(map[string]string{"last_assistant_message": doc.Text})
		if err = run(`INSERT INTO events(event_id,session_id,project_id,device_id,event_type,occurred_at,received_at,payload_json) VALUES (?,?,?,?,'agent.turn_completed',?,?,?)`, doc.ID, doc.Session, doc.Project, dev, now, now, string(payload)); err != nil {
			return nil, nil, err
		}
		err = search.IndexEvent(ctx, tx, search.Event{EventID: doc.ID, EventType: "agent.turn_completed", ProjectID: doc.Project, SessionID: doc.Session, DeviceID: dev, AgentType: agent, OccurredAt: anchor, Payload: payload})
		if err != nil {
			return nil, nil, err
		}
		promptID := "prompt-" + doc.ID
		promptText := "请记录本次编码任务的进展"
		promptPayload, _ := json.Marshal(map[string]string{"prompt": promptText})
		if err = run(`INSERT INTO events(event_id,session_id,project_id,device_id,event_type,occurred_at,received_at,payload_json) VALUES (?,?,?,?,'user.prompt',?,?,?)`, promptID, doc.Session, doc.Project, dev, model.FormatTime(anchor.Add(-time.Minute)), now, string(promptPayload)); err != nil {
			return nil, nil, err
		}
		if err = search.IndexEvent(ctx, tx, search.Event{EventID: promptID, EventType: "user.prompt", ProjectID: doc.Project, SessionID: doc.Session, DeviceID: dev, AgentType: agent, OccurredAt: anchor.Add(-time.Minute), Payload: promptPayload}); err != nil {
			return nil, nil, err
		}
		docs[promptID] = document{ID: promptID, Project: doc.Project, Session: doc.Session, Text: promptText, Evidence: promptText}

		docs[doc.ID] = doc
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, err
	}
	return docs, paths, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	corpusPath := flag.String("corpus", "scripts/bench/data/coding-v1.json", "fixed labeled corpus")
	out := flag.String("out", "/tmp/memspace-bench", "new output directory (must not exist)")
	url := flag.String("embedding-url", "", "embedding endpoint; empty runs keyword only")
	modelName := flag.String("model", "BAAI/bge-m3", "embedding model")
	repeats := flag.Int("repeats", 3, "independent measured repetitions per question")
	timeout := flag.Duration("query-timeout", time.Second, "production query embedding deadline")
	distractorPath := flag.String("distractors", "", "optional frozen JSON array of {id,project,session,text,evidence} documents; use export_distractors.py")
	diagnosticOnly := flag.Bool("diagnose-only", false, "inspect current output with counterfactual baseline gate labels; no latency measurements")
	freeze := flag.Bool("freeze-queries", false, "freeze real query vectors for quality-only evaluation; latency excludes query embedding")
	queryVectors := flag.String("query-vectors", "", "reuse frozen query snapshot (implies -freeze-queries)")
	candidateDepths := flag.String("export-candidates", "", "optional comma-separated candidate depths for offline rerank study (e.g. 12,24)")
	candidateFloor := flag.Float64("candidate-floor", -1, "experimental candidate-only similarity floor; -1 keeps production default")
	focused := flag.Bool("focused", false, "allow a small focused regression fixture outside the default 50–100 questions")
	policyPath := flag.String("admission-policy", "", "optional policy JSON; qualification only")
	relevancePath := flag.String("relevance-labels", "", "optional SHA-bound exhaustive relevance judgments for a subset of questions")
	flag.Parse()
	var policy admission.Policy
	if *policyPath != "" {
		var e error
		policy, e = admission.Load(*policyPath)
		if e != nil {
			return e
		}
	}
	if *candidateFloor != -1 && !(*candidateFloor >= 0 && *candidateFloor <= 1) {
		return errors.New("candidate-floor must be -1 or within [0,1]")
	}
	if *queryVectors != "" {
		*freeze = true
	}
	if *freeze && *url == "" {
		return errors.New("frozen query mode requires an embedding endpoint for document indexing")
	}
	if *repeats < 1 {
		return errors.New("repeats must be positive")
	}
	raw, err := os.ReadFile(*corpusPath)
	if err != nil {
		return err
	}
	var c corpus
	if err = json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if err = validate(c); err != nil {
		return err
	}
	if !*focused && (len(c.Questions) < 50 || len(c.Questions) > 100) {
		return fmt.Errorf("expected 50–100 questions; use -focused for a focused regression, got %d", len(c.Questions))
	}
	corpusHash := sha256.Sum256(raw)
	var relevanceRaw []byte
	var relevanceHash string
	if *relevancePath != "" {
		relevanceRaw, err = os.ReadFile(*relevancePath)
		if err != nil {
			return err
		}
		if err = applyRelevanceLabels(&c, relevanceRaw, hex.EncodeToString(corpusHash[:])); err != nil {
			return err
		}
		h := sha256.Sum256(relevanceRaw)
		relevanceHash = hex.EncodeToString(h[:])
		if err = validate(c); err != nil {
			return err
		}
	}
	sourceCopies := map[string][]byte{}
	sourceHashes := map[string]string{}
	for _, path := range []string{"scripts/bench/retrieval/main.go", "scripts/bench/retrieval/diagnostics.go", "scripts/bench/retrieval/frozen.go", "scripts/bench/retrieval/candidates.go", "scripts/bench/retrieval/relevance.go", "internal/contextpack/contextpack.go", "internal/contextpack/history.go", "internal/model/model.go", "internal/admission/policy.go", "internal/search/query.go", "internal/prompt/recall.go", "internal/inject/inject.go", "internal/embedding/client.go", "internal/embedding/service.go", "go.mod", "go.sum"} {
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		sourceCopies[filepath.Base(path)] = data
		h := sha256.Sum256(data)
		sourceHashes[path] = hex.EncodeToString(h[:])
	}
	var distractorHash string
	var distractorRaw []byte
	labeledDocs := len(c.Documents)
	if *distractorPath != "" {
		b, e := os.ReadFile(*distractorPath)
		if e != nil {
			return e
		}
		distractorRaw = b
		h := sha256.Sum256(b)
		distractorHash = hex.EncodeToString(h[:])
		var extra []document
		if e = json.Unmarshal(b, &extra); e != nil {
			return e
		}
		c.Documents = append(c.Documents, extra...)
		if e = validate(c); e != nil {
			return e
		}
	}
	if err = os.Mkdir(*out, 0700); err != nil {
		return fmt.Errorf("create new output directory: %w", err)
	}
	work, err := os.MkdirTemp("", "memspace-bench-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	d, err := db.Open(filepath.Join(work, "bench.db"))
	if err != nil {
		return err
	}
	defer d.Close()
	ctx := admission.WithPolicy(context.Background(), policy)
	seedStart := time.Now()
	docs, paths, err := seed(ctx, d, c, work)
	if err != nil {
		return err
	}
	seedMS := float64(time.Since(seedStart).Microseconds()) / 1000
	var indexedBefore int
	if err = d.QueryRow(`SELECT COUNT(*) FROM search_docs`).Scan(&indexedBefore); err != nil {
		return err
	}
	var observer *observedSemantic
	modes := []string{"keyword"}
	var status any
	embedStart := time.Now()
	if *url != "" {
		svc := embedding.New(d, embedding.Config{URL: *url, Model: *modelName, QueryTimeout: *timeout})
		for {
			more, e := svc.Sweep(ctx, 128)
			if e != nil {
				return e
			}
			if !more {
				break
			}
		}
		observer = &observedSemantic{svc: svc}
		if *candidateFloor >= 0 {
			observer.candidateFloor = candidateFloor
		}
		if *freeze {
			backend, e := freezeQueries(ctx, d, svc, c, *url, *modelName, *queryVectors, *out)
			if e != nil {
				return e
			}
			observer.backend = backend
		}
		search.SetSemantic(observer)
		defer search.SetSemantic(nil)
		status = svc.Status(ctx)
		modes = append(modes, "semantic", "hybrid")
	}
	embedMS := float64(time.Since(embedStart).Microseconds()) / 1000
	if *candidateDepths != "" {
		if err = exportCandidates(ctx, d, c, *candidateDepths, *out); err != nil {
			return err
		}
	}
	if *diagnosticOnly {
		return diagnose(ctx, d, c, docs, *out)
	}
	f, err := os.OpenFile(filepath.Join(*out, "receipts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	count := 0
	var before embedding.Status
	reset := func() {
		if observer != nil {
			observer.stats = semanticStats{}
			before = observer.svc.Status(ctx)
		}
	}
	emit := func(r receipt) error {
		r.Characters = utf8.RuneCountInString(r.Text)
		r.Bytes = len(r.Text)
		if observer != nil {
			after := observer.svc.Status(ctx) // explicitly outside timed region
			failed := after.Active == nil || (after.BreakerUntil != nil && after.BreakerUntil.After(time.Now()))
			if after.LastErrorAt != nil && (before.LastErrorAt == nil || !after.LastErrorAt.Equal(*before.LastErrorAt)) {
				failed = true
			}
			if failed && observer.stats.Calls > 0 {
				observer.stats.Degraded = observer.stats.Calls
				observer.stats.LastError = after.LastError
			}
			r.Semantic = observer.stats
		}
		q := question{Project: strings.TrimPrefix(r.Question, "start-"), Scope: model.ScopeCurrentProject}
		for _, candidate := range c.Questions {
			if candidate.ID == r.Question {
				q = candidate
				break
			}
		}
		auditProvenance(&r, q, docs)
		count++
		return enc.Encode(r)
	}
	mk := func(q question, layer, mode string, rep int) receipt {
		return receipt{Question: q.ID, Split: q.Split, Category: q.Category, Query: q.Query, Layer: layer, Mode: mode, Repetition: rep}
	}
	base := func(q question) contextpack.Request {
		return contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: q.Project}, Query: q.Query, Budget: q.Budget, AgentType: "claude-code", NativeSessionID: "benchmark-new-session"}
	}
	// Each call gets an endpoint whose Close waits for cancelled handlers,
	// preventing late work from contaminating the next receipt's counters.
	callHook := func(cfg inject.Config, input []byte) ([]byte, *contextpack.Pack, error, float64) {
		var pack *contextpack.Pack
		var handlerErr error
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req contextpack.Request
			if r.URL.Path != "/api/v1/context" || json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			pack, handlerErr = contextpack.Build(admission.WithPolicy(r.Context(), policy), d, owner, device, req)
			if handlerErr != nil {
				http.Error(w, handlerErr.Error(), 500)
				return
			}
			json.NewEncoder(w).Encode(pack)
		}))
		transport := &trackedTransport{base: http.DefaultTransport}
		cfg.ServerURL = server.URL
		cfg.HTTP = &http.Client{Transport: transport}
		start := time.Now()
		output := inject.Run(ctx, cfg, input)
		ms := float64(time.Since(start).Microseconds()) / 1000
		server.Close()
		if transport.err != nil {
			return output, pack, transport.err, ms
		}
		return output, pack, handlerErr, ms
	}
	productMode := "keyword"
	if observer != nil {
		productMode = "hybrid"
	}
	// Warm the tokenizer/database/embedding endpoint with an unscored probe.
	if _, err = search.Search(ctx, d, owner, model.SearchRequest{Query: "benchmark warmup", Scope: model.ScopeAllProjects, Limit: 5}); err != nil {
		return err
	}
	for rep := 0; rep < *repeats; rep++ {
		for _, q := range c.Questions {
			for _, mode := range modes {
				reset()
				r := mk(q, "search", mode, rep)
				m := mode
				if m == "hybrid" {
					m = ""
				}
				start := time.Now()
				resp, e := search.Search(ctx, d, owner, model.SearchRequest{Query: q.Query, Scope: q.Scope, ProjectID: q.Project, ProjectIDs: q.Selected, Limit: 5, Mode: m})
				r.DurationMS = float64(time.Since(start).Microseconds()) / 1000
				if e != nil {
					r.Error = e.Error()
					r.Timeout = errors.Is(e, context.DeadlineExceeded)
				}
				var refs []string
				if resp != nil {
					r.Results = resp.Results
					for _, h := range resp.Results {
						refs = append(refs, h.RefID)
					}
				}
				r.Score = score(q, refs, "", false, docs)
				if err = emit(r); err != nil {
					return err
				}
			}
			// Automatic context is current-project only in the product. Do not
			// pretend an all-project search request can be automatically injected.
			if q.Scope == model.ScopeAllProjects {
				continue
			}
			reset()
			r := mk(q, "context", productMode, rep)
			start := time.Now()
			p, e := contextpack.Build(ctx, d, owner, device, base(q))
			r.DurationMS = float64(time.Since(start).Microseconds()) / 1000
			if e != nil {
				r.Error = e.Error()
				r.Timeout = errors.Is(e, context.DeadlineExceeded)
			}
			var refs []string
			if p != nil {
				r.Pack = p
				r.Text = p.Text
				for _, it := range p.Items {
					refs = append(refs, it.Ref)
				}
			}
			r.Score = score(q, refs, r.Text, true, docs)
			if err = emit(r); err != nil {
				return err
			}
			// Independent first calls get clean state; only the repeat call shares
			// its predecessor's state. Timing excludes state directory creation.
			state, e := os.MkdirTemp(work, "hook-state-")
			if e != nil {
				return e
			}
			cfg := inject.Config{StateDir: state, Agent: "claude-code"}
			input, _ := json.Marshal(inject.Input{SessionID: "benchmark-new-session", Cwd: paths[q.Project], HookEventName: "UserPromptSubmit", Prompt: q.Query})
			for _, layer := range []string{"hook_first", "hook_repeat"} {
				reset()
				r = mk(q, layer, productMode, rep)
				output, hookPack, hookErr, ms := callHook(cfg, input)
				r.DurationMS = ms
				var envelope struct {
					Hook struct {
						Text string `json:"additionalContext"`
					} `json:"hookSpecificOutput"`
				}
				if len(output) > 0 {
					if e = json.Unmarshal(output, &envelope); e != nil {
						return e
					}
					r.Text = envelope.Hook.Text
				}
				refs = nil
				if hookPack != nil {
					r.Pack = hookPack
					for _, it := range hookPack.Items {
						refs = append(refs, it.Ref)
					}
				}
				if hookErr != nil {
					r.Error = hookErr.Error()
					r.Timeout = errors.Is(hookErr, context.DeadlineExceeded)
				}
				r.Score = score(q, refs, r.Text, true, docs)
				if err = emit(r); err != nil {
					return err
				}
			}
			if err = os.RemoveAll(state); err != nil {
				return err
			}
		}
		fmt.Fprintf(os.Stderr, "finished repetition %d/%d (%d receipts)\n", rep+1, *repeats, count)
	}
	// Session-start has its own four deterministic fixtures. Generate summaries
	// only now so they cannot change the scored retrieval corpus.
	startFixtures := []string{"jwt_failure", "stripe_recovery", "release_gate", "stripe_notes"}
	if *focused {
		startFixtures = nil
	}
	for _, id := range startFixtures {
		doc := docs[id]
		if _, err = d.Exec(`UPDATE sessions SET status='active' WHERE session_id=?`, doc.Session); err != nil {
			return err
		}
		if _, err = summary.Generate(ctx, d, doc.Session); err != nil {
			return err
		}
		for rep := 0; rep < *repeats; rep++ {
			q := question{ID: "start-" + doc.Project, Split: "system", Category: "session_start", Project: doc.Project, Scope: model.ScopeCurrentProject, Groups: [][]string{{id}}}
			state, e := os.MkdirTemp(work, "start-")
			if e != nil {
				return e
			}
			reset()
			r := mk(q, "hook_session_start", productMode, rep)
			input, _ := json.Marshal(inject.Input{SessionID: "new-start", Cwd: paths[doc.Project], HookEventName: "SessionStart"})
			b, hookPack, hookErr, ms := callHook(inject.Config{StateDir: state, Agent: "claude-code"}, input)
			r.DurationMS = ms
			var envelope struct {
				Hook struct {
					Text string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if len(b) > 0 {
				if err = json.Unmarshal(b, &envelope); err != nil {
					return err
				}
				r.Text = envelope.Hook.Text
			}
			var refs []string
			if hookPack != nil {
				r.Pack = hookPack
				for _, it := range hookPack.Items {
					if it.Ref == doc.Session {
						refs = append(refs, id)
					} else {
						refs = append(refs, it.Ref)
					}
				}
			}
			if hookErr != nil {
				r.Error = hookErr.Error()
				r.Timeout = errors.Is(hookErr, context.DeadlineExceeded)
			}
			r.Score = score(q, refs, r.Text, true, docs)
			if err = emit(r); err != nil {
				return err
			}
			os.RemoveAll(state)
		}
	}
	var fixtureAnchor string
	if err = d.QueryRow(`SELECT MIN(occurred_at) FROM events`).Scan(&fixtureAnchor); err != nil {
		return err
	}
	var docsN, chunksN int
	d.QueryRow(`SELECT COUNT(*) FROM search_docs`).Scan(&docsN)
	d.QueryRow(`SELECT COUNT(*) FROM embedding_chunks`).Scan(&chunksN)
	git := func(args ...string) string {
		b, e := exec.Command("git", args...).Output()
		if e != nil {
			return "unavailable"
		}
		return strings.TrimSpace(string(b))
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	manifest := map[string]any{"schema_version": 1, "focused_fixture": *focused, "candidate_depths": *candidateDepths, "experimental_candidate_floor": *candidateFloor, "quality_only_frozen_queries": *freeze, "earliest_fixture_event_at": fixtureAnchor, "source_sha256": sourceHashes, "corpus_version": c.Version, "corpus_sha256": hex.EncodeToString(corpusHash[:]), "distractors_sha256": distractorHash, "questions": len(c.Questions), "labeled_documents": labeledDocs, "fixture_recipe": "one indexed generic user.prompt and one indexed agent.turn_completed per corpus document; last hour, ended sessions; standard runs add four summaries only for session-start fixtures, focused runs skip them", "distractor_documents": len(c.Documents) - labeledDocs, "documents_before_session_start": indexedBefore, "documents_after_session_start": docsN, "embedding_chunks_before_session_start": chunksN, "repeats": *repeats, "receipts": count, "modes": modes, "embedding": status, "embedding_at_end": func() any {
		if observer != nil {
			return observer.svc.Status(ctx)
		}
		return nil
	}(), "query_embedding_timeout_ms": timeout.Milliseconds(), "seed_ms": seedMS, "embedding_build_ms": embedMS, "git_commit": git("rev-parse", "HEAD"), "git_status": git("status", "--short"), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cpu_count": runtime.NumCPU(), "go_heap_bytes_end": mem.Alloc, "at": time.Now().UTC(), "limits": []string{"synthetic authored labels, not independently reviewed; no claim about real-world task success", "warm sequential runs on loopback; no CLI startup, production auth or LAN latency", "context and hooks use current project; all-project query excluded", "context honors per-question budget; hooks use production default budget", "hook_repeat deliberately shares delivered state; not included in independent recall", "token counts require tokenize_report.py; characters are not tokens", "corpus is small unless optional distractors supplied; not a production-scale latency benchmark", "no background ingest, concurrent load, failure injection, or coding-agent execution"}}
	manifest["admission_policy"] = policy
	modelMin, _ := search.MinSimilarities()
	resolvedPolicy, _ := policy.Resolve(modelMin)
	manifest["resolved_admission_policy"] = resolvedPolicy
	if relevanceHash != "" {
		manifest["relevance_labels_sha256"] = relevanceHash
		n := 0
		for _, q := range c.Questions {
			if q.RelevantRefs != nil {
				n++
			}
		}
		manifest["judged_questions"] = n
	}
	for _, name := range []string{"bench.db", "bench.db-wal", "bench.db-shm"} {
		if info, e := os.Stat(filepath.Join(work, name)); e == nil {
			manifest[name+"_bytes"] = info.Size()
		}
	}
	if b, e := os.ReadFile("/proc/cpuinfo"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "model name") {
				manifest["cpu_model"] = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
				break
			}
		}
	}
	if b, e := os.ReadFile("/proc/self/status"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmHWM:") {
				manifest["peak_rss"] = strings.TrimSpace(strings.TrimPrefix(line, "VmHWM:"))
			}
		}
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(*out, "manifest.json"), append(b, '\n'), 0600); err != nil {
		return err
	}
	// Freeze exact corpus alongside receipts so a future edited source file
	// cannot silently change the meaning of an old result.
	if err = os.WriteFile(filepath.Join(*out, "corpus.json"), raw, 0600); err != nil {
		return err
	}
	if relevanceRaw != nil {
		if err = os.WriteFile(filepath.Join(*out, "relevance-labels.json"), relevanceRaw, 0600); err != nil {
			return err
		}
	}
	for name, data := range sourceCopies {
		if err = os.WriteFile(filepath.Join(*out, name), data, 0600); err != nil {
			return err
		}
	}
	if len(distractorRaw) > 0 {
		if err = os.WriteFile(filepath.Join(*out, "distractors.json"), distractorRaw, 0600); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %d receipts to %s\n", count, *out)
	return nil
}
