//go:build integration

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDockerImageBuildsAndServesRequests(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if output, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not available: %v\n%s", err, output)
	}

	upstreamURL, requests := startHostUpstream(t)
	image := fmt.Sprintf("weather-proxy:integration-%d", time.Now().UnixNano())
	container := fmt.Sprintf("weather-proxy-integration-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", container).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "image", "rm", "-f", image).Run()
	})

	if output, err := exec.CommandContext(ctx, "docker", "build", "-t", image, ".").CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, output)
	}

	runArgs := []string{
		"run",
		"--detach",
		"--name", container,
		"--add-host", "host.docker.internal:host-gateway",
		"--publish", "127.0.0.1::8080",
		"--env", "WEATHER_UPSTREAM=" + upstreamURL,
		image,
	}
	if output, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, output)
	}

	portOutput, err := exec.CommandContext(ctx, "docker", "port", container, "8080/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("docker port failed: %v\n%s", err, portOutput)
	}

	addr := strings.TrimSpace(string(portOutput))
	if addr == "" {
		t.Fatal("docker did not publish 8080/tcp")
	}

	baseURL := "http://" + addr
	waitForHealthz(t, ctx, baseURL)

	assertWeather(t, ctx, baseURL, http.MethodGet, http.StatusOK, "10C", "MISS")
	assertWeather(t, ctx, baseURL, http.MethodGet, http.StatusOK, "10C", "HIT")
	if got := requests.Load(); got != 1 {
		t.Fatalf("expected cached weather to use 1 upstream request, got %d", got)
	}

	assertWeather(t, ctx, baseURL, http.MethodHead, http.StatusOK, "", "HIT")
	assertStatus(t, ctx, baseURL+"/missing", http.MethodGet, http.StatusNotFound)
	assertMethodNotAllowed(t, ctx, baseURL+"/healthz")

	badContainer := container + "-bad-url"
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "-f", badContainer).Run()
	})
	assertInvalidUpstreamExits(t, ctx, image, badContainer)
}

func startHostUpstream(t *testing.T) (string, *atomic.Int32) {
	t.Helper()

	var requests atomic.Int32
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/Amsterdam" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		if r.URL.RawQuery != "m&format=%t" {
			t.Fatalf("unexpected upstream query: %s", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, "+10°C")
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return "http://host.docker.internal:" + port, &requests
}

func assertWeather(t *testing.T, ctx context.Context, baseURL, method string, status int, body, cacheHeader string) {
	t.Helper()

	res, gotBody := doRequest(t, ctx, method, baseURL+"/")
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != status {
		t.Fatalf("expected %s / status %d, got %d", method, status, res.StatusCode)
	}
	if gotBody != body {
		t.Fatalf("expected %s / body %q, got %q", method, body, gotBody)
	}
	if got := res.Header.Get("X-Weather-Cache"); got != cacheHeader {
		t.Fatalf("expected X-Weather-Cache %q, got %q", cacheHeader, got)
	}
}

func assertStatus(t *testing.T, ctx context.Context, url, method string, status int) {
	t.Helper()

	res, _ := doRequest(t, ctx, method, url)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != status {
		t.Fatalf("expected %s %s status %d, got %d", method, url, status, res.StatusCode)
	}
}

func assertMethodNotAllowed(t *testing.T, ctx context.Context, url string) {
	t.Helper()

	res, _ := doRequest(t, ctx, http.MethodPost, url)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected POST %s status 405, got %d", url, res.StatusCode)
	}
	if got := res.Header.Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("expected Allow: GET, HEAD, got %q", got)
	}
}

func doRequest(t *testing.T, ctx context.Context, method, url string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		_ = res.Body.Close()
		t.Fatal(err)
	}
	return res, string(body)
}

func assertInvalidUpstreamExits(t *testing.T, ctx context.Context, image, container string) {
	t.Helper()

	runArgs := []string{
		"run",
		"--detach",
		"--name", container,
		"--env", "WEATHER_UPSTREAM=ftp://example.com",
		image,
	}
	if output, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput(); err != nil {
		t.Fatalf("docker run with invalid upstream failed before container start: %v\n%s", err, output)
	}

	output, err := exec.CommandContext(ctx, "docker", "wait", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker wait failed: %v\n%s", err, output)
	}
	if code := strings.TrimSpace(string(output)); code == "0" {
		t.Fatal("expected invalid WEATHER_UPSTREAM container to exit non-zero")
	}
}

func waitForHealthz(t *testing.T, ctx context.Context, baseURL string) {
	t.Helper()

	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}

		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusNoContent {
				return
			}
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatalf("container did not become healthy at %s", baseURL)
}
