package core

import "path/filepath"

// spawner_delegation_workspace.go — D4 fix helper
// (DELEGATION_HANDOVER_REDESIGN.md §4.4).
//
// Extracted from tryDelegationMode to avoid growing spawner_utils.go
// past its frozen size-baseline entry (1181 lines).

// delegationWorkspaceBlock builds the "workspace" section of the delegation
// envelope, giving the host the absolute paths it needs to implement
// write_file/read_file in delegation mode.
func delegationWorkspaceBlock(outputBase, taskID, sessionRoot string) map[string]any {
	return map[string]any{
		"task_output_dir":    filepath.Join(outputBase, taskID),
		"shared_context_dir": filepath.Join(outputBase, taskID, "shared_context"),
		"session_root":       sessionRoot,
		"path_semantics":     "relative to task_output_dir",
	}
}
