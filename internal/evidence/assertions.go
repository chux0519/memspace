package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Span offsets refer to canonical UTF-8 bytes in the retained, redacted source.
// Hash covers the selected bytes; Endpoint.Revision pins the whole version.
type Span struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	SHA   string `json:"sha256"`
}
type Endpoint struct {
	ObjectID string `json:"object_id,omitempty"`
	EventID  string `json:"event_id,omitempty"`
	Revision string `json:"revision_sha"`
	Span     *Span  `json:"span,omitempty"`
}
type Scope struct {
	Entity      string            `json:"entity"`
	Attribute   string            `json:"attribute"`
	Environment string            `json:"environment"`
	Qualifiers  map[string]string `json:"qualifiers,omitempty"`
}
type Assertion struct {
	Relation  string     `json:"relation"`
	From      Endpoint   `json:"from"`
	To        Endpoint   `json:"to"`
	Scope     Scope      `json:"scope"`
	Witnesses []Endpoint `json:"witnesses"`
	// Only explicit_assertion and model_inference are accepted from clients.
	// Neither is promoted into native_ref or a claim of factual correctness.
	Basis string `json:"basis"`
}
type LinkScope struct {
	Key          string           `json:"key,omitempty"`
	Scope        Scope            `json:"scope"`
	FromSpan     *Span            `json:"from_span,omitempty"`
	ToSpan       *Span            `json:"to_span,omitempty"`
	WitnessSpans map[string]*Span `json:"witness_spans,omitempty"`
}

func ref(e Endpoint) string {
	if e.ObjectID != "" {
		return e.ObjectID
	}
	return e.EventID
}
func SpanText(body string, span *Span) (string, error) {
	if span == nil {
		return body, nil
	}
	if span.Start < 0 || span.End <= span.Start || span.End > len(body) || !utf8.ValidString(body[:span.Start]) || !utf8.ValidString(body[:span.End]) {
		return "", errors.New("span not on canonical UTF-8 boundaries")
	}
	s := body[span.Start:span.End]
	if SHA(s) != span.SHA {
		return "", errors.New("span hash mismatch after redaction/version change")
	}
	return s, nil
}
func indexAssertion(ctx context.Context, tx *sql.Tx, id, project, typ, payload string) error {
	var a Assertion
	var target struct {
		AssertionID string `json:"assertion_event_id"`
	}
	op := "assert"
	if typ == "evidence.link_retracted" {
		op = "retract"
		json.Unmarshal([]byte(payload), &target)
	} else {
		json.Unmarshal([]byte(payload), &a)
	}
	witnesses := []string{}
	for _, w := range a.Witnesses {
		witnesses = append(witnesses, ref(w))
	}
	b, _ := json.Marshal(witnesses)
	_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO evidence_inputs(event_id,project_id,operation,target_event_id,from_ref,to_ref,witness_refs,status) VALUES (?,?,?,?,?,?,?,'pending')`, id, project, op, target.AssertionID, ref(a.From), ref(a.To), string(b))
	if e != nil {
		return e
	}
	if _, e = journal(ctx, tx, project, "device", id, "relation_input", json.RawMessage(payload)); e != nil {
		return e
	}
	if op == "retract" {
		return applyRetraction(ctx, tx, project, id, target.AssertionID)
	}
	return resolveAssertion(ctx, tx, project, id)
}
func inputState(ctx context.Context, tx *sql.Tx, project, id, status, diagnostic, link string) error {
	if _, e := journal(ctx, tx, project, "server_rule", id, "relation_status", map[string]string{"status": status, "diagnostic": diagnostic, "link_id": link}); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, `UPDATE evidence_inputs SET status=?,diagnostic=?,link_id=? WHERE event_id=? AND project_id=?`, status, diagnostic, link, id, project)
	return e
}
func resolveEndpoint(ctx context.Context, tx *sql.Tx, owner, project string, ep Endpoint) (Object, string, error) {
	var o Object
	var e error
	if (ep.ObjectID == "") == (ep.EventID == "") {
		return o, "", errors.New("exactly one endpoint reference required")
	}
	if ep.ObjectID != "" {
		o, e = scanObject(tx.QueryRowContext(ctx, `SELECT `+objectCols+` FROM evidence_objects WHERE project_id=? AND object_id=?`, project, ep.ObjectID))
	} else if ep.EventID != "" {
		o, e = scanObject(tx.QueryRowContext(ctx, `SELECT `+objectCols+` FROM evidence_objects WHERE project_id=? AND ref_id=? AND source_kind='event'`, project, ep.EventID))
	} else {
		return o, "", errors.New("missing endpoint reference")
	}
	if e != nil {
		return o, "", e
	}
	if ep.Revision == "" || ep.Revision != o.Revision {
		return o, "", errors.New("endpoint revision missing or mismatched")
	}
	body, e := textFrom(ctx, tx, owner, o)
	if e != nil {
		return o, "", e
	}
	body, e = SpanText(body, ep.Span)
	return o, body, e
}
func resolveAssertion(ctx context.Context, tx *sql.Tx, project, id string) error {
	var payload, owner string
	if e := tx.QueryRowContext(ctx, `SELECT e.payload_json,p.user_id FROM events e JOIN projects p ON p.project_id=e.project_id WHERE e.event_id=? AND e.project_id=?`, id, project).Scan(&payload, &owner); e != nil {
		return e
	}
	var a Assertion
	if e := json.Unmarshal([]byte(payload), &a); e != nil {
		return inputState(ctx, tx, project, id, "invalid", "invalid assertion JSON", "")
	}
	var retracted bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_inputs WHERE project_id=? AND operation='retract' AND target_event_id=? AND status='active')`, project, id).Scan(&retracted); e != nil {
		return e
	}
	if retracted {
		var link string
		if e := tx.QueryRowContext(ctx, `SELECT link_id FROM evidence_inputs WHERE event_id=? AND project_id=?`, id, project).Scan(&link); e != nil {
			return e
		}
		return inputState(ctx, tx, project, id, "retracted", "explicit retraction", link)
	}
	if a.Relation != "derived_from" && a.Relation != "supersedes" {
		return inputState(ctx, tx, project, id, "invalid", "unsupported explicit relation", "")
	}
	if a.Basis != "explicit_assertion" && a.Basis != "model_inference" {
		return inputState(ctx, tx, project, id, "invalid", "client cannot assert native/generator basis", "")
	}
	if len(a.Witnesses) == 0 || len(a.Witnesses) > 16 {
		return inputState(ctx, tx, project, id, "candidate", "1..16 retained witnesses required", "")
	}
	if a.Relation == "supersedes" && (strings.TrimSpace(a.Scope.Entity) == "" || strings.TrimSpace(a.Scope.Attribute) == "" || strings.TrimSpace(a.Scope.Environment) == "" || a.From.Span == nil || a.To.Span == nil) {
		return inputState(ctx, tx, project, id, "candidate", "correction requires entity/attribute/environment and exact endpoint spans", "")
	}
	endpoints := append([]Endpoint{a.From, a.To}, a.Witnesses...)
	objects := []Object{}
	for _, ep := range endpoints {
		o, _, e := resolveEndpoint(ctx, tx, owner, project, ep)
		if errors.Is(e, sql.ErrNoRows) {
			return inputState(ctx, tx, project, id, "pending", "endpoint not yet available in this project", "")
		}
		if e != nil {
			return inputState(ctx, tx, project, id, "invalid", e.Error(), "")
		}
		objects = append(objects, o)
	}
	if objects[0].ID == objects[1].ID {
		return inputState(ctx, tx, project, id, "invalid", "self link", "")
	}
	if objects[1].Availability == "locator_only" {
		return inputState(ctx, tx, project, id, "candidate", "only a source locator is retained", "")
	}
	scopeBytes, _ := json.Marshal(a.Scope)
	scope := LinkScope{Key: SHA(string(scopeBytes)), Scope: a.Scope, FromSpan: a.From.Span, ToSpan: a.To.Span, WitnessSpans: map[string]*Span{}}
	witnesses := []string{}
	for i, o := range objects[2:] {
		witnesses = append(witnesses, o.ID)
		scope.WitnessSpans[o.ID] = a.Witnesses[i].Span
	}
	scopeJSON, _ := json.Marshal(scope)
	status := "active"
	if a.Basis == "model_inference" {
		status = "candidate"
	}
	l, e := storeLinkBy(ctx, tx, project, a.Relation, objects[0].ID, objects[1].ID, a.Basis, status, string(scopeJSON), witnesses, "device", id)
	if e != nil {
		return e
	}
	if e = inputState(ctx, tx, project, id, status, "structure/version validated; assertion is not truth verification", l.ID); e != nil {
		return e
	}
	// Preserve an independent active observation if a model candidate arrives.
	if e = recomputeLink(ctx, tx, project, l.ID); e != nil {
		return e
	}
	if a.Relation == "supersedes" {
		return detectConflicts(ctx, tx, project, scope.Key)
	}
	return nil
}
func reconcileInputs(ctx context.Context, tx *sql.Tx, project, changed string) error {
	rows, e := tx.QueryContext(ctx, `SELECT event_id FROM evidence_inputs WHERE project_id=? AND operation='assert' AND status='pending' AND (from_ref=? OR to_ref=? OR witness_refs LIKE ?) ORDER BY event_id LIMIT 256`, project, changed, changed, "%"+changed+"%")
	if e != nil {
		return e
	}
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = resolveAssertion(ctx, tx, project, id); e != nil {
			return e
		}
	}
	return nil
}
func applyRetraction(ctx context.Context, tx *sql.Tx, project, id, target string) error {
	if target == "" || target == id {
		return inputState(ctx, tx, project, id, "invalid", "retraction requires another assertion event ID", "")
	}
	var exists bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE event_id=? AND project_id=? AND event_type='evidence.link_asserted')`, target, project).Scan(&exists); e != nil {
		return e
	}
	// Keep a scoped tombstone even before the asserted event arrives.
	if e := inputState(ctx, tx, project, id, "active", "explicit retraction tombstone", ""); e != nil {
		return e
	}
	if !exists {
		return nil
	}
	var link string
	e := tx.QueryRowContext(ctx, `SELECT link_id FROM evidence_inputs WHERE event_id=? AND project_id=?`, target, project).Scan(&link)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = inputState(ctx, tx, project, target, "retracted", "explicit retraction", link); e != nil {
		return e
	}
	if link != "" {
		return recomputeLink(ctx, tx, project, link)
	}
	return nil
}
func recomputeLink(ctx context.Context, tx *sql.Tx, project, link string) error {
	var active, candidate, pending int
	if e := tx.QueryRowContext(ctx, `SELECT coalesce(sum(status='active'),0),coalesce(sum(status='candidate'),0),coalesce(sum(status='pending'),0) FROM evidence_inputs WHERE project_id=? AND link_id=?`, project, link).Scan(&active, &candidate, &pending); e != nil {
		return e
	}
	status := "retracted"
	if pending > 0 {
		status = "pending"
	}
	if candidate > 0 {
		status = "candidate"
	}
	if active > 0 {
		status = "active"
	}
	if _, e := journal(ctx, tx, project, "server_rule", "", "link_status", map[string]string{"link_id": link, "status": status}); e != nil {
		return e
	}
	basis := "model_inference"
	if active > 0 {
		basis = "explicit_assertion"
	} else {
		var explicit bool
		if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM evidence_inputs i JOIN events e ON e.event_id=i.event_id WHERE i.project_id=? AND i.link_id=? AND i.status='candidate' AND json_extract(e.payload_json,'$.basis')='explicit_assertion')`, project, link).Scan(&explicit); e != nil {
			return e
		}
		if explicit {
			basis = "explicit_assertion"
		}
	}
	_, e := tx.ExecContext(ctx, `UPDATE evidence_links SET status=?,basis=? WHERE project_id=? AND link_id=?`, status, basis, project, link)
	return e
}
func detectConflicts(ctx context.Context, tx *sql.Tx, project, key string) error {
	rows, e := tx.QueryContext(ctx, `SELECT `+linkCols+` FROM evidence_links WHERE project_id=? AND relation='supersedes' AND status IN ('active','candidate') AND basis='explicit_assertion' AND json_extract(scope_json,'$.key')=?`, project, key)
	if e != nil {
		return e
	}
	var links []Link
	for rows.Next() {
		l, e := scanLink(rows)
		if e != nil {
			rows.Close()
			return e
		}
		links = append(links, l)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	// Conservative structural conflict detection. Never break cycles or choose
	// conflicting revisions by timestamp. Every historical endpoint stays stored.
	graph := map[string][]string{}
	incoming := map[string]map[string]bool{}
	conflict := false
	for _, l := range links {
		graph[l.From] = append(graph[l.From], l.To)
		if incoming[l.To] == nil {
			incoming[l.To] = map[string]bool{}
		}
		incoming[l.To][l.From] = true
		if len(incoming[l.To]) > 1 {
			conflict = true
		}
	}
	seen := map[string]int{}
	var visit func(string) bool
	visit = func(id string) bool {
		if seen[id] == 1 {
			return true
		}
		if seen[id] == 2 {
			return false
		}
		seen[id] = 1
		for _, n := range graph[id] {
			if visit(n) {
				return true
			}
		}
		seen[id] = 2
		return false
	}
	for id := range graph {
		if visit(id) {
			conflict = true
		}
	}
	if !conflict {
		return nil
	}
	for _, l := range links {
		if _, e = journal(ctx, tx, project, "server_rule", "", "link_conflict", map[string]string{"link_id": l.ID, "status": "candidate", "reason": "cycle or competing correction under same scope"}); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE evidence_links SET status='candidate' WHERE link_id=?`, l.ID); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE evidence_inputs SET status='candidate',diagnostic='cycle or competing correction under same scope' WHERE project_id=? AND link_id=? AND status='active'`, project, l.ID); e != nil {
			return e
		}
	}
	return nil
}

func (s Span) String() string { return fmt.Sprintf("[%d,%d)@%s", s.Start, s.End, s.SHA) }
