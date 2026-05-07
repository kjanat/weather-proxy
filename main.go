package main

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const upstreamTimeout = 5 * time.Second

//go:embed favicon.ico
var faviconICO []byte

type cacheStatus string

const (
	cacheHit   cacheStatus = "HIT"
	cacheMiss  cacheStatus = "MISS"
	cacheStale cacheStatus = "STALE"
)

type cacheResult struct {
	value  string
	status cacheStatus
}

type cache struct {
	mu        sync.Mutex
	group     singleflight.Group
	value     string
	fetchedAt time.Time
}

func main() {
	addr := env("LISTEN_ADDR", ":8080")
	location := env("WEATHER_LOCATION", "Amsterdam")
	ttl := envDuration("WEATHER_TTL", 15*time.Minute)
	upstream := mustURL(env("WEATHER_UPSTREAM", "https://wttr.in"))

	handler := newHandler(&cache{}, &http.Client{Timeout: upstreamTimeout}, upstream, location, ttl)

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("weather-proxy %s (commit %s, built %s) listening on %s location=%s ttl=%s",
		version, commit, date, addr, location, ttl)
	log.Fatal(srv.ListenAndServe())
}

func newHandler(c *cache, client *http.Client, upstream, location string, ttl time.Duration) http.Handler {
	weatherHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		value, status, err := c.get(r.Context(), client, upstream, location, ttl)
		if err != nil {
			log.Printf("weather fetch failed: %v", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())))
		w.Header().Set("X-Weather-Cache", string(status))
		_, _ = fmt.Fprint(w, value)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", weatherHandler)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(faviconICO)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	return methodGuard(mux)
}

func methodGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (c *cache) get(ctx context.Context, client *http.Client, upstream, location string, ttl time.Duration) (string, cacheStatus, error) {
	c.mu.Lock()
	if c.value != "" && time.Since(c.fetchedAt) < ttl {
		value := c.value
		c.mu.Unlock()
		return value, cacheHit, nil
	}
	c.mu.Unlock()

	ch := c.group.DoChan("weather", func() (any, error) {
		// Double-check: another caller may have refreshed while we waited.
		c.mu.Lock()
		if c.value != "" && time.Since(c.fetchedAt) < ttl {
			value := c.value
			c.mu.Unlock()
			return cacheResult{value: value, status: cacheHit}, nil
		}
		c.mu.Unlock()

		fetchCtx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
		defer cancel()

		value, err := fetch(fetchCtx, client, upstream, location)

		c.mu.Lock()
		defer c.mu.Unlock()
		if err != nil {
			if c.value != "" {
				return cacheResult{value: c.value, status: cacheStale}, nil
			}
			return cacheResult{}, err
		}
		c.value = value
		c.fetchedAt = time.Now()
		return cacheResult{value: c.value, status: cacheMiss}, nil
	})

	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return "", "", result.Err
		}
		cr := result.Val.(cacheResult)
		return cr.value, cr.status, nil
	}
}

func fetch(ctx context.Context, client *http.Client, upstream, location string) (string, error) {
	endpoint := upstream + "/" + url.PathEscape(location) + "?m&format=%t"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}

	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := res.Body.Close(); err != nil {
			log.Printf("failed to close upstream response body: %v", err)
		}
	}()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", fmt.Errorf("upstream returned %s", res.Status)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 1024))
	if err != nil {
		return "", err
	}

	value := strings.TrimSpace(string(body))
	value = strings.NewReplacer("+", "", "°C", "C", "°F", "F", " ", "").Replace(value)
	if value == "" || strings.Contains(strings.ToLower(value), "unknown") {
		return "", fmt.Errorf("invalid upstream weather response")
	}
	return value, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		log.Printf("invalid %s=%q, using %s", key, value, fallback)
		return fallback
	}
	return duration
}

func isValidUpstreamURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func mustURL(raw string) string {
	if !isValidUpstreamURL(raw) {
		log.Fatalf("invalid WEATHER_UPSTREAM=%q", raw)
	}
	return strings.TrimRight(raw, "/")
}
