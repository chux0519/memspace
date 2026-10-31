// Command worker provides a private, per-evaluation-user JSONL bridge to the
// production ingestion handlers, embedding service and context/search code.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/chux0519/memspace/internal/adapter"
	"github.com/chux0519/memspace/internal/auth"
	"github.com/chux0519/memspace/internal/contextpack"
	memdb "github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/internal/server/handler"
	"github.com/chux0519/memspace/internal/summary"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

const owner = "omnimemeval-owner"
const device = "omnimemeval-device"

type message struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	ChatTime string `json:"chat_time"`
	ID       string `json:"id,omitempty"`
}
type request struct {
	ID       string    `json:"id"`
	Op       string    `json:"op"`
	Session  string    `json:"session_key"`
	Batch    string    `json:"batch_key"`
	Messages []message `json:"messages"`
	Query    string    `json:"query"`
	TopK     int       `json:"top_k"`
}
type state struct {
	UserKey      string `json:"user_key"`
	Token        string `json:"token"`
	Project      string `json:"project"`
	EmbeddingURL string `json:"embedding_url"`
	Model        string `json:"model"`
	Mode         string `json:"mode"`
}
type engine struct {
	d   *sql.DB
	st  state
	emb *embedding.Service
	mux http.Handler
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func open(dir, userKey, endpoint, modelName, mode string) (*engine, error) {
	if decoded, err := hex.DecodeString(userKey); err != nil || len(decoded) != 32 {
		return nil, errors.New("invalid user key")
	}
	if mode != "context_pack" && mode != "search" {
		return nil, errors.New("mode must be context_pack or search")
	}
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(abs, 0700); e != nil {
		return nil, e
	}
	real, e := filepath.EvalSymlinks(abs)
	if e != nil || real != abs {
		return nil, errors.New("worker directory must be canonical, without symlinks")
	}
	marker := filepath.Join(abs, "worker.json")
	var st state
	raw, e := os.ReadFile(marker)
	if e == nil {
		if e = json.Unmarshal(raw, &st); e != nil {
			return nil, e
		}
		if st.UserKey != userKey || st.EmbeddingURL != endpoint || st.Model != modelName || st.Mode != mode {
			return nil, errors.New("worker identity/configuration changed")
		}
	} else if os.IsNotExist(e) {
		entries, e := os.ReadDir(abs)
		if e != nil {
			return nil, e
		}
		if len(entries) > 0 {
			return nil, errors.New("refusing unowned nonempty directory")
		}
		token, _, e := auth.GenerateToken()
		if e != nil {
			return nil, e
		}
		st = state{UserKey: userKey, Token: token, EmbeddingURL: endpoint, Model: modelName, Mode: mode}
		// Create the ownership marker before any database; no preexisting DB is opened.
		if e = writeState(marker, st); e != nil {
			return nil, e
		}
	} else {
		return nil, e
	}
	dbPath := filepath.Join(abs, "memspace.db")
	if _, err := os.Lstat(dbPath); err == nil {
		if err = benchutil.RequireCopy(dbPath); err != nil {
			return nil, err
		}
		info, _ := os.Lstat(dbPath)
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("refusing symlink database")
		}
	}
	d, e := memdb.Open(dbPath)
	if e != nil {
		return nil, e
	}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	now := model.FormatTime(time.Now().UTC())
	if _, e = d.Exec(`INSERT OR IGNORE INTO users(user_id,created_at) VALUES (?,?)`, owner, now); e != nil {
		return nil, e
	}
	if _, e = d.Exec(`INSERT OR IGNORE INTO devices(device_id,user_id,name,platform,token_hash,created_at) VALUES (?,?,?,'benchmark',?,?)`, device, owner, device, auth.HashToken(st.Token), now); e != nil {
		return nil, e
	}
	if _, e = d.Exec(`CREATE TABLE IF NOT EXISTS benchmark_batches(batch_key TEXT PRIMARY KEY,request_sha TEXT NOT NULL,status TEXT NOT NULL)`); e != nil {
		return nil, e
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /projects", handler.EnsureProject(d, owner))
	mux.HandleFunc("POST /sessions", handler.EnsureSession(d, owner))
	mux.HandleFunc("POST /events", handler.BatchEvents(d))
	en := &engine{d: d, st: st, mux: auth.Middleware(d)(mux)}
	if st.Project == "" {
		var p model.EnsureProjectResponse
		if e = en.post(context.Background(), "/projects", model.EnsureProjectRequest{Name: "isolated-benchmark"}, &p); e != nil {
			return nil, e
		}
		en.st.Project = p.ProjectID
		if e = writeState(marker, en.st); e != nil {
			return nil, e
		}
	}
	en.emb = embedding.New(d, embedding.Config{URL: endpoint, Model: modelName, QueryTimeout: time.Second})
	if en.emb != nil {
		search.SetSemantic(en.emb)
	} else {
		search.SetSemantic(nil)
	}
	ok = true
	return en, nil
}
func writeState(path string, st state) error {
	b, e := json.Marshal(st)
	if e != nil {
		return e
	}
	tmp := path + ".tmp"
	if e = os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func (e *engine) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+e.st.Token)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code < 200 || w.Code >= 300 {
		return fmt.Errorf("production handler %s HTTP%d", path, w.Code)
	}
	return json.Unmarshal(w.Body.Bytes(), out)
}
func (e *engine) add(ctx context.Context, r request) (any, error) {
	if r.Session == "" || r.Batch == "" {
		return nil, errors.New("session_key and batch_key required for idempotent add")
	}
	if len(r.Messages) == 0 {
		return map[string]any{"events": 0}, nil
	}
	// Validate the entire request before allowing partial commits across API batches.
	for _, m := range r.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "tool" {
			return nil, errors.New("unsupported message role")
		}
		if _, err := time.Parse(time.RFC3339Nano, m.ChatTime); err != nil {
			return nil, errors.New("each message requires RFC3339 chat_time")
		}
	}
	raw, _ := json.Marshal(struct {
		Session  string
		Messages []message
	}{r.Session, r.Messages})
	hash := digest(raw)
	key := digest([]byte(r.Session + "\x00" + r.Batch))
	var before, status string
	err := e.d.QueryRowContext(ctx, `SELECT request_sha,status FROM benchmark_batches WHERE batch_key=?`, key).Scan(&before, &status)
	if err == nil && before != hash {
		return nil, errors.New("batch_key reused with different contents")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if status == "complete" {
		return map[string]any{"events": len(r.Messages), "duplicate": true}, nil
	}
	if _, err = e.d.ExecContext(ctx, `INSERT OR IGNORE INTO benchmark_batches VALUES (?,?,'pending')`, key, hash); err != nil {
		return nil, err
	}
	var session model.EnsureSessionResponse
	if err = e.post(ctx, "/sessions", model.EnsureSessionRequest{ProjectID: e.st.Project, AgentType: "benchmark", NativeSessionID: r.Session}, &session); err != nil {
		return nil, err
	}
	var events []model.Event
	for i, m := range r.Messages {
		typ, field := "source.message", "content"
		if m.Role == "user" {
			typ, field = "user.prompt", "prompt"
		}
		if m.Role == "assistant" {
			typ, field = "agent.turn_completed", "last_assistant_message"
		}
		p := map[string]any{field: m.Content, "role": m.Role, "origin": map[string]any{"source_kind": "ordered_import", "batch_key": key, "source_ordinal": i, "source_complete": true, "native_message_id": m.ID}}
		b, _ := json.Marshal(p)
		at, _ := time.Parse(time.RFC3339Nano, m.ChatTime)
		seq := int64(i)
		events = append(events, model.Event{EventID: adapter.StableID("omni", e.st.UserKey, r.Session, r.Batch, fmt.Sprint(i)), SessionID: &session.SessionID, EventType: typ, OccurredAt: at, SequenceNumber: &seq, PayloadJSON: string(b), SchemaVersion: 1})
	}
	for i := 0; i < len(events); i += handler.MaxBatchEvents {
		var res model.BatchEventsResponse
		if err = e.post(ctx, "/events", model.BatchEventsRequest{ProjectID: e.st.Project, Events: events[i:min(i+handler.MaxBatchEvents, len(events))]}, &res); err != nil {
			return nil, err
		}
		for _, receipt := range res.Receipts {
			if receipt.Status == model.ReceiptError {
				return nil, fmt.Errorf("event rejected: %s", receipt.Code)
			}
		}
	}
	if _, err = e.d.ExecContext(ctx, `UPDATE sessions SET status='ended',ended_at=? WHERE session_id=?`, model.FormatTime(time.Now().UTC()), session.SessionID); err != nil {
		return nil, err
	}
	// Settle the normal rule-based summary/memory pipeline before acknowledging
	// a complete source session; no synthetic unfinished-work state is created.
	if _, err = summary.Generate(ctx, e.d, session.SessionID); err != nil {
		return nil, err
	}
	if _, err = e.d.ExecContext(ctx, `UPDATE benchmark_batches SET status='complete' WHERE batch_key=?`, key); err != nil {
		return nil, err
	}
	return map[string]any{"events": len(events), "duplicate": false}, nil
}
func (e *engine) ready(ctx context.Context) (any, error) {
	var pending int
	if err := e.d.QueryRowContext(ctx, `SELECT count(*) FROM benchmark_batches WHERE status!='complete'`).Scan(&pending); err != nil {
		return nil, err
	}
	if pending > 0 {
		return nil, errors.New("unfinished ingestion batch")
	}
	if e.emb == nil {
		return map[string]any{"retrieval": "keyword", "ready": true}, nil
	}
	for {
		more, err := e.emb.Sweep(ctx, 128)
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}
	st := e.emb.Status(ctx)
	if st.Active == nil || st.Service == nil || st.Active.Pending != 0 || st.Active.Fingerprint != st.Service.Fingerprint() {
		return nil, errors.New("embedding space not ready")
	}
	return map[string]any{"retrieval": "hybrid", "ready": true, "embedding": st}, nil
}
func (e *engine) query(ctx context.Context, r request) (any, error) {
	if r.TopK <= 0 {
		return nil, errors.New("top_k must be positive")
	}
	if _, err := e.ready(ctx); err != nil {
		return nil, err
	}
	if e.st.Mode == "search" {
		p, err := search.Search(ctx, e.d, owner, model.SearchRequest{Query: r.Query, ProjectID: e.st.Project, Scope: model.ScopeCurrentProject, Limit: r.TopK})
		if err != nil {
			return nil, err
		}
		var text []string
		for _, h := range p.Results {
			text = append(text, h.Snippet)
		}
		return map[string]any{"texts": text, "results": p.Results, "mode": "search"}, nil
	}
	queryCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	ctx = queryCtx
	at := time.Now()
	req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: e.st.Project}, Query: r.Query, AgentType: "benchmark", NativeSessionID: "omni-read-only-query"}
	p, err := contextpack.BuildWithOptions(ctx, e.d, owner, device, req, contextpack.BuildOptions{MaxItems: min(6, r.TopK)})
	if err != nil {
		return nil, err
	}
	if e.emb != nil {
		st := e.emb.Status(ctx)
		if st.LastErrorAt != nil && !st.LastErrorAt.Before(at) {
			return nil, errors.New("hybrid query degraded")
		}
	}
	text := []string{}
	if p.Text != "" {
		text = append(text, p.Text)
	}
	return map[string]any{"texts": text, "pack": p, "mode": "context_pack", "duration_ms": float64(time.Since(at).Microseconds()) / 1000}, nil
}
func (e *engine) dispatch(ctx context.Context, r request) (any, error) {
	switch r.Op {
	case "add":
		return e.add(ctx, r)
	case "ready":
		return e.ready(ctx)
	case "search":
		return e.query(ctx, r)
	case "stats":
		var n int
		err := e.d.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
		return map[string]any{"events": n}, err
	default:
		return nil, errors.New("unknown operation")
	}
}
func run(in io.Reader, out io.Writer, en *engine) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 64<<20)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		var r request
		err := json.Unmarshal(scanner.Bytes(), &r)
		var data any
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			data, err = en.dispatch(ctx, r)
			cancel()
		}
		response := map[string]any{"id": r.ID, "data": data}
		if err != nil {
			response["error"] = err.Error()
		}
		if err = enc.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}
func main() {
	dir := flag.String("dir", "", "owned user directory")
	key := flag.String("user-key", "", "SHA256 user key")
	url := flag.String("embedding-url", "", "empty=explicit keyword-only")
	modelName := flag.String("model", "BAAI/bge-m3", "embedding model")
	mode := flag.String("mode", "context_pack", "context_pack or search")
	flag.Parse()
	log.SetOutput(os.Stderr)
	en, err := open(*dir, *key, *url, *modelName, *mode)
	if err == nil {
		defer en.d.Close()
		err = run(os.Stdin, os.Stdout, en)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
