package tools

import (
	"testing"

	"github.com/daybeam/vortex/config"
)

// Regression: App.EmbedClient had no production assignment in any of the five
// entrypoints, so ChatHarness.indexTurn hit its `h.Embed == nil` early return
// and no chat turn ever reached the archive — search_archive always returned 0
// hits and the bot reported it had no stored history summaries.
// Found 2026-11-11 while diagnosing Telegram conversation defects.
func TestInitArchiveWiresEmbedClient(t *testing.T) {
	app := &App{Registry: &config.Registry{}}
	app.InitArchive(t.TempDir())
	defer app.StopArchive()

	if app.EmbedClient == nil {
		t.Fatal("InitArchive left EmbedClient nil: chat turns will never be archived")
	}
}

// A nil Registry must not panic: EmbedWithFallback dereferences Registry.Mu,
// so the wiring is guarded and such an app simply keeps the lexical-only path.
func TestInitArchiveNilRegistryKeepsEmbedNil(t *testing.T) {
	app := &App{}
	app.InitArchive(t.TempDir())
	defer app.StopArchive()

	if app.EmbedClient != nil {
		t.Fatal("expected no embedding client without a Registry")
	}
}