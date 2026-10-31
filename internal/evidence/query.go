package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const linkCols = `link_id,project_id,relation,from_object_id,to_object_id,basis,status,rule_version,scope_json,witness_refs,assertion_record_id`

func scanLink(s interface{ Scan(...any) error }) (Link, error) {
	var l Link
	e := s.Scan(&l.ID, &l.Project, &l.Relation, &l.From, &l.To, &l.Basis, &l.Status, &l.Rule, &l.Scope, &l.Witnesses, &l.Record)
	return l, e
}

// Related returns only owner/project-scoped actionable direct relationships.
// The caller still validates versions and budgets; a graph edge is not proof.
func Related(ctx context.Context, db *sql.DB, owner, project string, seeds []string, limit int) ([]Link, error) {
	if len(seeds) == 0 {
		return []Link{}, nil
	}
	if len(seeds) > 128 || limit < 1 || limit > 512 {
		return nil, fmt.Errorf("invalid evidence query bounds")
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(seeds)), ",")
	args := []any{project, owner}
	for _, id := range seeds {
		args = append(args, id)
	}
	for _, id := range seeds {
		args = append(args, id)
	}
	args = append(args, limit)
	rows, e := db.QueryContext(ctx, `SELECT `+qualifiedLinkCols("l")+` FROM evidence_links l JOIN projects p ON p.project_id=l.project_id JOIN evidence_objects a ON a.object_id=l.from_object_id AND a.project_id=l.project_id JOIN evidence_objects b ON b.object_id=l.to_object_id AND b.project_id=l.project_id WHERE l.project_id=? AND p.user_id=? AND l.status='active' AND (l.from_object_id IN (`+marks+`) OR l.to_object_id IN (`+marks+`)) ORDER BY CASE l.relation WHEN 'reply_to' THEN 0 WHEN 'supersedes' THEN 1 ELSE 2 END,l.link_id LIMIT ?`, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Link{}
	for rows.Next() {
		l, e := scanLink(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func qualifiedLinkCols(alias string) string {
	return alias + "." + strings.ReplaceAll(linkCols, ",", ","+alias+".")
}
func GetObject(ctx context.Context, db *sql.DB, owner, project, id string) (Object, error) {
	return scanObject(db.QueryRowContext(ctx, `SELECT `+"o."+strings.ReplaceAll(objectCols, ",", ",o.")+` FROM evidence_objects o JOIN projects p ON p.project_id=o.project_id WHERE o.object_id=? AND o.project_id=? AND p.user_id=?`, id, project, owner))
}
