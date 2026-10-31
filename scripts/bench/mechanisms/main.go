// Command mechanisms replays fixed production candidates for mechanism ablation.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
	_ "github.com/mattn/go-sqlite3"
)

type scores struct {
	ID     string    `json:"id"`
	Single []float64 `json:"single"`
	Window []float64 `json:"window"`
}
type fixedSemantic struct{ minimum float64 }

func (f fixedSemantic) MinSimilarities() (float64, float64) { return f.minimum, 0 }
func (f fixedSemantic) SemanticSearch(context.Context, string, []string, int) ([]search.SemanticHit, error) {
	return nil, fmt.Errorf("unexpected search during fixed-candidate replay")
}
func readLines[T any](path string) ([]T, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	d := json.NewDecoder(f)
	var out []T
	for d.More() {
		var x T
		if e = d.Decode(&x); e != nil {
			return nil, e
		}
		out = append(out, x)
	}
	return out, nil
}
func ranked(c benchutil.MechanismCase, s []float64, gate bool, minimum float64) ([]model.SearchResult, error) {
	if len(s) != len(c.Candidates) {
		return nil, fmt.Errorf("score length mismatch")
	}
	idx := make([]int, len(s))
	for i, v := range s {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("invalid score")
		}
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return s[idx[i]] > s[idx[j]] })
	var out []model.SearchResult
	for _, i := range idx {
		if gate && s[i] <= minimum {
			continue
		}
		h := c.Candidates[i].Hit
		h.EvidenceText = c.Candidates[i].Chunk
		out = append(out, h)
	}
	return out, nil
}

// Keep every selected anchor first. Add only unique adjacent messages to spare
// slots; every original fragment costs one of the same six delivery slots.
func expand(c benchutil.MechanismCase, p *contextpack.Pack) ([]model.SearchResult, error) {
	byID := map[string]benchutil.MechanismCandidate{}
	for _, x := range c.Candidates {
		byID[fmt.Sprintf("doc:%d", x.Hit.DocID)] = x
	}
	var anchors []benchutil.MechanismCandidate
	var out []model.SearchResult
	seen := map[int64]bool{}
	for _, it := range p.Items {
		x, ok := byID[it.Key]
		if !ok {
			return nil, fmt.Errorf("selected anchor absent from fixed pool")
		}
		h := x.Hit
		h.EvidenceText = x.Chunk
		out = append(out, h)
		seen[h.DocID] = true
		anchors = append(anchors, x)
	}
	for _, x := range anchors {
		for _, r := range x.Window {
			if len(out) >= 6 {
				return out, nil
			}
			if seen[r.Hit.DocID] {
				continue
			}
			if r.Hit.ProjectID != c.Request.Project.ID || r.Hit.SessionID != x.Hit.SessionID {
				return nil, fmt.Errorf("window scope mismatch")
			}
			h := r.Hit
			h.EvidenceText = r.Chunk
			out = append(out, h)
			seen[h.DocID] = true
		}
	}
	return out, nil
}
func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("cases", "", "private cases JSONL")
	scorePath := flag.String("scores", "", "fixed model scores.jsonl")
	out := flag.String("out", "", "new output directory")
	minimum := flag.Float64("judge-logit-min", 0, "fixed reference raw-logit boundary; not a calibrated answerability probability")
	flag.Parse()
	if math.IsNaN(*minimum) || math.IsInf(*minimum, 0) {
		return fmt.Errorf("nonfinite judge boundary")
	}
	cases, err := readLines[benchutil.MechanismCase](*path)
	if err != nil {
		return err
	}
	rows, err := readLines[scores](*scorePath)
	if err != nil {
		return err
	}
	if len(cases) != len(rows) {
		return fmt.Errorf("case count mismatch")
	}
	var modelManifest map[string]any
	b, err := os.ReadFile(filepath.Join(filepath.Dir(*scorePath), "manifest.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, &modelManifest); err != nil {
		return err
	}
	if modelManifest["cases_sha256"] != benchutil.FileSHA(*path) || modelManifest["scores_sha256"] != benchutil.FileSHA(*scorePath) {
		return fmt.Errorf("score provenance mismatch")
	}
	if err = benchutil.NewDir(*out); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(*out, "receipts.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	defer search.SetSemantic(nil)
	for i, c := range cases {
		if c.ID != rows[i].ID {
			return fmt.Errorf("case order mismatch")
		}
		if err = benchutil.RequireCopy(c.Index); err != nil {
			return err
		}
		err = func() error {
			d, e := sql.Open("sqlite3", "file:"+c.Index+"?mode=ro&_busy_timeout=5000")
			if e != nil {
				return e
			}
			defer d.Close()
			search.SetSemantic(fixedSemantic{c.ModelMin})
			ctx := context.Background()
			none := admission.WithPolicy(ctx, admission.Policy{Mode: "none"})
			var hits []model.SearchResult
			for _, x := range c.Candidates {
				h := x.Hit
				h.EvidenceText = x.Chunk
				hits = append(hits, h)
			}
			baseline, e := contextpack.BuildFromCandidates(ctx, d, c.Owner, c.Device, c.Request, hits, nil)
			if e != nil {
				return e
			}
			if !reflect.DeepEqual(baseline, c.Baseline) {
				return fmt.Errorf("baseline differs: %s", c.ID)
			}
			for _, mode := range []string{"baseline", "judge_single", "dialogue", "judge_dialogue", "rank_single", "rank_dialogue", "ungated_original", "ungated_dialogue"} {
				at := time.Now()
				p := baseline
				if mode == "baseline" || mode == "dialogue" {
					p, e = contextpack.BuildFromCandidates(ctx, d, c.Owner, c.Device, c.Request, hits, nil)
					if e != nil {
						return e
					}
				}
				if mode != "baseline" {
					anchorPack := p
					if mode != "dialogue" {
						s := rows[i].Single
						if mode == "judge_dialogue" || mode == "rank_dialogue" {
							s = rows[i].Window
						}
						rank := hits
						if mode != "ungated_original" && mode != "ungated_dialogue" {
							rank, e = ranked(c, s, mode == "judge_single" || mode == "judge_dialogue", *minimum)
							if e != nil {
								return e
							}
						}
						anchorPack, e = contextpack.BuildFromCandidates(none, d, c.Owner, c.Device, c.Request, rank, nil)
						if e != nil {
							return e
						}
					}
					p = anchorPack
					if mode == "dialogue" || mode == "judge_dialogue" || mode == "rank_dialogue" || mode == "ungated_dialogue" {
						live := c
						live.Candidates = append([]benchutil.MechanismCandidate(nil), c.Candidates...)
						selected := map[string]bool{}
						for _, it := range anchorPack.Items {
							selected[it.Key] = true
						}
						for j, x := range live.Candidates {
							if !selected[fmt.Sprintf("doc:%d", x.Hit.DocID)] {
								continue
							}
							h := x.Hit
							h.EvidenceText = x.Chunk
							w, e := contextpack.DialogueWindow(ctx, d, c.Owner, c.Request.Project.ID, h)
							if e != nil {
								return e
							}
							if len(w) != len(x.Window) {
								return fmt.Errorf("frozen window changed")
							}
							for k, r := range w {
								if r.Body != x.Window[k].Body || r.Hit.DocID != x.Window[k].Hit.DocID {
									return fmt.Errorf("window content changed")
								}
							}
							live.Candidates[j].Window = w
						}
						expanded, e := expand(live, anchorPack)
						if e != nil {
							return e
						}
						p, e = contextpack.BuildFromEvidence(none, d, c.Owner, c.Device, c.Request, expanded)
						if e != nil {
							return e
						}
					}
				}
				if len(p.Items) > 6 || len([]rune(p.Text)) > contextpack.PromptBudget {
					return fmt.Errorf("output budget violated")
				}
				if e = enc.Encode(map[string]any{"id": c.ID, "mode": mode, "pack": p, "replay_ms": float64(time.Since(at).Microseconds()) / 1000}); e != nil {
					return e
				}
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}
	m := benchutil.Manifest()
	m["cases_sha256"] = benchutil.FileSHA(*path)
	m["scores_sha256"] = benchutil.FileSHA(*scorePath)
	m["model"] = modelManifest
	m["baseline_replays_identical"] = len(cases)
	m["judge_logit_min"] = *minimum
	return benchutil.JSON(filepath.Join(*out, "manifest.json"), m)
}
