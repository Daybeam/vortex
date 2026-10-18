package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// resetAssetGate clears the sync.Once so AllowExternalAssets can be
// re-evaluated with a new env var between tests.
func resetAssetGate() {
	assetGateOnce = sync.Once{}
	assetGateFlag = false
}

func TestAllowExternalAssets_DefaultTrue(t *testing.T) {
	resetAssetGate()
	os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	if !AllowExternalAssets() {
		t.Fatal("default should be true (backward compat)")
	}
}

func TestAllowExternalAssets_False(t *testing.T) {
	resetAssetGate()
	os.Setenv("VORTEX_ALLOW_EXTERNAL_ASSETS", "false")
	defer os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	if AllowExternalAssets() {
		t.Fatal("VORTEX_ALLOW_EXTERNAL_ASSETS=false should disable external assets")
	}
}

func TestAllowExternalAssets_True(t *testing.T) {
	resetAssetGate()
	os.Setenv("VORTEX_ALLOW_EXTERNAL_ASSETS", "true")
	defer os.Unsetenv("VORTEX_ALLOW_EXTERNAL_ASSETS")
	if !AllowExternalAssets() {
		t.Fatal("VORTEX_ALLOW_EXTERNAL_ASSETS=true should enable external assets")
	}
}

func TestVerifyAssetManifest_NoManifest_NoError(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte("{}"), 0644)
	// No manifest.lock — should return nil (legacy mode)
	if err := VerifyAssetManifest(configPath); err != nil {
		t.Fatalf("expected nil for missing manifest, got: %v", err)
	}
}

func TestVerifyAssetManifest_ValidPasses(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	// Create an asset file and compute its hash
	assetPath := filepath.Join(dir, "workspace/roles/test.json")
	os.MkdirAll(filepath.Dir(assetPath), 0755)
	assetContent := []byte(`{"id":"test"}`)
	os.WriteFile(assetPath, assetContent, 0644)
	hash := sha256.Sum256(assetContent)
	hashHex := hex.EncodeToString(hash[:])

	// Create manifest.lock
	manifest := AssetManifest{
		"workspace/roles/test.json": hashHex,
	}
	manifestData, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.lock"), manifestData, 0644)

	if err := VerifyAssetManifest(configPath); err != nil {
		t.Fatalf("expected nil for valid asset, got: %v", err)
	}
}

func TestVerifyAssetManifest_TamperedFails(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	// Create an asset file
	assetPath := filepath.Join(dir, "workspace/roles/test.json")
	os.MkdirAll(filepath.Dir(assetPath), 0755)
	os.WriteFile(assetPath, []byte(`{"id":"test"}`), 0644)

	// Create manifest.lock with WRONG hash
	manifest := AssetManifest{
		"workspace/roles/test.json": "abc123invalidhash",
	}
	manifestData, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.lock"), manifestData, 0644)

	err := VerifyAssetManifest(configPath)
	if err == nil {
		t.Fatal("expected error for tampered asset, got nil")
	}
}

func TestVerifyAssetManifest_EmptyHashSkips(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	os.WriteFile(configPath, []byte("{}"), 0644)

	// manifest with empty hash — VerifyScriptIntegrity skips (backward compat)
	manifest := AssetManifest{
		"workspace/roles/nonexistent.json": "",
	}
	manifestData, _ := json.Marshal(manifest)
	os.WriteFile(filepath.Join(dir, "manifest.lock"), manifestData, 0644)

	if err := VerifyAssetManifest(configPath); err != nil {
		t.Fatalf("expected nil for empty hash (skip), got: %v", err)
	}
}
