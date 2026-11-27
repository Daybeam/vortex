package core

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/store"
)

func formatAntiPatternWarnings(precedents []store.AntiPatternPrecedent) string {
	var warn strings.Builder
	warn.WriteString("# ⚠️ Historical Anti-Pattern Warnings\n")
	warn.WriteString("The following development pitfalls have occurred in this codebase before. DO NOT repeat them — follow the correct pattern:\n\n")
	for _, p := range precedents {
		warn.WriteString(fmt.Sprintf("## %s\n", p.AntiPattern))
		warn.WriteString(fmt.Sprintf("- **Symptom**: %s\n", p.Symptom))
		warn.WriteString(fmt.Sprintf("- **Trigger**: %s\n", p.TriggerCondition))
		warn.WriteString(fmt.Sprintf("- **✅ Correct Pattern**: %s\n\n", p.CorrectPattern))
	}
	return warn.String()
}
