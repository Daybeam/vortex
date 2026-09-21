package tools

import "testing"

func TestSmartRouteIntent(t *testing.T) {
	tests := []struct {
		task     string
		expected []string
		role     string
		reason   string
		shouldBe bool
	}{
		{
			task:     "Generate a sci-fi cover image of a futuristic office with AI agents replacing human workers",
			expected: []string{"ghost-driver"},
			role:     "ghost_operator",
			reason:   "Matched browser/automation/image-generation intent",
			shouldBe: true,
		},
		{
			task:     "Analyze the latest stock market trends for tech companies",
			expected: []string{"invest-research-lite"},
			role:     "data_researcher",
			reason:   "Matched financial/research intent",
			shouldBe: true,
		},
		{
			task:     "Create a Word document with the project summary",
			expected: []string{"docx-mcp"},
			role:     "document_specialist",
			reason:   "Matched document/docx intent",
			shouldBe: true,
		},
		{
			task:     "Just a simple hello world task",
			expected: nil,
			role:     "",
			reason:   "",
			shouldBe: false,
		},
	}

	for _, tt := range tests {
		mcps, role, reason := SmartRouteIntent(tt.task)
		if tt.shouldBe {
			if len(mcps) == 0 {
				t.Errorf("Expected MCPs for task '%s', got none", tt.task)
			} else {
				if !sliceEqual(mcps, tt.expected) {
					t.Errorf("Expected MCPs %v for task '%s', got %v", tt.expected, tt.task, mcps)
				}
				if role != tt.role {
					t.Errorf("Expected role %s for task '%s', got %s", tt.role, tt.task, role)
				}
				if reason != tt.reason {
					t.Errorf("Expected reason %s for task '%s', got %s", tt.reason, tt.task, reason)
				}
			}
		} else {
			if len(mcps) > 0 {
				t.Errorf("Expected no MCPs for task '%s', got %v", tt.task, mcps)
			}
		}
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}
