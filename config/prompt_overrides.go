package config

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// PromptOverrides holds optional externalized prompt text that, when
// present, takes priority over the hardcoded Go string constants in
// tools/server_instructions.go and the OUTPUT CONTRACT template built by
// schemas.BuildOutputConstraint. This mirrors the pattern already
// established for Skills (workspace/skills/*.json) and SOPs
// (workspace/sops/*.json): externally editable, hot-reloaded via
// StartWatcher's 5s poll, with zero-config fallback to the compiled-in
// defaults when no override file is present.
//
// ADDED (2026-07-28). Context: a discussion about whether a "system prompt
// repository" concept was missing from this project. It was largely
// already present for role/skill prompts (Skill.GetPrompt's per-family
// resolution, Role.Instruction, the split-layout workspace/roles/
// directory) -- the actual gap was that tools.ServerInstructions /
// tools.ServerInstructionsPublic / the OUTPUT CONTRACT template were still
// hardcoded Go string constants requiring a recompile+redeploy to change.
// This closes that specific gap using the exact same directory-override-
// with-fallback shape already established for Skills/SOPs, rather than
// inventing a new mechanism.
type PromptOverrides struct {
	// ServerInstructions overrides tools.ServerInstructions (the admin/
	// Tier2 MCP server's `initialize` instructions field) when non-empty.
	// Loaded from workspace/prompts/server_instructions.txt.
	ServerInstructions string
	// ServerInstructionsPublic overrides tools.ServerInstructionsPublic
	// (the public/Tier1 MCP server's instructions) when non-empty. Loaded
	// from workspace/prompts/server_instructions_public.txt.
	ServerInstructionsPublic string
	// OutputContract overrides the OUTPUT CONTRACT template normally built
	// by schemas.BuildOutputConstraint when non-empty. Loaded from
	// workspace/prompts/output_contract.txt.
	//
	// Use the literal tokens {{RESULT_HINT}} and {{CAPABILITY}} anywhere the
	// capability-specific result-schema hint and capability name should be
	// substituted -- these are replaced via strings.Replace (see
	// schemas.BuildOutputConstraintWithTemplate), NOT fmt.Sprintf, so a
	// literal '%' character in an externally-edited prompt file is never
	// misinterpreted as a format verb.
	OutputContract string
}

// promptOverridesDir returns the directory prompt override files are read
// from, mirroring workspace/skills/ and workspace/sops/'s existing path
// convention (see the skills-loading and LoadSOPs call sites in Load()).
func promptOverridesDir(configPath string) string {
	return filepath.Join(ConfigDir(configPath), "workspace/prompts/")
}

// loadPromptOverrides reads the (optional) override files for the given
// config path. A missing file is not an error -- an empty PromptOverrides
// field means "use the compiled-in default", exactly like an unset
// RoleCookbookSource(s) means "no cookbook configured". A read error on a
// file that DOES exist (e.g. a permissions problem) is logged rather than
// silently swallowed, matching the non-fatal-but-visible treatment already
// given to a malformed workspace/skills/*.json or workspace/sops/*.json
// file elsewhere in Load().
func loadPromptOverrides(configPath string) PromptOverrides {
	dir := promptOverridesDir(configPath)
	return PromptOverrides{
		ServerInstructions:       readPromptOverrideFile(filepath.Join(dir, "server_instructions.txt")),
		ServerInstructionsPublic: readPromptOverrideFile(filepath.Join(dir, "server_instructions_public.txt")),
		OutputContract:           readPromptOverrideFile(filepath.Join(dir, "output_contract.txt")),
	}
}

func readPromptOverrideFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("warning: failed to read prompt override %s: %v", path, err)
		}
		return ""
	}
	return strings.TrimSpace(string(data))
}
