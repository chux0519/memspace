// Command lme evaluates MemSpace retrieval on LongMemEval-S.
//
//	lme embed -data longmemeval_s_cleaned.json -cache vectors.db -url http://host:port/v1/embeddings
//
// embed turns every haystack session into MemSpace events, derives the search
// documents and embedding chunks exactly as the server does, and stores one
// vector per distinct chunk text in the cache (keyed by its SHA-256), so a
// later run never embeds the same text twice. It resumes where it stopped.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/search"
)

type turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type question struct {
	ID         string   `json:"question_id"`
	Type       string   `json:"question_type"`
	Question   string   `json:"question"`
	Date       string   `json:"question_date"`
	AnswerIDs  []string `json:"answer_session_ids"`
	Dates      []string `json:"haystack_dates"`
	SessionIDs []string `json:"haystack_session_ids"`
	Sessions   [][]turn `json:"haystack_sessions"`
}

func load(path string) ([]question, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var qs []question
	if err := json.Unmarshal(raw, &qs); err != nil {
		return nil, err
	}
	return qs, nil
}

// event maps a conversation turn to the event the agent hooks would record.
func event(t turn) (string, map[string]any) {
	if t.Role == "user" {
		return "user.prompt", map[string]any{"prompt": t.Content}
	}
	return "agent.turn_completed", map[string]any{"last_assistant_message": t.Content}
}

// chunks lists the texts the embedding service would receive for a session.
func chunks(sess []turn) []string {
	var out []string
	for _, t := range sess {
		typ, payload := event(t)
		raw, _ := json.Marshal(payload)
		d := search.DocFromEvent(search.Event{EventType: typ, Payload: raw})
		if d == nil {
			continue
		}
		out = append(out, embedding.Chunk(embedding.DocText(d.Title, d.Body, d.Files))...)
	}
	return out
}

func openCache(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS vectors (hash BLOB PRIMARY KEY, vec BLOB NOT NULL)`)
	return db, err
}

func hash(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func embedCmd(args []string) error {
	fs := flag.NewFlagSet("embed", flag.ExitOnError)
	data := fs.String("data", "", "LongMemEval JSON")
	cachePath := fs.String("cache", "vectors.db", "vector cache")
	url := fs.String("url", "", "embeddings endpoint")
	batch := fs.Int("batch", 16, "texts per request")
	fs.Parse(args)

	qs, err := load(*data)
	if err != nil {
		return err
	}
	cache, err := openCache(*cachePath)
	if err != nil {
		return err
	}
	defer cache.Close()

	seenSession := map[string]bool{}
	seenText := map[string]bool{}
	var todo []string
	total := 0
	for _, q := range qs {
		for i, sess := range q.Sessions {
			if seenSession[q.SessionIDs[i]] {
				continue
			}
			seenSession[q.SessionIDs[i]] = true
			for _, c := range chunks(sess) {
				if seenText[c] {
					continue
				}
				seenText[c] = true
				total++
				var one int
				if cache.QueryRow(`SELECT 1 FROM vectors WHERE hash = ?`, hash(c)).Scan(&one) == nil {
					continue
				}
				todo = append(todo, c)
			}
		}
		// Query prefix is empty for bge-m3. Fill query vectors on resume too.
		if !seenText[q.Question] {
			seenText[q.Question] = true
			var one int
			if cache.QueryRow(`SELECT 1 FROM vectors WHERE hash=?`, hash(q.Question)).Scan(&one) != nil {
				todo = append(todo, q.Question)
			}
		}
	}
	log.Printf("%d questions, %d sessions, %d distinct chunks, %d to embed", len(qs), len(seenSession), total, len(todo))

	cl := &embedding.Client{URL: *url}
	start := time.Now()
	for i := 0; i < len(todo); i += *batch {
		part := todo[i:min(i+*batch, len(todo))]
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		vecs, err := cl.EmbedBackground(ctx, part)
		cancel()
		if err != nil {
			return fmt.Errorf("after %d chunks: %w", i, err)
		}
		tx, err := cache.Begin()
		if err != nil {
			return err
		}
		for j, v := range vecs {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO vectors (hash, vec) VALUES (?, ?)`, hash(part[j]), embedding.Blob(v)); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if done := i + len(part); done%(*batch*64) == 0 || done == len(todo) {
			rate := float64(done) / time.Since(start).Seconds()
			log.Printf("%d/%d chunks, %.1f/s, eta %s", done, len(todo), rate,
				time.Duration(float64(len(todo)-done)/rate*float64(time.Second)).Round(time.Minute))
		}
	}
	return nil
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lme embed|serve|run ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(os.Args[2:])
	case "run":
		err = runCmd(os.Args[2:])
	case "embed":
		err = embedCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
