package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func TestAntiSlop_Enabled_Strict_Block(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  true,
				MinHits: 3,
				Phrases: nil, // use defaults
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"summary": "It's important to note that as an AI language model, " +
				"in conclusion, it is worth noting that furthermore, " +
				"at the end of the day, it bears mentioning that as previously mentioned",
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictBlock {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictBlock, hits)
	}
	if hits < 3 {
		t.Errorf("Hits = %d, want >= 3", hits)
	}
}

func TestAntiSlop_Enabled_NonStrict_Warn(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  false,
				MinHits: 3,
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"note": "It's important to note that furthermore it is worth noting that moreover",
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictWarn {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictWarn, hits)
	}
	if hits < 3 {
		t.Errorf("Hits = %d, want >= 3", hits)
	}
}

func TestAntiSlop_Clean_ResponseNoFiller(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  false,
				MinHits: 3,
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"answer": "The file contains 42 lines of Go code in package main.",
			"lines":  42,
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictClean {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictClean, hits)
	}
	if hits > 0 {
		t.Errorf("Hits = %d, want 0", hits)
	}
}

func TestAntiSlop_Disabled_PassThrough(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: false,
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})

	ctx := context.Background()
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}
	called := false
	next := func(_ context.Context, r *SpawnRequest) (*SpawnResult, error) {
		called = true
		return &SpawnResult{
			Output: schemas.SubagentOutput{
				Result: map[string]any{"summary": "It's important to note furthermore moreover"},
			},
		}, nil
	}

	isc := NewAntiSlopInterceptor(reg, logger)
	result, err := isc.Wrap(ctx, req, next)
	if err != nil {
		t.Fatalf("Wrap error: %v", err)
	}
	if !called {
		t.Fatal("next was not called")
	}
	if len(result.Output.Warnings) != 0 {
		t.Errorf("Expected 0 warnings (disabled), got %d", len(result.Output.Warnings))
	}
}

func TestAntiSlop_NestedMap_Detection(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  false,
				MinHits: 3,
				Phrases: []string{"it is important to note", "as an AI", "in conclusion"},
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"outer": map[string]any{
				"inner": map[string]any{
					"text": "it is important to note that as an AI in conclusion",
				},
			},
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictWarn {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictWarn, hits)
	}
	if hits != 3 {
		t.Errorf("Hits = %d, want 3", hits)
	}
}

func TestAntiSlop_UpperCase_CaseInsensitive(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				MinHits: 3,
				Phrases: []string{"in conclusion", "moreover", "nevertheless"},
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"text": "IN CONCLUSION, MOREOVER, NEVERTHELESS",
		},
	}

	_, hits := isc.ScanOutput(output)
	if hits != 3 {
		t.Errorf("Hits = %d, want 3 (case-insensitive)", hits)
	}
}

func TestAntiSlop_MinHits_Configurable(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				MinHits: 10,
				Phrases: []string{"it is important to note", "furthermore", "moreover", "nevertheless"},
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"text": "it is important to note that furthermore moreover nevertheless",
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictClean {
		t.Errorf("Verdict = %s, want %s (hits=%d, min_hits=10)", verdict, VerdictClean, hits)
	}
	if hits != 4 {
		t.Errorf("Hits = %d, want 4 (distinct phrases)", hits)
	}
}

func TestAntiSlop_DefaultMinHits(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				MinHits: 0, // zero means use default 3
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"text": "it's important to note that furthermore moreover at the end of the day it bears mentioning that it is worth noting",
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictWarn {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictWarn, hits)
	}
}

func TestAntiSlop_Interceptor_StrictBlock_ReturnsError(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  true,
				MinHits: 3,
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	ctx := context.Background()
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}

	next := func(_ context.Context, r *SpawnRequest) (*SpawnResult, error) {
		return &SpawnResult{
			Output: schemas.SubagentOutput{
				Result: map[string]any{
					"text": "It is worth noting that furthermore moreover nevertheless at the end of the day",
				},
			},
		}, nil
	}

	isc := NewAntiSlopInterceptor(reg, logger)
	_, err := isc.Wrap(ctx, req, next)
	if err == nil {
		t.Fatal("Expected error for strict block, got nil")
	}
	if !hasSubstring(err.Error(), "anti_slop_block") {
		t.Errorf("Error = %s, want contains 'anti_slop_block'", err.Error())
	}
}

func TestAntiSlop_Interceptor_Warn_AppendsWarning(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  false,
				MinHits: 3,
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	ctx := context.Background()
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}

	next := func(_ context.Context, r *SpawnRequest) (*SpawnResult, error) {
		return &SpawnResult{
			Output: schemas.SubagentOutput{
				Result: map[string]any{
					"text": "It's important to note that furthermore moreover",
				},
			},
		}, nil
	}

	isc := NewAntiSlopInterceptor(reg, logger)
	result, err := isc.Wrap(ctx, req, next)
	if err != nil {
		t.Fatalf("Wrap error: %v", err)
	}
	if len(result.Output.Warnings) != 1 {
		t.Fatalf("Warnings len = %d, want 1", len(result.Output.Warnings))
	}
	if !hasSubstring(result.Output.Warnings[0], "Anti-Slop") {
		t.Errorf("Warning = %s, want contains 'Anti-Slop'", result.Output.Warnings[0])
	}
}

func TestAntiSlop_ScanResultMapSliceOfMaps(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				MinHits: 3,
				Phrases: []string{"furthermore"},
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Result: map[string]any{
			"items": []any{
				map[string]any{"text": "furthermore one"},
				map[string]any{"text": "furthermore two"},
				map[string]any{"text": "furthermore three"},
			},
		},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictWarn {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictWarn, hits)
	}
	if hits != 3 {
		t.Errorf("Hits = %d, want 3", hits)
	}
}

func TestAntiSlop_ScanAssumptions(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				MinHits: 2,
				Phrases: []string{"it bears mentioning"},
			},
		},
	}
	logger, _ := NewLogger("anti_slop_test", &config.SystemSettings{})
	defer logger.Close()
	isc := NewAntiSlopInterceptor(reg, logger)

	output := schemas.SubagentOutput{
		Assumptions: []string{"it bears mentioning that one", "it bears mentioning that two"},
	}

	verdict, hits := isc.ScanOutput(output)
	if verdict != VerdictWarn {
		t.Errorf("Verdict = %s, want %s (hits=%d)", verdict, VerdictWarn, hits)
	}
	if hits != 2 {
		t.Errorf("Hits = %d, want 2", hits)
	}
}

func TestAntiSlop_FakeProvider_EndToEnd(t *testing.T) {
	reg := &config.Registry{
		System: config.SystemSettings{
			AntiSlop: config.AntiSlopConfig{
				Enabled: true,
				Strict:  false,
				MinHits: 3,
			},
		},
	}
	logger, _ := NewLogger("/tmp", &config.SystemSettings{})

	ctx := context.Background()
	req := &SpawnRequest{TaskID: "t1", StepID: "s1"}

	next := func(_ context.Context, r *SpawnRequest) (*SpawnResult, error) {
		return &SpawnResult{
			Output: schemas.SubagentOutput{
				Result: map[string]any{
					"text": fmt.Sprintf("It's important to note furthermore moreover at the end of the day %s", time.Now().Format("15:04:05")),
				},
			},
		}, nil
	}

	isc := NewAntiSlopInterceptor(reg, logger)
	result, err := isc.Wrap(ctx, req, next)
	if err != nil {
		t.Fatalf("Wrap error: %v", err)
	}
	if len(result.Output.Warnings) == 0 {
		t.Fatal("Expected warning, got none")
	}
}

func hasSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
