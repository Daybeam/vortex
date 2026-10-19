package config

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/daybeam/vortex/pkg/pluginverify"
)

// AssetGate controls whether external asset files (roles, skills, SOPs,
// prompt overrides) are loaded from disk or restricted to compiled-in
// defaults. This implements the Immutable Asset Pipeline design's
// read-write path separation + startup integrity gate.
//
// Three modes:
//  1. Tamper-proof (VORTEX_ALLOW_EXTERNAL_ASSETS=false): skip disk loading,
//     use compiled-in defaults only. Zero external file dependencies.
//  2. Verified (true + manifest.lock present): load external files but
//     verify each against SHA-256 hashes in manifest.lock. Tampered files
//     are rejected at startup.
//  3. Legacy (true + no manifest.lock): load external files without
//     verification (backward-compatible default).
//
// See docs/plan/IMMUTABLE_ASSET_PIPELINE_DESIGN.md.

var (
	assetGateOnce sync.Once
	assetGateFlag bool
)

// AllowExternalAssets reports whether external asset files may be loaded
// from disk. Reads VORTEX_ALLOW_EXTERNAL_ASSETS once (cached). Default is
// "true" for backward compatibility; set to "false" for tamper-proof mode.
func AllowExternalAssets() bool {
	assetGateOnce.Do(func() {
		v := os.Getenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
		assetGateFlag = v != "false" // default true
	})
	return assetGateFlag
}

// AssetManifest is the JSON shape of manifest.lock: a map from
// asset file path (relative to config dir) to expected SHA-256 hex.
type AssetManifest map[string]string

// VerifyAssetManifest reads manifest.lock from the config directory and
// verifies each listed file's SHA-256 via pluginverify.VerifyScriptIntegrity.
// If manifest.lock is absent, returns nil (legacy mode — no verification).
// If manifest.lock is present but any file fails verification, returns an
// error listing all failures.
func VerifyAssetManifest(configPath string) error {
	manifestPath := filepath.Join(ConfigDir(configPath), "manifest.lock")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no manifest = legacy mode, no verification
		}
		return fmt.Errorf("asset_gate: cannot read manifest.lock: %w", err)
	}
	var manifest AssetManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("asset_gate: malformed manifest.lock: %w", err)
	}
	baseDir := ConfigDir(configPath)
	var failures []string
	for relPath, expectedHash := range manifest {
		absPath := filepath.Join(baseDir, relPath)
		if err := pluginverify.VerifyScriptIntegrity(absPath, expectedHash); err != nil {
			failures = append(failures, fmt.Sprintf("  %s: %v", relPath, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("asset_gate: %d asset(s) failed integrity check:\n%s",
			len(failures), joinStrings(failures, "\n"))
	}
	log.Printf("asset_gate: verified %d asset(s) against manifest.lock", len(manifest))
	return nil
}

func joinStrings(items []string, sep string) string {
	result := ""
	for i, s := range items {
		if i > 0 {
			result += sep
		}
		result += s
	}
	return result
}
