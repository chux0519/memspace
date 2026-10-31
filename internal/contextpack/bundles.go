package contextpack

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
)

// EvidenceGroup describes only the sources actually delivered. Complete means
// all required relationship endpoints were delivered in full, not that they
// prove an answer or exhaust the project's evidence.
type EvidenceGroup struct {
	ID       string           `json:"group_id"`
	Members  []EvidenceMember `json:"members"`
	Links    []evidence.Link  `json:"links"`
	Complete bool             `json:"complete"`
}
type EvidenceMember struct {
	ObjectID     string `json:"object_id"`
	Ref          string `json:"ref"`
	Revision     string `json:"revision_sha"`
	Availability string `json:"availability"`
	Partial      bool   `json:"partial"`
}

// relationSections forms direct, source-backed bundles before legacy role
// suppression. Unrelated hits keep legacy ordering and qualification. Edges do
// not alter scores, and coarse generator provenance never forces all sources
// into a bundle. Query bounds and the visible fragment budget remain separate.
func relationSections(ctx context.Context, db *sql.DB, owner, project, current string, hits []model.SearchResult, limit int, inject float64, observer Observer) ([]section, map[string]bool, error) {
	p := evidence.FromContext(ctx)
	if !p.Enabled() || len(hits) == 0 {
		return nil, nil, nil
	}
	seeds := make([]evidence.Seed, 0, len(hits))
	for _, h := range hits {
		seeds = append(seeds, evidence.Seed{Kind: h.Kind, Ref: h.RefID})
	}
	objects, err := evidence.CurrentObjects(ctx, db, owner, project, seeds)
	if err != nil {
		return nil, nil, err
	}
	ids := []string{}
	for _, h := range hits {
		if o, ok := objects[evidence.Seed{Kind: h.Kind, Ref: h.RefID}]; ok {
			ids = append(ids, o.ID)
		}
	}
	links, err := evidence.Related(ctx, db, owner, project, ids, p.Limit())
	if err != nil {
		return nil, nil, err
	}
	observe(observer, TraceEvent{Stage: "evidence", Note: fmt.Sprintf("%s: %d source versions, %d direct active links", p.Mode, len(ids), len(links))})
	if p.Mode != "on" {
		return nil, nil, nil
	}
	// A group is anchored by an independently admitted hit. Native or complete
	// source-order reply relationships are actionable; similarity is not one.
	byID := map[string]evidence.Object{}
	for _, o := range objects {
		byID[o.ID] = o
	}
	valid := []evidence.Link{}
	for _, l := range links {
		if l.Relation != "reply_to" || (l.Basis != "native_ref" && l.Basis != "ordered_source") {
			continue
		}
		ok := true
		for _, id := range []string{l.From, l.To} {
			if _, exists := byID[id]; !exists {
				o, e := evidence.GetObject(ctx, db, owner, project, id)
				if e != nil {
					ok = false
					break
				}
				byID[id] = o
			}
			o := byID[id]
			if o.Session == current && current != "" {
				ok = false
				break
			}
		}
		if ok && byID[l.From].Role == "reply" && byID[l.To].Role == "prompt" {
			valid = append(valid, l)
		}
	}
	// Union endpoints sharing a prompt so one physical source is never counted
	// twice. The traversal remains limited to direct edges of admitted seeds.
	parent := map[string]string{}
	var root func(string) string
	root = func(id string) string {
		v, ok := parent[id]
		if !ok {
			parent[id] = id
			return id
		}
		if v != id {
			parent[id] = root(v)
		}
		return parent[id]
	}
	for _, l := range valid {
		a, b := root(l.From), root(l.To)
		parent[a] = b
	}
	groups := map[string][]string{}
	for id := range parent {
		r := root(id)
		groups[r] = append(groups[r], id)
	}
	claimed := map[string]bool{}
	out := []section{}
	seen := map[string]bool{}
	for _, h := range hits {
		o, ok := objects[evidence.Seed{Kind: h.Kind, Ref: h.RefID}]
		if !ok {
			continue
		}
		if _, ok = parent[o.ID]; !ok {
			continue
		}
		r := root(o.ID)
		if seen[r] {
			continue
		}
		seen[r] = true
		members := groups[r]
		if len(members) > limit {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			a, b := byID[members[i]], byID[members[j]]
			if a.Role != b.Role {
				return a.Role == "prompt"
			}
			if a.At != b.At {
				return a.At < b.At
			}
			return a.ID < b.ID
		})
		group := &EvidenceGroup{ID: "bundle_" + evidence.SHA(r)[:20], Complete: true}
		var batch []section
		for _, id := range members {
			obj := byID[id]
			body, e := evidence.Text(ctx, db, owner, obj)
			if e != nil || body == "" {
				batch = nil
				break
			}
			body = redact.String(body)
			partial := utf8.RuneCountInString(body) > completeHistoryChars || obj.Availability != "full"
			body = oneLine(evidenceWindow(body, ""))
			group.Complete = group.Complete && !partial
			group.Members = append(group.Members, EvidenceMember{ObjectID: id, Ref: obj.Ref, Revision: obj.Revision, Availability: obj.Availability, Partial: partial})
			at, _ := time.Parse(time.RFC3339Nano, obj.At)
			label := obj.Role
			if partial {
				label += " excerpt"
			}
			batch = append(batch, section{item: Item{Key: "evidence:" + id, Kind: "history", Ref: obj.Ref, ProjectID: project, Device: h.DeviceName, Agent: h.AgentType, At: at}, text: fmt.Sprintf("- [%s; %s] %s (ref %s, version %s)\n", label, group.ID, body, obj.Ref, obj.Revision[:12]), group: group})
		}
		if len(batch) != len(members) {
			observe(observer, TraceEvent{Stage: "evidence_bundle", Decision: "missing_or_stale"})
			continue
		}
		for _, l := range valid {
			if root(l.From) == r {
				group.Links = append(group.Links, l)
			}
		}
		batch[0].text = fmt.Sprintf("Evidence group %s: reply_to (%s); completeness=%t. Correspondence is not proof of correctness.\n", group.ID, group.Links[0].Basis, group.Complete) + batch[0].text
		out = append(out, batch...)
		for _, id := range members {
			claimed[byID[id].Ref] = true
		}
	}
	return out, claimed, nil
}

// Preserve the anchor's original rank and session diversity. A relationship
// does not grant a rank bonus. Each bundle is one scheduling unit but every
// delivered source consumes one physical item slot.
func mergeEvidenceSections(in []section, hits []model.SearchResult, limit int) []section {
	type unit struct {
		parts   []section
		rank    int
		session string
	}
	ranks := map[string]int{}
	sessions := map[string]string{}
	for i, h := range hits {
		if _, ok := ranks[h.RefID]; !ok {
			ranks[h.RefID] = i
			sessions[h.RefID] = h.SessionID
		}
	}
	var units []unit
	for i := 0; i < len(in); {
		end := i + 1
		s := in[i]
		rank, ok := ranks[s.item.Ref]
		if !ok {
			rank = len(hits)
		}
		session := sessions[s.item.Ref]
		if s.group != nil {
			for end < len(in) && in[end].group == s.group {
				if r, ok := ranks[in[end].item.Ref]; ok && r < rank {
					rank = r
					session = sessions[in[end].item.Ref]
				}
				end++
			}
		}
		units = append(units, unit{in[i:end], rank, session})
		i = end
	}
	sort.SliceStable(units, func(i, j int) bool { return units[i].rank < units[j].rank })
	var buckets [][]unit
	bySession := map[string]int{}
	for _, u := range units {
		idx, ok := bySession[u.session]
		if !ok || u.session == "" {
			idx = len(buckets)
			buckets = append(buckets, nil)
			if u.session != "" {
				bySession[u.session] = idx
			}
		}
		buckets[idx] = append(buckets[idx], u)
	}
	var out []section
	for round := 0; ; round++ {
		any := false
		for _, bucket := range buckets {
			if round < len(bucket) {
				any = true
				u := bucket[round]
				if len(out)+len(u.parts) <= limit {
					out = append(out, u.parts...)
				}
			}
		}
		if !any {
			break
		}
	}
	return out
}

// Whole bundles share every downstream item/exclusion/character budget. A
// companion cannot be removed while the output still claims a complete group.
func boundSections(sections []section, maxItems int, exclude map[string]bool) ([]section, bool) {
	truncated := false
	blocked := map[string]bool{}
	for _, s := range sections {
		if exclude[s.item.Key] && s.group != nil {
			blocked[s.group.ID] = true
		}
	}
	var out []section
	for i := 0; i < len(sections); {
		end := i + 1
		s := sections[i]
		if s.group != nil {
			for end < len(sections) && sections[end].group == s.group {
				end++
			}
		}
		if !exclude[s.item.Key] && (s.group == nil || !blocked[s.group.ID]) {
			if maxItems <= 0 || len(out)+end-i <= maxItems {
				out = append(out, sections[i:end]...)
			} else {
				truncated = true
			}
		}
		i = end
	}
	return out, truncated
}
