// Package embedding adds semantic search on top of the full-text index: it
// chunks search documents, embeds them through an OpenAI-compatible
// embeddings service (e.g. embeddings.cpp's embeddings_server), stores the
// vectors with sqlite-vec, and answers nearest-neighbour queries.
//
// Vectors are grouped in "spaces", one per embedding service fingerprint
// (model, quantization, dimensions, truncation). Changing the service
// creates a new space that is built in the background and replaces the old
// one only when complete, so vectors from different services are never
// compared.
package embedding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible embeddings endpoint.
type Client struct {
	URL        string // full endpoint, e.g. http://127.0.0.1:8090/v1/embeddings
	Model      string
	APIKey     string
	Dimensions int // request Matryoshka truncation; 0 = native size
	HTTP       *http.Client
}

type embedRequest struct {
	Input      []string `json:"input"`
	Model      string   `json:"model,omitempty"`
	Dimensions int      `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// Embed returns one vector per input, in order.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.embed(ctx, inputs, false)
}

// EmbedBackground marks index-building work as lower priority. Providers that
// do not implement the optional header still receive a standard embeddings body.
func (c *Client) EmbedBackground(ctx context.Context, inputs []string) ([][]float32, error) {
	return c.embed(ctx, inputs, true)
}

func (c *Client) embed(ctx context.Context, inputs []string, background bool) ([][]float32, error) {
	body, _ := json.Marshal(embedRequest{Input: inputs, Model: c.Model, Dimensions: c.Dimensions})
	req, err := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if background {
		req.Header.Set("X-Embedding-Priority", "background")
	} else {
		req.Header.Set("X-Embedding-Priority", "query")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding service: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("embedding service: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding service: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("embedding service: decode: %w", err)
	}
	if len(er.Data) != len(inputs) {
		return nil, fmt.Errorf("embedding service: got %d vectors for %d inputs", len(er.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) || len(d.Embedding) == 0 {
			return nil, errors.New("embedding service: malformed response")
		}
		out[d.Index] = d.Embedding
	}
	for i := range out {
		if out[i] == nil || len(out[i]) != len(out[0]) {
			return nil, errors.New("embedding service: missing or ragged vectors")
		}
	}
	return out, nil
}

// ServiceInfo identifies the embedding service configuration. Two services
// with equal fingerprints produce comparable vectors.
type ServiceInfo struct {
	Model        string `json:"model"`
	Architecture string `json:"architecture,omitempty"`
	FType        string `json:"ftype,omitempty"`
	// WeightTypes tells mixed quantizations apart (their ftype is the same),
	// e.g. "q4_K:49,q8_0:97" from embeddings.cpp's /info.
	WeightTypes string `json:"weight_types,omitempty"`
	Pooling     string `json:"pooling,omitempty"`
	MaxTokens   int    `json:"max_tokens,omitempty"`
	Dimensions  int    `json:"dimensions"`
	QueryPrefix string `json:"query_prefix,omitempty"`
}

// Fingerprint is a stable identifier of the info.
func (i ServiceInfo) Fingerprint() string {
	b, _ := json.Marshal(i)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

// Describe returns the service identity. It reads embeddings.cpp's /info
// when the service has one; otherwise it embeds a probe text to learn the
// dimension. The requested dimension and the query prefix are part of the
// identity because they change the vectors MemSpace stores and compares.
func (c *Client) Describe(ctx context.Context, queryPrefix string) (ServiceInfo, error) {
	info := ServiceInfo{Model: c.Model, QueryPrefix: queryPrefix}
	if base, ok := strings.CutSuffix(c.URL, "/v1/embeddings"); ok {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/info", nil)
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		if resp, err := c.http().Do(req); err == nil {
			var si ServiceInfo
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&si) == nil && si.Model != "" {
				info.Model, info.Architecture, info.FType, info.WeightTypes, info.Pooling, info.MaxTokens = si.Model, si.Architecture, si.FType, si.WeightTypes, si.Pooling, si.MaxTokens
			}
			resp.Body.Close()
		}
	}
	vecs, err := c.Embed(ctx, []string{"memspace probe"})
	if err != nil {
		return ServiceInfo{}, err
	}
	info.Dimensions = len(vecs[0])
	if info.Model == "" {
		info.Model = "unknown"
	}
	return info, nil
}

// DefaultTimeout bounds background embedding requests.
const DefaultTimeout = 60 * time.Second
