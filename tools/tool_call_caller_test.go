package tools

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/daybeam/vortex/core/obs"
)

// Regression (V9 in docs/plan/VERIFIED_ISSUES_2026-11-11.md): every tool_call
// record carried tier=admin and no caller field, so one operator's MCP calls
// could not be told apart from another session's. That already produced one
// mis-attribution (61 operator calls counted as front-desk calls). Both phases
// must carry the same identity CheckTaskOwnership uses, so a denied call and its
// tool_call record can be joined.
func TestToolCallRecordsCarryCaller(t *testing.T) {
	cases := []struct {
		name  string
		owner string
		want  string
	}{
		{"keyed caller is identified", "alice", "alice"},
		{"local stdio operator stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, err := obs.NewLogger(t.TempDir(), nil)
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			events := logger.SubscribeEvents()

			app := &App{Logger: logger}
			wrapped := app.loggedHandler("probe_tool", func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			})

			ctx := context.WithValue(context.Background(), AuthOwnerKey, tc.owner)
			req := mcp.CallToolRequest{Params: mcp.CallToolParams{
				Name:      "probe_tool",
				Arguments: map[string]any{"task_id": "t-caller"},
			}}
			if _, err := wrapped(ctx, req); err != nil {
				t.Fatalf("wrapped handler: %v", err)
			}
			// Close drains the async writer, which is what fans out to subscribers.
			logger.Close()

			var started, finished int
			for {
				select {
				case ev := <-events:
					if ev.Event != obs.EventToolCall {
						continue
					}
					switch ev.Detail["phase"] {
					case "started":
						started++
					case "finished":
						finished++
					default:
						t.Errorf("unexpected phase %v", ev.Detail["phase"])
					}
					if got := ev.Detail["caller"]; got != tc.want {
						t.Errorf("phase %v: caller = %v, want %q", ev.Detail["phase"], got, tc.want)
					}
					if ev.Detail["tier"] != TierAdmin {
						t.Errorf("phase %v: tier = %v, want admin (existing behavior must not change)", ev.Detail["phase"], ev.Detail["tier"])
					}
				default:
					if started != 1 || finished != 1 {
						t.Fatalf("want one started and one finished record, got %d/%d", started, finished)
					}
					return
				}
			}
		})
	}
}