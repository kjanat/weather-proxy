package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type cache struct {
	mu        sync.Mutex
	value     string
	fetchedAt time.Time
}

func main() {
	addr := env("LISTEN_ADDR", ":8080")
	location := env("WEATHER_LOCATION", "Amsterdam")
	ttl := envDuration("WEATHER_TTL", 15*time.Minute)
	upstream := env("WEATHER_UPSTREAM", "https://wttr.in")

	handler := newHandler(&cache{}, &http.Client{Timeout: 5 * time.Second}, upstream, location, ttl)

	log.Printf("weather-proxy listening on %s location=%s ttl=%s", addr, location, ttl)
	log.Fatal(http.ListenAndServe(addr, handler))
}

func newHandler(c *cache, client *http.Client, upstream, location string, ttl time.Duration) http.Handler {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		value, stale, err := c.get(r.Context(), client, upstream, location, ttl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())))
		if stale {
			w.Header().Set("X-Weather-Cache", "STALE")
		} else {
			w.Header().Set("X-Weather-Cache", "HIT")
		}
		_, _ = fmt.Fprintln(w, value)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func (c *cache) get(ctx context.Context, client *http.Client, upstream, location string, ttl time.Duration) (string, bool, error) {
	c.mu.Lock()
	if c.value != "" && time.Since(c.fetchedAt) < ttl {
		value := c.value
		c.mu.Unlock()
		return value, false, nil
	}
	c.mu.Unlock()

	value, err := fetch(ctx, client, upstream, location)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.value != "" {
			return c.value, true, nil
		}
		return "", false, err
	}
	c.value = value
	c.fetchedAt = time.Now()
	return c.value, false, nil
}

func fetch(ctx context.Context, client *http.Client, upstream, location string) (string, error) {
	endpoint := strings.TrimRight(upstream, "/") + "/" + url.PathEscape(location) + "?m&format=%t"
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

	body, err := io.ReadAll(io.LimitReader(res.Body, 128))
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
	if err != nil {
		log.Printf("invalid %s=%q, using %s", key, value, fallback)
		return fallback
	}
	return duration
}
