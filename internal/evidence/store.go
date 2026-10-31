package evidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
)

const ruleVersion = "evidence-v1"

func Canonical(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
func SHA(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func stable(prefix string, parts ...string) string {
	b, _ := json.Marshal(parts)
	return prefix + "_" + SHA(string(b))[:32]
}

type Object struct {
	ID           string `json:"object_id"`
	Project      string `json:"project_id"`
	Kind         string `json:"kind"`
	Ref          string `json:"ref_id"`
	Revision     string `json:"revision_sha"`
	BodySHA      string `json:"body_sha"`
	SourceKind   string `json:"source_kind"`
	SourceID     string `json:"source_id"`
	Field        string `json:"field_path"`
	Availability string `json:"availability"`
	Session      string `json:"session_id"`
	Device       string `json:"device_id"`
	Role         string `json:"role"`
	Turn         string `json:"turn_key"`
	Prompt       string `json:"prompt_key"`
	Message      string `json:"native_message_id"`
	Parent       string `json:"parent_message_id"`
	Batch        string `json:"batch_key"`
	Ordinal      *int   `json:"source_ordinal,omitempty"`
	Count        *int   `json:"source_count,omitempty"`
	At           string `json:"occurred_at"`
	Current      bool   `json:"is_current"`
}
type Link struct {
	ID        string `json:"link_id"`
	Project   string `json:"project_id"`
	Relation  string `json:"relation"`
	From      string `json:"from_object_id"`
	To        string `json:"to_object_id"`
	Basis     string `json:"basis"`
	Status    string `json:"status"`
	Rule      string `json:"rule_version"`
	Scope     string `json:"scope_json"`
	Witnesses string `json:"witness_refs"`
	Record    string `json:"assertion_record_id"`
}
type SourceRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type derivedPayload struct {
	Title   string      `json:"title"`
	Content string      `json:"content"`
	Sources []SourceRef `json:"source_refs"`
}

func derivedRevision(title, body string, sources []SourceRef) string {
	b, _ := json.Marshal(derivedPayload{title, Canonical(body), sources})
	return SHA(string(b))
}

type Derived struct {
	Kind, Ref, Project, Session, Title, Content string
	Sources                                     []SourceRef
	At                                          time.Time
}

const objectCols = `object_id,project_id,kind,ref_id,revision_sha,body_sha,source_kind,source_id,field_path,availability,session_id,device_id,role,turn_key,prompt_key,native_message_id,parent_message_id,batch_key,source_ordinal,source_count,occurred_at,is_current`

func scanObject(s interface{ Scan(...any) error }) (Object, error) {
	var o Object
	err := s.Scan(&o.ID, &o.Project, &o.Kind, &o.Ref, &o.Revision, &o.BodySHA, &o.SourceKind, &o.SourceID, &o.Field, &o.Availability, &o.Session, &o.Device, &o.Role, &o.Turn, &o.Prompt, &o.Message, &o.Parent, &o.Batch, &o.Ordinal, &o.Count, &o.At, &o.Current)
	return o, err
}
func storeObject(ctx context.Context, tx *sql.Tx, o Object) error {
	_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO evidence_objects (`+objectCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.Project, o.Kind, o.Ref, o.Revision, o.BodySHA, o.SourceKind, o.SourceID, o.Field, o.Availability, o.Session, o.Device, o.Role, o.Turn, o.Prompt, o.Message, o.Parent, o.Batch, o.Ordinal, o.Count, o.At, true)
	return e
}
func journal(ctx context.Context, tx *sql.Tx, project, actor, eventID, kind string, payload any) (string, error) {
	b, e := json.Marshal(payload)
	if e != nil {
		return "", e
	}
	hash := SHA(string(b))
	id := stable("evr", project, actor, eventID, kind, hash)
	_, e = tx.ExecContext(ctx, `INSERT OR IGNORE INTO evidence_records(record_id,project_id,actor_kind,source_event_id,kind,payload_json,payload_sha,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, id, project, actor, eventID, kind, string(b), hash, time.Now().UTC().Format(time.RFC3339Nano))
	return id, e
}
func storeLink(ctx context.Context, tx *sql.Tx, project, relation, from, to, basis, status, scope string, witnesses []string) (Link, error) {
	if from == to {
		return Link{}, errors.New("self evidence link")
	}
	var n int
	if e := tx.QueryRowContext(ctx, `SELECT count(*) FROM evidence_objects WHERE project_id=? AND object_id IN (?,?)`, project, from, to).Scan(&n); e != nil {
		return Link{}, e
	}
	if n != 2 {
		return Link{}, errors.New("evidence endpoints outside project")
	}
	w, _ := json.Marshal(witnesses)
	id := stable("evl", project, relation, from, to, scope)
	l := Link{ID: id, Project: project, Relation: relation, From: from, To: to, Basis: basis, Status: status, Rule: ruleVersion, Scope: scope, Witnesses: string(w)}
	record, e := journal(ctx, tx, project, "server_rule", "", "link_asserted", l)
	if e != nil {
		return l, e
	}
	l.Record = record
	_, e = tx.ExecContext(ctx, `INSERT INTO evidence_links VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(link_id) DO UPDATE SET basis=excluded.basis,status=excluded.status,rule_version=excluded.rule_version,witness_refs=excluded.witness_refs,assertion_record_id=excluded.assertion_record_id`, l.ID, l.Project, l.Relation, l.From, l.To, l.Basis, l.Status, l.Rule, l.Scope, l.Witnesses, l.Record)
	return l, e
}

// IndexEvent observes a committed-to-this-transaction raw source. Call after
// ingestion redaction, in the same transaction. Missing native refs stay absent
// until their endpoint arrives; nearest timestamps never fabricate a link.
func IndexEvent(ctx context.Context, tx *sql.Tx, id string) error {
	if !FromContext(ctx).Enabled() {
		return nil
	}
	var project, session, device, typ, payload, at string
	err := tx.QueryRowContext(ctx, `SELECT project_id,coalesce(session_id,''),device_id,event_type,payload_json,occurred_at FROM events WHERE event_id=?`, id).Scan(&project, &session, &device, &typ, &payload, &at)
	if err != nil {
		return err
	}
	var p map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &p) != nil {
		return nil
	}
	str := func(key string) string { var s string; json.Unmarshal(p[key], &s); return s }
	role, field := "", ""
	switch typ {
	case "user.prompt":
		role, field = "prompt", "prompt"
	case "agent.turn_completed":
		role, field = "reply", "last_assistant_message"
	case "source.message":
		role, field = str("role"), "content"
	case "source.document_captured":
		role, field = "document", "content"
	default:
		if strings.HasPrefix(typ, "evidence.") {
			return nil
		}
		role, field = "source", "__payload__"
	}
	body := Canonical(str(field))
	if field == "__payload__" {
		var v any
		if json.Unmarshal([]byte(payload), &v) != nil {
			return nil
		}
		b, _ := json.Marshal(v)
		body = Canonical(string(b))
	}
	rev := SHA(body)
	o := Object{ID: stable("evo", project, "event", id, field, rev), Project: project, Kind: "event", Ref: id, Revision: rev, BodySHA: rev, SourceKind: "event", SourceID: id, Field: field, Availability: "full", Session: session, Device: device, Role: role, Turn: str("turn_id"), Prompt: str("prompt_id"), At: at}
	var origin struct {
		Kind     string `json:"source_kind"`
		Turn     string `json:"native_turn_id"`
		Message  string `json:"native_message_id"`
		Parent   string `json:"parent_message_id"`
		Batch    string `json:"batch_key"`
		Ordinal  *int   `json:"source_ordinal"`
		Count    *int   `json:"source_count"`
		Complete bool   `json:"source_complete"`
	}
	json.Unmarshal(p["origin"], &origin)
	o.Message = origin.Message
	o.Parent = origin.Parent
	if o.Turn == "" {
		o.Turn = origin.Turn
	}
	if origin.Kind == "ordered_import" && origin.Complete && origin.Count != nil {
		o.Batch = origin.Batch
		o.Ordinal = origin.Ordinal
		o.Count = origin.Count
	}
	if typ == "source.document_captured" {
		o.Kind = "document_snapshot"
		o.Availability = "excerpt"
		var full bool
		json.Unmarshal(p["complete"], &full)
		if full {
			o.Availability = "full"
		}
		if body == "" {
			o.Availability = "locator_only"
		}
	}
	if err = storeObject(ctx, tx, o); err != nil {
		return err
	}
	if session == "" || (role != "prompt" && role != "reply" && o.Batch == "") {
		return nil
	}
	return reconcileObject(ctx, tx, o)
}
func reconcileObject(ctx context.Context, tx *sql.Tx, changed Object) error {
	project, session, device := changed.Project, changed.Session, changed.Device
	var clauses []string
	args := []any{project, session, device}
	for _, pair := range [][2]string{{"turn_key", changed.Turn}, {"prompt_key", changed.Prompt}} {
		if pair[1] != "" {
			clauses = append(clauses, pair[0]+"=?")
			args = append(args, pair[1])
		}
	}
	for _, id := range []string{changed.Message, changed.Parent} {
		if id != "" {
			clauses = append(clauses, "native_message_id=? OR parent_message_id=?")
			args = append(args, id, id)
		}
	}
	// In ordinary ordered imports, only the final ordinal loads the batch.
	// Earlier out-of-order arrivals retry once a final-ordinal marker exists.
	if changed.Batch != "" && changed.Count != nil {
		var last bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_objects WHERE project_id=? AND session_id=? AND device_id=? AND batch_key=? AND source_ordinal=?)`, project, session, device, changed.Batch, *changed.Count-1).Scan(&last); err != nil {
			return err
		}
		if last {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM evidence_objects WHERE project_id=? AND session_id=? AND device_id=? AND batch_key=?`, project, session, device, changed.Batch).Scan(&n); err != nil {
				return err
			}
			if n == *changed.Count {
				clauses = append(clauses, "batch_key=?")
				args = append(args, changed.Batch)
			}
		}
	}
	if len(clauses) == 0 {
		return nil
	}
	rows, e := tx.QueryContext(ctx, `SELECT `+objectCols+` FROM evidence_objects WHERE project_id=? AND session_id=? AND device_id=? AND source_kind='event' AND (`+strings.Join(clauses, " OR ")+`) ORDER BY source_ordinal,object_id`, args...)
	if e != nil {
		return e
	}
	var objects []Object
	for rows.Next() {
		o, err := scanObject(rows)
		if err != nil {
			rows.Close()
			return err
		}
		objects = append(objects, o)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	prompts := map[string][]Object{}
	for _, o := range objects {
		if o.Role != "prompt" {
			continue
		}
		if o.Turn != "" {
			prompts["t:"+o.Turn] = append(prompts["t:"+o.Turn], o)
		}
		if o.Prompt != "" {
			prompts["p:"+o.Prompt] = append(prompts["p:"+o.Prompt], o)
		}
		if o.Message != "" {
			prompts["m:"+o.Message] = append(prompts["m:"+o.Message], o)
		}
	}
	for _, o := range objects {
		if o.Role != "reply" {
			continue
		}
		key := ""
		if o.Parent != "" {
			key = "m:" + o.Parent
		} else if o.Turn != "" {
			key = "t:" + o.Turn
		} else if o.Prompt != "" {
			key = "p:" + o.Prompt
		}
		if key == "" {
			continue
		}
		matches := prompts[key]
		if len(matches) > 1 {
			if _, e = tx.ExecContext(ctx, `UPDATE evidence_links SET status='candidate' WHERE project_id=? AND relation='reply_to' AND from_object_id=? AND basis='native_ref'`, project, o.ID); e != nil {
				return e
			}
			continue
		}
		if len(matches) == 1 {
			if _, e = storeLink(ctx, tx, project, "reply_to", o.ID, matches[0].ID, "native_ref", "active", "{}", []string{o.ID, matches[0].ID}); e != nil {
				return e
			}
		}
	}
	batches := map[string][]Object{}
	for _, o := range objects {
		if o.Batch != "" {
			batches[o.Batch] = append(batches[o.Batch], o)
		}
	}
	for _, group := range batches {
		if len(group) == 0 || group[0].Count == nil || *group[0].Count != len(group) {
			continue
		}
		complete := true
		for i, o := range group {
			if o.Ordinal == nil || *o.Ordinal != i || o.Count == nil || *o.Count != len(group) {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		var parent *Object
		for i := range group {
			o := group[i]
			if o.Role == "prompt" {
				parent = &group[i]
			}
			if o.Role == "reply" && parent != nil && o.Turn == "" && o.Prompt == "" && o.Parent == "" {
				if _, e = storeLink(ctx, tx, project, "reply_to", o.ID, parent.ID, "ordered_source", "active", "{}", []string{o.ID, parent.ID}); e != nil {
					return e
				}
			}
		}
	}
	return nil
}

// RecordDerived freezes the actual generated version and its declared sources.
// It records provenance, not semantic proof that every source supports a fact.
func RecordDerived(ctx context.Context, tx *sql.Tx, d Derived) error {
	if !FromContext(ctx).Enabled() {
		return nil
	}
	payload := derivedPayload{d.Title, Canonical(d.Content), d.Sources}
	raw, _ := json.Marshal(payload)
	revision := SHA(string(raw))
	snapshot := struct {
		derivedPayload
		Kind    string `json:"kind"`
		Ref     string `json:"ref_id"`
		Session string `json:"session_id"`
		At      string `json:"occurred_at"`
	}{payload, d.Kind, d.Ref, d.Session, d.At.UTC().Format(time.RFC3339Nano)}
	record, e := journal(ctx, tx, d.Project, "server_rule", "", d.Kind+".revision_created", snapshot)
	if e != nil {
		return e
	}
	o := Object{ID: stable("evo", d.Project, d.Kind, d.Ref, revision), Project: d.Project, Kind: d.Kind, Ref: d.Ref, Revision: revision, BodySHA: SHA(payload.Content), SourceKind: "record", SourceID: record, Field: "content", Availability: "full", Session: d.Session, At: d.At.UTC().Format(time.RFC3339Nano)}
	var previous string
	err := tx.QueryRowContext(ctx, `SELECT object_id FROM evidence_objects WHERE project_id=? AND kind=? AND ref_id=? AND is_current=1`, d.Project, d.Kind, d.Ref).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if previous != o.ID {
		selected, _ := json.Marshal(map[string]string{"kind": d.Kind, "ref_id": d.Ref, "object_id": o.ID, "previous_object_id": previous})
		if _, e = tx.ExecContext(ctx, `INSERT INTO evidence_records(record_id,project_id,actor_kind,source_event_id,kind,payload_json,payload_sha,recorded_at) VALUES (?,?,'server_rule','','current_version_selected',?,?,?)`, "evr_"+uuid.NewString(), d.Project, string(selected), SHA(string(selected)), time.Now().UTC().Format(time.RFC3339Nano)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE evidence_objects SET is_current=0 WHERE project_id=? AND kind=? AND ref_id=?`, d.Project, d.Kind, d.Ref); e != nil {
			return e
		}
	}
	if e = storeObject(ctx, tx, o); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE evidence_objects SET is_current=1 WHERE object_id=?`, o.ID); e != nil {
		return e
	}
	for _, ref := range d.Sources {
		if ref.Kind != "event" {
			continue
		}
		var source Object
		source, e = scanObject(tx.QueryRowContext(ctx, `SELECT `+objectCols+` FROM evidence_objects WHERE project_id=? AND ref_id=? AND source_kind='event' LIMIT 1`, d.Project, ref.ID))
		if errors.Is(e, sql.ErrNoRows) {
			var exists int
			if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_id=? AND project_id=?`, ref.ID, d.Project).Scan(&exists); e != nil {
				return e
			}
			if exists == 0 {
				continue
			}
			if e = IndexEvent(ctx, tx, ref.ID); e != nil {
				return e
			}
			source, e = scanObject(tx.QueryRowContext(ctx, `SELECT `+objectCols+` FROM evidence_objects WHERE project_id=? AND ref_id=? AND source_kind='event' LIMIT 1`, d.Project, ref.ID))
		}
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return e
		}
		if _, e = storeLink(ctx, tx, d.Project, "derived_from", o.ID, source.ID, "generator_sources", "active", "{}", []string{source.ID}); e != nil {
			return e
		}
	}
	return nil
}

// Text resolves the exact stored source version and validates the content hash.
func Text(ctx context.Context, db *sql.DB, owner string, o Object) (string, error) {
	var allowed int
	if e := db.QueryRowContext(ctx, `SELECT count(*) FROM projects WHERE project_id=? AND user_id=?`, o.Project, owner).Scan(&allowed); e != nil {
		return "", e
	}
	if allowed != 1 {
		return "", errors.New("source outside owner scope")
	}
	var payload string
	var e error
	switch o.SourceKind {
	case "event":
		e = db.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE event_id=? AND project_id=?`, o.SourceID, o.Project).Scan(&payload)
	case "record":
		e = db.QueryRowContext(ctx, `SELECT payload_json FROM evidence_records WHERE record_id=? AND project_id=?`, o.SourceID, o.Project).Scan(&payload)
	default:
		return "", errors.New("unknown source kind")
	}
	if e != nil {
		return "", e
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal([]byte(payload), &fields); e != nil {
		return "", e
	}
	var body string
	if o.Field == "__payload__" {
		var v any
		if e = json.Unmarshal([]byte(payload), &v); e != nil {
			return "", e
		}
		b, _ := json.Marshal(v)
		body = string(b)
	} else if e = json.Unmarshal(fields[o.Field], &body); e != nil {
		return "", e
	}
	body = Canonical(body)
	if SHA(body) != o.BodySHA {
		return "", fmt.Errorf("evidence source version mismatch")
	}
	return body, nil
}
