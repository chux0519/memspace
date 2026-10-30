package search

import (
	"context"
	"database/sql"
	"github.com/chux0519/memspace/internal/model"
	"sort"
)

// FuseSessionRanks gives each retrieval view one vote per distinct session.
// Repeated matching passages cannot give a long session extra votes. Earlier
// streams supply the representative source; score ties keep their stable order.
func FuseSessionRanks(streams [][]model.SearchResult, limit int) []model.SearchResult {
	if limit <= 0 {
		return nil
	}
	scores := map[string]float64{}
	byID := map[string]model.SearchResult{}
	order := []string{}
	for _, stream := range streams {
		seen := map[string]bool{}
		rank := 0
		for _, h := range stream {
			id := h.SessionID
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			rank++
			if _, ok := byID[id]; !ok {
				byID[id] = h
				order = append(order, id)
			}
			scores[id] += 1 / float64(rrfK+rank)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return scores[order[i]] > scores[order[j]] })
	out := []model.SearchResult{}
	for _, id := range order[:min(limit, len(order))] {
		h := byID[id]
		h.Score = scores[id]
		out = append(out, h)
	}
	return out
}

// SearchSessions combines session summaries with individual records using
// independent BM25 rankings, then returns distinct sessions. Both views use the
// same owner/project/time filters as Search. No summary or answer is generated.
func SearchSessions(ctx context.Context, db *sql.DB, owner string, req model.SearchRequest) (*model.SearchResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	accepts := func(kind string) bool {
		if len(req.Kinds) == 0 {
			return true
		}
		for _, k := range req.Kinds {
			if k == kind {
				return true
			}
		}
		return false
	}
	var streams [][]model.SearchResult
	result := &model.SearchResponse{Results: []model.SearchResult{}}
	for _, kinds := range [][]string{{KindSummary}, {KindPrompt, KindReply, KindFailure, KindChange, KindCommand, KindMemory}} {
		selected := []string{}
		for _, kind := range kinds {
			if accepts(kind) {
				selected = append(selected, kind)
			}
		}
		if len(selected) == 0 {
			continue
		}
		view := req
		view.Mode = "bm25"
		view.Limit = maxLimit
		view.Kinds = selected
		response, err := Search(ctx, db, owner, view)
		if err != nil {
			return nil, err
		}
		result.Scope, result.ProjectIDs, result.Note = response.Scope, response.ProjectIDs, response.Note
		streams = append(streams, response.Results)
	}
	if len(streams) == 0 {
		view := req
		view.Mode = "bm25"
		return Search(ctx, db, owner, view)
	}
	result.Results = FuseSessionRanks(streams, limit)
	return result, nil
}
