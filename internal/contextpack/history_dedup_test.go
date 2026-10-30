package contextpack

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/db"
	"github.com/chux0519/memspace/internal/model"
)

func TestUniqueHistoryUsesFullScopedSourceAndProvenance(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "dedup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{`INSERT INTO users(user_id) VALUES ('u')`, `INSERT INTO projects(project_id,user_id,name) VALUES ('p','u','p'),('other','u','other')`} {
		if _, err = d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var hits []model.SearchResult
	at := time.Unix(10, 0)
	for i, body := range []string{"same preview tail-A", "same preview tail-A", "same preview tail-B", "same preview tail-A", "same preview tail-A", "same preview tail-A", "outside"} {
		project := "p"
		if i == 6 {
			project = "other"
		}
		r, e := d.Exec(`INSERT INTO search_docs(kind,ref_id,project_id,occurred_at,body) VALUES ('reply',?,?,?,?)`, string(rune('a'+i)), project, model.FormatTime(at), body)
		if e != nil {
			t.Fatal(e)
		}
		id, _ := r.LastInsertId()
		h := model.SearchResult{DocID: id, SessionID: string(rune('a' + i)), Kind: "reply", AgentType: "agent", DeviceID: "d", OccurredAt: at, Snippet: "same preview"}
		if i == 3 {
			h.OccurredAt = at.Add(time.Second)
		}
		if i == 4 {
			h.Kind = "prompt"
		}
		if i == 5 {
			h.DeviceID = "another"
		}
		hits = append(hits, h)
	}
	got, err := uniqueHistoryHits(context.Background(), d, "u", "p", hits, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("expected only exact duplicate and wrong project removed: %+v", got)
	}
	for i, want := range []int{0, 2, 3, 4, 5} {
		if got[i].DocID != hits[want].DocID {
			t.Fatalf("wrong stable source at %d: %+v", i, got)
		}
	}
	got, err = uniqueHistoryHits(context.Background(), d, "wrong-owner", "p", hits, 24)
	if err != nil || len(got) != 0 {
		t.Fatalf("owner leak: %+v %v", got, err)
	}
	got, err = uniqueHistoryHits(context.Background(), d, "u", "p", hits, 2)
	if err != nil || len(got) != 2 || got[1].DocID != hits[2].DocID {
		t.Fatalf("duplicate consumed candidate budget: %+v %v", got, err)
	}
}
