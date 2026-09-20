package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// FastPathEngine handles deterministic artifact contract execution.
type FastPathEngine struct {
	workdir string
}

func NewFastPathEngine(workdir string) *FastPathEngine {
	return &FastPathEngine{workdir: workdir}
}

// ExecuteFastPath checks if an artifact already satisfies a contract.
// If it does, it returns the validation result, effectively bypassing the LLM.
func (e *FastPathEngine) ExecuteFastPath(ctx context.Context, contract schemas.ArtifactContract, args map[string]any) (any, error) {
	// 1. Path Sanitization
	absPath := contract.Path
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(e.workdir, absPath)
	}

	// Ensure path is within workdir
	rel, err := filepath.Rel(e.workdir, absPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("fastpath: path %q escapes workspace boundary", contract.Path)
	}

	// 2. Deterministic Validation
	err = ValidateArtifact(contract)
	if err != nil {
		return nil, fmt.Errorf("fastpath: contract violation: %w", err)
	}

	// 3. Return Success Metadata
	info, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("fastpath: could not stat artifact: %w", err)
	}

	return map[string]any{
		"status":     "success",
		"path":       contract.Path,
		"size_bytes": info.Size(),
		"validated":  true,
		"fastpath":   true,
	}, nil
}
