package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/chux0519/memspace/internal/admission"
	"github.com/chux0519/memspace/internal/contextpack"
	memdb "github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

type namedPolicy struct {
	Name   string           `json:"name"`
	Policy admission.Policy `json:"policy"`
}
type policyOutput struct {
	namedPolicy
	dir              string
	scores, evidence *gzip.Writer
	sf, ef           *os.File
	rows             []scoreRow
	resolved         admission.Resolved
}

func readPolicies(path string) ([]namedPolicy, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	var policies []namedPolicy
	if err = d.Decode(&policies); err != nil {
		return nil, err
	}
	var trailing any
	if err = d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("expected one policies array")
	}
	if len(policies) < 2 {
		return nil, fmt.Errorf("expected at least two named policies")
	}
	seen := map[string]bool{}
	for _, p := range policies {
		if p.Name == "" || filepath.Base(p.Name) != p.Name || p.Name == "." || p.Name == ".." || seen[p.Name] {
			return nil, fmt.Errorf("invalid/duplicate policy name %q", p.Name)
		}
		seen[p.Name] = true
		if err := p.Policy.Validate(); err != nil {
			return nil, fmt.Errorf("policy %s: %w", p.Name, err)
		}
	}
	return policies, nil
}
func packSources(ctx context.Context, d *sql.DB, st indexState, pid string, pack *contextpack.Pack) ([]string, error) {
	var ranked []string
	seen := map[string]bool{}
	for _, it := range pack.Items {
		if it.ProjectID != pid {
			return nil, fmt.Errorf("pack item project mismatch")
		}
		sid := it.Ref
		if _, ok := st.Sessions[sid]; !ok {
			if err := d.QueryRowContext(ctx, `SELECT coalesce(session_id,'') FROM search_docs WHERE ref_id=? AND project_id=? ORDER BY doc_id LIMIT 1`, it.Ref, pid).Scan(&sid); err != nil {
				return nil, err
			}
		}
		source, ok := st.Sessions[sid]
		if !ok {
			return nil, fmt.Errorf("unmapped pack source")
		}
		if !seen[source] {
			ranked = append(ranked, source)
			seen[source] = true
		}
	}
	return ranked, nil
}
func sweepCmd(args []string) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	data := fs.String("data", "", "verified dataset")
	indexes := fs.String("indexes", "", "existing per-question index directory")
	out := fs.String("out", "", "new private results directory")
	policiesPath := fs.String("policies", "", "JSON named policy array")
	endpoint := fs.String("embed-url", "http://127.0.0.1:18792/v1/embeddings", "cached quality endpoint")
	producer := fs.String("reuse-index-source", "", "explicit original READY index producer SHA256")
	limit := fs.Int("limit", 0, "smoke questions; 0=all500")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if b, err := hex.DecodeString(*producer); err != nil || len(b) != 32 {
		return fmt.Errorf("reuse-index-source must be an explicit SHA256")
	}
	qs, err := runDataset(*data)
	if err != nil {
		return err
	}
	if *limit < 0 {
		return fmt.Errorf("negative limit")
	}
	if *limit > 0 {
		qs = qs[:min(*limit, len(qs))]
	}
	profiles, err := readPolicies(*policiesPath)
	if err != nil {
		return err
	}
	if err = benchutil.NewDir(*out); err != nil {
		return err
	}
	var outputs []*policyOutput
	for _, p := range profiles {
		po := &policyOutput{namedPolicy: p, dir: filepath.Join(*out, p.Name)}
		if err = os.Mkdir(po.dir, 0700); err != nil {
			return err
		}
		po.sf, err = os.OpenFile(filepath.Join(po.dir, "scores.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer po.sf.Close()
		po.ef, err = os.OpenFile(filepath.Join(po.dir, "evidence.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer po.ef.Close()
		po.scores = gzip.NewWriter(po.sf)
		po.evidence = gzip.NewWriter(po.ef)
		outputs = append(outputs, po)
	}
	manifest := benchutil.Manifest()
	manifest["dataset_sha256"] = datasetSHA
	manifest["production_source_sha256"] = sourceSHA()
	manifest["policies_sha256"] = benchutil.FileSHA(*policiesPath)
	manifest["index_reuse_expected_source_sha256"] = *producer
	manifest["query_count"] = len(qs)
	manifest["mode"] = "pack"
	manifest["limits"] = []string{"one real production search snapshot per question, replayed through the same qualification/hydration/render pipeline", "snapshots include original semantic EvidenceText; no LLM labels supplied to retrieval or qualification", "search floor, search48, dedup24, final6, expansion threshold and character budgets unchanged", "quality replay timings exclude retrieval; live CPU hook latency must be measured separately", "LongMemEval labels source sessions, not answer spans; 30 abstention cases have no retrieval score"}
	manifest["profile_order"] = "rotate first profile by question index"
	stateHashes := map[string]string{}
	candidateHashes := map[string]string{}
	identical := 0
	for i, q := range qs {
		key := benchutil.SHA([]byte(q.ID))[:24]
		path := filepath.Join(*indexes, key+".db")
		if err = benchutil.RequireCopy(path); err != nil {
			return err
		}
		raw, err := os.ReadFile(path + ".state.json")
		if err != nil {
			return err
		}
		var state indexState
		if err = json.Unmarshal(raw, &state); err != nil {
			return err
		}
		if err = validateIndexProvenance(state, sourceSHA(), *producer); err != nil {
			return err
		}
		stateHashes[q.ID] = benchutil.SHA(raw)
		err = func() error {
			d, e := memdb.Open(path)
			if e != nil {
				return e
			}
			defer d.Close()
			ctx := context.Background()
			emb := embedding.New(d, embedding.Config{URL: *endpoint, Model: "BAAI/bge-m3", QueryTimeout: time.Second})
			emb.Start(ctx)
			status := emb.Status(ctx)
			if status.Active == nil || status.Service == nil || status.Active.Fingerprint != status.Service.Fingerprint() {
				return fmt.Errorf("space/provider mismatch")
			}
			if status.Active.Pending != 0 {
				return fmt.Errorf("index not fully embedded")
			}
			manifest["embedding"] = status
			search.SetSemantic(emb)
			defer search.SetSemantic(nil)
			req := contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: state.Projects[q.ID]}, Query: q.Question}
			var hits []model.SearchResult
			searchAt := time.Now()
			original, e := contextpack.BuildWithTrace(ctx, d, evalOwner, state.Device, req, func(event contextpack.TraceEvent) {
				if event.Stage == "search" {
					hits = event.Hits
				}
			})
			if e != nil {
				return e
			}
			after := emb.Status(ctx)
			if after.LastErrorAt != nil && !after.LastErrorAt.Before(searchAt) {
				return fmt.Errorf("semantic degraded: %s", after.LastError)
			}
			if after.BreakerUntil != nil && after.BreakerUntil.After(searchAt) {
				return fmt.Errorf("semantic circuit breaker open")
			}
			chunks := make([]string, len(hits))
			for j, h := range hits {
				chunks[j] = h.EvidenceText
			}
			snapshot, _ := json.Marshal(struct {
				Hits   []model.SearchResult
				Chunks []string
			}{hits, chunks})
			candidateHashes[q.ID] = benchutil.SHA(snapshot)
			control, e := contextpack.BuildFromCandidates(ctx, d, evalOwner, state.Device, req, hits, nil)
			if e != nil || !reflect.DeepEqual(original, control) {
				return fmt.Errorf("default candidate replay changed original pack for %s: %v", q.ID, e)
			}
			identical++
			for offset := range outputs {
				po := outputs[(i+offset)%len(outputs)]
				modelMin, _ := emb.MinSimilarities()
				po.resolved, e = po.Policy.Resolve(modelMin)
				if e != nil {
					return e
				}
				policyCtx := admission.WithPolicy(ctx, po.Policy)
				at := time.Now()
				pack, e := contextpack.BuildFromCandidates(policyCtx, d, evalOwner, state.Device, req, hits, nil)
				duration := float64(time.Since(at).Microseconds()) / 1000
				if e != nil {
					return e
				}
				ranked, e := packSources(ctx, d, state, req.Project.ID, pack)
				if e != nil {
					return e
				}
				r := score(q, ranked)
				r.Mode = "pack"
				r.Duration = duration
				r.ReturnedDocs = len(pack.Items)
				r.Characters = len([]rune(pack.Text))
				r.Tokens = embedding.EstimateTokens(pack.Text)
				po.rows = append(po.rows, r)
				if e = json.NewEncoder(po.scores).Encode(r); e != nil {
					return e
				}
				gold := q.AnswerIDs
				if !r.Answerable {
					gold = []string{}
				}
				if e = json.NewEncoder(po.evidence).Encode(map[string]any{"question_id": q.ID, "project_id": req.Project.ID, "returned_sessions": ranked, "pack": pack, "doc_ids": []int64{}, "gold_sessions": gold, "raw_dataset_answer_session_ids": q.AnswerIDs, "candidate_snapshot_sha256": candidateHashes[q.ID]}); e != nil {
					return e
				}
			}
			if benchutil.FileSHA(path+".state.json") != stateHashes[q.ID] {
				return fmt.Errorf("index state changed")
			}
			return nil
		}()
		if err != nil {
			return fmt.Errorf("question %s: %w", q.ID, err)
		}
		if (i+1)%25 == 0 || i+1 == len(qs) {
			log.Printf("admission sweep %d/%d; %d profiles", i+1, len(qs), len(outputs))
		}
	}
	manifest["index_state_sha256"] = stateHashes
	manifest["candidate_snapshot_sha256"] = candidateHashes
	manifest["default_replay_identical"] = identical
	manifest["end_environment"] = benchutil.Manifest()
	for _, po := range outputs {
		if err = po.scores.Close(); err != nil {
			return err
		}
		if err = po.evidence.Close(); err != nil {
			return err
		}
		child := map[string]any{}
		for k, v := range manifest {
			child[k] = v
		}
		child["admission_policy"] = po.Policy
		child["resolved_admission_policy"] = po.resolved
		child["scores_sha256"] = benchutil.FileSHA(po.sf.Name())
		child["evidence_sha256"] = benchutil.FileSHA(po.ef.Name())
		if err = benchutil.JSON(filepath.Join(po.dir, "manifest.json"), child); err != nil {
			return err
		}
		if err = benchutil.JSON(filepath.Join(po.dir, "summary.json"), aggregate(po.rows)); err != nil {
			return err
		}
	}
	return benchutil.JSON(filepath.Join(*out, "manifest.json"), manifest)
}
