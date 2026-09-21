//go:build test
// +build test

package config

import (
	"os"
	"testing"
)

func TestProviderConfig_ResolveAPIKey(t *testing.T) {
	pc := &ProviderConfig{
		APIKeyEnv: "TEST_SECRET_KEY",
	}
	mock := &EnvSecretProvider{}
	os.Setenv("TEST_SECRET_KEY", "secret_val")
	defer os.Unsetenv("TEST_SECRET_KEY")

	val := pc.ResolveAPIKey(mock)
	if val != "secret_val" {
		t.Errorf("expected secret_val, got %s", val)
	}
}
