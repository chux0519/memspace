package search_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/mattn/go-sqlite3"
)

type budgetSemantic struct {
	hits  []search.SemanticHit
	err   error
	calls int
}

func (s *budgetSemantic) SemanticSearch(ctx context.Context, _ string, _ []string, _ int) ([]search.SemanticHit, error) {
	s.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.hits, s.err
}
func (*budgetSemantic) MinSimilarities() (float64, float64) { return .6, .45 }

// Cancel a real SQL statement on the single-connection production DB pool.
// After the callback returns, SQLite must release that connection for semantic
// result hydration, using the caller's live context rather than the expired one.
func slowKeyword(t *testing.T, f *fixture) {
	t.Helper()
	conn, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("jieba_query", func(string) string {
			time.Sleep(25 * time.Millisecond)
			return "connection*"
		}, true)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestKeywordBudgetKeepsScopedSemanticResults(t *testing.T) {
	f := newFixture(t)
	slowKeyword(t, f)
	var inside, outside int64
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE project_id='webapp' LIMIT 1`).Scan(&inside); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE project_id='billing' LIMIT 1`).Scan(&outside); err != nil {
		t.Fatal(err)
	}
	sem := &budgetSemantic{hits: []search.SemanticHit{{DocID: outside, Similarity: .99}, {DocID: inside, Similarity: .9}}}
	search.SetSemantic(sem)
	t.Cleanup(func() { search.SetSemantic(nil) })
	r, err := search.SearchWithOptions(context.Background(), f.db, owner, model.SearchRequest{
		Query: "connection", Scope: model.ScopeCurrentProject, ProjectID: "webapp",
	}, search.Options{KeywordTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if sem.calls != 1 || len(r.Results) != 1 || r.Results[0].DocID != inside || r.Note == "" {
		t.Fatalf("lost fallback, scope, or partial-results note: %+v, calls=%d", r, sem.calls)
	}
}

func TestKeywordBudgetDoesNotInventSuccessOrHideCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{"no semantic results", nil},
		{"semantic failure", errors.New("semantic failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			slowKeyword(t, f)
			sem := &budgetSemantic{err: test.err}
			search.SetSemantic(sem)
			t.Cleanup(func() { search.SetSemantic(nil) })
			_, err := search.SearchWithOptions(context.Background(), f.db, owner, all("connection"), search.Options{KeywordTimeout: time.Millisecond})
			want := test.err
			if want == nil {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || sem.calls != 1 {
				t.Fatalf("empty fallback/failure hidden: %v, calls=%d", err, sem.calls)
			}
		})
	}
	f := newFixture(t)
	sem := &budgetSemantic{}
	search.SetSemantic(sem)
	t.Cleanup(func() { search.SetSemantic(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := search.SearchWithOptions(ctx, f.db, owner, all("connection"), search.Options{KeywordTimeout: time.Millisecond})
	if !errors.Is(err, context.Canceled) || sem.calls != 0 {
		t.Fatalf("caller cancellation hidden: %v, calls=%d", err, sem.calls)
	}
}

func TestKeywordBudgetDoesNotCapKeywordOnlySearch(t *testing.T) {
	for _, mode := range []string{"keyword", "bm25", ""} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.event("s-web-1", "agent.turn_completed", map[string]any{"last_assistant_message": "connection repaired"})
			slowKeyword(t, f)
			sem := &budgetSemantic{}
			if mode != "" {
				search.SetSemantic(sem)
			} else {
				search.SetSemantic(nil)
			}
			t.Cleanup(func() { search.SetSemantic(nil) })
			req := all("connection")
			req.Mode = mode
			r, err := search.SearchWithOptions(context.Background(), f.db, owner, req, search.Options{KeywordTimeout: time.Millisecond})
			if err != nil || len(r.Results) == 0 || sem.calls != 0 || r.Note != "" {
				t.Fatalf("keyword-only search truncated: %+v %v, calls=%d", r, err, sem.calls)
			}
		})
	}
}
