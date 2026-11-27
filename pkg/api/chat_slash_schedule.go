package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/tools"
)

func (h *ChatHandler) listSchedulesForCaller(callerOwner, userTier string) string {
	if h.App == nil || h.App.SStore == nil {
		return "Error: schedule store not available."
	}
	ctx := context.WithValue(context.Background(), tools.AuthOwnerKey, callerOwner)
	ctx = context.WithValue(ctx, tools.AuthTierKey, userTier)
	schedules := h.App.SStore.ListVisible(tools.CallerOwnerID(ctx), tools.ResourceScopeIsUnrestricted(ctx, h.App))
	if len(schedules) == 0 {
		return "No scheduled tasks."
	}
	var lines []string
	for _, s := range schedules {
		lines = append(lines, fmt.Sprintf("  %s — %s [%s] %s", s.ID, s.Name, s.Type, s.Status))
	}
	return fmt.Sprintf("Scheduled tasks (%d):\n%s", len(schedules), strings.Join(lines, "\n"))
}
