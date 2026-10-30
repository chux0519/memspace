package main

import (
	"bytes"
	"encoding/json"
	"github.com/chux0519/memspace/internal/embedding"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCacheEmbeddingsFallbackOrderingAndStrictMiss(t *testing.T) {
	cache, e := openCache(filepath.Join(t.TempDir(), "vectors.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer cache.Close()
	cache.Exec(`INSERT INTO vectors(hash,vec) VALUES (?,?)`, hash("known"), embedding.Blob([]float32{1, 0}))
	var calls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var in struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		data := []map[string]any{}
		for i := range in.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{0, 1}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "BAAI/bge-m3"})
	}))
	defer fallback.Close()
	srv := httptest.NewServer(cacheHandler(cache, fallback.URL, false))
	defer srv.Close()
	cl := &embedding.Client{URL: srv.URL + "/v1/embeddings"}
	v, e := cl.Embed(t.Context(), []string{"known", "new", "new"})
	if e != nil || len(v) != 3 || v[0][0] != 1 || v[1][1] != 1 || v[2][1] != 1 {
		t.Fatalf("ordering/cache: %+v %v", v, e)
	}
	_, e = cl.Embed(t.Context(), []string{"new", "known"})
	if e != nil || calls.Load() != 1 {
		t.Fatal("miss not cached")
	}
	rr := httptest.NewRecorder()
	cacheHandler(cache, fallback.URL, true).ServeHTTP(rr, httptest.NewRequest("POST", "/v1/embeddings", bytes.NewBufferString(`{"input":["missing"]}`)))
	if rr.Code != 503 || calls.Load() != 1 {
		t.Fatal("strict mode invoked inference")
	}
}
