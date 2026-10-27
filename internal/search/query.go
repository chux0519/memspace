package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/redact"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

// bm25 column weights: title, body, files.
const rankExpr = `bm25(search_fts, 3.0, 1.0, 2.0)`

// stopTerms are function words that jieba emits as separate terms; requiring
// them would make AND queries needlessly strict.
var stopTerms = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`的 了 吗 呢 啊 吧 呀 是 在 和 与 及 或 也 就 都 而 还 又 再 很 太
		什么 怎么 怎样 如何 为什么 为何 哪个 哪些 哪里 这个 那个 这些 那些 这 那 这里 那里
		我 你 他 她 它 我们 你们 他们 自己 一个 一下 一些 请 帮 帮我 能 能否 可以 会 要 想 把 被 给 对 从 到 让
		么 有 没 没有 不 吗？ 呢？ 之 其 以 及其 等 等等`) {
		stopTerms[w] = true
	}
}

// Terms turns a jieba_query() expression into its top-level terms, dropping
// punctuation and function words. Each term is a valid FTS5 expression on
// its own: a quoted phrase, a prefix term, or a parenthesized OR-group (for
// ASCII words with pinyin alternatives).
func Terms(expr string) []string {
	var terms []string
	for _, t := range splitTopLevel(expr, " AND ") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, `"`) && strings.HasSuffix(t, `"`) && len(t) >= 2 {
			inner := strings.ReplaceAll(t[1:len(t)-1], `""`, `"`)
			if stopTerms[inner] || !hasWordRune(inner) || allFunctionRunes(inner) {
				continue
			}
		}
		if w := asciiWord(t); w != "" && englishStop[strings.ToLower(w)] {
			continue
		}
		terms = append(terms, t)
	}
	return terms
}

// englishStop are English function words; jieba_query renders ASCII words as
// "( w+h+y* OR why* )" or "why*".
var englishStop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but if then else of to in on at by for from with without about into over
		is are was were be been being am do does did done have has had having can could should would will shall may might must
		i me my mine we us our you your he him his she her it its they them their this that these those there here
		what which who whom whose why how when where before after again also just only very too so than not no yes
		please pls thanks thank ok okay hi hello let lets get got make made use used`) {
		englishStop[w] = true
	}
}

var asciiTermRE = regexp.MustCompile(`^(?:\(\s*\S+\s+OR\s+)?([A-Za-z]+)\*\s*\)?$`)

// asciiWord returns the plain word behind an ASCII term, or "".
func asciiWord(term string) string {
	if m := asciiTermRE.FindStringSubmatch(term); m != nil {
		return m[1]
	}
	return ""
}

// functionRunes are characters that only carry grammar; a term made of
// nothing else ("了吗", "这个") is not worth requiring.
const functionRunes = "的了吗呢啊吧呀么是在和与及或也就都而还又很太之其以等这那我你他她它们个些下请帮能可会要想把被给对从到让有没不"

func allFunctionRunes(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune(functionRunes, r) {
			return false
		}
	}
	return s != ""
}

func hasWordRune(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// splitTopLevel splits on sep outside quotes and parentheses.
func splitTopLevel(s, sep string) []string {
	var parts []string
	depth, inQuote, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case depth == 0 && strings.HasPrefix(s[i:], sep):
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(parts, s[start:])
}

// ErrBadRequest marks invalid search requests.
var ErrBadRequest = errors.New("bad search request")

// SemanticHit is a document found by vector similarity.
type SemanticHit struct {
	DocID      int64
	Text       string // the best-matching chunk
	Similarity float64
}

// Semantic finds documents similar in meaning to a query. It returns no hits
// (and no error) when semantic search is unavailable at the moment.
type Semantic interface {
	SemanticSearch(ctx context.Context, query string, projectIDs []string, k int) ([]SemanticHit, error)
	// MinSimilarities returns the model's thresholds: for injecting a
	// meaning-only match into an agent's context, and for keeping it in
	// search results at all.
	MinSimilarities() (inject, searchFloor float64)
}

var semantic Semantic

// SetSemantic enables hybrid search (nil disables it). Called once at server
// start.
func SetSemantic(s Semantic) { semantic = s }

// rrfK is the Reciprocal Rank Fusion constant (Cormack et al.).
const rrfK = 60

// MinSimilarities returns the current semantic thresholds (see
// Semantic.MinSimilarities); zero when semantic search is off.
func MinSimilarities() (inject, searchFloor float64) {
	if semantic == nil {
		return 0, 0
	}
	return semantic.MinSimilarities()
}

// Search runs req for the owner. Keyword search segments the query with
// jieba; results matching every term come first, then partial matches. When
// semantic search is available, its results are fused with the keyword
// results by Reciprocal Rank Fusion.
func Search(ctx context.Context, db *sql.DB, ownerUserID string, req model.SearchRequest) (*model.SearchResponse, error) {
	resp := &model.SearchResponse{Results: []model.SearchResult{}, Scope: req.Scope}
	if resp.Scope == "" {
		resp.Scope = model.ScopeCurrentProject
	}
	switch req.Mode {
	case "", "keyword", "semantic":
	default:
		return nil, fmt.Errorf("%w: mode must be keyword or semantic", ErrBadRequest)
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)

	projectIDs, note, err := resolveScope(ctx, db, ownerUserID, resp.Scope, req)
	if err != nil {
		return nil, err
	}
	resp.ProjectIDs, resp.Note = projectIDs, note
	if note != "" {
		return resp, nil
	}

	var kw []model.SearchResult
	var kwNote string
	if req.Mode != "semantic" {
		if kw, kwNote, err = keyword(ctx, db, ownerUserID, req, projectIDs, limit); err != nil {
			return nil, err
		}
	}
	var sem []model.SearchResult
	if req.Mode != "keyword" && semantic != nil && strings.TrimSpace(req.Query) != "" {
		if sem, err = semanticResults(ctx, db, ownerUserID, req, projectIDs, limit); err != nil {
			return nil, err
		}
	}
	switch {
	case len(sem) == 0:
		resp.Results = kw
		if len(kw) == 0 {
			resp.Note = kwNote
		}
	case len(kw) == 0:
		resp.Results = sem[:min(len(sem), limit)]
	default:
		resp.Results = fuse(kw, sem, limit)
	}
	return resp, nil
}

// fuse merges ranked lists with Reciprocal Rank Fusion.
func fuse(kw, sem []model.SearchResult, limit int) []model.SearchResult {
	score := map[int64]float64{}
	byID := map[int64]model.SearchResult{}
	var order []int64
	add := func(list []model.SearchResult) {
		for i, r := range list {
			if _, ok := byID[r.DocID]; !ok {
				byID[r.DocID] = r
				order = append(order, r.DocID)
			} else if r.Similarity > 0 {
				h := byID[r.DocID]
				h.Similarity = r.Similarity
				byID[r.DocID] = h
			}
			score[r.DocID] += 1.0 / float64(rrfK+i+1)
		}
	}
	add(kw)
	add(sem)
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	out := make([]model.SearchResult, 0, min(limit, len(order)))
	for _, id := range order[:min(limit, len(order))] {
		r := byID[id]
		r.Score = score[id]
		out = append(out, r)
	}
	return out
}

// keyword runs the full-text part of a search.
func keyword(ctx context.Context, db *sql.DB, owner string, req model.SearchRequest, projectIDs []string, limit int) ([]model.SearchResult, string, error) {
	var expr string
	if err := db.QueryRowContext(ctx, `SELECT jieba_query(?)`, req.Query).Scan(&expr); err != nil {
		return nil, "", fmt.Errorf("search: segment query: %w", err)
	}
	terms := Terms(expr)
	if len(terms) == 0 {
		return nil, "query has no searchable terms", nil
	}
	seen := map[int64]bool{}
	results, err := query(ctx, db, owner, strings.Join(terms, " AND "), projectIDs, req, limit, seen)
	if err != nil {
		return nil, "", err
	}
	for i := range results {
		results[i].Match, results[i].MatchedTerms, results[i].TotalTerms = "all", len(terms), len(terms)
		seen[results[i].DocID] = true
	}
	if len(terms) > 1 && len(results) < limit {
		// Partial matches: fetch extra candidates, then rank by how many
		// query terms each covers before relevance, so a document matching
		// three of four words beats one matching a single common word.
		anyHits, err := query(ctx, db, owner, strings.Join(terms, " OR "), projectIDs, req, 4*(limit-len(results)), seen)
		if err != nil {
			return nil, "", err
		}
		if err := countTerms(ctx, db, terms, anyHits); err != nil {
			return nil, "", err
		}
		sort.SliceStable(anyHits, func(i, j int) bool {
			if anyHits[i].MatchedTerms != anyHits[j].MatchedTerms {
				return anyHits[i].MatchedTerms > anyHits[j].MatchedTerms
			}
			return anyHits[i].Score > anyHits[j].Score
		})
		for _, h := range anyHits {
			if len(results) >= limit {
				break
			}
			h.Match, h.TotalTerms = "any", len(terms)
			results = append(results, h)
		}
	}
	return results, "", nil
}

// semanticResults turns nearest neighbours into results, applying the same
// owner, scope and filters as keyword search.
func semanticResults(ctx context.Context, db *sql.DB, owner string, req model.SearchRequest, projectIDs []string, limit int) ([]model.SearchResult, error) {
	hits, err := semantic.SemanticSearch(ctx, req.Query, projectIDs, 3*limit)
	if err != nil || len(hits) == 0 {
		return nil, err
	}
	where, args := filters(owner, projectIDs, req)
	ids := make([]any, len(hits))
	for i, h := range hits {
		ids[i] = h.DocID
	}
	where = append(where, "d.doc_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")")
	args = append(args, ids...)
	rows, err := db.QueryContext(ctx, `
		SELECT d.doc_id, d.kind, d.ref_id, d.project_id, p.name, COALESCE(d.session_id, ''),
			COALESCE(d.agent_type, ''), COALESCE(d.device_id, ''), COALESCE(dv.name, ''), d.occurred_at, d.title
		FROM search_docs d
		JOIN projects p ON p.project_id = d.project_id
		LEFT JOIN devices dv ON dv.device_id = d.device_id
		WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, fmt.Errorf("search: semantic docs: %w", err)
	}
	defer rows.Close()
	found := map[int64]model.SearchResult{}
	for rows.Next() {
		var r model.SearchResult
		var occurred string
		if err := rows.Scan(&r.DocID, &r.Kind, &r.RefID, &r.ProjectID, &r.ProjectName, &r.SessionID,
			&r.AgentType, &r.DeviceID, &r.DeviceName, &occurred, &r.Title); err != nil {
			return nil, err
		}
		r.OccurredAt, _ = time.Parse(time.RFC3339Nano, occurred)
		found[r.DocID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, floor := MinSimilarities()
	var out []model.SearchResult
	for _, h := range hits { // keep similarity order
		r, ok := found[h.DocID]
		if !ok || h.Similarity < floor {
			continue // filtered out
		}
		r.Match, r.Similarity, r.Score = "semantic", h.Similarity, h.Similarity
		r.Snippet = redact.Truncate(strings.Join(strings.Fields(h.Text), " "), 240)
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// filters builds the WHERE clauses shared by keyword and semantic search
// (documents d, projects p).
func filters(owner string, projectIDs []string, req model.SearchRequest) ([]string, []any) {
	where := []string{"p.user_id = ?"}
	args := []any{owner}
	in := func(col string, vals []string) {
		if len(vals) == 0 {
			return
		}
		where = append(where, col+" IN ("+strings.TrimSuffix(strings.Repeat("?,", len(vals)), ",")+")")
		for _, v := range vals {
			args = append(args, v)
		}
	}
	in("d.project_id", projectIDs)
	in("d.kind", req.Kinds)
	in("d.agent_type", req.AgentTypes)
	in("d.device_id", req.DeviceIDs)
	if req.SessionID != "" {
		where = append(where, "d.session_id = ?")
		args = append(args, req.SessionID)
	}
	if req.Since != nil {
		where = append(where, "d.occurred_at >= ?")
		args = append(args, model.FormatTime(*req.Since))
	}
	if req.Until != nil {
		where = append(where, "d.occurred_at < ?")
		args = append(args, model.FormatTime(*req.Until))
	}
	return where, args
}

// resolveScope returns the project IDs to search (nil means every project of
// the owner), or a note explaining why nothing can match.
func resolveScope(ctx context.Context, db *sql.DB, owner, scope string, req model.SearchRequest) ([]string, string, error) {
	switch scope {
	case model.ScopeAllProjects:
		return nil, "", nil
	case model.ScopeSelectedProjects:
		if len(req.ProjectIDs) == 0 {
			return nil, "", fmt.Errorf("%w: selected_projects needs project_ids", ErrBadRequest)
		}
		return req.ProjectIDs, "", nil
	case model.ScopeCurrentProject:
		id, err := ResolveProject(ctx, db, owner, req.ProjectID, req.GitIdentity, req.ProjectName)
		if err != nil {
			return nil, "", err
		}
		if id == "" {
			return nil, "unknown project: nothing has been recorded for it yet", nil
		}
		return []string{id}, "", nil
	default:
		return nil, "", fmt.Errorf("%w: unknown scope %q", ErrBadRequest, scope)
	}
}

// ResolveProject finds an existing project by ID, Git identity or name (the
// same matching rules as project creation), without creating one.
func ResolveProject(ctx context.Context, db *sql.DB, owner, projectID, gitIdentity, name string) (string, error) {
	var id string
	var err error
	switch {
	case projectID != "":
		err = db.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE project_id = ? AND user_id = ?`, projectID, owner).Scan(&id)
	case gitIdentity != "":
		err = db.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE user_id = ? AND canonical_git_identity = ?`, owner, gitIdentity).Scan(&id)
	case name != "":
		err = db.QueryRowContext(ctx, `SELECT project_id FROM projects WHERE user_id = ? AND name = ? AND canonical_git_identity IS NULL ORDER BY created_at LIMIT 1`, owner, name).Scan(&id)
	default:
		return "", fmt.Errorf("%w: current_project needs project_id, git_identity or project_name", ErrBadRequest)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// Snippet highlight markers. Brackets read well in terminals; the web UI asks
// for private-use code points, which cannot be confused with captured text.
const (
	MarkOpen  = "\uE000"
	MarkClose = "\uE001"
)

func snippetMarks(highlight string) string {
	if highlight == model.HighlightMarks {
		return "'" + MarkOpen + "', '" + MarkClose + "'"
	}
	return "'[', ']'"
}

func query(ctx context.Context, db *sql.DB, owner, match string, projectIDs []string, req model.SearchRequest, limit int, exclude map[int64]bool) ([]model.SearchResult, error) {
	if limit <= 0 {
		return nil, nil
	}
	where, fargs := filters(owner, projectIDs, req)
	where = append([]string{"search_fts MATCH ?"}, where...)
	args := append([]any{match}, fargs...)
	// Over-fetch by the number already returned, then drop those.
	args = append(args, limit+len(exclude))

	rows, err := db.QueryContext(ctx, `
		SELECT d.doc_id, d.kind, d.ref_id, d.project_id, p.name, COALESCE(d.session_id, ''),
			COALESCE(d.agent_type, ''), COALESCE(d.device_id, ''), COALESCE(dv.name, ''),
			d.occurred_at, d.title,
			COALESCE(NULLIF(simple_snippet(search_fts, 1, `+snippetMarks(req.Highlight)+`, '…', 24), ''),
				simple_snippet(search_fts, 2, `+snippetMarks(req.Highlight)+`, '…', 24)),
			`+rankExpr+`
		FROM search_fts
		JOIN search_docs d ON d.doc_id = search_fts.rowid
		JOIN projects p ON p.project_id = d.project_id
		LEFT JOIN devices dv ON dv.device_id = d.device_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY `+rankExpr+`, d.occurred_at DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	defer rows.Close()
	var out []model.SearchResult
	for rows.Next() {
		var r model.SearchResult
		var occurred string
		var snippet sql.NullString
		if err := rows.Scan(&r.DocID, &r.Kind, &r.RefID, &r.ProjectID, &r.ProjectName, &r.SessionID,
			&r.AgentType, &r.DeviceID, &r.DeviceName, &occurred, &r.Title, &snippet, &r.Score); err != nil {
			return nil, err
		}
		if exclude[r.DocID] || len(out) >= limit {
			continue
		}
		r.Score = -r.Score // bm25 is lower-is-better
		r.OccurredAt, _ = time.Parse(time.RFC3339Nano, occurred)
		r.Snippet = snippet.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// countTerms sets MatchedTerms on each hit: one indexed lookup per term,
// restricted to the hits.
func countTerms(ctx context.Context, db *sql.DB, terms []string, hits []model.SearchResult) error {
	if len(hits) == 0 {
		return nil
	}
	ids := make([]any, 0, len(hits)+1)
	idx := map[int64]int{}
	for i, h := range hits {
		idx[h.DocID] = i
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hits)), ",")
	for _, term := range terms {
		ids = append(ids[:0], term)
		for _, h := range hits {
			ids = append(ids, h.DocID)
		}
		rows, err := db.QueryContext(ctx, `SELECT rowid FROM search_fts WHERE search_fts MATCH ? AND rowid IN (`+placeholders+`)`, ids...)
		if err != nil {
			return fmt.Errorf("search: count terms: %w", err)
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil {
				hits[idx[id]].MatchedTerms++
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

// ProjectRef identifies a project the way different callers can: by ID, by
// Git identity or name (clients that inspect their working directory), or by
// a working directory on the calling device (remote MCP tools, which cannot
// inspect the caller's filesystem but know the agent's cwd).
type ProjectRef struct {
	ID          string `json:"project_id,omitempty"`
	GitIdentity string `json:"git_identity,omitempty"`
	Name        string `json:"project_name,omitempty"`
	Cwd         string `json:"cwd,omitempty"`
}

// Resolve finds the project for ref; deviceID is needed for Cwd lookups.
// It returns "" when nothing matches.
func (ref ProjectRef) Resolve(ctx context.Context, db *sql.DB, owner, deviceID string) (string, error) {
	if ref.ID != "" || ref.GitIdentity != "" || ref.Name != "" {
		return ResolveProject(ctx, db, owner, ref.ID, ref.GitIdentity, ref.Name)
	}
	if ref.Cwd == "" || deviceID == "" {
		return "", fmt.Errorf("%w: a project_id, git_identity, project_name or cwd is required", ErrBadRequest)
	}
	cwd := strings.TrimRight(ref.Cwd, `/\`)
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT l.project_id FROM project_locations l JOIN projects p ON p.project_id = l.project_id
		WHERE l.device_id = ? AND p.user_id = ?
			AND (? = l.absolute_path OR substr(?, 1, length(l.absolute_path) + 1) IN (l.absolute_path || '/', l.absolute_path || '\'))
		ORDER BY length(l.absolute_path) DESC LIMIT 1`,
		deviceID, owner, cwd, cwd).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
