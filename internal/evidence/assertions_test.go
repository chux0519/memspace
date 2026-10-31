package evidence

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

func ep(id, body string) Endpoint {
	body = Canonical(body)
	return Endpoint{EventID: id, Revision: SHA(body), Span: &Span{Start: 0, End: len(body), SHA: SHA(body)}}
}
func annotation(t *testing.T, ctx context.Context, d *sql.DB, id string, a Assertion) {
	t.Helper()
	b, _ := json.Marshal(a)
	var p map[string]any
	json.Unmarshal(b, &p)
	insert(t, ctx, d, id, "evidence.link_asserted", p)
}
func state(t *testing.T, d *sql.DB, id string) string {
	t.Helper()
	var s string
	if e := d.QueryRow(`SELECT status FROM evidence_inputs WHERE event_id=?`, id).Scan(&s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestExplicitSourcesPendingAndUnicodeVersionChecks(t *testing.T) {
	ctx, d := setup(t)
	a := Assertion{Relation: "derived_from", From: ep("fact", "阈值为6。"), To: ep("article", "保留的原文\r\n阈值为6。"), Witnesses: []Endpoint{ep("fact", "阈值为6。")}, Basis: "explicit_assertion"}
	annotation(t, ctx, d, "assert", a)
	if state(t, d, "assert") != "pending" {
		t.Fatal("must wait")
	}
	insert(t, ctx, d, "fact", "user.prompt", map[string]any{"prompt": "阈值为6。"})
	insert(t, ctx, d, "article", "source.document_captured", map[string]any{"url": "https://example.invalid/spec", "content": "保留的原文\r\n阈值为6。", "complete": false})
	if state(t, d, "assert") != "active" {
		t.Fatal(state(t, d, "assert"))
	}
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active' AND relation='derived_from'`) != 1 {
		t.Fatal("missing source")
	}
	a.To.Span.Start = 1
	annotation(t, ctx, d, "bad-span", a)
	if state(t, d, "bad-span") != "invalid" {
		t.Fatal("UTF8 boundary accepted")
	}
	a.To = ep("article", "different version")
	annotation(t, ctx, d, "bad-version", a)
	if state(t, d, "bad-version") != "invalid" {
		t.Fatal("version accepted")
	}
	a.To = ep("article", "保留的原文\r\n阈值为6。")
	a.Basis = "native_ref"
	annotation(t, ctx, d, "fake-native", a)
	if state(t, d, "fake-native") != "invalid" {
		t.Fatal("forged native")
	}
}
func TestCorrectionsScopeConflictsAndHistory(t *testing.T) {
	ctx, d := setup(t)
	for _, p := range []struct{ id, body string }{{"old", "staging pool=8; production=4"}, {"new", "correction staging pool=6"}, {"third", "correction staging pool=7"}} {
		insert(t, ctx, d, p.id, "user.prompt", map[string]any{"prompt": p.body})
	}
	a := Assertion{Relation: "supersedes", From: ep("new", "correction staging pool=6"), To: ep("old", "staging pool=8; production=4"), Witnesses: []Endpoint{ep("new", "correction staging pool=6")}, Basis: "explicit_assertion", Scope: Scope{Entity: "supermarket", Attribute: "pool", Environment: "staging"}}
	annotation(t, ctx, d, "a", a)
	if state(t, d, "a") != "active" {
		t.Fatal(state(t, d, "a"))
	}
	if count(t, d, `SELECT count(*) FROM events WHERE event_id='old' AND payload_json LIKE '%production=4%'`) != 1 {
		t.Fatal("old history changed")
	}
	missing := a
	missing.Scope.Environment = ""
	annotation(t, ctx, d, "missing", missing)
	if state(t, d, "missing") != "candidate" {
		t.Fatal("unknown scope became wildcard")
	}
	other := a
	other.Scope.Environment = "production"
	annotation(t, ctx, d, "other-env", other)
	if state(t, d, "other-env") != "active" {
		t.Fatal("independent scope blocked")
	}
	a.From = ep("third", "correction staging pool=7")
	a.Witnesses = []Endpoint{a.From}
	annotation(t, ctx, d, "competing", a)
	if state(t, d, "a") != "candidate" || state(t, d, "competing") != "candidate" {
		t.Fatal("chose latest conflicting correction")
	}
	// A cycle remains a candidate, including after a duplicate assertion replay.
	cycle := other
	cycle.From, cycle.To = other.To, other.From
	cycle.Witnesses = []Endpoint{cycle.From}
	annotation(t, ctx, d, "cycle", cycle)
	annotation(t, ctx, d, "cycle", cycle)
	if state(t, d, "cycle") != "candidate" || state(t, d, "other-env") != "candidate" {
		t.Fatal("cycle active")
	}
}
func TestRetractionTombstoneAndIndependentObservations(t *testing.T) {
	ctx, d := setup(t)
	insert(t, ctx, d, "s", "user.prompt", map[string]any{"prompt": "source"})
	insert(t, ctx, d, "f", "user.prompt", map[string]any{"prompt": "fact"})
	a := Assertion{Relation: "derived_from", From: ep("f", "fact"), To: ep("s", "source"), Witnesses: []Endpoint{ep("f", "fact")}, Basis: "explicit_assertion"}
	insert(t, ctx, d, "retract-before", "evidence.link_retracted", map[string]any{"assertion_event_id": "later"})
	annotation(t, ctx, d, "later", a)
	if state(t, d, "later") != "retracted" || count(t, d, `SELECT count(*) FROM evidence_links`) != 0 {
		t.Fatal("tombstone ignored")
	}
	annotation(t, ctx, d, "first", a)
	annotation(t, ctx, d, "second", a)
	insert(t, ctx, d, "retract-first", "evidence.link_retracted", map[string]any{"assertion_event_id": "first"})
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active'`) != 1 {
		t.Fatal("retraction erased independent observation")
	}
	model := a
	model.Basis = "model_inference"
	annotation(t, ctx, d, "model", model)
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active' AND basis='explicit_assertion'`) != 1 {
		t.Fatal("model overwrote explicit basis")
	}
	insert(t, ctx, d, "retract-second", "evidence.link_retracted", map[string]any{"assertion_event_id": "second"})
	if count(t, d, `SELECT count(*) FROM evidence_links WHERE status='active'`) != 0 {
		t.Fatal("retracted relation used")
	}
}
