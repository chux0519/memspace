package embedding

import "strings"

// Thresholds are the cosine similarities a meaning-only match needs. Their
// scale depends on the model: Snowflake arctic-embed scores relevant pairs
// around 0.32, bge-m3 around 0.60 on a Chinese/English agent-history
// benchmark (scripts/bench/eval.py), so one set of numbers cannot serve both.
type Thresholds struct {
	// Inject: a semantic-only match is put into an agent's context.
	Inject float64 `json:"inject"`
	// SearchFloor: below it, semantic hits are noise and dropped from search.
	SearchFloor float64 `json:"search_floor"`
	// Source says where the values come from: historical model defaults,
	// "config", or "default" for an uncalibrated model.
	Source string `json:"source"`
}

// historical maps model names to defaults informed by a small authored
// embedding-pair study, not independent end-to-end admission validation.
var historical = []struct {
	fragment string
	t        Thresholds
}{
	// 2040 docs with real distractors: at 0.60, 51% of relevant and 0.016%
	// of assumed-irrelevant pairs pass; five easy off-topic queries top out
	// at 0.50. These are pair rates, not query-level false-injection rates.
	{"bge-m3", Thresholds{Inject: 0.60, SearchFloor: 0.45}},
	// At 0.35, 40% of relevant and 0.03% of irrelevant pairs pass;
	// off-topic queries top out at 0.29.
	{"arctic-embed", Thresholds{Inject: 0.35, SearchFloor: 0.20}},
}

// uncalibrated is used for models without measurements. Most modern
// embedding models score higher than arctic-embed, so these lean strict for
// injection; calibrate and set them in the config.
var uncalibrated = Thresholds{Inject: 0.60, SearchFloor: 0.30, Source: "default"}

// ThresholdsFor returns the thresholds for a model, with non-zero overrides
// (from the config) taking precedence.
func ThresholdsFor(model string, inject, searchFloor float64) Thresholds {
	t := uncalibrated
	m := strings.ToLower(model)
	for _, c := range historical {
		if strings.Contains(m, c.fragment) {
			t = c.t
			t.Source = c.fragment
			break
		}
	}
	if inject > 0 {
		t.Inject, t.Source = inject, "config"
	}
	if searchFloor > 0 {
		t.SearchFloor, t.Source = searchFloor, "config"
	}
	return t
}
