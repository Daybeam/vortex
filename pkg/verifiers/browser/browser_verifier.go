package browser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/daybeam/vortex/core"
)

// DEPRECATED (2026-09-20): BrowserVerifier is domain-specific to browser operations
// and does not belong in the orchestrator core. The orchestrator delegates browser
// operations to external MCP servers (e.g. ghost-driver-mcp) which handle their own
// verification. The core.Verifier interface remains as the contract for future
// verifiers, but this implementation should not be wired.
//
// Future verifiers for the core.Verifier (SAV) interface might include:
//   - APIVerifier: verify API response status/body changes
//   - DatabaseVerifier: verify database state changes
//   - GitVerifier: verify git operations (commit exists, branch created)
// Until such verifiers are needed, this file is retained as a reference
// implementation but marked deprecated and not exposed externally.

type BrowserSnapshot struct {
	URL       string
	DOMHash   string
	Timestamp time.Time
}

type BrowserVerifier struct {
	caps map[string]string
}

func NewBrowserVerifier(caps map[string]string) *BrowserVerifier {
	return &BrowserVerifier{caps: caps}
}

func (bv *BrowserVerifier) Sense(ctx context.Context, mcpID, toolName string, executor core.ToolExecutor) (core.VerificationState, error) {
	urlTool := bv.caps["GET_URL"]
	domTool := bv.caps["GET_DOM"]

	url, err := executor.DirectExecute(ctx, mcpID, urlTool, nil)
	if err != nil {
		return nil, fmt.Errorf("sense url failed: %w", err)
	}

	dom, err := executor.DirectExecute(ctx, mcpID, domTool, nil)
	if err != nil {
		return nil, fmt.Errorf("sense dom failed: %w", err)
	}

	return &BrowserSnapshot{
		URL:       fmt.Sprintf("%v", url),
		DOMHash:   fmt.Sprintf("%v", dom),
		Timestamp: time.Now(),
	}, nil
}

func (bv *BrowserVerifier) Verify(ctx context.Context, mcpID, toolName string, before core.VerificationState, result any, executor core.ToolExecutor) (bool, error) {
	snapshot, ok := before.(*BrowserSnapshot)
	if !ok || snapshot == nil {
		return false, fmt.Errorf("invalid before state: expected *BrowserSnapshot, got %T", before)
	}

	currentURL, err := executor.DirectExecute(ctx, mcpID, bv.caps["GET_URL"], nil)
	if err != nil {
		return false, fmt.Errorf("verify url failed: %w", err)
	}
	urlStr := fmt.Sprintf("%v", currentURL)

	// Logic: If URL changed, it's a successful navigation/submission
	if urlStr != snapshot.URL {
		return true, nil
	}

	// If URL didn't change, check if DOM changed significantly
	currentDOM, err := executor.DirectExecute(ctx, mcpID, bv.caps["GET_DOM"], nil)
	if err != nil {
		return false, fmt.Errorf("verify dom failed: %w", err)
	}
	domStr := fmt.Sprintf("%v", currentDOM)
	if domStr != snapshot.DOMHash {
		return true, nil
	}

	// Check for common error indicators in DOM
	if strings.Contains(domStr, "error") || strings.Contains(domStr, "denied") || strings.Contains(domStr, "failed") {
		return false, fmt.Errorf("detected error indicators in page content")
	}

	return false, fmt.Errorf("no state change detected after %s", toolName)
}

func (bv *BrowserVerifier) Recover(ctx context.Context, mcpID, toolName string, args map[string]any, err error, executor core.ToolExecutor) (any, error) {
	// Recovery: Refresh and retry once
	refreshTool := bv.caps["REFRESH"]
	_, _ = executor.DirectExecute(ctx, mcpID, refreshTool, nil)
	return executor.DirectExecute(ctx, mcpID, toolName, args)
}
