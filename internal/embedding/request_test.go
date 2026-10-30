package embedding_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chux0519/memspace/internal/embedding"
)

func TestRequestPriorityHint(t *testing.T) {
	hints := make(chan string, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hints <- r.Header.Get("X-Embedding-Priority")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 1 || body["input"] == nil {
			t.Errorf("unexpected embeddings body: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()
	c := embedding.Client{URL: srv.URL}
	if _, err := c.Embed(context.Background(), []string{"query"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EmbedBackground(context.Background(), []string{"document"}); err != nil {
		t.Fatal(err)
	}
	if a, b := <-hints, <-hints; a != "query" || b != "background" {
		t.Fatalf("priority hints: %q %q", a, b)
	}
}

func TestCallerCancellationDoesNotTripServiceBreaker(t *testing.T) {
	e := setup(t)
	e.build()
	upstream, err := url.Parse(e.f.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var blocked atomic.Bool
	var blockedCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blocked.Load() && r.URL.Path == "/v1/embeddings" {
			blockedCalls.Add(1)
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	svc := embedding.New(e.db, embedding.Config{URL: srv.URL + "/v1/embeddings", Model: "fake", QueryTimeout: time.Second})
	svc.Start(context.Background())
	blocked.Store(true)
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		hits, err := svc.Similar(ctx, "登录很慢", nil, 5)
		cancel()
		if err != nil || len(hits) != 0 {
			t.Fatalf("cancelled request: %+v %v", hits, err)
		}
	}
	if blockedCalls.Load() != 3 {
		t.Fatalf("cancelled requests did not reach backend: %d", blockedCalls.Load())
	}
	st := svc.Status(context.Background())
	if st.BreakerUntil != nil || st.LastError != "" {
		t.Fatalf("caller cancellation marked server down: %+v", st)
	}
	blocked.Store(false)
	if hits, err := svc.Similar(context.Background(), "登录很慢", nil, 5); err != nil || len(hits) == 0 {
		t.Fatalf("healthy service not immediately usable: %+v %v", hits, err)
	}
}
