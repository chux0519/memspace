package main

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"path/filepath"

	"github.com/chux0519/memspace/internal/embedding"
	"github.com/chux0519/memspace/internal/redact"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

// Audit is read-only and never calls an embedding service. It distinguishes
// precomputed source/query inputs from texts changed by real API redaction.
func auditCacheCmd(args []string) error {
	fs := flag.NewFlagSet("audit-cache", flag.ContinueOnError)
	data := fs.String("data", "", "pinned dataset")
	cachePath := fs.String("cache", "", "existing cache, opened read-only")
	out := fs.String("out", "", "private audit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cachePath == "" || *out == "" {
		return fmt.Errorf("-cache and -out required")
	}
	qs, err := runDataset(*data)
	if err != nil {
		return err
	}
	raw, actual, queries := map[string]bool{}, map[string]bool{}, map[string]bool{}
	seen := map[string]bool{}
	for _, q := range qs {
		queries[hex.EncodeToString(hash(q.Question))] = true
		for si, session := range q.Sessions {
			if seen[q.SessionIDs[si]] {
				continue
			}
			seen[q.SessionIDs[si]] = true
			for _, text := range chunks(session) {
				raw[hex.EncodeToString(hash(text))] = true
			}
			for _, turn := range session {
				typ, payload := event(turn)
				b, _ := json.Marshal(payload)
				d := search.DocFromEvent(search.Event{EventType: typ, Payload: redact.JSON(b)})
				if d == nil {
					continue
				}
				for _, text := range embedding.Chunk(embedding.DocText(d.Title, d.Body, d.Files)) {
					actual[hex.EncodeToString(hash(text))] = true
				}
			}
		}
	}
	absolute, err := filepath.Abs(*cachePath)
	if err != nil {
		return err
	}
	uri := url.URL{Scheme: "file", Path: absolute, RawQuery: "mode=ro&_busy_timeout=5000"}
	cache, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return err
	}
	defer cache.Close()
	rows, err := cache.Query(`SELECT hash,length(vec) FROM vectors`)
	if err != nil {
		return err
	}
	defer rows.Close()
	present := map[string]bool{}
	redactedExtra, unknown, badDimensions := 0, 0, 0
	for rows.Next() {
		var key []byte
		var bytes int
		if err = rows.Scan(&key, &bytes); err != nil {
			return err
		}
		h := hex.EncodeToString(key)
		present[h] = true
		if bytes != 1024*4 {
			badDimensions++
		}
		if !raw[h] && !queries[h] {
			if actual[h] {
				redactedExtra++
			} else {
				unknown++
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	missing := func(want map[string]bool) int {
		n := 0
		for h := range want {
			if !present[h] {
				n++
			}
		}
		return n
	}
	audit := map[string]any{
		"dataset_sha256": datasetSHA, "cached": len(present), "raw_source_unique": len(raw),
		"query_unique": len(queries), "api_redacted_source_unique": len(actual),
		"missing_raw_sources": missing(raw), "missing_queries": missing(queries),
		"missing_api_sources": missing(actual), "extra_from_api_redaction": redactedExtra,
		"unclassified_extra": unknown, "wrong_dimensions": badDimensions,
		"scope": "SHA256 text coverage and 1024 float32 dimensions; no inference or vector recomputation",
	}
	if err = benchutil.JSON(*out, audit); err != nil {
		return err
	}
	if missing(raw)+missing(queries)+missing(actual)+badDimensions != 0 {
		return fmt.Errorf("incomplete or corrupt cache; see audit")
	}
	return nil
}
