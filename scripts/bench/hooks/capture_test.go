package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type cancelAfterHeaders struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w cancelAfterHeaders) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	w.cancel() // A fast client consumed the successful response and closed it.
}

func TestCaptureDoesNotCountNormalClientClosureAsTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &captureWriter{ResponseWriter: cancelAfterHeaders{httptest.NewRecorder(), cancel}, ctx: ctx}
	rec.WriteHeader(http.StatusOK)
	if _, err := rec.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil || rec.requestErr != nil {
		t.Fatalf("normal response closure counted as timeout: %v", rec.requestErr)
	}
}

func TestCaptureRetainsCancellationBeforeResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &captureWriter{ResponseWriter: httptest.NewRecorder(), ctx: ctx}
	if _, err := rec.Write([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(rec.requestErr, context.Canceled) || rec.status != http.StatusOK {
		t.Fatalf("lost pre-response cancellation: %+v", rec)
	}
}
