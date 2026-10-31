package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

type Seed struct {
	Kind string
	Ref  string
}

// CurrentObjects batches reference lookup. It never reuses an obsolete derived
// version after memory/summary text or provenance changes while capture is off.
func CurrentObjects(ctx context.Context, db *sql.DB, owner, project string, seeds []Seed) (map[Seed]Object, error) {
	if len(seeds) > 128 {
		return nil, fmt.Errorf("too many evidence seeds")
	}
	out := map[Seed]Object{}
	if len(seeds) == 0 {
		return out, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(seeds)), ",")
	args := []any{project, owner}
	wanted := map[Seed]bool{}
	for _, seed := range seeds {
		args = append(args, seed.Ref)
		wanted[seed] = true
	}
	rows, e := db.QueryContext(ctx, `SELECT o.`+strings.ReplaceAll(objectCols, ",", ",o.")+` FROM evidence_objects o JOIN projects p ON p.project_id=o.project_id WHERE o.project_id=? AND p.user_id=? AND o.is_current=1 AND o.ref_id IN (`+marks+`)`, args...)
	if e != nil {
		return nil, e
	}
	var objects []Object
	for rows.Next() {
		o, e := scanObject(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		objects = append(objects, o)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	for _, o := range objects {
		kind := o.Kind
		if kind == "event" {
			switch o.Role {
			case "prompt":
				kind = "prompt"
			case "reply":
				kind = "reply"
			default:
				for seed := range wanted {
					if seed.Ref == o.Ref && seed.Kind != "memory" && seed.Kind != "summary" {
						out[seed] = o
					}
				}
				continue
			}
		} else if kind == "memory_version" {
			kind = "memory"
		} else if kind == "summary_version" {
			kind = "summary"
		} else {
			continue
		}
		seed := Seed{kind, o.Ref}
		if !wanted[seed] {
			continue
		}
		if o.SourceKind == "record" {
			var title, body, sources string
			if kind == "memory" {
				e = db.QueryRowContext(ctx, `SELECT title,content,source_refs FROM memories WHERE memory_id=? AND project_id=?`, o.Ref, project).Scan(&title, &body, &sources)
			} else {
				e = db.QueryRowContext(ctx, `SELECT d.title,d.body,s.evidence FROM search_docs d JOIN session_summaries s ON s.session_id=d.ref_id WHERE d.kind='summary' AND d.ref_id=? AND d.project_id=?`, o.Ref, project).Scan(&title, &body, &sources)
			}
			if e == sql.ErrNoRows {
				continue
			}
			if e != nil {
				return nil, e
			}
			var refs []SourceRef
			if kind == "memory" {
				if e = json.Unmarshal([]byte(sources), &refs); e != nil {
					return nil, e
				}
			} else {
				var ids []string
				if e = json.Unmarshal([]byte(sources), &ids); e != nil {
					return nil, e
				}
				refs = []SourceRef{{Kind: "session", ID: o.Ref}}
				for _, id := range ids {
					refs = append(refs, SourceRef{Kind: "event", ID: id})
				}
			}
			if derivedRevision(title, body, refs) != o.Revision {
				continue
			}
		}
		out[seed] = o
	}
	return out, nil
}
