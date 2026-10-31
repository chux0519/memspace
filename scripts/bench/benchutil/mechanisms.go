package benchutil

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chux0519/memspace/internal/contextpack"
	"github.com/chux0519/memspace/internal/model"
)

// MechanismCase contains only selector inputs, never expected answers or types.
// Keep these exports private: they contain source text and local index paths.
type MechanismCase struct {
	ID         string               `json:"id"`
	Query      string               `json:"query"`
	Owner      string               `json:"owner"`
	Device     string               `json:"device"`
	Index      string               `json:"index"`
	Request    contextpack.Request  `json:"request"`
	ModelMin   float64              `json:"model_min"`
	Baseline   *contextpack.Pack    `json:"baseline"`
	Candidates []MechanismCandidate `json:"candidates"`
}
type MechanismCandidate struct {
	Hit    model.SearchResult           `json:"hit"`
	Chunk  string                       `json:"chunk"`
	Window []contextpack.EvidenceRecord `json:"window"`
}

func MakeMechanismCase(ctx context.Context, d *sql.DB, id, owner, device, index string, req contextpack.Request, modelMin float64, baseline *contextpack.Pack, hits []model.SearchResult) (MechanismCase, error) {
	c := MechanismCase{ID: id, Query: req.Query, Owner: owner, Device: device, Index: index, Request: req, ModelMin: modelMin, Baseline: baseline, Candidates: []MechanismCandidate{}}
	for _, h := range hits {
		window, err := contextpack.DialogueWindow(ctx, d, owner, req.Project.ID, h)
		if err != nil {
			return c, err
		}
		c.Candidates = append(c.Candidates, MechanismCandidate{h, h.EvidenceText, window})
	}
	return c, nil
}
func WriteMechanismCases(path string, cases []MechanismCase) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, c := range cases {
		if e = enc.Encode(c); e != nil {
			return e
		}
	}
	return nil
}

// SnapshotCheckpointedIndex copies only a disposable, quiescent fixture DB.
func SnapshotCheckpointedIndex(ctx context.Context, d *sql.DB, src, dst string) error {
	if err := RequireCopy(src); err != nil {
		return err
	}

	var busy, logN, done int
	if err := d.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logN, &done); err != nil {
		return err
	}
	if busy != 0 || logN != done {
		return fmt.Errorf("fixture checkpoint busy")
	}
	if filepath.Clean(src) == filepath.Clean(dst) {
		return fmt.Errorf("copy onto itself")
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = RequireCopy(dst); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
