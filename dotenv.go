package main

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv parses a .env file and sets environment variables.
// It follows the [Minimalist] principle: no external dependencies.
// Shared by both main.go (!web) and main_web.go (web) builds.
//
// Existing env vars are NOT overwritten (if os.Getenv(key) == "" guard):
// process-level env vars take precedence over .env file values. This is the
// correct behavior at startup — a process started with GEMINI_API_KEY=foo
// should use foo, not the .env value.
func loadDotEnv(path string) {
	loadDotEnvWithMode(path, false)
}

// loadDotEnvForReload re-reads a .env file and overwrites existing env vars.
// Used by StartWatcher on .env file modification to pick up changed values.
// Unlike loadDotEnv, this unconditionally calls os.Setenv so that updated
// .env values replace stale cached values.
//
// See docs/plan/ENV_MIGRATION_HOT_RELOAD_DESIGN.md §改动 5.
func loadDotEnvForReload(path string) {
	loadDotEnvWithMode(path, true)
}

func loadDotEnvWithMode(path string, overwrite bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.ReplaceAll(scanner.Text(), "\r", ""))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Remove quotes
		val = strings.Trim(val, "\"'")
		if overwrite || os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
