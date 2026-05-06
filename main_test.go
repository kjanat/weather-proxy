package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
	assertResponse(t, first, http.StatusOK, "10C\n", "HIT")
	if got := first.Header().Get("Cache-Control"); got != "public, max-age=900" {
		t.Fatalf("unexpected cache-control: %q", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, second, http.StatusOK, "10C\n", "HIT")
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
	assertResponse(t, first, http.StatusOK, "10C\n", "HIT")

	time.Sleep(time.Millisecond)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/", nil))
	assertResponse(t, second, http.StatusOK, "10C\n", "STALE")
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
