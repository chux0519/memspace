package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

var parsedRunData struct {
	path      string
	size      int64
	mod       time.Time
	questions []question
}

// Only immutable JSON input is shared. Each question still gets its own DB.
func runDataset(path string) ([]question, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	info, e := os.Stat(absolute)
	if e != nil {
		return nil, e
	}
	if parsedRunData.path == absolute && parsedRunData.size == info.Size() && parsedRunData.mod.Equal(info.ModTime()) {
		return parsedRunData.questions, nil
	}
	if benchutil.FileSHA(absolute) != datasetSHA {
		return nil, fmt.Errorf("dataset SHA mismatch")
	}
	qs, e := load(absolute)
	if e != nil {
		return nil, e
	}
	if len(qs) != 500 {
		return nil, fmt.Errorf("expected500 questions")
	}
	parsedRunData.path = absolute
	parsedRunData.size = info.Size()
	parsedRunData.mod = info.ModTime()
	parsedRunData.questions = qs
	return qs, nil
}
func isolatedRun(data, dbBase, out, url, mode string, noFloor bool, qs []question) error {
	if e := benchutil.NewDir(out); e != nil {
		return e
	}
	parts := filepath.Join(out, "parts")
	if e := os.MkdirAll(parts, 0700); e != nil {
		return e
	}
	dbDir := dbBase + "-projects"
	if e := os.MkdirAll(dbDir, 0700); e != nil {
		return e
	}
	manifest := benchutil.Manifest()
	manifest["dataset_sha256"] = datasetSHA
	manifest["mode"] = mode
	manifest["diagnostic_no_floor"] = noFloor
	manifest["index_isolation"] = "independent database per question; immutable dataset parsing and text vectors may be cached"
	manifest["production_source_sha256"] = sourceSHA()
	manifest["limits"] = []string{"real API event ingestion, historical dates, DocFromEvent truncation/chunking retained", "top100 documents mapped to distinct top10 source sessions; shortage not padded", "pack measures selected source coverage, not semantic answer completeness", "cached embedding latency is not production hook latency", "30 _abs questions execution only,470 scored"}
	sf, e := os.OpenFile(filepath.Join(out, "scores.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer sf.Close()
	sg := gzip.NewWriter(sf)
	defer sg.Close()
	ef, e := os.OpenFile(filepath.Join(out, "evidence.ndjson.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer ef.Close()
	eg := gzip.NewWriter(ef)
	defer eg.Close()
	var rows []scoreRow
	childHashes := map[string]string{}
	counts := map[string]int64{}
	duplicates := 0
	fingerprints := map[string]bool{}
	for i, q := range qs {
		key := benchutil.SHA([]byte(q.ID))[:24]
		child := filepath.Join(parts, key)
		index := filepath.Join(dbDir, key+".db")
		// A prior run's source index is reusable; result directories are immutable.
		args := []string{"-data", data, "-db", index, "-out", child, "-embed-url", url, "-mode", mode, "-question-id", q.ID}
		if noFloor {
			args = append(args, "-no-floor")
		}
		if e = runCmd(args); e != nil {
			return fmt.Errorf("isolated question %s: %w", q.ID, e)
		}
		var score scoreRow
		if e = decodeSingleGzip(filepath.Join(child, "scores.ndjson.gz"), &score); e != nil {
			return e
		}
		if score.QuestionID != q.ID {
			return fmt.Errorf("question output mismatch")
		}
		rows = append(rows, score)
		if e = json.NewEncoder(sg).Encode(score); e != nil {
			return e
		}
		var evidence map[string]any
		if e = decodeSingleGzip(filepath.Join(child, "evidence.ndjson.gz"), &evidence); e != nil {
			return e
		}
		if e = json.NewEncoder(eg).Encode(evidence); e != nil {
			return e
		}
		var cm map[string]json.RawMessage
		b, e := os.ReadFile(filepath.Join(child, "manifest.json"))
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &cm); e != nil {
			return e
		}
		childHashes[q.ID] = benchutil.SHA(b)
		var cc map[string]int64
		json.Unmarshal(cm["db_counts"], &cc)
		for k, v := range cc {
			counts[k] += v
		}
		var dup int
		json.Unmarshal(cm["duplicate_sources_coalesced"], &dup)
		duplicates += dup
		if raw, ok := cm["provider_info"]; ok {
			manifest["provider_info"] = raw
		}
		if raw, ok := cm["embedding"]; ok {
			var es struct {
				Active struct {
					Fingerprint string `json:"fingerprint"`
				} `json:"active_space"`
				Thresholds any `json:"thresholds"`
			}
			json.Unmarshal(raw, &es)
			fingerprints[es.Active.Fingerprint] = true
			manifest["thresholds"] = es.Thresholds
		}
		if e = sg.Flush(); e != nil {
			return e
		}
		if e = eg.Flush(); e != nil {
			return e
		}
		log.Printf("isolated %s %d/%d", mode, i+1, len(qs))
	}
	if e = sg.Close(); e != nil {
		return e
	}
	if e = eg.Close(); e != nil {
		return e
	}
	manifest["summed_db_counts"] = counts
	manifest["question_count"] = len(qs)
	manifest["duplicate_sources_coalesced"] = duplicates
	manifest["child_manifest_sha256"] = childHashes
	manifest["embedding_fingerprints"] = fingerprints
	manifest["scores_sha256"] = benchutil.FileSHA(filepath.Join(out, "scores.ndjson.gz"))
	manifest["evidence_sha256"] = benchutil.FileSHA(filepath.Join(out, "evidence.ndjson.gz"))
	manifest["end_environment"] = benchutil.Manifest()
	if e = benchutil.JSON(filepath.Join(out, "manifest.json"), manifest); e != nil {
		return e
	}
	return benchutil.JSON(filepath.Join(out, "summary.json"), aggregate(rows))
}
func decodeSingleGzip(path string, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	z, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer z.Close()
	dec := json.NewDecoder(z)
	if e = dec.Decode(out); e != nil {
		return e
	}
	var extra any
	if e = dec.Decode(&extra); e != io.EOF {
		return fmt.Errorf("expected one receipt in %s", path)
	}
	return nil
}
