// Package releasecheck asks GitHub, about once a day, for the latest MemSpace
// release, so the web UI can tell the administrator about a newer server.
// It only reports; nothing is downloaded or installed.
package releasecheck

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/chux0519/memspace/internal/model"
)

// Interval between checks, and between retries after a failed one.
const (
	Interval   = 24 * time.Hour
	RetryAfter = time.Hour
)

// Checker holds the latest release seen.
type Checker struct {
	// URL redirects to the latest release's page (.../releases/tag/<tag>).
	// The web URL is used rather than the API, which limits requests
	// without a token per IP address (shared behind proxies and NAT).
	URL  string
	HTTP *http.Client
	Logf func(format string, a ...any)

	mu     sync.Mutex
	latest string
}

// New returns a Checker for the MemSpace repository.
func New(logf func(string, ...any)) *Checker {
	return &Checker{
		URL: "https://github.com/" + model.ReleaseRepo + "/releases/latest",
		HTTP: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		Logf: logf,
	}
}

// Latest is the latest release tag seen ("" before the first successful
// check). A nil Checker has none.
func (c *Checker) Latest() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run checks now and then every Interval until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	for {
		wait := Interval
		if err := c.Check(ctx); err != nil {
			wait = RetryAfter
			if c.Logf != nil {
				c.Logf("release check: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Check asks GitHub once.
func (c *Checker) Check(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode/100 != 3 || !strings.Contains(loc, "/releases/tag/") {
		return fmt.Errorf("%s: HTTP %d, no release redirect", c.URL, resp.StatusCode)
	}
	tag := path.Base(loc)
	if !model.IsRelease(tag) {
		return fmt.Errorf("unexpected tag %q", tag)
	}
	c.mu.Lock()
	c.latest = tag
	c.mu.Unlock()
	return nil
}
