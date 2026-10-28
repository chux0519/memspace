package releasecheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	tag := "v0.3.0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/chux0519/memspace/releases/tag/"+tag, http.StatusFound)
	}))
	defer srv.Close()
	c := New(nil)
	c.URL = srv.URL
	if c.Latest() != "" {
		t.Fatal("latest before a check")
	}
	if err := c.Check(context.Background()); err != nil || c.Latest() != "v0.3.0" {
		t.Fatalf("latest = %q, err %v", c.Latest(), err)
	}
	tag = "nightly"
	if err := c.Check(context.Background()); err == nil || c.Latest() != "v0.3.0" {
		t.Errorf("bad tag accepted: %q, %v", c.Latest(), err)
	}
	if (*Checker)(nil).Latest() != "" {
		t.Error("nil checker")
	}
}
