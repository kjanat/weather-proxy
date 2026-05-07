///usr/bin/env go run "$0" "$@"; exit "$?"

//go:build ignore

// Build script that embeds version, commit, and build date into the binary.
//
// Usage:
//
//	go run build.go               # builds ./weather-proxy
//	./build.go ./out/binary       # same, via the shell shebang trick
//
// The first line is both a Go comment and a shell command: shells collapse the
// leading // to / and exec `go run`, while `go run` ignores it as a comment.
//
// Overrides: VERSION, COMMIT, DATE env vars take precedence over git lookups,
// which is useful for reproducible/CI builds where git context may be missing.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	output := "weather-proxy"
	if len(os.Args) > 1 {
		output = os.Args[1]
	}

	version := firstNonEmpty(os.Getenv("VERSION"), gitOutput("describe", "--tags", "--always", "--dirty"), "dev")
	commit := firstNonEmpty(os.Getenv("COMMIT"), gitOutput("rev-parse", "--short", "HEAD"), "none")
	date := firstNonEmpty(os.Getenv("DATE"), time.Now().UTC().Format(time.RFC3339))

	ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
		version, commit, date)

	log.Printf("building %s version=%s commit=%s date=%s", output, version, commit, date)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", output, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if err := cmd.Run(); err != nil {
		log.Fatalf("build failed: %v", err)
	}

	if err := emitGitHubOutput(output, version, commit, date); err != nil {
		log.Fatalf("failed to write GITHUB_OUTPUT: %v", err)
	}
}

// emitGitHubOutput appends a `build-meta=<json>` step output when running under
// GitHub Actions, so downstream steps can read it via `${{ steps.<id>.outputs.build-meta }}`.
func emitGitHubOutput(output, version, commit, date string) error {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return nil
	}
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	meta, err := json.Marshal(map[string]string{
		"output":  output,
		"version": version,
		"commit":  commit,
		"date":    date,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "build-meta=%s\n", meta)
	return err
}

func gitOutput(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
