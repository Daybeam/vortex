package core

import (
	"context"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// Regression coverage for the O14 doSpawn extraction: telemetry and provider
// fallback policy must remain attached to the same per-step state after the
// 950-line turn loop is split into cohesive helpers.
func TestSpawnerObserveResponsePreservesTelemetry(t *testing.T) {
	logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
	s := &Spawner{logger: logger, cacheSentinel: NewCacheSentinel()}
	req := &SpawnRequest{TaskID: "observe-extraction", StepID: "step-1", MaxOutputTokens: 777}
	st := &spawnLoopState{userBlocks: []schemas.ContentBlock{{Text: "history"}}}
	pCfg := &config.ProviderConfig{Provider: "telemetry-provider", Model: "telemetry-model"}
	model := pCfg.Model
	before, _ := GetGlobalStats().GetSnapshot()[model].(map[string]any)
	if before == nil {
		before = map[string]any{"prompt_tokens": int64(0), "completion_tokens": int64(0)}
	}

	for i := 0; i < 3; i++ {
		s.observeSpawnResponse(req, st, &providers.ProviderResponse{
			PromptTokens: 6000, CompletionTokens: 12, StopReason: "length",
		}, pCfg)
	}
	logger.Close()

	events, err := logger.ReadTaskLogs(req.TaskID)
	if err != nil {
		t.Fatalf("ReadTaskLogs: %v", err)
	}
	var truncated, cacheRate, inefficient int
	for _, event := range events {
		detail, _ := event["detail"].(map[string]any)
		switch event["event"] {
		case string(obs.EventOutputTruncated):
			truncated++
			if detail["max_tokens"] != float64(777) || detail["stop_reason"] != "length" {
				t.Fatalf("truncation detail lost request/response fields: %#v", detail)
			}
			if detail["visible_output_empty"] != true || detail["likely_reasoning_dominated"] != true {
				t.Fatalf("reasoning-dominated empty output flags missing: %#v", detail)
			}
		case "cache_hit_rate":
			cacheRate++
			if detail["cache_hit_rate"] != float64(0) {
				t.Fatalf("cache_hit_rate = %v, want 0", detail["cache_hit_rate"])
			}
		case string(obs.EventCacheInefficiency):
			inefficient++
			if detail["consecutive_zero_rounds"] != float64(3) {
				t.Fatalf("cache alert lost per-step threshold state: %#v", detail)
			}
		}
	}
	if truncated != 3 || cacheRate != 3 || inefficient != 1 {
		t.Fatalf("telemetry event counts = truncation:%d cache_rate:%d inefficiency:%d, want 3/3/1", truncated, cacheRate, inefficient)
	}

	after := GetGlobalStats().GetSnapshot()[model].(map[string]any)
	if got := after["prompt_tokens"].(int64) - before["prompt_tokens"].(int64); got != 18000 {
		t.Fatalf("prompt token delta = %d, want 18000", got)
	}
	if got := after["completion_tokens"].(int64) - before["completion_tokens"].(int64); got != 36 {
		t.Fatalf("completion token delta = %d, want 36", got)
	}
}

func TestSpawnerResolveEmptyResponsePreservesCandidateAndRetryPolicy(t *testing.T) {
	candidates := []spawnCandidate{{providerID: "primary"}, {providerID: "fallback"}}
	tests := []struct {
		name          string
		candIdx       int
		initial       int
		wantAction    spawnTurnAction
		wantRetries   int
		wantErrorText string
	}{
		{name: "candidate fallback does not consume outer retry", candIdx: 0, initial: 0, wantAction: spawnTurnNextCandidate, wantRetries: 0},
		{name: "first empty last candidate retries turn", candIdx: 1, initial: 0, wantAction: spawnTurnRetry, wantRetries: 1},
		{name: "second empty last candidate retries turn", candIdx: 1, initial: 1, wantAction: spawnTurnRetry, wantRetries: 2},
		{name: "third empty last candidate fails", candIdx: 1, initial: 2, wantAction: spawnTurnSuccess, wantRetries: 3, wantErrorText: "exhausted 2 retries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
			defer logger.Close()
			s := &Spawner{logger: logger}
			st := &spawnLoopState{emptyRespRetries: tt.initial}
			attempt := &spawnCandidateAttempt{
				ctx:  context.Background(),
				resp: &providers.ProviderResponse{},
				pCfg: &config.ProviderConfig{Provider: "primary", Model: "empty-model"},
			}
			action, result, err := s.resolveSpawnCandidate(
				&SpawnRequest{TaskID: "empty-policy", StepID: "step-1"},
				&config.Role{BaseCapability: "worker"}, st, 0, attempt, candidates, tt.candIdx, 2,
			)
			if action != tt.wantAction || result != nil {
				t.Fatalf("action/result = %v/%v, want %v/nil", action, result, tt.wantAction)
			}
			if st.emptyRespRetries != tt.wantRetries {
				t.Fatalf("emptyRespRetries = %d, want %d", st.emptyRespRetries, tt.wantRetries)
			}
			if tt.wantErrorText == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErrorText != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErrorText)) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErrorText)
			}
		})
	}
}

func TestSpawnerCandidateRequestPreservesStickyContextAndOverrides(t *testing.T) {
	temp, freq := float32(0.25), float32(0.5)
	attempt := &spawnCandidateAttempt{
		systemBlocks: []schemas.ContentBlock{{Text: "system"}},
		userBlocks:   []schemas.ContentBlock{{Text: "current"}},
		attachments:  []schemas.Attachment{{MimeType: "image/png", Path: "image.png"}},
		pCfg:         &config.ProviderConfig{Model: "request-model"},
		temp:         &temp,
		freqPen:      &freq,
		forceTool:    true,
		constraints:  map[string]any{"response_format": "json"},
	}
	t.Setenv("O14_EXTRACTION_SECRET", "secret")
	req := &SpawnRequest{Hub: &ContextHub{Registry: &config.Registry{Secrets: &config.EnvSecretProvider{}}}}
	st := &spawnLoopState{stickyContext: "sticky"}
	got := attempt.request(req, st, 1234)
	if got.Model != "request-model" || got.MaxTokens != 1234 || !got.ForceToolCall {
		t.Fatalf("request lost model/token/tool fields: %#v", got)
	}
	if got.Temperature != &temp || got.FrequencyPenalty != &freq {
		t.Fatal("request lost provider sampling overrides")
	}
	if len(got.UserBlocks) != 2 || !strings.HasPrefix(got.UserBlocks[0].Text, "## Persistent Context\nsticky") || got.UserBlocks[1].Text != "current" {
		t.Fatalf("sticky/current user blocks = %#v", got.UserBlocks)
	}
	secret, secretErr := got.Secrets.GetSecret("O14_EXTRACTION_SECRET")
	if len(got.Attachments) != 1 || secretErr != nil || secret != "secret" || got.Constraints["response_format"] != "json" {
		t.Fatalf("request lost attachments, secrets, or constraints: attachments=%v secret=%q err=%v constraints=%v",
			got.Attachments, secret, secretErr, got.Constraints)
	}
}
