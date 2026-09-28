package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/daybeam/vortex/core"
)

// TestHandleAdminDeployFile_RejectsPathTraversal is a regression test for
// audit S-C2: admin_deploy must reject paths containing ".." segments to
// prevent path traversal attacks (e.g. writing to /etc/cron.d/ or
// ~/.ssh/authorized_keys).
//
// Before the fix, any path was accepted when VORTEX_ALLOW_DEPLOY_WRITE=1.
// After the fix, paths containing "../" or "/.." or equal to ".." are rejected.
//
// Reproduction: a deploy request with a traversal path should return an
// error result mentioning "audit S-C2".
func TestHandleAdminDeployFile_RejectsPathTraversal(t *testing.T) {
	t.Setenv("VORTEX_ALLOW_DEPLOY_WRITE", "1")

	tempDir, err := os.MkdirTemp("", "admin_traversal_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	logger, err := core.NewLogger(filepath.Join(tempDir, "logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Logger: logger}

	traversalPaths := []string{
		tempDir + "/../../../etc/passwd",
		"../etc/cron.d/evil",
		"..",
		tempDir + "/subdir/../../evil.txt",
		"output/../../secret.txt",
	}

	for _, p := range traversalPaths {
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "orchestrator_admin_deploy_file",
				Arguments: map[string]any{
					"path":    p,
					"content": "malicious",
					"_reason": "traversal test",
				},
			},
		}

		res, err := HandleAdminDeployFile(context.Background(), app, req)
		if err != nil {
			t.Errorf("path %q: unexpected error: %v", p, err)
		}
		if !res.IsError {
			t.Errorf("path %q: expected error result for path traversal, but it succeeded", p)
		}
	}
}

// TestHandleAdminDeployFile_NormalPathStillWorks verifies that the S-C2 fix
// does not over-reject legitimate paths without traversal.
func TestHandleAdminDeployFile_NormalPathStillWorks(t *testing.T) {
	t.Setenv("VORTEX_ALLOW_DEPLOY_WRITE", "1")

	tempDir, err := os.MkdirTemp("", "admin_normal_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)
	t.Setenv("VORTEX_DEPLOY_ROOT", tempDir)

	logger, err := core.NewLogger(filepath.Join(tempDir, "logs"), nil)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Logger: logger}

	dest := filepath.Join(tempDir, "normal.txt")
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "orchestrator_admin_deploy_file",
			Arguments: map[string]any{
				"path":    dest,
				"content": "safe content",
				"_reason": "normal write test",
			},
		},
	}

	res, err := HandleAdminDeployFile(context.Background(), app, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Fatalf("normal path should still work after S-C2 fix, got error result")
	}

	content, _ := os.ReadFile(dest)
	if string(content) != "safe content" {
		t.Errorf("expected 'safe content', got %q", string(content))
	}
}
