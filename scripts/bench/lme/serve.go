package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chux0519/memspace/internal/embedding"
)

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	path := fs.String("cache", "vectors.db", "cache file")
	port := fs.Int("port", 18792, "loopback port")
	fallback := fs.String("fallback", "http://192.168.1.101:8791", "provider base URL")
	strict := fs.Bool("strict", false, "reject cache misses; no inference")
	if e := fs.Parse(args); e != nil {
		return e
	}
	cache, e := openCache(*path)
	if e != nil {
		return e
	}
	defer cache.Close()
	cache.SetMaxOpenConns(4)
	// Bind the precomputed cache to the provider's actual model configuration.
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 30 * time.Second}
	resp, err := hc.Get(strings.TrimSuffix(*fallback, "/") + "/info")
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("provider info HTTP%d", resp.StatusCode)
	}
	var info embedding.ServiceInfo
	if err = json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if info.Dimensions <= 0 {
		return fmt.Errorf("provider dimensions missing")
	}
	if _, err = cache.Exec(`CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL)`); err != nil {
		return err
	}
	var bound string
	err = cache.QueryRow(`SELECT value FROM metadata WHERE key='fingerprint'`).Scan(&bound)
	if err == nil && bound != info.Fingerprint() {
		return fmt.Errorf("cache/provider fingerprint mismatch")
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err = cache.Exec(`INSERT OR IGNORE INTO metadata(key,value) VALUES ('fingerprint',?)`, info.Fingerprint()); err != nil {
		return err
	}
	if _, err = cache.Exec(`INSERT OR REPLACE INTO metadata(key,value) VALUES ('producer_info',?)`, string(raw)); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "cache server 127.0.0.1:%d strict=%v\n", *port, *strict)
	return http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), cacheHandler(cache, *fallback, *strict))
}
func cacheHandler(cache *sql.DB, fallback string, strict bool) http.Handler {
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Minute}
	base := strings.TrimSuffix(fallback, "/")
	cl := &embedding.Client{URL: base + "/v1/embeddings", HTTP: hc}
	var hits, misses, requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		resp, e := hc.Get(base + "/info")
		if e != nil {
			http.Error(w, e.Error(), 502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"requests": requests.Load(), "hits": hits.Load(), "misses": misses.Load(), "strict": strict})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			Input json.RawMessage `json:"input"`
			Model string          `json:"model"`
		}
		if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		var texts []string
		if json.Unmarshal(req.Input, &texts) != nil {
			var s string
			if json.Unmarshal(req.Input, &s) != nil {
				http.Error(w, "input must be string or string[]", 400)
				return
			}
			texts = []string{s}
		}
		if len(texts) == 0 || len(texts) > 1024 {
			http.Error(w, "invalid batch", 400)
			return
		}
		vectors := make([][]float32, len(texts))
		var missing []string
		var indices []int
		first := map[string]int{}
		duplicates := map[int]int{}
		for i, text := range texts {
			if previous, ok := first[text]; ok {
				duplicates[i] = previous
				continue
			}
			first[text] = i
			var b []byte
			readErr := cache.QueryRowContext(r.Context(), `SELECT vec FROM vectors WHERE hash=?`, hash(text)).Scan(&b)
			if readErr == nil {
				if len(b)%4 != 0 || len(b) == 0 {
					http.Error(w, "corrupt vector cache", 500)
					return
				}
				v := make([]float32, len(b)/4)
				for j := range v {
					v[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[j*4:]))
				}
				vectors[i] = v
				hits.Add(1)
			} else if readErr == sql.ErrNoRows {
				missing = append(missing, text)
				indices = append(indices, i)
				misses.Add(1)
			} else {
				http.Error(w, readErr.Error(), 500)
				return
			}
		}
		if len(missing) > 0 {
			if strict {
				http.Error(w, fmt.Sprintf("%d cache misses", len(missing)), 503)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
			defer cancel()
			for at := 0; at < len(missing); at += 16 {
				end := min(at+16, len(missing))
				vv, e := cl.EmbedBackground(ctx, missing[at:end])
				if e != nil {
					http.Error(w, e.Error(), 502)
					return
				}
				tx, e := cache.BeginTx(ctx, nil)
				if e != nil {
					http.Error(w, e.Error(), 500)
					return
				}
				for j, v := range vv {
					idx := at + j
					vectors[indices[idx]] = v
					if _, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO vectors(hash,vec) VALUES (?,?)`, hash(missing[idx]), embedding.Blob(v)); e != nil {
						break
					}
				}
				if e != nil {
					tx.Rollback()
					http.Error(w, e.Error(), 500)
					return
				}
				if e = tx.Commit(); e != nil {
					http.Error(w, e.Error(), 500)
					return
				}
			}
		}
		for duplicate, original := range duplicates {
			vectors[duplicate] = vectors[original]
		}
		data := []map[string]any{}
		for i, v := range vectors {
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": v})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "model": "BAAI/bge-m3", "data": data})
	})
	return mux
}
