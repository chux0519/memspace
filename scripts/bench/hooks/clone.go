package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// cloneIndex expands derived records into independent project partitions.
// Raw events are deliberately not duplicated; original requests remain fixed.
func cloneIndex(ctx context.Context, d *sql.DB, total int) error {
	if total < 2 || total > 20 {
		return fmt.Errorf("clone total must be 2..20")
	}
	var existing int
	if e := d.QueryRow(`SELECT count(*) FROM projects WHERE project_id LIKE 'bench-clone-%'`).Scan(&existing); e != nil {
		return e
	}
	if existing > 0 {
		return fmt.Errorf("clone index already contains synthetic partitions")
	}
	var maxDoc, maxChunk int64
	if e := d.QueryRow(`SELECT coalesce(max(doc_id),0) FROM search_docs`).Scan(&maxDoc); e != nil {
		return e
	}
	if e := d.QueryRow(`SELECT coalesce(max(chunk_id),0) FROM embedding_chunks`).Scan(&maxChunk); e != nil {
		return e
	}
	rows, e := d.Query(`SELECT vec_table FROM embedding_spaces WHERE status='active'`)
	if e != nil {
		return e
	}
	var tables []string
	for rows.Next() {
		var table string
		rows.Scan(&table)
		tables = append(tables, table)
	}
	rows.Close()
	for i := 1; i < total; i++ {
		prefix := fmt.Sprintf("bench-clone-%d-", i)
		tx, e := d.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		queries := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO projects(project_id,user_id,name,created_at,updated_at) SELECT ?||project_id,user_id,?||name,created_at,updated_at FROM projects WHERE project_id NOT LIKE 'bench-clone-%'`, []any{prefix, prefix}},
			{`INSERT INTO sessions(session_id,project_id,device_id,agent_type,native_session_id,started_at,ended_at,status) SELECT ?||session_id,?||project_id,device_id,agent_type,?||native_session_id,started_at,ended_at,status FROM sessions WHERE project_id NOT LIKE 'bench-clone-%'`, []any{prefix, prefix, prefix}},
			{`INSERT INTO search_docs(doc_id,kind,ref_id,project_id,session_id,device_id,agent_type,occurred_at,title,body,files) SELECT doc_id+?,kind,?||ref_id,?||project_id,CASE WHEN session_id IS NULL THEN NULL ELSE ?||session_id END,device_id,agent_type,occurred_at,title,body,files FROM search_docs WHERE doc_id<=?`, []any{int64(i) * maxDoc, prefix, prefix, prefix, maxDoc}},
			{`INSERT INTO embedding_chunks(chunk_id,space_id,doc_id,chunk_no,text) SELECT chunk_id+?,space_id,doc_id+?,chunk_no,text FROM embedding_chunks WHERE chunk_id<=?`, []any{int64(i) * maxChunk, int64(i) * maxDoc, maxChunk}},
		}
		for _, q := range queries {
			if _, e = tx.ExecContext(ctx, q.sql, q.args...); e != nil {
				tx.Rollback()
				return e
			}
		}
		for _, table := range tables {
			q := `INSERT INTO ` + table + ` (rowid,project_id,embedding) SELECT rowid+?,?||project_id,embedding FROM ` + table + ` WHERE rowid<=?`
			if _, e = tx.ExecContext(ctx, q, int64(i)*maxChunk, prefix, maxChunk); e != nil {
				tx.Rollback()
				return e
			}
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		log.Printf("cloned partition set %d/%d", i+1, total)
	}
	_, e = d.ExecContext(ctx, `INSERT INTO search_fts(search_fts) VALUES ('rebuild')`)
	return e
}
