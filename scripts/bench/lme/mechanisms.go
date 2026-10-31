package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/contextpack"
	memdb "github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

// Deterministic pilot: SHA256(question ID) order within each type, ten
// answerable questions/type and five abstentions. Types select the sample only;
// neither types nor gold source IDs are written into cases.jsonl.
func mechanismsCmd(args []string) error {
	fs := flag.NewFlagSet("mechanisms", flag.ContinueOnError)
	data := fs.String("data", "", "verified full dataset")
	indexes := fs.String("indexes", "", "READY indexes")
	out := fs.String("out", "", "new private directory")
	producer := fs.String("reuse-index-source", "", "explicit original source SHA")
	endpoint := fs.String("embed-url", "http://127.0.0.1:18792/v1/embeddings", "cached endpoint")
	if e := fs.Parse(args); e != nil {
		return e
	}
	qs, err := runDataset(*data)
	if err != nil {
		return err
	}
	sort.Slice(qs, func(i, j int) bool { return benchutil.SHA([]byte(qs[i].ID)) < benchutil.SHA([]byte(qs[j].ID)) })
	counts := map[string]int{}
	var selected []question
	for _, q := range qs {
		key := q.Type
		limit := 10
		if strings.HasSuffix(q.ID, "_abs") {
			key = "abstention"
			limit = 5
		}
		if counts[key] >= limit {
			continue
		}
		counts[key]++
		selected = append(selected, q)
	}
	if err = benchutil.NewDir(*out); err != nil {
		return err
	}
	// Write sample before retrieving/model scoring.
	var ids []string
	for _, q := range selected {
		ids = append(ids, q.ID)
	}
	if err = benchutil.JSON(filepath.Join(*out, "sample.json"), ids); err != nil {
		return err
	}
	var cases []benchutil.MechanismCase
	labels := map[string]any{}
	states := map[string]string{}
	for _, q := range selected {
		path := filepath.Join(*indexes, benchutil.SHA([]byte(q.ID))[:24]+".db")
		if err = benchutil.RequireCopy(path); err != nil {
			return err
		}
		raw, e := os.ReadFile(path + ".state.json")
		if e != nil {
			return e
		}
		var st indexState
		if e = json.Unmarshal(raw, &st); e != nil {
			return e
		}
		if e = validateIndexProvenance(st, sourceSHA(), *producer); e != nil {
			return e
		}
		states[q.ID] = benchutil.SHA(raw)
		e = func() error {
			d, e := memdb.Open(path)
			if e != nil {
				return e
			}
			defer d.Close()
			ctx := context.Background()
			emb := embedding.New(d, embedding.Config{URL: *endpoint, Model: "BAAI/bge-m3", QueryTimeout: time.Second})
			emb.Start(ctx)
			status := emb.Status(ctx)
			if status.Active == nil || status.Service == nil || status.Active.Pending != 0 || status.Active.Fingerprint != status.Service.Fingerprint() {
				return fmt.Errorf("embedding space not ready")
			}
			search.SetSemantic(emb)
			defer search.SetSemantic(nil)
			req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: st.Projects[q.ID]}, Query: q.Question}
			var hits []model.SearchResult
			at := time.Now()
			pack, e := contextpack.BuildWithTrace(ctx, d, evalOwner, st.Device, req, func(ev contextpack.TraceEvent) {
				if ev.Stage == "deduplicated" {
					hits = ev.Hits
				}
			})
			if e != nil {
				return e
			}
			after := emb.Status(ctx)
			if after.LastErrorAt != nil && !after.LastErrorAt.Before(at) {
				return fmt.Errorf("semantic degraded: %s", after.LastError)
			}
			control, e := contextpack.BuildFromCandidates(ctx, d, evalOwner, st.Device, req, hits, nil)
			if e != nil || !reflect.DeepEqual(pack, control) {
				return fmt.Errorf("pilot replay differs: %v", e)
			}
			minimum, _ := emb.MinSimilarities()
			row, e := benchutil.MakeMechanismCase(ctx, d, q.ID, evalOwner, st.Device, path, req, minimum, pack, hits)
			if e != nil {
				return e
			}
			cases = append(cases, row)
			gold := q.AnswerIDs
			if strings.HasSuffix(q.ID, "_abs") {
				gold = []string{}
			}
			labels[q.ID] = map[string]any{"gold": gold, "type": q.Type, "sessions": st.Sessions}
			return nil
		}()
		if e != nil {
			return fmt.Errorf("%s: %w", q.ID, e)
		}
		if benchutil.FileSHA(path+".state.json") != states[q.ID] {
			return fmt.Errorf("index state changed")
		}
	}
	if err = benchutil.WriteMechanismCases(filepath.Join(*out, "cases.jsonl"), cases); err != nil {
		return err
	}
	if err = benchutil.JSON(filepath.Join(*out, "labels.json"), labels); err != nil {
		return err
	}
	m := benchutil.Manifest()
	m["dataset_sha256"] = datasetSHA
	m["production_source_sha256"] = sourceSHA()
	m["sample_counts"] = counts
	m["index_state_sha256"] = states
	m["index_producer_sha256"] = *producer
	return benchutil.JSON(filepath.Join(*out, "manifest.json"), m)
}
