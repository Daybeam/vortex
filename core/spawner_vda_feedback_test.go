package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

type fakeScreenshotSender struct {
	result any
	err    error
}

func (f fakeScreenshotSender) SendRequest(context.Context, string, any) (any, error) {
	return f.result, f.err
}

// F6 regression: ENGINE_EVAL_VALIDATION_PLAN-2026-11-27.md — failed or empty
// captures must not be reported as attached screenshots.
func TestF6_ForcedVisualAuditReportsCaptureOutcome(t *testing.T) {
	oldBus := obs.DefaultBus
	obs.DefaultBus = obs.NewEventBus()
	t.Cleanup(func() { obs.DefaultBus = oldBus })

	events := make(chan obs.AgentEvent, 8)
	unsubscribe := obs.DefaultBus.SubscribeWithCancel(string(obs.EventVDACapture), func(event obs.AgentEvent) error {
		events <- event
		return nil
	})
	t.Cleanup(unsubscribe)

	logger, err := obs.NewLogger(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)
	spawner := &Spawner{logger: logger}
	req := &SpawnRequest{TaskID: "task-f6", StepID: "step-f6"}
	image := filepath.Join(t.TempDir(), "screen.png")
	if err := os.WriteFile(image, []byte("png-data"), 0600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		sender     fakeScreenshotSender
		wantStatus string
		wantCount  int
	}{
		{
			name: "captured",
			sender: fakeScreenshotSender{result: schemas.SubagentOutput{Attachments: []schemas.Attachment{{
				MimeType: "image/png", Path: image, Label: "screen",
			}}}},
			wantStatus: "captured",
			wantCount:  1,
		},
		{name: "capture error", sender: fakeScreenshotSender{err: errors.New("capture failed")}, wantStatus: "capture_error"},
		{name: "empty", sender: fakeScreenshotSender{result: schemas.SubagentOutput{}}, wantStatus: "empty"},
		{name: "invalid response", sender: fakeScreenshotSender{result: "not an output"}, wantStatus: "invalid_response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &vdaState{}
			spawner.captureForcedVisualAudit(context.Background(), tt.sender, req, "ghost", st)
			if len(st.captured) != tt.wantCount || len(st.turnAttachments) != tt.wantCount {
				t.Fatalf("captured=%d turnAttachments=%d, want %d", len(st.captured), len(st.turnAttachments), tt.wantCount)
			}
			select {
			case event := <-events:
				if got := event.Payload["capture_status"]; got != tt.wantStatus {
					t.Fatalf("capture_status=%v, want %s", got, tt.wantStatus)
				}
			default:
				t.Fatal("capture outcome event was not emitted")
			}
			joined := strings.Join(st.toolResults, "\n")
			if tt.wantCount == 0 && strings.Contains(joined, "screenshot captured") {
				t.Fatalf("failed capture produced positive evidence: %q", joined)
			}
			if tt.wantCount == 1 && !strings.Contains(joined, "screenshot captured") {
				t.Fatalf("successful capture missing evidence: %q", joined)
			}
		})
	}
}

// F6 regression: ENGINE_EVAL_VALIDATION_PLAN-2026-11-27.md — the verification
// prompt is conditional on a real attachment, not merely a state-changing tool.
func TestF6_VerificationPromptRequiresCapturedAttachment(t *testing.T) {
	spawner := &Spawner{}
	resp := &providers.ProviderResponse{Text: "done"}
	for _, captured := range []bool{false, true} {
		blocks := spawner.appendTurnObservation(nil, resp, []string{"tool result"}, captured)
		got := blocks[len(blocks)-1].Text
		if has := strings.Contains(got, SpawnerVDAPrompt); has != captured {
			t.Fatalf("captured=%v prompt present=%v: %q", captured, has, got)
		}
	}
}

func TestF6_NextTurnAttachmentsIncludeVDAWithoutDuplicates(t *testing.T) {
	attachment := schemas.Attachment{MimeType: "image/png", Path: "screen.png"}
	got := appendUniqueAttachments(
		[]schemas.Attachment{attachment},
		[]schemas.Attachment{attachment, {MimeType: "image/png", Path: "second.png"}},
	)
	if len(got) != 2 {
		t.Fatalf("attachments=%v, want two unique images", got)
	}
}
