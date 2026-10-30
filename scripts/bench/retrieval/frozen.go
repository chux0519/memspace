package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/search"
)

// Frozen real query vectors isolate retrieval/injection quality from availability
// of a shared embedding server. Ranking, project filters, thresholds and source
// text still run through production code. Timings are NOT live service latency.
type querySnapshot struct {
	Fingerprint string               `json:"fingerprint"`
	Vectors     map[string][]float32 `json:"vectors"`
}
type frozenSemantic struct {
	db      *sql.DB
	svc     *embedding.Service
	space   *embedding.Space
	queries querySnapshot
}

func (f *frozenSemantic) MinSimilarities() (float64, float64) { return f.svc.MinSimilarities() }
func (f *frozenSemantic) SemanticSearch(ctx context.Context, q string, p []string, k int) ([]search.SemanticHit, error) {
	v, ok := f.queries.Vectors[q]
	if !ok {
		return nil, fmt.Errorf("missing frozen query: %q", q)
	}
	hits, err := embedding.KNN(ctx, f.db, f.space, v, p, k)
	if err != nil {
		return nil, err
	}
	out := make([]search.SemanticHit, len(hits))
	for i, h := range hits {
		out[i] = search.SemanticHit{DocID: h.DocID, Text: h.ChunkText, Similarity: h.Similarity}
	}
	return out, nil
}
func freezeQueries(ctx context.Context, db *sql.DB, svc *embedding.Service, c corpus, url, model, path, out string) (search.Semantic, error) {
	space, err := embedding.ActiveSpace(ctx, db)
	if err != nil {
		return nil, err
	}
	if space == nil {
		return nil, fmt.Errorf("no active embedding space")
	}
	status := svc.Status(ctx)
	if status.Service == nil {
		return nil, fmt.Errorf("missing embedding service identity")
	}
	snapshot := querySnapshot{Fingerprint: space.Fingerprint, Vectors: map[string][]float32{}}
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(b, &snapshot); e != nil {
			return nil, e
		}
		if snapshot.Fingerprint != space.Fingerprint {
			return nil, fmt.Errorf("frozen query model fingerprint mismatch")
		}
	}
	client := embedding.Client{URL: url, Model: model}
	queries := []string{"benchmark warmup"}
	for _, q := range c.Questions {
		queries = append(queries, q.Query)
	}
	for _, q := range queries {
		if _, ok := snapshot.Vectors[q]; !ok {
			if path != "" {
				return nil, fmt.Errorf("snapshot lacks query %q", q)
			}
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			v, e := client.Embed(cctx, []string{status.Service.QueryPrefix + q})
			cancel()
			if e != nil {
				return nil, e
			}
			snapshot.Vectors[q] = v[0]
		}
		if len(snapshot.Vectors[q]) != space.Dimensions {
			return nil, fmt.Errorf("frozen vector dimension mismatch")
		}
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(out, "query-vectors.json"), append(b, '\n'), 0600); err != nil {
		return nil, err
	}
	return &frozenSemantic{db: db, svc: svc, space: space, queries: snapshot}, nil
}
