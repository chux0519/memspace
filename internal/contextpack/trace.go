package contextpack

import "github.com/chux0519/memspace/internal/model"

// TraceEvent observes one completed stage of context construction. It contains
// source text and is intended for local diagnostics, never the public response.
// No labels or expected answers are supplied to the retrieval pipeline.
type TraceEvent struct {
	Stage    string               `json:"stage"`
	Hits     []model.SearchResult `json:"hits,omitempty"`
	Items    []Item               `json:"items,omitempty"`
	Text     string               `json:"text,omitempty"`
	Decision string               `json:"decision,omitempty"`
	Note     string               `json:"note,omitempty"`
}

// Observer is per-call and synchronous. Instrumented runs are quality
// diagnostics, not measurements of uninstrumented production latency.
type Observer func(TraceEvent)

func observe(observer Observer, event TraceEvent) {
	if observer == nil {
		return
	}
	// An observer must not be able to change the pipeline's ranked lists.
	event.Hits = append([]model.SearchResult(nil), event.Hits...)
	event.Items = append([]Item(nil), event.Items...)
	observer(event)
}
