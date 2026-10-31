// Command evidence builds optional evidence projections in an explicit DB copy.
// It never opens the production database and never calls a model provider.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chux0519/memspace/internal/contextpack"
	memdb "github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/search"
	"github.com/chux0519/memspace/scripts/bench/benchutil"
)

type probe struct{ Project, Query string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("db", "", "existing disposable SQLite backup only")
	out := flag.String("out", "", "new private output directory")
	flag.Parse()
	if err := benchutil.RequireCopy(*path); err != nil {
		return err
	}
	if err := benchutil.NewDir(*out); err != nil {
		return err
	}
	db, err := memdb.Open(*path)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	rows, err := db.Query(`SELECT DISTINCT user_id FROM projects`)
	if err != nil {
		return err
	}
	var owners []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		owners = append(owners, id)
	}
	rows.Close()
	if len(owners) != 1 {
		return fmt.Errorf("expected one owner in evaluation backup")
	}
	owner := owners[0]
	// The output comparison uses the same DB/source content before/after capture.
	// No semantic provider is registered: parity here is explicitly keyword-only.
	var probes []probe
	rows, err = db.Query(`SELECT project_id,json_extract(payload_json,'$.prompt') FROM events WHERE event_type='user.prompt' AND length(json_extract(payload_json,'$.prompt'))>=4 ORDER BY event_id LIMIT 50`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p probe
		if err = rows.Scan(&p.Project, &p.Query); err != nil {
			rows.Close()
			return err
		}
		r := []rune(p.Query)
		if len(r) > 128 {
			p.Query = string(r[:128])
		}
		probes = append(probes, p)
	}
	rows.Close()
	before := []*contextpack.Pack{}
	for _, p := range probes {
		pack, e := contextpack.Build(ctx, db, owner, "", contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: p.Project}, Query: p.Query})
		if e != nil {
			return e
		}
		before = append(before, pack)
	}
	fingerprint := func() (string, error) {
		h := sha256.New()
		r, e := db.Query(`SELECT event_id,payload_json FROM events ORDER BY event_id`)
		if e != nil {
			return "", e
		}
		defer r.Close()
		for r.Next() {
			var id, payload string
			if e = r.Scan(&id, &payload); e != nil {
				return "", e
			}
			b, _ := json.Marshal([]string{id, payload})
			h.Write(b)
		}
		return hex.EncodeToString(h.Sum(nil)), r.Err()
	}
	original, e := fingerprint()
	if e != nil {
		return e
	}
	rows, err = db.Query(`SELECT event_id FROM events ORDER BY occurred_at,event_id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	capture := evidence.WithPolicy(ctx, evidence.Policy{Mode: "shadow"})
	at := time.Now()
	for start := 0; start < len(ids); start += 256 {
		tx, e := db.BeginTx(capture, nil)
		if e != nil {
			return e
		}
		for _, id := range ids[start:min(start+256, len(ids))] {
			if e = evidence.IndexEvent(capture, tx, id); e != nil {
				tx.Rollback()
				return e
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	type derivedRow struct{ Kind, Ref, Project, Session, Title, Body, Sources, At string }
	var derived []derivedRow
	rows, err = db.Query(`SELECT 'memory_version',memory_id,project_id,coalesce(session_id,''),title,content,source_refs,updated_at FROM memories`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r derivedRow
		if e = rows.Scan(&r.Kind, &r.Ref, &r.Project, &r.Session, &r.Title, &r.Body, &r.Sources, &r.At); e != nil {
			rows.Close()
			return e
		}
		derived = append(derived, r)
	}
	rows.Close()
	rows, err = db.Query(`SELECT 'summary_version',d.ref_id,d.project_id,d.session_id,d.title,d.body,s.evidence,d.occurred_at FROM search_docs d JOIN session_summaries s ON s.session_id=d.ref_id WHERE d.kind='summary'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r derivedRow
		if e = rows.Scan(&r.Kind, &r.Ref, &r.Project, &r.Session, &r.Title, &r.Body, &r.Sources, &r.At); e != nil {
			rows.Close()
			return e
		}
		derived = append(derived, r)
	}
	rows.Close()
	for _, r := range derived {
		var refs []evidence.SourceRef
		if r.Kind == "memory_version" {
			if e = json.Unmarshal([]byte(r.Sources), &refs); e != nil {
				return e
			}
		} else {
			var ids []string
			if e = json.Unmarshal([]byte(r.Sources), &ids); e != nil {
				return e
			}
			refs = []evidence.SourceRef{{Kind: "session", ID: r.Session}}
			for _, id := range ids {
				refs = append(refs, evidence.SourceRef{Kind: "event", ID: id})
			}
		}
		stamp, e := time.Parse(time.RFC3339Nano, r.At)
		if e != nil {
			return e
		}
		tx, e := db.BeginTx(capture, nil)
		if e != nil {
			return e
		}
		e = evidence.RecordDerived(capture, tx, evidence.Derived{Kind: r.Kind, Ref: r.Ref, Project: r.Project, Session: r.Session, Title: r.Title, Content: r.Body, Sources: refs, At: stamp})
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	after, e := fingerprint()
	if e != nil {
		return e
	}
	if original != after {
		return fmt.Errorf("raw event content changed")
	}
	identical := 0
	for i, p := range probes {
		pack, e := contextpack.Build(capture, db, owner, "", contextpack.Request{Mode: contextpack.ModePrompt, Project: search.ProjectRef{ID: p.Project}, Query: p.Query})
		if e != nil {
			return e
		}
		a, _ := json.Marshal(before[i])
		b, _ := json.Marshal(pack)
		if string(a) != string(b) {
			return fmt.Errorf("shadow changed original pack")
		}
		identical++
	}
	counts := map[string]int{}
	for _, table := range []string{"events", "search_docs", "evidence_objects", "evidence_links", "evidence_records"} {
		var n int
		if e = db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); e != nil {
			return e
		}
		counts[table] = n
	}
	links := map[string]int{}
	rows, e = db.Query(`SELECT relation||':'||basis||':'||status,count(*) FROM evidence_links GROUP BY relation,basis,status`)
	if e != nil {
		return e
	}
	for rows.Next() {
		var key string
		var n int
		rows.Scan(&key, &n)
		links[key] = n
	}
	rows.Close()
	manifest := benchutil.Manifest()
	manifest["counts"] = counts
	manifest["relations"] = links
	manifest["shadow_pack_identical"] = identical
	manifest["raw_events_sha256"] = original
	manifest["capture_ms"] = float64(time.Since(at).Microseconds()) / 1000
	manifest["parity_scope"] = "keyword-only on same DB; no external model calls"
	manifest["historical_snapshot_limit"] = "current stored memory/summary versions only; absent old bodies are not invented"
	return benchutil.JSON(filepath.Join(*out, "manifest.json"), manifest)
}
