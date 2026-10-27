package embedding_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"hash/fnv"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
)

// fakeService embeds text deterministically: synonyms in different
// languages share a "concept" dimension, everything else contributes weakly
// through hashed character bigrams. Related meaning with no shared words
// therefore lands close, as with a real model.
type fakeService struct {
	*httptest.Server
	mu     sync.Mutex
	model  string
	down   bool
	calls  atomic.Int32
	inputs atomic.Int32
}

const dims = 48

var concepts = [][]string{
	{"登录", "login", "认证", "auth", "sign-in", "鉴权"},
	{"超时", "timeout", "慢", "slow", "卡住", "hang"},
	{"支付", "payment", "stripe", "扣款"},
	{"幂等", "idempotent", "去重", "dedupe", "重复"},
	{"迁移", "migration", "schema", "表结构"},
}

func vectorFor(text string) []float32 {
	text = strings.ToLower(strings.TrimPrefix(text, "query: "))
	v := make([]float64, dims)
	for i, words := range concepts {
		for _, w := range words {
			if strings.Contains(text, w) {
				v[i] += 3
			}
		}
	}
	runes := []rune(text)
	for i := 0; i+1 < len(runes); i++ {
		h := fnv.New32a()
		h.Write([]byte(string(runes[i : i+2])))
		v[len(concepts)+int(h.Sum32())%(dims-len(concepts))] += 0.15
	}
	var n float64
	for _, x := range v {
		n += x * x
	}
	n = math.Sqrt(n)
	out := make([]float32, dims)
	for i := range v {
		if n > 0 {
			out[i] = float32(v[i] / n)
		}
	}
	return out
}

func newFake(t *testing.T) *fakeService {
	f := &fakeService{model: "fake-embed-v1"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down, model := f.down, f.model
		f.mu.Unlock()
		if down {
			http.Error(w, `{"error":{"message":"down"}}`, 503)
			return
		}
		switch r.URL.Path {
		case "/info":
			json.NewEncoder(w).Encode(map[string]any{"model": model, "ftype": "q4_k", "dimensions": dims, "max_tokens": 512})
		case "/v1/embeddings":
			f.calls.Add(1)
			var req struct{ Input []string }
			json.NewDecoder(r.Body).Decode(&req)
			f.inputs.Add(int32(len(req.Input)))
			data := []map[string]any{}
			for i, in := range req.Input {
				data = append(data, map[string]any{"index": i, "embedding": vectorFor(in)})
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data, "model": model})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeService) set(model string, down bool) {
	f.mu.Lock()
	f.model, f.down = model, down
	f.mu.Unlock()
}

type env struct {
	t   *testing.T
	db  *sql.DB
	svc *embedding.Service
	f   *fakeService
	n   int
}

func setup(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	now := model.FormatTime(time.Now())
	mustExec(t, d, `INSERT INTO users (user_id, created_at) VALUES ('owner', ?)`, now)
	for _, p := range []string{"webapp", "billing"} {
		mustExec(t, d, `INSERT INTO projects (project_id, user_id, name, created_at, updated_at) VALUES (?, 'owner', ?, ?, ?)`, p, p, now, now)
	}
	f := newFake(t)
	svc := embedding.New(d, embedding.Config{URL: f.URL + "/v1/embeddings", Model: "fake", BatchSize: 4, QueryTimeout: time.Second})
	e := &env{t: t, db: d, svc: svc, f: f}
	e.doc("webapp", search.KindReply, "修复了认证模块的超时问题：连接池耗尽")
	e.doc("webapp", search.KindPrompt, "重构数据库迁移脚本，调整表结构")
	e.doc("webapp", search.KindFailure, "go test ./ui — snapshot mismatch in header component")
	e.doc("billing", search.KindPrompt, "实现 Stripe webhook 的幂等处理，用 event_id 去重")
	e.doc("billing", search.KindReply, "Payment retries are now idempotent via a unique event table.")
	return e
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func (e *env) doc(project, kind, body string) string {
	e.t.Helper()
	e.n++
	ref := "ref" + string(rune('A'+e.n))
	e.put(project, kind, ref, body)
	return ref
}

func (e *env) put(project, kind, ref, body string) {
	tx, _ := e.db.Begin()
	if err := search.Upsert(context.Background(), tx, &search.Doc{Kind: kind, RefID: ref, ProjectID: project, OccurredAt: time.Now(), Body: body}); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		e.t.Fatal(err)
	}
}

// build runs sweeps until the space is complete.
func (e *env) build() {
	e.t.Helper()
	e.svc.Start(context.Background())
	for i := 0; i < 20; i++ {
		more, err := e.svc.Sweep(context.Background(), 2)
		if err != nil {
			e.t.Fatal(err)
		}
		if !more {
			return
		}
	}
	e.t.Fatal("sweep did not converge")
}

func (e *env) similar(q string, projects ...string) []embedding.Hit {
	e.t.Helper()
	hits, err := e.svc.Similar(context.Background(), q, projects, 5)
	if err != nil {
		e.t.Fatal(err)
	}
	return hits
}

func (e *env) bodyOf(docID int64) string {
	var b string
	e.db.QueryRow(`SELECT body FROM search_docs WHERE doc_id = ?`, docID).Scan(&b)
	return b
}

func count(t *testing.T, d *sql.DB, q string) int {
	var n int
	if err := d.QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestChunking(t *testing.T) {
	if got := embedding.Chunk("短文本"); len(got) != 1 {
		t.Errorf("short: %q", got)
	}
	long := strings.Repeat("修复认证模块超时问题，并补充集成测试。", 120) // ~2000 CJK chars
	chunks := embedding.Chunk(long)
	if len(chunks) < 4 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for i, c := range chunks {
		if n := embedding.EstimateTokens(c); n > embedding.ChunkTokens+embedding.ChunkOverlap || c == "" {
			t.Errorf("chunk %d: %d tokens", i, n)
		}
	}
	// Neighbours overlap so a phrase across a boundary survives.
	if !strings.Contains(chunks[1], strings.TrimSpace(chunks[0][len(chunks[0])-30:])) {
		t.Error("no overlap between chunks")
	}
	en := strings.Repeat("the uploader retries with exponential backoff and jitter ", 200)
	for _, c := range embedding.Chunk(en) {
		if embedding.EstimateTokens(c) > embedding.ChunkTokens+embedding.ChunkOverlap {
			t.Errorf("english chunk too long: %d", embedding.EstimateTokens(c))
		}
	}
	if embedding.Chunk("   ") != nil {
		t.Error("blank text produced chunks")
	}
}

func TestBuildAndSimilar(t *testing.T) {
	e := setup(t)
	if hits := e.similar("登录很慢"); hits != nil {
		t.Fatal("results before the space is built")
	}
	e.build()
	st := e.svc.Status(context.Background())
	if st.Active == nil || st.Active.Docs != 5 || st.Active.Pending != 0 || st.Building != nil {
		t.Fatalf("status = %+v", st)
	}
	// No shared words: 登录很慢 ~ 认证…超时 by meaning.
	hits := e.similar("登录很慢")
	if len(hits) == 0 || !strings.Contains(e.bodyOf(hits[0].DocID), "认证模块的超时") {
		t.Fatalf("top hit = %+v", hits)
	}
	// Cross-language.
	if hits := e.similar("dedupe payment webhooks"); len(hits) == 0 || !strings.Contains(e.bodyOf(hits[0].DocID), "Stripe") && !strings.Contains(e.bodyOf(hits[0].DocID), "Payment") {
		t.Errorf("cross-language: %+v", hits)
	}
	// Project partition.
	for _, h := range e.similar("支付 幂等", "webapp") {
		var p string
		e.db.QueryRow(`SELECT project_id FROM search_docs WHERE doc_id = ?`, h.DocID).Scan(&p)
		if p != "webapp" {
			t.Errorf("partition leak: %s", p)
		}
	}
	if count(t, e.db, `SELECT COUNT(*) FROM embedding_chunks`) != count(t, e.db, `SELECT COUNT(*) FROM vec_space_1`) {
		t.Error("chunks and vectors out of sync")
	}
}

func TestChangedDocumentIsReembedded(t *testing.T) {
	e := setup(t)
	e.build()
	before := count(t, e.db, `SELECT COUNT(*) FROM vec_space_1`)
	var ref string
	e.db.QueryRow(`SELECT ref_id FROM search_docs WHERE body LIKE '%迁移脚本%'`).Scan(&ref)
	e.put("webapp", search.KindPrompt, ref, "给登录页加验证码") // same document, new text
	if count(t, e.db, `SELECT COUNT(*) FROM embedding_orphans`) == 0 {
		t.Fatal("old vectors not orphaned")
	}
	e.build()
	if count(t, e.db, `SELECT COUNT(*) FROM embedding_orphans`) != 0 || count(t, e.db, `SELECT COUNT(*) FROM vec_space_1`) != before {
		t.Errorf("orphans not cleaned: vectors %d -> %d", before, count(t, e.db, `SELECT COUNT(*) FROM vec_space_1`))
	}
	hits := e.similar("迁移 表结构")
	if len(hits) > 0 && strings.Contains(e.bodyOf(hits[0].DocID), "验证码") && hits[0].Similarity > 0.9 {
		t.Error("old text still matches strongly")
	}
	if hits := e.similar("验证码 登录"); len(hits) == 0 || !strings.Contains(e.bodyOf(hits[0].DocID), "验证码") {
		t.Errorf("new text not found: %+v", hits)
	}
	// Unchanged text keeps its vectors (summaries are re-upserted often).
	calls := e.f.inputs.Load()
	e.put("webapp", search.KindPrompt, ref, "给登录页加验证码")
	e.build()
	if e.f.inputs.Load() != calls+1 { // +1: the describe probe
		t.Errorf("unchanged document was re-embedded (%d -> %d inputs)", calls, e.f.inputs.Load())
	}
}

func TestServiceChangeBuildsNewSpace(t *testing.T) {
	e := setup(t)
	e.build()
	e.f.set("fake-embed-v2", false)
	e.svc.Start(context.Background()) // re-describes: new fingerprint
	if hits := e.similar("登录很慢"); hits != nil {
		t.Fatal("queried the old space with the new model")
	}
	if st := e.svc.Status(context.Background()); st.Building == nil || st.Active == nil {
		t.Logf("status during rebuild: %+v", st) // building space appears on first sweep
	}
	e.build()
	st := e.svc.Status(context.Background())
	if st.Active == nil || st.Active.Model != "fake-embed-v2" || st.Building != nil {
		t.Fatalf("status = %+v", st)
	}
	if count(t, e.db, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'vec_space_1'`) != 0 {
		t.Error("retired vector table not dropped")
	}
	if count(t, e.db, `SELECT COUNT(*) FROM embedding_chunks WHERE space_id = 1`) != 0 {
		t.Error("retired chunks kept")
	}
	if hits := e.similar("登录很慢"); len(hits) == 0 {
		t.Error("new space not queried")
	}
}

func TestServiceDownDegrades(t *testing.T) {
	e := setup(t)
	e.build()
	search.SetSemantic(e.svc)
	t.Cleanup(func() { search.SetSemantic(nil) })
	e.f.set("fake-embed-v1", true)
	for i := 0; i < 3; i++ {
		if hits, err := e.svc.Similar(context.Background(), "登录很慢", nil, 5); hits != nil || err != nil {
			t.Errorf("down service: %v %v", hits, err)
		}
		if st := e.svc.Status(context.Background()); (st.BreakerUntil != nil) != (i == 2) {
			t.Errorf("after %d failures breaker open = %v", i+1, st.BreakerUntil != nil)
		}
	}
	calls := e.f.calls.Load()
	e.svc.Similar(context.Background(), "登录很慢", nil, 5)
	if e.f.calls.Load() != calls {
		t.Error("open breaker still called the service")
	}
	st := e.svc.Status(context.Background())
	if st.LastError == "" || st.BreakerUntil == nil {
		t.Errorf("failure not visible: %+v", st)
	}
	// Keyword search keeps working.
	resp, err := search.Search(context.Background(), e.db, "owner", model.SearchRequest{Query: "幂等", Scope: model.ScopeAllProjects})
	if err != nil || len(resp.Results) == 0 {
		t.Errorf("keyword search while embeddings are down: %v %+v", err, resp)
	}
	e.put("webapp", search.KindReply, "new-doc", "新的文档")
	if _, err := e.svc.Sweep(context.Background(), 10); err == nil {
		t.Error("sweep should fail (and be retried by the job queue)")
	}
}

func TestHybridSearch(t *testing.T) {
	e := setup(t)
	e.build()
	search.SetSemantic(e.svc)
	t.Cleanup(func() { search.SetSemantic(nil) })
	all := model.SearchRequest{Scope: model.ScopeAllProjects}

	q := all
	q.Query = "登录很慢" // no keyword overlap with the relevant document
	resp, err := search.Search(context.Background(), e.db, "owner", q)
	if err != nil || len(resp.Results) == 0 {
		t.Fatalf("%v %+v", err, resp)
	}
	top := resp.Results[0]
	if top.Match != "semantic" || top.Similarity == 0 || !strings.Contains(top.Snippet, "认证") {
		t.Errorf("top = %+v", top)
	}

	q.Query = "幂等 支付"
	resp, _ = search.Search(context.Background(), e.db, "owner", q)
	if len(resp.Results) == 0 || resp.Results[0].Match == "semantic" || resp.Results[0].Similarity == 0 {
		t.Errorf("keyword+semantic hit should rank first with both signals: %+v", resp.Results)
	}

	q.Query, q.Kinds = "登录很慢", []string{search.KindPrompt}
	resp, _ = search.Search(context.Background(), e.db, "owner", q)
	for _, r := range resp.Results {
		if r.Kind != search.KindPrompt {
			t.Errorf("kind filter not applied to semantic hits: %s", r.Kind)
		}
	}

	q = all
	q.Query, q.Mode = "登录很慢", "keyword"
	if resp, _ := search.Search(context.Background(), e.db, "owner", q); len(resp.Results) != 0 {
		t.Errorf("keyword mode used vectors: %+v", resp.Results)
	}
}

func TestFingerprintWeightTypes(t *testing.T) {
	base := embedding.ServiceInfo{Model: "BAAI/bge-m3", FType: "q8_0", Dimensions: 1024}
	q8, mixed := base, base
	q8.WeightTypes, mixed.WeightTypes = "q8_0:146", "q4_K:49,q8_0:97"
	if q8.Fingerprint() == mixed.Fingerprint() {
		t.Fatal("mixed quantization shares the fingerprint of q8_0")
	}
	// Services without weight_types keep their fingerprint.
	if base.Fingerprint() != (embedding.ServiceInfo{Model: "BAAI/bge-m3", FType: "q8_0", Dimensions: 1024}).Fingerprint() {
		t.Fatal("fingerprint not stable")
	}
	b, _ := json.Marshal(base)
	if strings.Contains(string(b), "weight_types") {
		t.Fatalf("empty weight_types serialized: %s", b)
	}
}
