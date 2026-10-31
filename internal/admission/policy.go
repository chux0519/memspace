// Package admission configures context qualification independently of search
// ranking, scope checks, content expansion, and the final character budget.
package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
)

type Policy struct {
	Mode string `toml:"admission_mode" json:"mode,omitempty"`
	// Nil inherits the embedding model's existing injection threshold. Explicit
	// zero admits any nonnegative semantic hit; it does not mean "automatic".
	SemanticMin   *float64 `toml:"semantic_min_similarity" json:"semantic_min_similarity,omitempty"`
	KeywordMin    *float64 `toml:"keyword_min_coverage" json:"keyword_min_coverage,omitempty"`
	ExactKeywords *bool    `toml:"allow_exact_keywords" json:"allow_exact_keywords,omitempty"`
}

type Resolved struct {
	Mode            string  `json:"mode"`
	SemanticMin     float64 `json:"semantic_min_similarity"`
	SemanticEnabled bool    `json:"semantic_enabled"`
	KeywordMin      float64 `json:"keyword_min_coverage"`
	ExactKeywords   bool    `json:"allow_exact_keywords"`
}

func (p Policy) Validate() error {
	if p.Mode != "" && p.Mode != "hybrid" && p.Mode != "semantic" && p.Mode != "none" {
		return fmt.Errorf("context.admission_mode must be hybrid, semantic or none")
	}
	for name, value := range map[string]*float64{"semantic_min_similarity": p.SemanticMin, "keyword_min_coverage": p.KeywordMin} {
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 1) {
			return fmt.Errorf("context.%s must be finite and within [0,1]", name)
		}
	}
	return nil
}

func (p Policy) Resolve(modelMin float64) (Resolved, error) {
	if err := p.Validate(); err != nil {
		return Resolved{}, err
	}
	r := Resolved{Mode: p.Mode, SemanticMin: modelMin, SemanticEnabled: modelMin > 0, KeywordMin: .5, ExactKeywords: true}
	if r.Mode == "" {
		r.Mode = "hybrid"
	}
	if p.SemanticMin != nil {
		r.SemanticMin = *p.SemanticMin
		r.SemanticEnabled = true
	}
	if p.KeywordMin != nil {
		r.KeywordMin = *p.KeywordMin
	}
	if p.ExactKeywords != nil {
		r.ExactKeywords = *p.ExactKeywords
	}
	return r, nil
}

func (r Resolved) Accept(match string, similarity float64, matched, total int) bool {
	if r.Mode == "none" {
		return true
	}
	// Similarity zero denotes a keyword-only hit in search.SearchResult.
	semantic := r.SemanticEnabled && similarity > 0 && similarity >= r.SemanticMin
	if r.Mode == "semantic" {
		return semantic
	}
	return semantic || r.ExactKeywords && match == "all" || match == "any" && total > 0 && float64(matched)/float64(total) >= r.KeywordMin
}

type contextKey struct{}

// WithPolicy snapshots the values, so one request cannot mutate another's
// configuration. This is server configuration, never a public request field.
func WithPolicy(ctx context.Context, p Policy) context.Context {
	if p.SemanticMin != nil {
		v := *p.SemanticMin
		p.SemanticMin = &v
	}
	if p.KeywordMin != nil {
		v := *p.KeywordMin
		p.KeywordMin = &v
	}
	if p.ExactKeywords != nil {
		v := *p.ExactKeywords
		p.ExactKeywords = &v
	}
	return context.WithValue(ctx, contextKey{}, p)
}

func FromContext(ctx context.Context) Policy {
	p, _ := ctx.Value(contextKey{}).(Policy)
	return p
}

// Load reads a benchmark/server policy JSON and rejects misspelled fields.
func Load(path string) (Policy, error) {
	f, err := os.Open(path)
	if err != nil {
		return Policy{}, err
	}
	defer f.Close()
	var p Policy
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err = d.Decode(&p); err != nil {
		return p, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return p, fmt.Errorf("expected one policy JSON object")
	}
	return p, p.Validate()
}
