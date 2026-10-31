package search_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/model"
	"github.com/chux0519/memspace/internal/search"
	"github.com/mattn/go-sqlite3"
)

type parallelSemantic struct {
	run func(context.Context, string) ([]search.SemanticHit, error)
}

func (s parallelSemantic) SemanticSearch(ctx context.Context, q string, _ []string, _ int) ([]search.SemanticHit, error) {
	return s.run(ctx, q)
}
func (parallelSemantic) MinSimilarities() (float64, float64) { return .6, .45 }

func setKeywordFunction(t *testing.T, f *fixture, fn func(string) (string, error)) {
	t.Helper()
	conn, err := f.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.Raw(func(raw any) error { return raw.(*sqlite3.SQLiteConn).RegisterFunc("jieba_query", fn, true) }); err != nil {
		t.Fatal(err)
	}
}

// Each branch waits for the other to start. This proves overlap without timing
// assertions, while semantic hydration still uses the single SQLite connection.
func TestParallelRetrievalOverlapsAndPreservesScopedFusion(t *testing.T) {
	f := newFixture(t)
	f.event("s-web-1", "agent.turn_completed", map[string]any{"last_assistant_message": "connection repaired"})
	var inside, outside int64
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE project_id='webapp' LIMIT 1`).Scan(&inside); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE project_id='billing' LIMIT 1`).Scan(&outside); err != nil {
		t.Fatal(err)
	}
	hits := []search.SemanticHit{{DocID: outside, Similarity: .99}, {DocID: inside, Similarity: .9}}
	req := model.SearchRequest{Query: "connection", Scope: model.ScopeCurrentProject, ProjectID: "webapp"}
	search.SetSemantic(&budgetSemantic{hits: hits})
	t.Cleanup(func() { search.SetSemantic(nil) })
	serial, err := search.Search(context.Background(), f.db, owner, req)
	if err != nil {
		t.Fatal(err)
	}
	semanticStarted, keywordStarted := make(chan struct{}), make(chan struct{})
	search.SetSemantic(parallelSemantic{run: func(ctx context.Context, q string) ([]search.SemanticHit, error) {
		close(semanticStarted)
		select {
		case <-keywordStarted:
			return hits, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})
	setKeywordFunction(t, f, func(string) (string, error) {
		close(keywordStarted)
		select {
		case <-semanticStarted:
			return "connection*", nil
		case <-time.After(2 * time.Second):
			return "", errors.New("semantic branch did not overlap")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	parallel, err := search.SearchWithOptions(ctx, f.db, owner, req, search.Options{Parallel: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(serial, parallel) {
		t.Fatalf("fusion or scope changed: serial=%+v parallel=%+v", serial, parallel)
	}
}

func TestParallelRetrievalJoinsCancelledWork(t *testing.T) {
	for _, callerCancels := range []bool{false, true} {
		t.Run(map[bool]string{false: "keyword failure", true: "caller cancellation"}[callerCancels], func(t *testing.T) {
			f := newFixture(t)
			started, finished := make(chan struct{}), make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			search.SetSemantic(parallelSemantic{run: func(ctx context.Context, _ string) ([]search.SemanticHit, error) {
				close(started)
				defer close(finished)
				<-ctx.Done()
				return nil, ctx.Err()
			}})
			t.Cleanup(func() { search.SetSemantic(nil) })
			failure := errors.New("keyword failed")
			setKeywordFunction(t, f, func(string) (string, error) {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					return "", errors.New("semantic did not start")
				}
				if callerCancels {
					cancel()
					return "connection*", nil
				}
				return "", failure
			})
			_, err := search.SearchWithOptions(ctx, f.db, owner, all("connection"), search.Options{Parallel: true})
			if callerCancels {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost caller cancellation: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), failure.Error()) {
				t.Fatalf("lost SQL failure: %v", err)
			}
			select {
			case <-finished:
			default:
				t.Fatal("returned with semantic work still running")
			}
			// A cancellation must also release the single database connection.
			if err := f.db.PingContext(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestParallelKeywordBudgetKeepsSemanticFallback(t *testing.T) {
	f := newFixture(t)
	slowKeyword(t, f)
	var inside int64
	if err := f.db.QueryRow(`SELECT doc_id FROM search_docs WHERE project_id='webapp' LIMIT 1`).Scan(&inside); err != nil {
		t.Fatal(err)
	}
	sem := &budgetSemantic{hits: []search.SemanticHit{{DocID: inside, Similarity: .9}}}
	search.SetSemantic(sem)
	t.Cleanup(func() { search.SetSemantic(nil) })
	r, err := search.SearchWithOptions(context.Background(), f.db, owner, all("connection"), search.Options{Parallel: true, KeywordTimeout: time.Millisecond, SemanticQuery: "compact meaning"})
	if err != nil || len(r.Results) != 1 || r.Note == "" || sem.query != "compact meaning" {
		t.Fatalf("lost fallback: %+v %v", r, err)
	}
}
