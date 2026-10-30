// Command hooks measures real context Build and isolated hook processes on DB copies.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/internal/syncer"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

type sample struct{ ID, Owner, Device, Project, Agent, Native, Query string }
type receipt struct {
	MemoryEmpty     bool    `json:"memory_empty"`
	ResolvedProject string  `json:"resolved_project,omitempty"`
	Note            string  `json:"note,omitempty"`
	ID              string  `json:"id"`
	Layer           string  `json:"layer"`
	Mode            string  `json:"mode"`
	Duration        float64 `json:"duration_ms"`
	SemanticMS      float64 `json:"semantic_ms"`
	EmbedMS         float64 `json:"query_embedding_http_ms"`
	KNNMS           float64 `json:"knn_and_space_checks_ms"`
	OtherMS         float64 `json:"keyword_and_assembly_ms"`
	Characters      int     `json:"characters"`
	Bytes           int     `json:"bytes"`
	Tokens          int     `json:"estimated_tokens"`
	Items           int     `json:"items"`
	Truncated       bool    `json:"truncated"`
	Empty           bool    `json:"empty"`
	Timeout         bool    `json:"timeout"`
	Error           string  `json:"error,omitempty"`
	SemanticCalls   int     `json:"semantic_calls"`
	SemanticHits    int     `json:"semantic_hits"`
	SemanticErrors  int     `json:"semantic_errors"`
	HTTPCalls       int     `json:"http_calls"`
}
type timer struct {
	svc                          *embedding.Service
	mu                           sync.Mutex
	total, embed                 time.Duration
	calls, hits, errs, httpCalls int
}

func (t *timer) SemanticSearch(c context.Context, q string, p []string, k int) ([]search.SemanticHit, error) {
	at := time.Now()
	h, e := t.svc.SemanticSearch(c, q, p, k)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += time.Since(at)
	t.calls++
	t.hits += len(h)
	if e != nil {
		t.errs++
	}
	return h, e
}
func (t *timer) MinSimilarities() (float64, float64) { return t.svc.MinSimilarities() }
func (t *timer) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total = 0
	t.embed = 0
	t.calls = 0
	t.hits = 0
	t.errs = 0
	t.httpCalls = 0
}
func (t *timer) stamp(r *receipt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r.SemanticMS = ms(t.total)
	r.EmbedMS = ms(t.embed)
	r.KNNMS = ms(t.total - t.embed)
	if r.KNNMS < 0 {
		r.KNNMS = 0
	}
	r.OtherMS = r.Duration - r.SemanticMS
	r.SemanticCalls = t.calls
	r.SemanticHits = t.hits
	r.SemanticErrors = t.errs
	r.HTTPCalls = t.httpCalls
}
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("db", "", "sqlite3 .backup copy; never production")
	out := flag.String("out", "", "new private output directory")
	endpoint := flag.String("embed-url", "http://127.0.0.1:8771/v1/embeddings", "query embedding provider")
	count := flag.Int("samples", 300, "prompt samples")
	bin := flag.String("binary", "", "real memspace binary for end-to-end")
	scale := flag.Int("scale", 1, "declared corpus scale")
	clone := flag.Int("clone", 0, "clone search index into N total project partitions, then exit")
	e2eN := flag.Int("e2e", 50, "real processes per mode")
	flag.Parse()
	if e := benchutil.RequireCopy(*path); e != nil {
		return e
	}
	d, e := db.Open(*path)
	if e != nil {
		return e
	}
	defer d.Close()
	ctx := context.Background()
	if *clone > 0 {
		return cloneIndex(ctx, d, *clone)
	}
	if e = benchutil.NewDir(*out); e != nil {
		return e
	}
	manifest := benchutil.Manifest()
	manifest["db_sha256_before"] = benchutil.FileSHA(*path)
	manifest["db_counts"] = benchutil.Counts(ctx, d)
	manifest["scale"] = *scale
	manifest["seed"] = 20261031
	manifest["e2e_mcp_hint_enabled"] = true
	manifest["e2e_current_session"] = "fresh synthetic session; direct Build uses historical native session"
	manifest["warmup_per_group"] = 5
	manifest["limits"] = []string{"read-only production snapshot; no background sweep; original project partitions retained at 10x", "estimated tokens, not billed model tokens", "semantic minus embedding HTTP includes KNN, space checks and DB overhead", "end-to-end sync suppressed by isolated fresh sync.lock; capture/spool/startup/HTTP/injection retained"}
	samples, starts, e := inputs(ctx, d, *count)
	if e != nil {
		return e
	}
	if len(samples) == 0 {
		return fmt.Errorf("no eligible prompts")
	}
	if e = benchutil.JSON(filepath.Join(*out, "requests.json"), map[string]any{"prompts": samples, "starts": starts}); e != nil {
		return e
	}
	manifest["requests_sha256"] = benchutil.FileSHA(filepath.Join(*out, "requests.json"))
	t := &timer{}
	u, e := url.Parse(*endpoint)
	if e != nil {
		return e
	}
	u.Path = ""
	u.RawQuery = ""
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = &http.Transport{Proxy: nil}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) { http.Error(w, e.Error(), 502) }
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		at := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: 200}
		proxy.ServeHTTP(rec, r)
		if r.URL.Path == "/v1/embeddings" {
			t.mu.Lock()
			t.embed += time.Since(at)
			if rec.status >= 400 {
				t.errs++
			}
			t.mu.Unlock()
		}
	}))
	defer ps.Close()
	t.svc = embedding.New(d, embedding.Config{URL: ps.URL + "/v1/embeddings", Model: "BAAI/bge-m3", QueryTimeout: time.Second})
	t.svc.Start(ctx)
	st := t.svc.Status(ctx)
	if st.Service == nil || st.Active == nil || st.Service.Fingerprint() != st.Active.Fingerprint {
		return fmt.Errorf("active space/provider fingerprint mismatch: %+v", st)
	}
	manifest["embedding_before"] = st
	search.SetSemantic(t)
	defer search.SetSemantic(nil)
	f, e := os.OpenFile(filepath.Join(*out, "samples.ndjson"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	var receipts []receipt
	measure := func(s sample, mode string, emit bool) error {
		t.reset()
		deadline := 1500 * time.Millisecond
		if mode == contextpack.ModeSessionStart {
			deadline = 2 * time.Second
		}
		c, cancel := context.WithTimeout(ctx, deadline)
		at := time.Now()
		p, err := contextpack.Build(c, d, s.Owner, s.Device, contextpack.Request{Mode: mode, Project: search.ProjectRef{ID: s.Project}, AgentType: s.Agent, NativeSessionID: s.Native, Query: s.Query})
		r := receipt{ID: s.ID, Mode: mode, Layer: "build", Duration: ms(time.Since(at))}
		r.Timeout = c.Err() != nil
		cancel()
		if err != nil {
			r.Error = err.Error()
		}
		if p != nil {
			fill(&r, p)
		} else {
			r.Empty = true
			r.MemoryEmpty = true
		}
		t.stamp(&r)
		if emit {
			receipts = append(receipts, r)
			return enc.Encode(r)
		}
		return nil
	}
	for _, g := range []struct {
		in   []sample
		mode string
	}{{samples, contextpack.ModePrompt}, {starts, contextpack.ModeSessionStart}} {
		for i := 0; i < 5 && len(g.in) > 0; i++ {
			if e = measure(g.in[i%len(g.in)], g.mode, false); e != nil {
				return e
			}
		}
		for _, s := range g.in {
			if e = measure(s, g.mode, true); e != nil {
				return e
			}
		}
	}
	if *bin != "" && *e2eN > 0 {
		rr, err := realHooks(ctx, d, t, *bin, *out, samples, starts, *e2eN)
		if err != nil {
			return err
		}
		for _, r := range rr {
			receipts = append(receipts, r)
			if e = enc.Encode(r); e != nil {
				return e
			}
		}
	}
	manifest["embedding_after"] = t.svc.Status(ctx)
	manifest["end_environment"] = benchutil.Manifest()
	manifest["samples_sha256"] = benchutil.FileSHA(filepath.Join(*out, "samples.ndjson"))
	manifest["real_binary_sha256"] = benchutil.FileSHA(*bin)
	if e = benchutil.JSON(filepath.Join(*out, "manifest.json"), manifest); e != nil {
		return e
	}
	return benchutil.JSON(filepath.Join(*out, "summary.json"), summarize(receipts))
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(n int) { w.status = n; w.ResponseWriter.WriteHeader(n) }
func fill(r *receipt, p *contextpack.Pack) {
	r.ResolvedProject = p.ProjectID
	r.Note = p.Note
	r.Bytes = len(p.Text)
	r.Characters = utf8.RuneCountInString(p.Text)
	r.Tokens = embedding.EstimateTokens(p.Text)
	r.Items = len(p.Items)
	r.Truncated = p.Truncated
	r.Empty = p.Text == ""
	r.MemoryEmpty = r.Empty
}
func inputs(c context.Context, d *sql.DB, n int) ([]sample, []sample, error) {
	rows, e := d.QueryContext(c, `SELECT e.event_id,p.user_id,e.device_id,e.project_id,s.agent_type,s.native_session_id,e.payload_json FROM events e JOIN projects p ON p.project_id=e.project_id JOIN sessions s ON s.session_id=e.session_id WHERE e.event_type='user.prompt' ORDER BY e.event_id`)
	if e != nil {
		return nil, nil, e
	}
	var all []sample
	for rows.Next() {
		var s sample
		var raw string
		if e = rows.Scan(&s.ID, &s.Owner, &s.Device, &s.Project, &s.Agent, &s.Native, &raw); e != nil {
			rows.Close()
			return nil, nil, e
		}
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal([]byte(raw), &payload) != nil {
			continue
		}
		s.Query = prompt.OwnWords(payload.Prompt)
		if prompt.ShouldRecall(s.Query) {
			all = append(all, s)
		}
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, nil, e
	}
	rows.Close()
	rng := rand.New(rand.NewSource(20261031))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) > n {
		all = all[:n]
	}
	rows, e = d.QueryContext(c, `SELECT p.user_id,s.device_id,s.project_id,s.agent_type FROM sessions s JOIN projects p ON p.project_id=s.project_id WHERE s.project_id NOT LIKE 'bench-clone-%' GROUP BY s.project_id,s.agent_type ORDER BY s.project_id,s.agent_type`)
	if e != nil {
		return nil, nil, e
	}
	defer rows.Close()
	var starts []sample
	for rows.Next() {
		var s sample
		if e = rows.Scan(&s.Owner, &s.Device, &s.Project, &s.Agent); e != nil {
			return nil, nil, e
		}
		s.ID = "start-" + s.Project + "-" + s.Agent
		starts = append(starts, s)
	}
	return all, starts, rows.Err()
}
func summarize(rr []receipt) map[string]any {
	groups := map[string][]receipt{}
	for _, r := range rr {
		k := r.Layer + "/" + r.Mode
		groups[k] = append(groups[k], r)
	}
	out := map[string]any{}
	for k, rs := range groups {
		var durations, chars, tokens, items, sem, emb, knn, other []float64
		empty, trunc, timeout, errs, degraded, memoryEmpty := 0, 0, 0, 0, 0, 0
		for _, r := range rs {
			durations = append(durations, r.Duration)
			chars = append(chars, float64(r.Characters))
			tokens = append(tokens, float64(r.Tokens))
			items = append(items, float64(r.Items))
			sem = append(sem, r.SemanticMS)
			emb = append(emb, r.EmbedMS)
			knn = append(knn, r.KNNMS)
			other = append(other, r.OtherMS)
			if r.Items == 0 {
				memoryEmpty++
			}
			if r.Empty {
				empty++
			}
			if r.Truncated {
				trunc++
			}
			if r.Timeout {
				timeout++
			}
			if r.Error != "" {
				errs++
			}
			if r.SemanticErrors > 0 {
				degraded++
			}
		}
		n := float64(len(rs))
		out[k] = map[string]any{"n": len(rs), "latency_ms": benchutil.Dist(durations), "characters": benchutil.Dist(chars), "estimated_tokens": benchutil.Dist(tokens), "items": benchutil.Dist(items), "semantic_ms": benchutil.Dist(sem), "query_embedding_http_ms": benchutil.Dist(emb), "knn_and_space_checks_ms": benchutil.Dist(knn), "keyword_and_assembly_ms": benchutil.Dist(other), "memory_empty_rate": float64(memoryEmpty) / n, "empty_rate": float64(empty) / n, "truncated_rate": float64(trunc) / n, "timeout_rate": float64(timeout) / n, "error_rate": float64(errs) / n, "semantic_error_rate": float64(degraded) / n}
	}
	return out
}
func realHooks(ctx context.Context, d *sql.DB, t *timer, bin, out string, prompts, starts []sample, n int) ([]receipt, error) {
	ln, e := net.Listen("tcp", "127.0.0.1:18770")
	if e != nil {
		return nil, e
	}
	defer ln.Close()
	results := make(chan receipt, 16)
	owner := prompts[0].Owner
	mux := http.NewServeMux()
	mux.Handle("/api/v1/context", auth.Middleware(d)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.mu.Lock()
		t.httpCalls++
		t.mu.Unlock()
		at := time.Now()
		rec := &captureWriter{ResponseWriter: w}
		handler.Context(d, owner).ServeHTTP(rec, r)
		x := receipt{Duration: ms(time.Since(at))}
		var p contextpack.Pack
		if rec.status >= 400 {
			x.MemoryEmpty = true
			x.Error = fmt.Sprintf("context HTTP %d", rec.status)
		} else if json.Unmarshal(rec.body.Bytes(), &p) == nil {
			fill(&x, &p)
		}
		x.Timeout = r.Context().Err() != nil
		results <- x
	})))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	var rr []receipt
	for _, g := range []struct {
		in          []sample
		mode, event string
	}{{prompts, contextpack.ModePrompt, "UserPromptSubmit"}, {starts, contextpack.ModeSessionStart, "SessionStart"}} {
		if len(g.in) == 0 {
			continue
		}
		for i := -5; i < n; i++ {
			s := g.in[(i+5)%len(g.in)]
			home, e := os.MkdirTemp(out, "hook-home-")
			if e != nil {
				return nil, e
			}
			state := filepath.Join(home, "state")
			os.MkdirAll(state, 0700)
			if e = os.WriteFile(filepath.Join(state, "sync.lock"), []byte("benchmark-held\n"), 0600); e != nil {
				return nil, e
			}
			tok, hashed, e := auth.GenerateToken()
			if e != nil {
				return nil, e
			}
			if _, e = d.ExecContext(ctx, `UPDATE devices SET token_hash=?,revoked_at=NULL WHERE device_id=?`, hashed, s.Device); e != nil {
				return nil, e
			}
			mcpToken, err := auth.RotateMCPToken(ctx, d, s.Device)
			if err != nil {
				return nil, err
			}
			if e = syncer.SaveCredentials(filepath.Join(home, "credentials.json"), &syncer.Credentials{ServerURL: "http://127.0.0.1:18770", DeviceID: s.Device, Token: tok, MCPToken: mcpToken}); e != nil {
				return nil, e
			}
			if e = os.WriteFile(filepath.Join(home, "config.toml"), []byte("[client]\nauto_upgrade = 'off'\n"), 0600); e != nil {
				return nil, e
			}
			var name string
			var git sql.NullString
			if e = d.QueryRowContext(ctx, `SELECT name,canonical_git_identity FROM projects WHERE project_id=?`, s.Project).Scan(&name, &git); e != nil {
				return nil, e
			}
			cwd := filepath.Join(home, "work", filepath.Base(name))
			os.MkdirAll(cwd, 0700)
			if git.Valid && git.String != "" {
				os.MkdirAll(filepath.Join(cwd, ".git", "refs", "heads"), 0700)
				os.WriteFile(filepath.Join(cwd, ".git", "HEAD"), []byte("ref: refs/heads/benchmark\n"), 0600)
				remote := git.String
				if !strings.Contains(remote, "://") {
					remote = "https://" + remote
				}
				if e = os.WriteFile(filepath.Join(cwd, ".git", "config"), []byte(fmt.Sprintf("[remote \"origin\"]\nurl = %s\n", remote)), 0600); e != nil {
					return nil, e
				}
			}
			raw, _ := json.Marshal(map[string]any{"session_id": fmt.Sprintf("bench-%d-%s", i, s.ID), "cwd": cwd, "hook_event_name": g.event, "prompt": s.Query, "source": "startup"})
			cmd := exec.Command(bin, "--home", home, "--config", filepath.Join(home, "config.toml"), "hook", "claude")
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, "MEMSPACE_") && !strings.HasPrefix(env, "CLAUDE_PROJECT_DIR=") {
					cmd.Env = append(cmd.Env, env)
				}
			}
			cmd.Env = append(cmd.Env, "NO_PROXY=*", "MEMSPACE_HOME="+home)
			cmd.Stdin = bytes.NewReader(raw)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			t.reset()
			at := time.Now()
			b, err := cmd.Output()
			x := receipt{ID: s.ID, Layer: "hook_process", Mode: g.mode, Duration: ms(time.Since(at)), Empty: true}
			if err != nil {
				x.Error = err.Error()
			}
			t.mu.Lock()
			calls := t.httpCalls
			t.mu.Unlock()
			if calls > 0 {
				select {
				case backend := <-results:
					x.MemoryEmpty = backend.MemoryEmpty
					x.ResolvedProject = backend.ResolvedProject
					x.Note = backend.Note
					x.Items = backend.Items
					x.Truncated = backend.Truncated
					x.Timeout = backend.Timeout
					if backend.Error != "" {
						x.Error = backend.Error
					}
				case <-time.After(3 * time.Second):
					x.Error = "server receipt missing"
				}
			}
			var envelope struct {
				Hook struct {
					Text string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if len(b) > 0 {
				if e = json.Unmarshal(b, &envelope); e != nil {
					x.Error = e.Error()
				}
				text := envelope.Hook.Text
				x.Bytes = len(text)
				x.Characters = utf8.RuneCountInString(text)
				x.Tokens = embedding.EstimateTokens(text)
				x.Empty = text == ""
			}
			t.stamp(&x)
			if x.ResolvedProject != s.Project && x.Error == "" {
				x.Error = "hook resolved wrong project: " + x.Note
			}
			if calls != 1 && x.Error == "" {
				x.Error = fmt.Sprintf("expected one context request, got %d", calls)
			}
			if i >= 0 {
				rr = append(rr, x)
			}
			os.RemoveAll(home)
		}
	}
	return rr, nil
}

type captureWriter struct {
	http.ResponseWriter
	body   bytes.Buffer
	status int
}

func (w *captureWriter) WriteHeader(n int) { w.status = n; w.ResponseWriter.WriteHeader(n) }
func (w *captureWriter) Write(b []byte) (int, error) {
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}
