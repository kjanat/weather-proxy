package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRootFetchesAndCachesWeather(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/Amsterdam" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if r.URL.RawQuery != "m&format=%t" {
			t.Fatalf("unexpected upstream query: %s", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 15*time.Minute)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, first, http.StatusOK, "10C", "MISS")
	if got := first.Header().Get("Cache-Control"); got != "public, max-age=900" {
		t.Fatalf("unexpected cache-control: %q", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, second, http.StatusOK, "10C", "HIT")
	if requests != 1 {
		t.Fatalf("expected one upstream request, got %d", requests)
	}
}

func TestRootReturnsStaleWeatherWhenRefreshFails(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = fmt.Fprint(w, "+10°C")
			return
		}
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 1*time.Nanosecond)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, first, http.StatusOK, "10C", "MISS")

	time.Sleep(time.Millisecond)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, second, http.StatusOK, "10C", "STALE")
}

func TestRootFailsWhenUpstreamFailsWithoutCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream down", http.StatusBadGateway)
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 15*time.Minute)

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d, got %d", http.StatusBadGateway, res.Code)
	}
}

func TestHealthzAndUnknownPath(t *testing.T) {
	handler := newHandler(&cache{}, http.DefaultClient, "http://example.invalid", "Amsterdam", 15*time.Minute)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("expected health status %d, got %d", http.StatusNoContent, health.Code)
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/weather", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("expected unknown path status %d, got %d", http.StatusNotFound, unknown.Code)
	}
}

func TestFavicon(t *testing.T) {
	handler := newHandler(&cache{}, http.DefaultClient, "http://example.invalid", "Amsterdam", 15*time.Minute)

	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("expected favicon status %d, got %d", http.StatusOK, res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "image/x-icon" {
		t.Fatalf("expected favicon content type %q, got %q", "image/x-icon", got)
	}
	if got := res.Body.Bytes(); len(got) == 0 {
		t.Fatal("expected favicon body")
	}
}

func TestConcurrentRequestsDeduplicateFetches(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 15*time.Minute)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := httptest.NewRecorder()
			handler.ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
			if res.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", res.Code)
			}
		}()
	}
	wg.Wait()

	if got := requests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestSingleflightDeduplicatesExpiredFetches(t *testing.T) {
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		time.Sleep(30 * time.Millisecond)
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	t.Cleanup(upstream.Close)

	c := &cache{}
	client := upstream.Client()
	ttl := 15 * time.Minute
	ctx := context.Background()

	// First call: cold cache → MISS, 1 upstream request
	value, status, err := c.get(ctx, client, upstream.URL, "Amsterdam", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if status != cacheMiss || value != "10C" {
		t.Fatalf("expected MISS/10C, got %s/%s", status, value)
	}
	if requests.Load() != 1 {
		t.Fatalf("expected 1 request, got %d", requests.Load())
	}

	// Cache is fresh — second call should be HIT, no fetch
	_, status, err = c.get(ctx, client, upstream.URL, "Amsterdam", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if status != cacheHit {
		t.Fatalf("expected HIT, got %s", status)
	}
	if requests.Load() != 1 {
		t.Fatalf("expected still 1 request, got %d", requests.Load())
	}

	// Expire cache, then launch concurrent requests. Singleflight should
	// coalesce them into 1 fetch.
	c.mu.Lock()
	c.fetchedAt = time.Time{}
	c.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, status, err := c.get(ctx, client, upstream.URL, "Amsterdam", ttl)
			if err != nil {
				errs <- err
				return
			}
			if value != "10C" {
				errs <- fmt.Errorf("expected value 10C, got %q", value)
				return
			}
			if status != cacheMiss && status != cacheHit {
				errs <- fmt.Errorf("expected MISS or HIT, got %s", status)
				return
			}
			errs <- nil
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	// All 10 should have shared 1 singleflight, so total = 2 (initial + this batch)
	if got := requests.Load(); got != 2 {
		t.Fatalf("expected 2 total upstream requests, got %d", got)
	}
}

func TestCancelledCallerDoesNotPoisonSharedFetch(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	var requests atomic.Int32
	var enterOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		enterOnce.Do(func() { close(entered) })
		<-gate
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 15*time.Minute)

	// Request 1: will be cancelled
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx1)
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		// After cancel, this may return error — that's fine for caller 1
	}()
	<-entered
	cancel1()

	// Request 2: patient caller
	var res2 *httptest.ResponseRecorder
	started2 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		res2 = httptest.NewRecorder()
		close(started2)
		handler.ServeHTTP(res2, req)
	}()
	<-started2

	// Release upstream — the fetch should still complete for caller 2
	close(gate)
	<-done1
	<-done2

	// Caller 2 should have gotten a valid response
	if res2.Code != http.StatusOK {
		t.Fatalf("expected caller 2 to get 200, got %d (body: %s)", res2.Code, res2.Body.String())
	}
	if got := res2.Header().Get("X-Weather-Cache"); got != "MISS" && got != "HIT" {
		t.Fatalf("expected X-Weather-Cache MISS or HIT, got %q", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected 1 upstream request, got %d", got)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	t.Cleanup(upstream.Close)

	handler := newHandler(&cache{}, upstream.Client(), upstream.URL, "Amsterdam", 15*time.Minute)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, path := range []string{"/", "/healthz", "/favicon.ico"} {
			t.Run(method+" "+path, func(t *testing.T) {
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, httptest.NewRequest(method, path, nil))
				if res.Code != http.StatusMethodNotAllowed {
					t.Fatalf("expected 405 for %s %s, got %d", method, path, res.Code)
				}
				if got := res.Header().Get("Allow"); got != "GET, HEAD" {
					t.Fatalf("expected Allow: GET, HEAD, got %q", got)
				}
			})
		}
	}
}

func TestMustURLRejectsNonHTTPSchemes(t *testing.T) {
	// mustURL uses log.Fatalf which calls os.Exit — not recoverable.
	// Instead, test the validation logic directly.
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"https://example.com", true},
		{"https://example.com/base", true},
		{"http://example.com", true},
		{"https://example.com?token=x", false},
		{"https://example.com#weather", false},
		{"ftp://example.com", false},
		{"gopher://hole.example.com", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			valid := isValidUpstreamURL(tc.raw)
			if valid != tc.valid {
				t.Fatalf("isValidUpstreamURL(%q) = %v, want %v", tc.raw, valid, tc.valid)
			}
		})
	}
}

func assertResponse(t *testing.T, res *httptest.ResponseRecorder, status int, body, cacheHeader string) {
	t.Helper()
	if res.Code != status {
		t.Fatalf("expected status %d, got %d", status, res.Code)
	}
	if got := res.Body.String(); got != body {
		t.Fatalf("expected body %q, got %q", body, got)
	}
	if got := res.Header().Get("X-Weather-Cache"); got != cacheHeader {
		t.Fatalf("expected X-Weather-Cache %q, got %q", cacheHeader, got)
	}
}
