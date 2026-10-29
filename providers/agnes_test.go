package providers

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestAgnesProvider(t *testing.T) {
	// audit T-1.1: gate behind env var instead of unconditional skip so the
	// test is no longer permanently dead. Set RUN_LIVE_TESTS=1 to run.
	//
	// MOCK-BOUNDARY (B1): This is the ONLY test that hits a real LLM API,
	// but it's gated behind RUN_LIVE_TESTS=1 which is never set in CI —
	// real provider integration is UNVERIFIED. Verification checkpoint:
	// set RUN_LIVE_TESTS=1 with a valid config.json in staging.
	// See docs/MOCK_BOUNDARY_TEST_SCENARIOS.md §B1.
	if os.Getenv("RUN_LIVE_TESTS") != "1" {
		t.Skip("Skipping live provider test; set RUN_LIVE_TESTS=1 to run (requires config.json with 'agnes' provider)")
	}
	// 1. Initialize Registry to load config.json
	// We assume the test is run from the project root or the config is at the relative path
	reg, err := config.NewRegistry("config.json")
	if err != nil {
		t.Fatalf("failed to load registry: %v", err)
	}

	// 2. Get the 'agnes' provider config
	pc, ok := reg.Providers["agnes"]
	if !ok {
		t.Fatalf("provider 'agnes' not found in config.json")
	}

	// 3. Get the provider instance
	p, err := Get(pc, reg.ExternalRuntimes)
	if err != nil {
		t.Fatalf("failed to get agnes provider: %v", err)
	}

	// 4. Prepare a simple request
	req := schemas.CompleteRequest{
		Model:     pc.Model,
		System:    "You are a helpful assistant.",
		User:      "Hello! Are you working? Please respond with 'AGNES_OK'.",
		MaxTokens: 100,
	}

	fmt.Println("Testing Agnes Provider with model:", pc.Model)
	fmt.Println("Base URL:", pc.BaseURL)

	// 5. Execute call
	resp, err := p.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Agnes API call failed: %v", err)
	}

	fmt.Printf("Response: %s\n", resp.Text)

	if resp.Text == "" {
		t.Error("received empty response from Agnes AI")
	}
}
