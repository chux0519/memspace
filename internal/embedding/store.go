package embedding

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// Space is one embedding service configuration and its vector table.
type Space struct {
	ID          int64
	Fingerprint string
	Model       string
	Dimensions  int
	VecTable    string
	Status      string // building | active | retired
}

func scanSpace(sc interface{ Scan(...any) error }) (*Space, error) {
	var s Space
	if err := sc.Scan(&s.ID, &s.Fingerprint, &s.Model, &s.Dimensions, &s.VecTable, &s.Status); err != nil {
		return nil, err
	}
	return &s, nil
}

const spaceCols = `space_id, fingerprint, model, dimensions, vec_table, status`

// ActiveSpace returns the space used for queries, or nil.
func ActiveSpace(ctx context.Context, db *sql.DB) (*Space, error) {
	s, err := scanSpace(db.QueryRowContext(ctx,
		`SELECT `+spaceCols+` FROM embedding_spaces WHERE status = 'active' ORDER BY space_id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// EnsureSpace returns the space for info, creating it (status building, with
// its sqlite-vec table) when the fingerprint is new.
func EnsureSpace(ctx context.Context, db *sql.DB, info ServiceInfo) (*Space, error) {
	fp := info.Fingerprint()
	s, err := scanSpace(db.QueryRowContext(ctx, `SELECT `+spaceCols+` FROM embedding_spaces WHERE fingerprint = ?`, fp))
	if err == nil {
		return s, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if info.Dimensions <= 0 || info.Dimensions > 8192 {
		return nil, fmt.Errorf("embedding: unsupported dimension %d", info.Dimensions)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO embedding_spaces (fingerprint, model, dimensions, vec_table, status, created_at)
		VALUES (?, ?, ?, '', 'building', ?)`,
		fp, info.Model, info.Dimensions, model.FormatTime(time.Now()))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	table := fmt.Sprintf("vec_space_%d", id)
	// Table name and dimension are generated here, never user input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE VIRTUAL TABLE %s USING vec0(project_id text partition key, embedding float[%d] distance_metric=cosine)`,
		table, info.Dimensions)); err != nil {
		return nil, fmt.Errorf("embedding: create vector table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE embedding_spaces SET vec_table = ? WHERE space_id = ?`, table, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Space{ID: id, Fingerprint: fp, Model: info.Model, Dimensions: info.Dimensions, VecTable: table, Status: "building"}, nil
}

// PendingDoc is a document without chunks in a space.
type PendingDoc struct {
	DocID     int64
	ProjectID string
	Text      string
}

// PendingDocs lists up to limit documents not yet embedded in space.
func PendingDocs(ctx context.Context, db *sql.DB, space *Space, limit int) ([]PendingDoc, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.doc_id, d.project_id, d.title, d.body, d.files FROM search_docs d
		WHERE NOT EXISTS (SELECT 1 FROM embedding_chunks c WHERE c.space_id = ? AND c.doc_id = d.doc_id)
		ORDER BY d.doc_id DESC LIMIT ?`, space.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingDoc
	for rows.Next() {
		var p PendingDoc
		var title, body, files string
		if err := rows.Scan(&p.DocID, &p.ProjectID, &title, &body, &files); err != nil {
			return nil, err
		}
		p.Text = DocText(title, body, files)
		out = append(out, p)
	}
	return out, rows.Err()
}

// DocText is what gets embedded for a search document.
func DocText(title, body, files string) string {
	var parts []string
	for _, p := range []string{title, body, files} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, "\n")
}

// CountPending reports how many documents a space still lacks.
func CountPending(ctx context.Context, db *sql.DB, space *Space) (pending, total int, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM search_docs d WHERE NOT EXISTS
			(SELECT 1 FROM embedding_chunks c WHERE c.space_id = ? AND c.doc_id = d.doc_id)),
			(SELECT COUNT(*) FROM search_docs)`, space.ID).Scan(&pending, &total)
	return
}

// Embedded is one chunk's vector, ready to store.
type Embedded struct {
	DocID     int64
	ProjectID string
	ChunkNo   int
	Text      string
	Vector    []float32
}

// StoreChunks writes chunks and vectors in one transaction. A document whose
// content changed meanwhile (its doc_id was re-indexed) is skipped: its
// chunks were deleted, so it stays pending and is embedded again.
func StoreChunks(ctx context.Context, db *sql.DB, space *Space, items []Embedded, texts map[int64]string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stale := map[int64]bool{}
	for docID, text := range texts {
		var title, body, files string
		err := tx.QueryRowContext(ctx, `SELECT title, body, files FROM search_docs WHERE doc_id = ?`, docID).Scan(&title, &body, &files)
		if err != nil || DocText(title, body, files) != text {
			stale[docID] = true
		}
	}
	for _, it := range items {
		if stale[it.DocID] {
			continue
		}
		if len(it.Vector) != space.Dimensions {
			return fmt.Errorf("embedding: vector has %d dimensions, space %d expects %d", len(it.Vector), space.ID, space.Dimensions)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO embedding_chunks (space_id, doc_id, chunk_no, text) VALUES (?, ?, ?, ?)
			ON CONFLICT (space_id, doc_id, chunk_no) DO NOTHING`,
			space.ID, it.DocID, it.ChunkNo, it.Text)
		if err != nil {
			return fmt.Errorf("embedding: store chunk: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already embedded by a concurrent sweep
		}
		chunkID, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+space.VecTable+` (rowid, project_id, embedding) VALUES (?, ?, ?)`,
			chunkID, it.ProjectID, Blob(it.Vector)); err != nil {
			return fmt.Errorf("embedding: store vector: %w", err)
		}
	}
	return tx.Commit()
}

// Blob encodes a vector the way sqlite-vec expects (little-endian float32).
func Blob(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// CleanOrphans removes vectors whose chunks were deleted.
func CleanOrphans(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.chunk_id, s.vec_table FROM embedding_orphans o
		JOIN embedding_spaces s ON s.space_id = o.space_id LIMIT 1000`)
	if err != nil {
		return 0, err
	}
	type orphan struct {
		id    int64
		table string
	}
	var list []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.table); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	rows.Close()
	if len(list) == 0 {
		return 0, rows.Err()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, o := range list {
		if o.table != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+o.table+` WHERE rowid = ?`, o.id); err != nil {
				return 0, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_orphans WHERE chunk_id = ?`, o.id); err != nil {
			return 0, err
		}
	}
	// Orphans of retired spaces (table already dropped) are just forgotten.
	if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_orphans WHERE space_id IN
		(SELECT space_id FROM embedding_spaces WHERE status = 'retired')`); err != nil {
		return 0, err
	}
	return len(list), tx.Commit()
}

// Activate makes space the query space once it covers every document, and
// retires older spaces (dropping their vectors).
func Activate(ctx context.Context, db *sql.DB, space *Space) (bool, error) {
	pending, _, err := CountPending(ctx, db, space)
	if err != nil || pending > 0 || space.Status == "active" {
		return space.Status == "active", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT space_id, vec_table FROM embedding_spaces WHERE space_id <> ? AND status <> 'retired'`, space.ID)
	if err != nil {
		return false, err
	}
	var old []struct {
		id    int64
		table string
	}
	for rows.Next() {
		var o struct {
			id    int64
			table string
		}
		rows.Scan(&o.id, &o.table)
		old = append(old, o)
	}
	rows.Close()
	for _, o := range old {
		if o.table != "" {
			if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+o.table); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM embedding_chunks WHERE space_id = ?`, o.id); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE embedding_spaces SET status = 'retired' WHERE space_id = ?`, o.id); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE embedding_spaces SET status = 'active', activated_at = ? WHERE space_id = ?`,
		model.FormatTime(time.Now()), space.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	space.Status = "active"
	return true, nil
}

// Hit is a document found by vector similarity.
type Hit struct {
	DocID      int64
	ChunkText  string
	Similarity float64 // cosine similarity, 1 = identical direction
}

// KNN returns documents nearest to vector in the space, best chunk per
// document, optionally restricted to projects. It over-fetches chunks so
// that k documents remain after grouping.
func KNN(ctx context.Context, db *sql.DB, space *Space, vector []float32, projectIDs []string, k int) ([]Hit, error) {
	if len(vector) != space.Dimensions {
		return nil, fmt.Errorf("embedding: query has %d dimensions, space expects %d", len(vector), space.Dimensions)
	}
	fetch := min(4*k, 4096)
	blob := Blob(vector)
	best := map[int64]Hit{}
	var order []int64
	run := func(project string) error {
		// The KNN constraints must apply to the vec0 scan itself, so it
		// runs in a subquery and is joined afterwards.
		inner := `SELECT rowid, distance FROM ` + space.VecTable + ` WHERE embedding MATCH ? AND k = ?`
		args := []any{blob, fetch}
		if project != "" {
			inner += ` AND project_id = ?`
			args = append(args, project)
		}
		q := `SELECT v.rowid, v.distance, c.doc_id, c.text FROM (` + inner + `) v
			JOIN embedding_chunks c ON c.chunk_id = v.rowid`
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("embedding: knn: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var chunkID, docID int64
			var dist float64
			var text string
			if err := rows.Scan(&chunkID, &dist, &docID, &text); err != nil {
				return err
			}
			sim := 1 - dist
			if h, ok := best[docID]; !ok || sim > h.Similarity {
				if !ok {
					order = append(order, docID)
				}
				best[docID] = Hit{DocID: docID, ChunkText: text, Similarity: sim}
			}
		}
		return rows.Err()
	}
	if len(projectIDs) == 0 {
		if err := run(""); err != nil {
			return nil, err
		}
	}
	for _, p := range projectIDs {
		if err := run(p); err != nil {
			return nil, err
		}
	}
	out := make([]Hit, 0, len(best))
	for _, id := range order {
		out = append(out, best[id])
	}
	sortHits(out)
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func sortHits(h []Hit) {
	sort.SliceStable(h, func(i, j int) bool { return h[i].Similarity > h[j].Similarity })
}
