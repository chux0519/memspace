package contextpack

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
)

// EvidenceRecord is an owner/project-scoped source record, not a generated
// summary. Chunk is the search anchor's real matching chunk when available.
type EvidenceRecord struct {
	Hit   model.SearchResult `json:"hit"`
	Body  string             `json:"body"`
	Chunk string             `json:"chunk,omitempty"`
}

// DialogueWindow supplies at most one preceding and one following message
// around an anchor. These are diagnostic building blocks; ordinary Build does
// not expand its candidates or change qualification. Scope and provenance are
// re-read from the source table, never borrowed from a neighbouring session.
func DialogueWindow(ctx context.Context, db *sql.DB, owner, project string, hit model.SearchResult) ([]EvidenceRecord, error) {
	const fields = `SELECT d.doc_id,d.kind,d.ref_id,d.project_id,p.name,coalesce(d.session_id,''),coalesce(d.agent_type,''),coalesce(d.device_id,''),coalesce(dv.name,''),d.occurred_at,d.title,d.body FROM search_docs d JOIN projects p ON p.project_id=d.project_id LEFT JOIN devices dv ON dv.device_id=d.device_id `
	scan := func(row *sql.Row) (EvidenceRecord, string, error) {
		var r EvidenceRecord
		var stamp string
		err := row.Scan(&r.Hit.DocID, &r.Hit.Kind, &r.Hit.RefID, &r.Hit.ProjectID, &r.Hit.ProjectName, &r.Hit.SessionID, &r.Hit.AgentType, &r.Hit.DeviceID, &r.Hit.DeviceName, &stamp, &r.Hit.Title, &r.Body)
		if err != nil {
			return r, stamp, err
		}
		r.Hit.OccurredAt, err = time.Parse(time.RFC3339Nano, stamp)
		r.Hit.Match = "context"
		r.Hit.Snippet = redact.Truncate(strings.Join(strings.Fields(r.Body), " "), 300)
		return r, stamp, err
	}
	anchor, stamp, err := scan(db.QueryRowContext(ctx, fields+`WHERE d.doc_id=? AND d.project_id=? AND p.user_id=?`, hit.DocID, project, owner))
	if err != nil {
		return nil, fmt.Errorf("dialogue anchor: %w", err)
	}
	// Preserve retrieval signals but use canonical source identity and time.
	anchor.Hit.Match = hit.Match
	anchor.Hit.Score = hit.Score
	anchor.Hit.Similarity = hit.Similarity
	anchor.Hit.MatchedTerms = hit.MatchedTerms
	anchor.Hit.TotalTerms = hit.TotalTerms
	anchor.Hit.Snippet = hit.Snippet
	anchor.Hit.EvidenceText = hit.EvidenceText
	anchor.Chunk = hit.EvidenceText
	if anchor.Hit.SessionID == "" {
		return []EvidenceRecord{anchor}, nil
	}
	neighbor := func(op, order string) (EvidenceRecord, error) {
		r, _, e := scan(db.QueryRowContext(ctx, fields+`WHERE p.user_id=? AND d.project_id=? AND d.session_id=? AND d.kind IN ('prompt','reply') AND (d.occurred_at,d.doc_id) `+op+` (?,?) ORDER BY d.occurred_at `+order+`,d.doc_id `+order+` LIMIT 1`, owner, project, anchor.Hit.SessionID, stamp, anchor.Hit.DocID))
		return r, e
	}
	var result []EvidenceRecord
	prev, err := neighbor("<", "DESC")
	if err == nil {
		result = append(result, prev)
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	result = append(result, anchor)
	next, err := neighbor(">", "ASC")
	if err == nil {
		result = append(result, next)
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	return result, nil
}

type evidenceReplayKey struct{}

// BuildFromEvidence preserves the supplied fragment order and roles rather
// than allowing an answer to suppress a selected prompt. This trusted local
// experiment API retains the normal scope/content checks and output budgets.
// It is not enabled by normal HTTP requests or the server configuration.
func BuildFromEvidence(ctx context.Context, db *sql.DB, owner, device string, req Request, hits []model.SearchResult) (*Pack, error) {
	return BuildFromCandidates(context.WithValue(ctx, evidenceReplayKey{}, true), db, owner, device, req, hits, nil)
}
