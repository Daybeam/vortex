package chat

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/core"
)

// buildSystem composes the system prompt from context-tree and memory-bank state.
// It takes no arguments on purpose: this string is the first thing in every
// request, so anything that varies between turns makes the prefix uncacheable.
// Per-turn context belongs in buildTurnContext, appended to the tail of the
// last user message to keep the cached prefix intact.
func (h *ChatHarness) buildSystem() string {
	var b strings.Builder

	if h.Role != nil && h.Role.Instruction != "" {
		b.WriteString(fmt.Sprintf(core.ChatHarnessRolePrefix, h.Role.Name, h.Role.Instruction))
	}

	b.WriteString(core.ChatHarnessDefaultIdentity)
	b.WriteString(core.ChatHarnessToolsetDesc)
	b.WriteString(core.ChatHarnessUsageGuidance)
	b.WriteString(core.ChatHarnessWhenToDelegate)

	b.WriteString("\n## DELEGATION READINESS GATE (HARD RULE)\n")
	b.WriteString(core.ChatHarnessDelegatePrefix)
	b.WriteString(core.ChatHarnessDelegateRule1)
	b.WriteString(core.ChatHarnessDelegateRule2)
	b.WriteString(core.ChatHarnessDelegateRule3)
	b.WriteString("4. The expected outcome / success criteria is explicit.\n")
	if h.ReadinessGateSoft {
		b.WriteString("If any of these are unclear, consider asking the user for clarification before delegating.\n")
	} else {
		b.WriteString("If ANY of these fail, ask the user clarifying questions FIRST. NEVER blindly delegate a vague task.\n")
	}

	if h.MemoryBank != nil {
		if mb, err := h.MemoryBank.Load(); err == nil {
			if mb.ProductContext != "" {
				b.WriteString("\n## Product context\n" + mb.ProductContext + "\n")
			}
			if len(mb.ActiveContext.Goals) > 0 {
				b.WriteString("Goals: " + strings.Join(mb.ActiveContext.Goals, "; ") + "\n")
			}
			if len(mb.UserProfile.Expertise) > 0 || len(mb.UserProfile.Preferences) > 0 || mb.UserProfile.CommunicationStyle != "" {
				b.WriteString("\n## User Profile\n")
				if len(mb.UserProfile.Expertise) > 0 {
					b.WriteString("Expertise: " + strings.Join(mb.UserProfile.Expertise, ", ") + "\n")
				}
				if len(mb.UserProfile.Preferences) > 0 {
					b.WriteString("Preferences: " + strings.Join(mb.UserProfile.Preferences, ", ") + "\n")
				}
				if mb.UserProfile.CommunicationStyle != "" {
					b.WriteString("Communication style: " + mb.UserProfile.CommunicationStyle + "\n")
				}
			}
		}
	}
	return b.String()
}
