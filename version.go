package main

// Build-time metadata, populated via -ldflags by build.go and the Dockerfile.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)
