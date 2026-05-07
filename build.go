///usr/bin/env go run "$0" "$@"; exit "$?"

//go:build ignore

// Build script: runs vet + tests, then builds the binary with version, commit,
// and build date embedded via -ldflags.
//
// Usage:
//
//	go run build.go               # vet → test → build ./weather-proxy
//	./build.go ./out/binary       # same, via the shell shebang trick
//
// The first line is both a Go comment and a shell command: shells collapse the
// leading // to / and exec `go run`, while `go run` ignores it as a comment.
//
// Env overrides:
//
//	VERSION, COMMIT, DATE  override git-derived metadata (useful in CI).
//	TARGETS                comma-separated GOOS/GOARCH list (e.g.
//	                       "linux/amd64,linux/arm64"). When set, the script
//	                       cross-builds to <output>-<goos>-<goarch> for each
//	                       target instead of building a single native binary.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type target struct{ goos, goarch string }

type buildOutput struct {
	Path   string `json:"path"`
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

func main() {
	output := "weather-proxy"
	if len(os.Args) > 1 {
		output = os.Args[1]
	}

	version := firstNonEmpty(os.Getenv("VERSION"), gitOutput("describe", "--tags", "--always", "--dirty"), "dev")
	commit := firstNonEmpty(os.Getenv("COMMIT"), gitOutput("rev-parse", "--short", "HEAD"), "none")
	date := firstNonEmpty(os.Getenv("DATE"), time.Now().UTC().Format(time.RFC3339))

	targets, crossBuild := parseTargets(os.Getenv("TARGETS"))

	must("vet", group("vet", func() error {
		return runCmd(nil, "go", "vet", "./...")
	}))
	must("test", group("test", func() error {
		return runCmd(nil, "go", "test", "-count=1", "./...")
	}))

	ldflags := fmt.Sprintf("-s -w -X main.version=%s -X main.commit=%s -X main.date=%s",
		version, commit, date)

	var outputs []buildOutput
	must("build", group("build", func() error {
		for _, t := range targets {
			path := output
			if crossBuild {
				path = fmt.Sprintf("%s-%s-%s", output, t.goos, t.goarch)
			}
			log.Printf("building %s (%s/%s) version=%s commit=%s date=%s",
				path, t.goos, t.goarch, version, commit, date)
			env := []string{"CGO_ENABLED=0", "GOOS=" + t.goos, "GOARCH=" + t.goarch}
			if err := runCmd(env, "go", "build", "-trimpath", "-ldflags", ldflags, "-o", path, "."); err != nil {
				return err
			}
			outputs = append(outputs, buildOutput{Path: path, GOOS: t.goos, GOARCH: t.goarch})
		}
		return nil
	}))

	if err := emitGitHubOutput(version, commit, date, outputs); err != nil {
		log.Fatalf("failed to write GITHUB_OUTPUT: %v", err)
	}
}

func parseTargets(spec string) ([]target, bool) {
	if strings.TrimSpace(spec) == "" {
		return []target{{runtime.GOOS, runtime.GOARCH}}, false
	}
	var ts []target
	for _, part := range strings.Split(spec, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		bits := strings.SplitN(p, "/", 2)
		if len(bits) != 2 || bits[0] == "" || bits[1] == "" {
			log.Fatalf("invalid TARGETS entry %q (want goos/goarch)", p)
		}
		ts = append(ts, target{bits[0], bits[1]})
	}
	if len(ts) == 0 {
		log.Fatalf("TARGETS is set but parsed empty")
	}
	return ts, true
}

func runCmd(extraEnv []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	return cmd.Run()
}

// group wraps fn in GitHub Actions ::group::/::endgroup:: markers when running
// under Actions, so each phase collapses into its own folded section in the log.
func group(name string, fn func() error) error {
	if inGitHubActions() {
		fmt.Printf("::group::%s\n", name)
	}
	err := fn()
	if inGitHubActions() {
		fmt.Println("::endgroup::")
	}
	return err
}

func must(phase string, err error) {
	if err != nil {
		log.Fatalf("%s failed: %v", phase, err)
	}
}

func inGitHubActions() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true"
}

// emitGitHubOutput appends a `build-meta=<json>` step output when running under
// GitHub Actions, so downstream steps can read it via `${{ steps.<id>.outputs.build-meta }}`.
func emitGitHubOutput(version, commit, date string, outputs []buildOutput) error {
	if !inGitHubActions() {
		return nil
	}
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	meta, err := json.Marshal(struct {
		Version string        `json:"version"`
		Commit  string        `json:"commit"`
		Date    string        `json:"date"`
		Outputs []buildOutput `json:"outputs"`
	}{version, commit, date, outputs})
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
