// Label-free supplied-session adapter using MemSpace's real SQLite search.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"io"
	"os"
	"path/filepath"
	"time"
)

type session struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
}
type request struct {
	Op       string    `json:"op"`
	Sessions []session `json:"sessions,omitempty"`
	Query    string    `json:"query,omitempty"`
	K        int       `json:"k,omitempty"`
	Mode     string    `json:"mode,omitempty"`
	View     string    `json:"view,omitempty"`
}
type ranked struct {
	SessionID string  `json:"sessionId"`
	Score     float64 `json:"score"`
}
type response struct {
	Ranked  []ranked `json:"ranked"`
	Error   string   `json:"error,omitempty"`
	QueryMS float64  `json:"query_ms,omitempty"`
}
type worker struct {
	db  *sql.DB
	dir string
}

func (w *worker) close() error {
	var err error
	if w.db != nil {
		err = w.db.Close()
		w.db = nil
	}
	if w.dir != "" {
		if e := os.RemoveAll(w.dir); err == nil {
			err = e
		}
		w.dir = ""
	}
	return err
}
func (w *worker) index(sessions []session, multi bool) error {
	if err := w.close(); err != nil {
		return err
	}
	if len(sessions) == 0 || len(sessions) > 10000 {
		return fmt.Errorf("invalid session count")
	}
	var err error
	w.dir, err = os.MkdirTemp("", "memspace-lme-")
	if err != nil {
		return err
	}
	w.db, err = db.Open(filepath.Join(w.dir, "index.db"))
	if err != nil {
		return err
	}
	tx, err := w.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`INSERT INTO users(user_id) VALUES ('eval-owner')`,
		`INSERT INTO projects(project_id,user_id,name) VALUES ('eval-project','eval-owner','eval-project')`,
		`INSERT INTO devices(device_id,user_id,name,platform,token_hash) VALUES ('eval-device','eval-owner','eval-device','linux','unused')`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		if s.ID == "" || seen[s.ID] {
			return fmt.Errorf("empty or duplicate source ID")
		}
		seen[s.ID] = true
		if _, err = tx.Exec(`INSERT INTO sessions(session_id,project_id,device_id,agent_type,native_session_id,status) VALUES (?,'eval-project','eval-device','benchmark',?,'ended')`, s.ID, s.ID); err != nil {
			return err
		}
		text := s.Content
		if s.Timestamp != "" {
			text = "[Source session date: " + s.Timestamp + "]\n" + text
		}
		kind := search.KindReply
		if multi {
			kind = search.KindSummary
		}
		if err = search.Upsert(context.Background(), tx, &search.Doc{Kind: kind, RefID: s.ID, SessionID: s.ID, ProjectID: "eval-project", DeviceID: "eval-device", AgentType: "benchmark", OccurredAt: time.Unix(0, 0).UTC(), Body: text}); err != nil {
			return err
		}
		if multi {
			for i, body := range embedding.Chunk(s.Content) {
				if s.Timestamp != "" {
					body = "[Source session date: " + s.Timestamp + "]\n" + body
				}
				if err = search.Upsert(context.Background(), tx, &search.Doc{Kind: search.KindReply, RefID: fmt.Sprintf("%s#passage-%d", s.ID, i), SessionID: s.ID, ProjectID: "eval-project", DeviceID: "eval-device", AgentType: "benchmark", OccurredAt: time.Unix(0, 0).UTC(), Body: body}); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}
func (w *worker) handle(r request) (response, error) {
	switch r.Op {
	case "close":
		return response{}, w.close()
	case "index":
		return response{}, w.index(r.Sessions, r.View == "multi")
	case "query":
		if w.db == nil {
			return response{}, fmt.Errorf("index not initialized")
		}
		if r.K < 1 || r.K > 100 {
			return response{}, fmt.Errorf("invalid k")
		}
		if r.Mode != "keyword" && r.Mode != "bm25" && r.Mode != "session" {
			return response{}, fmt.Errorf("unsupported mode")
		}
		t := time.Now()
		hits, err := search.Search(context.Background(), w.db, "eval-owner", model.SearchRequest{Query: r.Query, Mode: r.Mode, Scope: model.ScopeCurrentProject, ProjectID: "eval-project", Limit: r.K})
		if err != nil {
			return response{}, err
		}
		out := response{Ranked: []ranked{}, QueryMS: float64(time.Since(t).Microseconds()) / 1000}
		for _, h := range hits.Results {
			out.Ranked = append(out.Ranked, ranked{h.SessionID, h.Score})
		}
		return out, nil
	default:
		return response{}, fmt.Errorf("unknown operation")
	}
}
func main() {
	w := &worker{}
	defer w.close()
	reader := bufio.NewReader(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF && len(line) == 0 {
			return
		}
		if err != nil && err != io.EOF {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var req request
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		var out response
		if len(line) > 64<<20 {
			out.Error = "request too large"
		} else if e := dec.Decode(&req); e != nil {
			out.Error = e.Error()
		} else {
			var e error
			out, e = w.handle(req)
			if e != nil {
				out.Error = e.Error()
			}
		}
		if e := encoder.Encode(out); e != nil {
			fmt.Fprintln(os.Stderr, e)
			return
		}
	}
}
