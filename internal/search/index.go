// Package search maintains the full-text index and answers
// scoped queries.
//
// The index holds derived, short-form documents rather than raw event JSON:
// the user's own prompt text, the agent's final replies, failed commands
// with their error head, shell commands without output, changed file paths,
// and session summaries. Every document points back to its source
// (kind + ref_id) for traceability.
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/prompt"
	"github.com/chux0519/memspace/internal/redact"
)

// Document kinds.
const (
	KindPrompt  = "prompt"
	KindReply   = "reply"
	KindFailure = "failure"
	KindCommand = "command"
	KindChange  = "change"
	KindSummary = "summary"
	KindMemory  = "memory"
)

// Doc is one searchable unit.
type Doc struct {
	Kind       string
	RefID      string // event_id, or session_id for summaries
	ProjectID  string
	SessionID  string
	DeviceID   string
	AgentType  string
	OccurredAt time.Time
	Title      string
	Body       string
	Files      string
}

// Event is the subset of an ingested event the indexer needs.
type Event struct {
	EventID    string
	EventType  string
	ProjectID  string
	SessionID  string
	DeviceID   string
	AgentType  string
	OccurredAt time.Time
	Payload    []byte
}

const (
	maxBodyBytes  = 4 << 10
	maxErrorBytes = 1 << 10
)

// DocFromEvent derives the searchable document for an event, or nil when the
// event carries nothing worth finding later.
func DocFromEvent(ev Event) *Doc {
	var p map[string]any
	if json.Unmarshal(ev.Payload, &p) != nil {
		return nil
	}
	d := &Doc{
		RefID: ev.EventID, ProjectID: ev.ProjectID, SessionID: ev.SessionID,
		DeviceID: ev.DeviceID, AgentType: ev.AgentType, OccurredAt: ev.OccurredAt,
	}
	str := func(k string) string { s, _ := p[k].(string); return s }
	toolName := str("tool_name")
	input, _ := p["tool_input"].(map[string]any)
	command, _ := input["command"].(string)

	switch ev.EventType {
	case "user.prompt":
		d.Kind = KindPrompt
		d.Body = prompt.StripPlaceholders(str("prompt"))
		// Error lines from pasted logs are what people search for later.
		if atts, ok := p["attachments"].([]any); ok {
			var errs []string
			for _, a := range atts {
				m, _ := a.(map[string]any)
				if lines, ok := m["errors"].([]any); ok {
					for _, l := range lines {
						if s, ok := l.(string); ok {
							errs = append(errs, s)
						}
					}
				}
			}
			if len(errs) > 0 {
				d.Body += "\n" + strings.Join(errs, "\n")
			}
		}
	case "agent.turn_completed":
		d.Kind = KindReply
		d.Body = str("last_assistant_message")
	case "tool.failed":
		d.Kind = KindFailure
		d.Title = strings.TrimSpace(toolName + " " + firstLine(command))
		errText := str("error")
		if errText == "" {
			errText = str("status")
		}
		d.Body = redact.Truncate(errText, maxErrorBytes)
		d.Files = filesOf(p, input)
	case "tool.completed":
		switch {
		case command != "":
			d.Kind = KindCommand
			d.Body = command
		case input["query"] != nil:
			d.Kind = KindCommand
			d.Title = toolName
			d.Body = fmt.Sprint(input["query"])
		default:
			return nil // reads, greps, edits (edits are file.modified)
		}
	case "file.modified":
		d.Kind = KindChange
		d.Files = filesOf(p, input)
		if d.Files == "" {
			return nil
		}
	default:
		return nil
	}
	d.Body = redact.Truncate(strings.TrimSpace(d.Body), maxBodyBytes)
	if d.Title == "" && d.Body == "" && d.Files == "" {
		return nil
	}
	return d
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return redact.Truncate(s, 200)
}

// filesOf collects file paths from Claude (path / tool_input.file_path) and
// Codex (files[].path) payloads.
func filesOf(p map[string]any, input map[string]any) string {
	var out []string
	add := func(v any) {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	add(p["path"])
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		add(input[k])
	}
	if files, ok := p["files"].([]any); ok {
		for _, f := range files {
			if m, ok := f.(map[string]any); ok {
				add(m["path"])
			}
		}
	}
	return strings.Join(out, "\n")
}

// Upsert writes d into the index inside tx, replacing an existing document
// with the same (kind, ref_id). The FTS table is external-content, so the
// old row's text has to be removed explicitly before it is replaced.
func Upsert(ctx context.Context, tx *sql.Tx, d *Doc) error {
	var docID int64
	var oldTitle, oldBody, oldFiles string
	err := tx.QueryRowContext(ctx,
		`SELECT doc_id, title, body, files FROM search_docs WHERE kind = ? AND ref_id = ?`,
		d.Kind, d.RefID).Scan(&docID, &oldTitle, &oldBody, &oldFiles)
	switch {
	case err == nil:
		if oldTitle != d.Title || oldBody != d.Body || oldFiles != d.Files {
			// Text changed: its embeddings are stale. Their vectors are
			// removed later by the embedding sweep (embedding_orphans).
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO embedding_orphans (chunk_id, space_id)
				SELECT chunk_id, space_id FROM embedding_chunks WHERE doc_id = ?`, docID); err != nil {
				return fmt.Errorf("search: orphan embeddings: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_chunks WHERE doc_id = ?`, docID); err != nil {
				return fmt.Errorf("search: drop embeddings: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO search_fts (search_fts, rowid, title, body, files) VALUES ('delete', ?, ?, ?, ?)`,
			docID, oldTitle, oldBody, oldFiles); err != nil {
			return fmt.Errorf("search: unindex %d: %w", docID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE search_docs SET project_id = ?, session_id = ?, device_id = ?, agent_type = ?,
				occurred_at = ?, title = ?, body = ?, files = ? WHERE doc_id = ?`,
			d.ProjectID, nullable(d.SessionID), nullable(d.DeviceID), nullable(d.AgentType),
			model.FormatTime(d.OccurredAt), d.Title, d.Body, d.Files, docID); err != nil {
			return fmt.Errorf("search: update doc: %w", err)
		}
	case err == sql.ErrNoRows:
		res, err := tx.ExecContext(ctx, `
			INSERT INTO search_docs (kind, ref_id, project_id, session_id, device_id, agent_type, occurred_at, title, body, files)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.Kind, d.RefID, d.ProjectID, nullable(d.SessionID), nullable(d.DeviceID), nullable(d.AgentType),
			model.FormatTime(d.OccurredAt), d.Title, d.Body, d.Files)
		if err != nil {
			return fmt.Errorf("search: insert doc: %w", err)
		}
		if docID, err = res.LastInsertId(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("search: lookup doc: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO search_fts (rowid, title, body, files) VALUES (?, ?, ?, ?)`,
		docID, d.Title, d.Body, d.Files); err != nil {
		return fmt.Errorf("search: index %d: %w", docID, err)
	}
	return nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// IndexEvent indexes one event inside tx (no-op for events without a doc).
func IndexEvent(ctx context.Context, tx *sql.Tx, ev Event) error {
	d := DocFromEvent(ev)
	if d == nil {
		return nil
	}
	return Upsert(ctx, tx, d)
}

// backfillVersion marks, in schema_migrations, that events stored before
// the index existed have been indexed.
const backfillVersion = "004_search_backfill"

// Backfill indexes events that predate the search index. It runs once, at
// server start, and is idempotent.
func Backfill(ctx context.Context, db *sql.DB) (int, error) {
	var done int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, backfillVersion).Scan(&done); err != nil {
		return 0, err
	}
	if done > 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT e.event_id, e.event_type, e.project_id, COALESCE(e.session_id, ''), e.device_id,
			COALESCE(s.agent_type, ''), e.occurred_at, e.payload_json
		FROM events e LEFT JOIN sessions s ON s.session_id = e.session_id`)
	if err != nil {
		return 0, err
	}
	var evs []Event
	for rows.Next() {
		var ev Event
		var occurred, payload string
		if err := rows.Scan(&ev.EventID, &ev.EventType, &ev.ProjectID, &ev.SessionID, &ev.DeviceID,
			&ev.AgentType, &occurred, &payload); err != nil {
			rows.Close()
			return 0, err
		}
		ev.OccurredAt, _ = time.Parse(time.RFC3339Nano, occurred)
		ev.Payload = []byte(payload)
		evs = append(evs, ev)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, ev := range evs {
		if d := DocFromEvent(ev); d != nil {
			if err := Upsert(ctx, tx, d); err != nil {
				return 0, err
			}
			n++
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, backfillVersion); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
