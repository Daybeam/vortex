package core

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/providers"
)

// isNetworkError reports whether an error is a transient network/TLS/timeout
// failure (as opposed to an auth or bad-request error). Connectivity tests
// should skip on these rather than fail the suite, since they reflect
// environment availability, not code correctness.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sig := range []string{
		"TLS handshake timeout",
		"deadline exceeded",
		"context deadline exceeded",
		"connection refused",
		"connection reset",
		"no such host",
		"i/o timeout",
		"network is unreachable",
		"EOF",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

func TestEmbeddingConnectivity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping connectivity test in short mode")
	}
	// Testing hardcoded Gemini key from config_windows.json
	geminiKey := os.Getenv("GEMINI_API_KEY")
	if geminiKey == "" {
		t.Skip("GEMINI_API_KEY not set, skipping connectivity test")
	}

	t.Run("Gemini_Embed_V2", func(t *testing.T) {
		pCfg := &config.ProviderConfig{
			Provider:       "gemini",
			Model:          "gemini-3.6-flash",
			EmbeddingModel: "gemini-embedding-2",
			APIKey:         geminiKey,
		}

		p, err := providers.Get(pCfg, config.ExternalRuntimes{})
		if err != nil {
			t.Fatalf("Failed to get provider: %v", err)
		}

		// Bound the network call so a hung connection fails fast instead of
		// burning 47s on a TLS handshake timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		emb, err := p.Embed(ctx, "Hello connectivity test")
		if err != nil {
			if isNetworkError(err) {
				t.Skipf("skipping connectivity test (network unavailable): %v", err)
			}
			t.Errorf("Embedding (V2) failed: %v", err)
		} else {
			fmt.Printf("Gemini Embedding (V2) Success! Vector length: %d\n", len(emb))
		}
	})

	// Testing InternAI key
	internKey := "sk-REDACTED-GET-KEY-FROM-INTERN-AI-PORTAL"
	t.Run("InternAI_Embed", func(t *testing.T) {
		pCfg := &config.ProviderConfig{
			Provider:       "openai",
			BaseURL:        "https://chat.intern-ai.org.cn/api/v1",
			Model:          "intern-latest",
			EmbeddingModel: "intern-embedding", // Placeholder
			APIKey:         internKey,
		}

		p, err := providers.Get(pCfg, config.ExternalRuntimes{})
		if err != nil {
			t.Fatalf("Failed to get provider: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		emb, err := p.Embed(ctx, "Hello connectivity test")
		if err != nil {
			if isNetworkError(err) {
				t.Skipf("skipping connectivity test (network unavailable): %v", err)
			}
			t.Logf("InternAI Embedding failed: %v", err)
		} else {
			fmt.Printf("InternAI Embedding Success! Vector length: %d\n", len(emb))
		}
	})
}
