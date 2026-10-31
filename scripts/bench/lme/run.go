package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/contextpack"
	memdb "github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/server"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

const datasetSHA = "d6f21ea9d60a0d56f34a05b609c79c88a451d2ae03597821ea3d5a9678c3a442"
const evalOwner = "lme-benchmark-owner"

type indexState struct {
	DataSHA             string            `json:"data_sha256"`
	Ready               bool              `json:"ready"`
	Projects            map[string]string `json:"projects"`
	Sessions            map[string]string `json:"sessions"`
	Device, Token       string
	Duplicates          int
	ProductionSourceSHA string
}
type scoreRow struct {
	SemanticDocs int      `json:"semantic_docs_before_floor,omitempty"`
	QuestionID   string   `json:"question_id"`
	Type         string   `json:"question_type"`
	Answerable   bool     `json:"answerable"`
	Mode         string   `json:"mode"`
	All5         *float64 `json:"recall_all_at_5"`
	Fractional5  *float64 `json:"fractional_at_5"`
	Any5         *float64 `json:"any_hit_at_5"`
	All10        *float64 `json:"recall_all_at_10"`
	NDCG10       *float64 `json:"ndcg_at_10"`
	Returned     int      `json:"returned_sessions"`
	ReturnedDocs int      `json:"returned_docs"`
	Under10      bool     `json:"under_10"`
	Duration     float64  `json:"duration_ms"`
	Error        string   `json:"error,omitempty"`
	Empty        bool     `json:"empty"`
	Characters   int      `json:"characters,omitempty"`
	Tokens       int      `json:"estimated_tokens,omitempty"`
	BelowFloor   int      `json:"below_search_floor,omitempty"`
}

func sourceSHA() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "unavailable"
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
	var paths []string
	filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	var all []byte
	for _, p := range paths {
		rel, _ := filepath.Rel(root, p)
		b, e := os.ReadFile(p)
		if e != nil {
			return "unavailable"
		}
		all = append(all, []byte(rel+"\x00")...)
		all = append(all, b...)
	}
	if len(paths) == 0 {
		return "unavailable"
	}
	return benchutil.SHA(all)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	data := fs.String("data", "", "dataset")
	path := fs.String("db", "", "persistent disposable index database")
	out := fs.String("out", "", "new private results directory")
	url := fs.String("embed-url", "http://127.0.0.1:18792/v1/embeddings", "cached endpoint")
	mode := fs.String("mode", "fts", "fts|vector|hybrid|pack")
	limit := fs.Int("limit", 0, "smoke question limit; 0 means all500")
	noFloor := fs.Bool("no-floor", false, "diagnostic hybrid floor disabled; not baseline")
	isolate := fs.Bool("isolate-projects", false, "independent per-question databases, agentmemory corpus isolation")
	questionID := fs.String("question-id", "", "one question, used by isolated runner")
	tracePack := fs.Bool("trace-pack", false, "record actual context stages; pack only, diagnostic timing")
	reuseSource := fs.String("reuse-index-source", "", "explicitly replay a ready index from this producer SHA; verify ingestion code is unchanged")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("limit must not be negative")
	}
	if *reuseSource != "" {
		b, err := hex.DecodeString(*reuseSource)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("reuse-index-source must be a SHA256")
		}
	}
	if *mode != "fts" && *mode != "vector" && *mode != "hybrid" && *mode != "pack" {
		return fmt.Errorf("invalid mode")
	}
	if *tracePack && *mode != "pack" {
		return fmt.Errorf("trace-pack requires pack mode")
	}
	if *path == "" {
		return fmt.Errorf("-db is required")
	}
	a, _ := filepath.Abs(*path)
	home, _ := os.UserHomeDir()
	if a == filepath.Join(home, ".local/share/memspace/memspace.db") {
		return fmt.Errorf("production DB forbidden")
	}
	if _, e := os.Stat(a); e == nil {
		if e = benchutil.RequireCopy(a); e != nil {
			return e
		}
	}
	qs, e := runDataset(*data)
	if e != nil {
		return e
	}
	if *questionID != "" {
		var selected []question
		for _, q := range qs {
			if q.ID == *questionID {
				selected = append(selected, q)
			}
		}
		if len(selected) != 1 {
			return fmt.Errorf("unknown question ID")
		}
		qs = selected
	}
	if *limit > 0 {
		qs = qs[:min(*limit, len(qs))]
	}
	if *isolate {
		return isolatedRun(*data, a, *out, *url, *mode, *noFloor, qs, *reuseSource, *tracePack)
	}
	if *reuseSource != "" {
		if err := benchutil.RequireCopy(a); err != nil {
			return fmt.Errorf("reuse requires an existing disposable DB: %w", err)
		}
		if _, err := os.Stat(a + ".state.json"); err != nil {
			return fmt.Errorf("reuse requires an existing index: %w", err)
		}
	}
	if e = benchutil.NewDir(*out); e != nil {
		return e
	}
	d, e := memdb.Open(a)
	if e != nil {
		return e
	}
	defer d.Close()
	statePath := a + ".state.json"
	state := indexState{DataSHA: datasetSHA, Projects: map[string]string{}, Sessions: map[string]string{}, ProductionSourceSHA: sourceSHA()}
	b, err := os.ReadFile(statePath)
	if err == nil {
		if e = json.Unmarshal(b, &state); e != nil {
			return e
		}
		if err := validateIndexProvenance(state, sourceSHA(), *reuseSource); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	ctx := context.Background()
	if !state.Ready {
		if e = ingest(ctx, d, qs, &state, statePath); e != nil {
			return e
		}
	} else {
		for _, q := range qs {
			if state.Projects[q.ID] == "" {
				return fmt.Errorf("index missing question %s", q.ID)
			}
		}
	}
	manifest := benchutil.Manifest()
	manifest["dataset_sha256"] = datasetSHA
	manifest["index_state_sha256"] = benchutil.FileSHA(statePath)
	manifest["index_producer_source_sha256"] = state.ProductionSourceSHA
	manifest["index_reuse_expected_source_sha256"] = *reuseSource
	manifest["production_source_sha256"] = sourceSHA()
	manifest["db_counts"] = benchutil.Counts(ctx, d)
	manifest["db_counts_before_embedding"] = manifest["db_counts"]
	manifest["mode"] = *mode
	manifest["trace_pack"] = *tracePack
	infoClient := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	defer infoClient.CloseIdleConnections()
	if infoResp, err := infoClient.Get(strings.TrimSuffix(*url, "/v1/embeddings") + "/info"); err == nil {
		var info map[string]any
		json.NewDecoder(infoResp.Body).Decode(&info)
		infoResp.Body.Close()
		manifest["provider_info"] = info
	}
	manifest["diagnostic_no_floor"] = *noFloor
	manifest["question_count"] = len(qs)
	manifest["duplicate_sources_coalesced"] = state.Duplicates
	manifest["ranking_document_limit"] = 100
	manifest["percentiles"] = "linear interpolation; all attempts, including failures"
	manifest["token_scope"] = "pack only; embedding.EstimateTokens, not billed tokens"
	manifest["limits"] = []string{"real event ingestion/redaction/DocFromEvent/chunking, not full supplied-session component indexing", "FTS statistics cover this database; use -isolate-projects for independent per-question indexes", "historical timestamps unchanged; 72h session-card cutoff retained", "pack scores source sessions in final items, not completeness of answer-bearing text", "cached embedding latency is not production latency; hook benchmark measures live CPU provider", "vector Similar has no threshold; hybrid/pack keep production thresholds"}
	var emb *embedding.Service
	var observer *observedSemantic
	if *mode != "fts" {
		emb = embedding.New(d, embedding.Config{URL: *url, Model: "BAAI/bge-m3", BatchSize: 64, QueryTimeout: time.Second})
		emb.Start(ctx)
		for {
			more, err := emb.Sweep(ctx, 2048)
			if err != nil {
				return err
			}
			st := emb.Status(ctx)
			log.Printf("embedding pending: %+v", st.Active)
			if !more {
				break
			}
		}
		st := emb.Status(ctx)
		if st.Active == nil || st.Service == nil || st.Active.Fingerprint != st.Service.Fingerprint() {
			return fmt.Errorf("index did not activate")
		}
		space, err := embedding.ActiveSpace(ctx, d)
		if err != nil {
			return err
		}
		pending, total, err := embedding.CountPending(ctx, d, space)
		if err != nil || pending != 0 {
			return fmt.Errorf("embedding incomplete %d/%d: %v", pending, total, err)
		}
		manifest["embedding"] = st
		observer = &observedSemantic{Service: emb, DisableFloor: *noFloor}
		search.SetSemantic(observer)
	} else {
		search.SetSemantic(nil)
	}
	defer search.SetSemantic(nil)
	if *noFloor {
		if emb == nil || *mode != "hybrid" {
			return fmt.Errorf("no-floor only supported in hybrid diagnostic")
		}
		search.SetSemantic(observer)
	}
	sf, e := os.OpenFile(filepath.Join(*out, "scores.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer sf.Close()
	sg := gzip.NewWriter(sf)
	defer sg.Close()
	ef, e := os.OpenFile(filepath.Join(*out, "evidence.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer ef.Close()
	eg := gzip.NewWriter(ef)
	defer eg.Close()
	se, ee := json.NewEncoder(sg), json.NewEncoder(eg)
	var scores []scoreRow
	for i, q := range qs {
		log.Printf("%s query %d/%d started", *mode, i+1, len(qs))
		pid := state.Projects[q.ID]
		if observer != nil {
			observer.Total = 0
			observer.Below = 0
		}
		at := time.Now()
		var docs []int64
		var refs []string
		var pack *contextpack.Pack
		var trace []contextpack.TraceEvent
		var queryErr error
		var below int
		if *mode == "vector" {
			h, err := emb.Similar(ctx, q.Question, []string{pid}, 100)
			queryErr = err
			_, floor := emb.MinSimilarities()
			for _, x := range h {
				docs = append(docs, x.DocID)
				if x.Similarity < floor {
					below++
				}
			}
		} else if *mode == "pack" {
			req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: pid}, Query: q.Question}
			if *tracePack {
				pack, queryErr = contextpack.BuildWithTrace(ctx, d, evalOwner, state.Device, req, func(event contextpack.TraceEvent) {
					trace = append(trace, event)
				})
			} else {
				pack, queryErr = contextpack.Build(ctx, d, evalOwner, state.Device, req)
			}
			if pack != nil {
				for _, it := range pack.Items {
					refs = append(refs, it.Ref)
				}
			}
		} else {
			r, err := search.Search(ctx, d, evalOwner, model.SearchRequest{Query: q.Question, Scope: model.ScopeCurrentProject, ProjectID: pid, Limit: 100})
			queryErr = err
			if r != nil {
				for _, h := range r.Results {
					docs = append(docs, h.DocID)
				}
			}
		}
		duration := float64(time.Since(at).Microseconds()) / 1000
		if emb != nil {
			st := emb.Status(ctx)
			if st.LastErrorAt != nil && !st.LastErrorAt.Before(at) {
				queryErr = fmt.Errorf("semantic degraded: %s", st.LastError)
			}
			if st.BreakerUntil != nil && st.BreakerUntil.After(at) {
				queryErr = fmt.Errorf("semantic circuit breaker open")
			}
		}
		var ranked []string
		seen := map[string]bool{}
		appendSession := func(sid string) {
			source, ok := state.Sessions[sid]
			if ok && !seen[source] {
				seen[source] = true
				ranked = append(ranked, source)
			}
		}
		for _, id := range docs {
			var sid string
			err := d.QueryRowContext(ctx, `SELECT coalesce(session_id,'') FROM search_docs WHERE doc_id=? AND project_id=?`, id, pid).Scan(&sid)
			if err != nil {
				queryErr = err
				continue
			}
			appendSession(sid)
		}
		for _, ref := range refs {
			if _, ok := state.Sessions[ref]; ok {
				appendSession(ref)
				continue
			}
			var sid string
			err := d.QueryRowContext(ctx, `SELECT coalesce(session_id,'') FROM search_docs WHERE ref_id=? AND project_id=? ORDER BY doc_id LIMIT 1`, ref, pid).Scan(&sid)
			if err != nil {
				queryErr = err
				continue
			}
			appendSession(sid)
		}
		if len(ranked) > 10 {
			ranked = ranked[:10]
		}
		r := score(q, ranked)
		r.Mode = *mode
		r.Duration = duration
		r.ReturnedDocs = len(docs) + len(refs)
		r.BelowFloor = below
		if observer != nil && *mode != "vector" {
			r.SemanticDocs = observer.Total
			r.BelowFloor = observer.Below
		}
		if queryErr != nil {
			r.Error = queryErr.Error()
			r.All5 = nil
			r.Fractional5 = nil
			r.Any5 = nil
			r.All10 = nil
			r.NDCG10 = nil
		}
		if pack != nil {
			r.Characters = len([]rune(pack.Text))
			r.Tokens = embedding.EstimateTokens(pack.Text)
		}
		scores = append(scores, r)
		if e = se.Encode(r); e != nil {
			return e
		}
		traceSources := map[string]string{}
		for _, stage := range trace {
			for _, hit := range stage.Hits {
				if source, ok := state.Sessions[hit.SessionID]; ok {
					traceSources[hit.SessionID] = source
				}
			}
			for _, item := range stage.Items {
				if source, ok := state.Sessions[item.Ref]; ok {
					traceSources[item.Ref] = source
				}
			}
		}
		if e = ee.Encode(map[string]any{"question_id": q.ID, "project_id": pid, "returned_sessions": ranked, "doc_ids": docs, "pack": pack, "trace": trace, "trace_session_sources": traceSources, "raw_dataset_answer_session_ids": q.AnswerIDs, "gold_sessions": func() []string {
			if r.Answerable {
				return q.AnswerIDs
			}
			return []string{}
		}()}); e != nil {
			return e
		}
		log.Printf("%s query %d/%d done %.1fms %d sessions", *mode, i+1, len(qs), duration, len(ranked))
		if (i+1)%10 == 0 {
			if e = sg.Flush(); e != nil {
				return e
			}
			if e = eg.Flush(); e != nil {
				return e
			}
		}
	}
	if e = sg.Close(); e != nil {
		return e
	}
	if e = eg.Close(); e != nil {
		return e
	}
	manifest["scores_sha256"] = benchutil.FileSHA(filepath.Join(*out, "scores.ndjson.gz"))
	manifest["evidence_sha256"] = benchutil.FileSHA(filepath.Join(*out, "evidence.ndjson.gz"))
	manifest["end_environment"] = benchutil.Manifest()
	manifest["db_counts_after_query"] = benchutil.Counts(ctx, d)
	if emb != nil {
		manifest["embedding_after"] = emb.Status(ctx)
	}
	if e = benchutil.JSON(filepath.Join(*out, "manifest.json"), manifest); e != nil {
		return e
	}
	return benchutil.JSON(filepath.Join(*out, "summary.json"), aggregate(scores))
}

func validateIndexProvenance(state indexState, current, reuse string) error {
	if state.DataSHA != datasetSHA {
		return fmt.Errorf("index dataset provenance mismatch")
	}
	if reuse != "" {
		if !state.Ready || state.ProductionSourceSHA != reuse {
			return fmt.Errorf("index reuse requires the expected ready producer")
		}
	} else if state.ProductionSourceSHA != current {
		return fmt.Errorf("index provenance mismatch")
	}
	return nil
}

type observedSemantic struct {
	*embedding.Service
	Total, Below int
	DisableFloor bool
}

func (o *observedSemantic) SemanticSearch(ctx context.Context, q string, projects []string, k int) ([]search.SemanticHit, error) {
	h, e := o.Service.SemanticSearch(ctx, q, projects, k)
	_, floor := o.Service.MinSimilarities()
	o.Total += len(h)
	for _, x := range h {
		if x.Similarity < floor {
			o.Below++
		}
	}
	return h, e
}
func (o *observedSemantic) MinSimilarities() (float64, float64) {
	a, b := o.Service.MinSimilarities()
	if o.DisableFloor {
		b = -1
	}
	return a, b
}
func date(s string) (time.Time, error) {
	for _, layout := range []string{"2006/01/02 (Mon) 15:04", "2006/01/02 (Mon) 15:04:05", "2006/01/02 (Monday) 15:04", "2006/01/02 (Monday) 15:04:05", "2006/01/02 (Monday)", time.RFC3339, "2006-01-02", "2006/01/02"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid dataset date %q", s)
}
func ingest(ctx context.Context, d *sql.DB, qs []question, state *indexState, statePath string) error {
	if _, e := d.ExecContext(ctx, `INSERT OR IGNORE INTO users(user_id) VALUES (?)`, evalOwner); e != nil {
		return e
	}
	if state.Device == "" {
		state.Device = "lme-device"
		plain, hashed, e := auth.GenerateToken()
		if e != nil {
			return e
		}
		state.Token = plain
		if _, e = d.ExecContext(ctx, `INSERT INTO devices(device_id,user_id,name,platform,token_hash) VALUES (?,?,'lme','linux',?)`, state.Device, evalOwner, hashed); e != nil {
			return e
		}
		if e = benchutil.JSON(statePath, state); e != nil {
			return e
		}
	}
	ts := httptest.NewServer(server.New(d, evalOwner))
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Minute}
	post := func(path string, value, out any) error {
		b, e := json.Marshal(value)
		if e != nil {
			return e
		}
		r, _ := http.NewRequestWithContext(ctx, "POST", ts.URL+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+state.Token)
		resp, e := client.Do(r)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1000))
			return fmt.Errorf("ingest %s HTTP%d %s", path, resp.StatusCode, b)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	for qi, q := range qs {
		if _, ok := state.Projects[q.ID]; ok {
			continue
		}
		if len(q.Sessions) != len(q.SessionIDs) || len(q.Dates) != len(q.Sessions) {
			return fmt.Errorf("invalid source arrays %s", q.ID)
		}
		var project model.EnsureProjectResponse
		if e := post("/api/v1/projects", model.EnsureProjectRequest{Name: "lme-" + q.ID}, &project); e != nil {
			return e
		}
		pid := project.ProjectID
		seen := map[string]string{}
		for si, turns := range q.Sessions {
			sid := q.SessionIDs[si]
			raw, _ := json.Marshal(turns)
			digest := benchutil.SHA(raw)
			if prior, ok := seen[sid]; ok {
				if prior != digest {
					return fmt.Errorf("conflicting repeated session %s", sid)
				}
				state.Duplicates++
				continue
			}
			seen[sid] = digest
			at, e := date(q.Dates[si])
			if e != nil {
				return e
			}
			var session model.EnsureSessionResponse
			native := "lme-" + q.ID + "-" + sid
			if e = post("/api/v1/sessions", model.EnsureSessionRequest{ProjectID: pid, AgentType: "lme", NativeSessionID: native, StartedAt: &at}, &session); e != nil {
				return e
			}
			if session.ProjectID != pid {
				return fmt.Errorf("session escaped question partition")
			}
			state.Sessions[session.SessionID] = sid
			var events []model.Event
			for ti, t := range turns {
				typ, payload := event(t)
				b, _ := json.Marshal(payload)
				id := "lme_" + benchutil.SHA([]byte(fmt.Sprintf("%s/%s/%d", q.ID, sid, ti)))[:40]
				seq := int64(ti)
				events = append(events, model.Event{EventID: id, SessionID: &session.SessionID, ProjectID: pid, DeviceID: state.Device, EventType: typ, OccurredAt: at, PayloadJSON: string(b), SequenceNumber: &seq, SchemaVersion: 1})
			}
			for off := 0; off < len(events); off += 100 {
				var receipt model.BatchEventsResponse
				if e = post("/api/v1/events/batch", model.BatchEventsRequest{ProjectID: pid, Events: events[off:min(off+100, len(events))]}, &receipt); e != nil {
					return e
				}
				if receipt.Errors != 0 {
					return fmt.Errorf("ingest event receipts: %d errors", receipt.Errors)
				}
			}
		}
		state.Projects[q.ID] = pid
		if e := benchutil.JSON(statePath, state); e != nil {
			return e
		}
		if (qi+1)%10 == 0 {
			log.Printf("ingested %d/%d questions", qi+1, len(qs))
		}
	}
	state.Ready = true
	return benchutil.JSON(statePath, state)
}
func score(q question, ranked []string) scoreRow {
	r := scoreRow{QuestionID: q.ID, Type: q.Type, Answerable: !strings.HasSuffix(q.ID, "_abs"), Returned: len(ranked), Under10: len(ranked) < 10, Empty: len(ranked) == 0}
	if !r.Answerable {
		return r
	}
	gold := map[string]bool{}
	for _, s := range q.AnswerIDs {
		gold[s] = true
	}
	hit5, hit10 := 0, 0
	dcg := 0.
	seen := map[string]bool{}
	rank := 0
	for _, id := range ranked {
		if seen[id] {
			continue
		}
		seen[id] = true
		rank++
		if gold[id] && rank <= 10 {
			hit10++
			dcg += 1 / math.Log2(float64(rank+1))
			if rank <= 5 {
				hit5++
			}
		}
	}
	ptr := func(x float64) *float64 { return &x }
	all5, all10 := 0., 0.
	if len(gold) > 0 && hit5 == len(gold) {
		all5 = 1
	}
	if len(gold) > 0 && hit10 == len(gold) {
		all10 = 1
	}
	any := 0.
	if hit5 > 0 {
		any = 1
	}
	frac := 0.
	if len(gold) > 0 {
		frac = float64(hit5) / float64(len(gold))
	}
	ideal := 0.
	for i := 1; i <= min(10, len(gold)); i++ {
		ideal += 1 / math.Log2(float64(i+1))
	}
	ndcg := 0.
	if ideal > 0 {
		ndcg = dcg / ideal
	}
	r.All5 = ptr(all5)
	r.Fractional5 = ptr(frac)
	r.Any5 = ptr(any)
	r.All10 = ptr(all10)
	r.NDCG10 = ptr(ndcg)
	return r
}
func aggregate(rows []scoreRow) map[string]any {
	groups := map[string][]scoreRow{"all": rows}
	for _, r := range rows {
		groups[r.Type] = append(groups[r.Type], r)
	}
	out := map[string]any{}
	for k, rr := range groups {
		n, failed, under, empty, success, positiveUnder := 0, 0, 0, 0, 0, 0
		sum := make([]float64, 5)
		var durations, chars, tokens []float64
		for _, r := range rr {
			durations = append(durations, r.Duration)
			if r.Mode == "pack" {
				chars = append(chars, float64(r.Characters))
				tokens = append(tokens, float64(r.Tokens))
			}
			if r.Error != "" {
				failed++
			}
			if r.Under10 {
				under++
			}
			if r.Empty {
				empty++
			}
			if r.Answerable {
				if r.Under10 {
					positiveUnder++
				}
				n++
				if r.Error == "" {
					for i, v := range []*float64{r.All5, r.Fractional5, r.Any5, r.All10, r.NDCG10} {
						sum[i] += *v
					}
					if *r.All5 == 1 {
						success++
					}
				}
			}
		}
		quality := map[string]any{}
		for i, key := range []string{"recall_all_at_5", "fractional_at_5", "any_hit_at_5", "recall_all_at_10", "ndcg_at_10"} {
			if n > 0 {
				quality[key] = sum[i] / float64(n)
			} else {
				quality[key] = nil
			}
		}
		out[k] = map[string]any{"attempted": len(rr), "answerable": n, "unanswerable": len(rr) - n, "failed": failed, "strict_successes": success, "under_10": under, "answerable_under_10": positiveUnder, "empty": empty, "quality": quality, "latency_ms": benchutil.Dist(durations), "characters": benchutil.Dist(chars), "estimated_tokens": benchutil.Dist(tokens)}
	}
	return out
}
