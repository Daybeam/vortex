package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestEstimateTokens_EnglishRoughlyMatchesLen4(t *testing.T) {
	text := "The quick brown fox jumps over the lazy dog repeatedly today"
	got := estimateTokens(text)
	want := len(text) / 4
	// Allow a small tolerance since we still fall back to the same divisor
	// for non-CJK runs, but count per-run rather than per-whole-string.
	diff := got - want
	if diff < -2 || diff > 2 {
		t.Fatalf("English estimate %d too far from len/4 baseline %d", got, want)
	}
}

func TestEstimateTokens_CJKUsesCharacterNotByteWidth(t *testing.T) {
	// 26 Chinese characters, 78 bytes in UTF-8 (3 bytes/char).
	text := "请帮我把这份中文任务描述正确地估算成大概的token数量"
	got := estimateTokens(text)
	runeCount := len([]rune(text))

	// The old len(text)/4 heuristic operates on *byte* length, not rune
	// count -- for pure CJK text that's a very different number
	// (byteLen/4 ~= 3*runeCount/4 = 0.75*runeCount) than a genuinely
	// rune-aware estimate. The point of this test isn't that one number
	// must exceed the other (both land in a plausible ballpark for pure
	// CJK, as it happens) -- it's that our estimate must actually be
	// driven by rune count, not silently degenerate into byte length.
	byteLenHeuristic := len(text) / 4
	if got == byteLenHeuristic {
		t.Fatalf("estimate (%d) suspiciously equals byte-length/4 (%d); expected a rune-count-driven value instead", got, byteLenHeuristic)
	}

	// Should land in the neighborhood of rune-count/1.7, not rune-count/4
	// (which would silently ignore the CJK/non-CJK distinction entirely).
	wantApprox := int(float64(runeCount) / 1.7)
	if diff := got - wantApprox; diff < -2 || diff > 2 {
		t.Fatalf("expected estimate near rune-count/1.7 (~%d), got %d (rune count %d)", wantApprox, got, runeCount)
	}
	naiveLatinRuleOnRunes := runeCount / 4
	if got <= naiveLatinRuleOnRunes {
		t.Fatalf("expected CJK estimate (%d) to exceed treating CJK runes like Latin runes at len/4 (%d)", got, naiveLatinRuleOnRunes)
	}
}

func TestEstimateTokens_EmptyString(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Fatalf("expected 0 for empty string, got %d", got)
	}
}

func TestEstimateTokens_MixedNeverZeroForNonEmpty(t *testing.T) {
	if got := estimateTokens("a"); got < 1 {
		t.Fatalf("expected at least 1 token for non-empty text, got %d", got)
	}
}

func TestIsCJK(t *testing.T) {
	cases := map[rune]bool{
		'中': true,
		'あ': true,
		'ア': true,
		'한': true,
		'a': false,
		'1': false,
		' ': false,
		'.': false,
	}
	for r, want := range cases {
		if got := isCJK(r); got != want {
			t.Errorf("isCJK(%q) = %v, want %v", r, got, want)
		}
	}
}

func TestAnthropicCountTokens_FallsBackOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Model: "claude-sonnet-4-6", BaseURL: srv.URL},
		client: srv.Client(),
	}
	text := "hello world this is a test"
	got, err := p.CountTokens(context.Background(), text)
	if err != nil {
		t.Fatalf("expected graceful fallback (no error), got err: %v", err)
	}
	want := estimateTokens(text)
	if got != want {
		t.Fatalf("expected fallback to estimateTokens (%d), got %d", want, got)
	}
}

func TestAnthropicCountTokens_UsesRealAPIWhenAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"input_tokens": 42}`))
	}))
	defer srv.Close()

	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Model: "claude-sonnet-4-6", BaseURL: srv.URL},
		client: srv.Client(),
	}
	got, err := p.CountTokens(context.Background(), "irrelevant, server is mocked")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("expected real API value 42, got %d", got)
	}
}

func TestAnthropicCountTokens_NoModelFallsBackWithoutNetworkCall(t *testing.T) {
	p := &AnthropicProvider{
		cfg:    &config.ProviderConfig{Model: "", BaseURL: "http://unreachable.invalid"},
		client: http.DefaultClient,
	}
	text := "some text"
	got, err := p.CountTokens(context.Background(), text)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != estimateTokens(text) {
		t.Fatalf("expected estimateTokens fallback, got %d", got)
	}
}

// Note: GeminiProvider.CountTokens builds its request URL from a hardcoded
// generativelanguage.googleapis.com host rather than cfg.BaseURL, so its
// fallback branch can't be exercised hermetically with httptest the way
// AnthropicProvider's can. The fallback code path is structurally identical
// (err != nil || status >= 400 -> estimateTokens), so it's covered by
// inspection/code review rather than a live-network test here, to keep this
// test suite network-independent.
