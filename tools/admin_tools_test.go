package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/core"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestHandleAdminDeployFile(t *testing.T) {
	os.Setenv("VORTEX_ALLOW_DEPLOY_WRITE", "1")
	defer os.Unsetenv("VORTEX_ALLOW_DEPLOY_WRITE")

	tempDir, err := os.MkdirTemp("", "admin_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	logDir := filepath.Join(tempDir, "logs")
	logger, _ := core.NewLogger(logDir, nil)
	app := &App{Logger: logger}

	t.Run("basic write", func(t *testing.T) {
		dest := filepath.Join(tempDir, "test.txt")
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "orchestrator_admin_deploy_file",
				Arguments: map[string]any{
					"path":    dest,
					"content": "hello world",
					"_reason": "unit test",
				},
			},
		}

		res, err := HandleAdminDeployFile(context.Background(), app, req)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if res.IsError {
			t.Errorf("result is error")
		}

		content, _ := os.ReadFile(dest)
		if string(content) != "hello world" {
			t.Errorf("expected 'hello world', got '%s'", string(content))
		}
	})

	t.Run("base64 write", func(t *testing.T) {
		dest := filepath.Join(tempDir, "b64.txt")
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "orchestrator_admin_deploy_file",
				Arguments: map[string]any{
					"path":     dest,
					"content":  "aGVsbG8gd29ybGQ=", // "hello world"
					"encoding": "base64",
					"_reason":  "unit test b64",
				},
			},
		}

		_, err := HandleAdminDeployFile(context.Background(), app, req)
		if err != nil {
			t.Fatal(err)
		}

		content, _ := os.ReadFile(dest)
		if string(content) != "hello world" {
			t.Errorf("expected 'hello world', got '%s'", string(content))
		}
	})

	t.Run("create dirs", func(t *testing.T) {
		dest := filepath.Join(tempDir, "nested/dir/test.txt")
		req := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Name: "orchestrator_admin_deploy_file",
				Arguments: map[string]any{
					"path":    dest,
					"content": "nested content",
					"_reason": "unit test nested",
				},
			},
		}

		_, err := HandleAdminDeployFile(context.Background(), app, req)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(dest); os.IsNotExist(err) {
			t.Error("file was not created in nested directory")
		}
	})

	t.Run("chunked transfer", func(t *testing.T) {
		dest := filepath.Join(tempDir, "chunked.txt")
		reason := "chunked test"

		// 1. Init
		initReq := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"path":       dest,
					"total_size": 10,
					"_reason":    reason,
				},
			},
		}
		initRes, _ := HandleAdminDeployInit(context.Background(), app, initReq)
		var initData map[string]any
		json.Unmarshal([]byte(initRes.Content[0].(mcp.TextContent).Text), &initData)
		sessionID := initData["session_id"].(string)

		// 2. Push chunks
		push1 := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"session_id":    sessionID,
					"chunk_content": "hello",
					"offset":        0,
				},
			},
		}
		HandleAdminDeployPush(context.Background(), app, push1)

		push2 := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"session_id":    sessionID,
					"chunk_content": "world",
					"offset":        5,
				},
			},
		}
		HandleAdminDeployPush(context.Background(), app, push2)

		// 3. Commit
		commitReq := mcp.CallToolRequest{
			Params: mcp.CallToolParams{
				Arguments: map[string]any{
					"session_id": sessionID,
				},
			},
		}
		res, err := HandleAdminDeployCommit(context.Background(), app, commitReq)
		if err != nil || res.IsError {
			t.Errorf("commit failed: %v, res: %+v", err, res)
		}

		content, _ := os.ReadFile(dest)
		if string(content) != "helloworld" {
			t.Errorf("expected 'helloworld', got '%s'", string(content))
		}
	})
}

func TestHandleAdminDeployFile_GatedByDefault(t *testing.T) {
	os.Unsetenv("VORTEX_ALLOW_DEPLOY_WRITE")
	tempDir, err := os.MkdirTemp("", "admin_gate_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	logDir := filepath.Join(tempDir, "logs")
	logger, _ := core.NewLogger(logDir, nil)
	app := &App{Logger: logger}

	dest := filepath.Join(tempDir, "should_not_exist.txt")
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "orchestrator_admin_deploy_file",
			Arguments: map[string]any{
				"path":    dest,
				"content": "hello",
				"_reason": "gate test",
			},
		},
	}

	res, err := HandleAdminDeployFile(context.Background(), app, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := res.Content[0].(mcp.TextContent).Text
	if !strings.Contains(text, "disabled") {
		t.Fatalf("expected gate-disabled error, got: %s", text)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("file should NOT be created when gate is closed")
	}
}

func TestHandleAdminSuspendUI(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "suspend_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	configPath := filepath.Join(tempDir, "config.json")
	_ = os.WriteFile(configPath, []byte("{}"), 0644)

	logDir := filepath.Join(tempDir, "logs")
	logger, _ := core.NewLogger(logDir, nil)
	app := &App{
		Logger:     logger,
		ConfigPath: configPath,
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "orchestrator_admin_suspend_ui",
			Arguments: map[string]any{
				"reason": "testing_suspend",
			},
		},
	}

	res, err := HandleAdminSuspendUI(context.Background(), app, req)
	if err != nil {
		t.Fatalf("HandleAdminSuspendUI failed: %v", err)
	}

	var resData map[string]any
	json.Unmarshal([]byte(res.Content[0].(mcp.TextContent).Text), &resData)

	if resData["status"] != "suspending" {
		t.Errorf("expected status 'suspending', got %v", resData["status"])
	}

	suspendFile := filepath.Join(tempDir, "tmp", "sig_ui_suspend")
	if _, err := os.Stat(suspendFile); os.IsNotExist(err) {
		t.Error("suspend signal file not created")
	}
}
