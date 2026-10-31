package server_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/evidence"
	"github.com/chux0519/memspace/internal/model"
)

func TestEvidenceDetailUsesAuthenticatedProjectScope(t *testing.T) {
	srv := newTestServer(t)
	token := registerDevice(t, srv, "evidence-reader")
	project := ensureProject(t, srv, token, "evidence-project")
	receipt := postBatch(t, srv, token, project, []model.Event{{EventID: "evt_source", OccurredAt: time.Now().UTC(), SchemaVersion: 1, EventType: "source.document_captured", PayloadJSON: `{"content":"retained article","url":"https://example.invalid/article","complete":false}`}})
	if receipt.Errors != 0 {
		t.Fatalf("ingest rejected: %+v", receipt)
	}
	value, _ := testDBs.Load(srv)
	d := value.(*sql.DB)
	ctx := evidence.WithPolicy(context.Background(), evidence.Policy{Mode: "shadow"})
	tx, e := d.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = evidence.IndexEvent(ctx, tx, "evt_source"); e != nil {
		tx.Rollback()
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var id string
	if e = d.QueryRow(`SELECT object_id FROM evidence_objects WHERE ref_id='evt_source'`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		auth    bool
		project string
		want    int
	}{{false, project, 401}, {true, "other-project", 404}, {true, "", 400}, {true, project, 200}} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+"/api/v1/evidence/"+id+"?project_id="+tc.project, nil)
			if tc.auth {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				t.Fatal(e)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatal(resp.StatusCode, tc.want)
			}
		})
	}
}
