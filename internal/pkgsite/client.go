// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pkgsite

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
	"golang.org/x/vulndb/internal/stdlib"
	"golang.org/x/vulndb/internal/worker/log"
)

type Client struct {
	url   string
	cache *cache
}

func Default() *Client {
	return New(URL)
}

func New(url string) *Client {
	return &Client{
		url:   url,
		cache: newCache(),
	}
}

func (pc *Client) SetKnownModules(known []string) {
	pc.cache.setKnownModules(known)
}

// Limit pkgsite requests to this many per second.
const pkgsiteQPS = 20

var (
	// The limiter used to throttle pkgsite requests.
	// The second argument to rate.NewLimiter is the burst, which
	// basically lets you exceed the rate briefly.
	pkgsiteRateLimiter = rate.NewLimiter(rate.Every(1*time.Second/pkgsiteQPS), 3)
)

var URL = "https://pkg.go.dev"

// KnownModule reports whether pkgsite knows that path actually refers
// to a module or package path.
func (pc *Client) KnownModule(ctx context.Context, path string) (bool, error) {
	return pc.lookupEndpoint(ctx, moduleEndpoint(path))
}

// KnownAtVersion reports whether pkgsite knows that the path exists at the given
// bare version.
func (pc *Client) KnownAtVersion(ctx context.Context, path, version string) (bool, error) {
	prefix := "v"
	if stdlib.Contains(path) {
		prefix = "go"
	}
	return pc.lookupEndpoint(ctx, "/"+path+"@"+prefix+version)
}

func (pc *Client) lookupEndpoint(ctx context.Context, endpoint string) (bool, error) {
	found, ok := pc.cache.lookup(endpoint)
	if ok {
		return found, nil
	}

	// Pause to maintain a max QPS.
	if err := pkgsiteRateLimiter.Wait(ctx); err != nil {
		return false, err
	}

	start := time.Now()
	res, err := http.Head(pc.url + endpoint)
	var status string
	if err == nil {
		status = strconv.Quote(res.Status)
	}
	log.With(
		"latency", time.Since(start),
		"status", status,
		"error", err,
	).Debugf(ctx, "checked if %s is known to pkgsite", endpoint)
	if err != nil {
		return false, err
	}

	known := res.StatusCode == http.StatusOK
	pc.cache.add(endpoint, known)
	return known, nil
}

func (pc *Client) URL() string {
	return pc.url
}

func readKnown(r io.Reader) (map[string]bool, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("no data")
	}
	seen := make(map[string]bool)
	if err := json.Unmarshal(b, &seen); err != nil {
		return nil, err
	}
	return seen, nil
}

func (c *cache) writeKnown(w io.Writer) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := json.MarshalIndent(c.seen, "", "   ")
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// cacheFile returns a default cache file that can be used as an input
// to testClient.
//
// For testing.
func cacheFile(t *testing.T) (*os.File, error) {
	filename := filepath.Join("testdata", "pkgsite", t.Name()+".json")
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return nil, err
	}

	// If the file doesn't exist, or is empty, add an empty map.
	fi, err := os.Stat(filename)
	if err != nil || fi.Size() == 0 {
		if err := os.WriteFile(filename, []byte("{}\n"), 0644); err != nil {
			return nil, err
		}
	}

	f, err := os.OpenFile(filename, os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})

	return f, nil
}

// TestClient returns a pkgsite client that talks to either
// a fake server or the real pkg.go.dev, depending on the useRealPkgsite value.
//
// For testing.
func TestClient(t *testing.T, useRealPkgsite bool) (*Client, error) {
	cf, err := cacheFile(t)
	if err != nil {
		return nil, err
	}
	return testClient(t, useRealPkgsite, cf)
}

func testClient(t *testing.T, useRealPkgsite bool, rw io.ReadWriter) (*Client, error) {
	if useRealPkgsite {
		c := Default()
		t.Cleanup(func() {
			err := c.cache.writeKnown(rw)
			if err != nil {
				t.Error(err)
			}
		})
		return c, nil
	}
	known, err := readKnown(rw)
	if err != nil {
		return nil, fmt.Errorf("could not read known modules: %w", err)
	}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !known[r.URL.Path] {
			http.Error(w, "unknown", http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return New(s.URL), nil
}

type cache struct {
	mu sync.Mutex
	// Endpoints already seen.
	seen map[string]bool
	// Does the cache contain all known endpoints
	complete bool
}

func newCache() *cache {
	return &cache{
		seen:     make(map[string]bool),
		complete: false,
	}
}

func (c *cache) setKnownModules(known []string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, km := range known {
		c.seen[moduleEndpoint(km)] = true
	}
	c.complete = true
}

func moduleEndpoint(path string) string {
	return "/mod/" + path
}

func (c *cache) lookup(endpoint string) (known bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// In the cache.
	if known, ok := c.seen[endpoint]; ok {
		return known, true
	}

	// Not in the cache, but the cache is complete, so this
	// endpoint is not known.
	if c.complete {
		return false, true
	}

	// We can't make a statement about this endpoint.
	return false, false
}

func (c *cache) add(endpoint string, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seen[endpoint] = known
}
