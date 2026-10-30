package main

import (
	"encoding/json"
	"fmt"
)

type relevanceLabels struct {
	CorpusSHA256 string              `json:"corpus_sha256"`
	Version      string              `json:"version"`
	Criterion    string              `json:"criterion"`
	Judgments    map[string][]string `json:"judgments"`
}

func applyRelevanceLabels(c *corpus, raw []byte, sha string) error {
	var labels relevanceLabels
	if err := json.Unmarshal(raw, &labels); err != nil {
		return err
	}
	if labels.CorpusSHA256 != sha {
		return fmt.Errorf("relevance labels corpus SHA mismatch")
	}
	known := map[string]int{}
	for i, q := range c.Questions {
		known[q.ID] = i
	}
	for id, refs := range labels.Judgments {
		i, ok := known[id]
		if !ok {
			return fmt.Errorf("unknown judged question %s", id)
		}
		if refs == nil {
			return fmt.Errorf("use [] for a judged empty relevance set: %s", id)
		}
		c.Questions[i].RelevantRefs = append([]string{}, refs...)
	}
	return nil
}
