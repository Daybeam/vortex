package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestScriptProvider_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "test_provider.lua")

	luaCode := `
provider = {
	name = "test-lua-provider",
	complete = function(req)
		local decoded = json.decode('{"hello":"world"}')
		local encoded = json.encode(decoded)

		local pathEnv = env("PATH")

		log.info("running lua test complete")

		if req.user == "fail" then
			return nil, "RATE_LIMITED: too fast"
		end

		return {
			text = "hello from lua",
			stop_reason = "stop",
			tool_calls = {
				{
					name = "test_tool",
					call_id = "call_123",
					arguments = { q = decoded.hello }
				}
			}
		}, nil
	end
}
`
	if err := os.WriteFile(scriptPath, []byte(luaCode), 0644); err != nil {
		t.Fatalf("failed to write test lua script: %v", err)
	}

	cfg := &config.ProviderConfig{Provider: "test-script"}
	sp, err := NewScriptProvider(cfg, scriptPath)
	if err != nil {
		t.Fatalf("NewScriptProvider failed: %v", err)
	}

	if name := sp.Name(); name != "test-lua-provider" {
		t.Errorf("expected name 'test-lua-provider', got '%s'", name)
	}

	resp, err := sp.Complete(context.Background(), CompleteRequest{
		System: "sys",
		User:   "hello-world",
		Model:  "lua-model",
	})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if resp.Text != "hello from lua" {
		t.Errorf("unexpected text response: %s", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "test_tool" {
		t.Errorf("unexpected tool name: %s", resp.ToolCalls[0].Name)
	}
	if resp.ToolCalls[0].Arguments["q"] != "world" {
		t.Errorf("unexpected tool call arguments: %v", resp.ToolCalls[0].Arguments)
	}

	// Rate limit error should be mapped to ProviderError with RATE_LIMITED marker
	_, err = sp.Complete(context.Background(), CompleteRequest{User: "fail"})
	if err == nil {
		t.Errorf("expected error for rate limit, got nil")
	} else if !strings.Contains(err.Error(), "RATE_LIMITED") {
		t.Errorf("expected rate limit marker in error, got: %v", err)
	}
}

func TestScriptProvider_HttpBridge(t *testing.T) {
	// HTTP test server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok_from_server"}`))
	}))
	defer ts.Close()

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "http_provider.lua")

	luaCode := `
provider = {
	name = "http-lua-provider",
	complete = function(req)
		local body, status, err = http.post("` + ts.URL + `", "test_body", {})
		if err ~= nil then
			return nil, err
		end
		return { text = body, stop_reason = "stop" }, nil
	end
}
`
	if err := os.WriteFile(scriptPath, []byte(luaCode), 0644); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	sp, err := NewScriptProvider(&config.ProviderConfig{Provider: "http-script"}, scriptPath)
	if err != nil {
		t.Fatalf("NewScriptProvider failed: %v", err)
	}

	resp, err := sp.Complete(context.Background(), CompleteRequest{User: "test"})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	if !strings.Contains(resp.Text, "ok_from_server") {
		t.Errorf("expected response to contain 'ok_from_server', got: %s", resp.Text)
	}
}

func TestScriptProvider_FsReadBridge(t *testing.T) {
	// Set up workspace with a test file
	wsRoot := t.TempDir()
	testContent := `{"key":"workspace_value"}`
	if err := os.WriteFile(filepath.Join(wsRoot, "config.json"), []byte(testContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Lua script reads config.json via fs.read and returns its content
	scriptPath := filepath.Join(t.TempDir(), "fs_provider.lua")
	luaCode := `
provider = {
	name = "fs-lua-provider",
	complete = function(req)
		local content, err = fs.read("config.json")
		if err ~= nil then
			return nil, err
		end
		return { text = content, stop_reason = "stop" }, nil
	end
}
`
	if err := os.WriteFile(scriptPath, []byte(luaCode), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ProviderConfig{
		Provider: "fs-script",
		Extra:    map[string]any{"workspace_root": wsRoot},
	}
	sp, err := NewScriptProvider(cfg, scriptPath)
	if err != nil {
		t.Fatalf("NewScriptProvider failed: %v", err)
	}

	resp, err := sp.Complete(context.Background(), CompleteRequest{User: "test"})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if !strings.Contains(resp.Text, "workspace_value") {
		t.Errorf("expected file content in response, got: %s", resp.Text)
	}
}

func TestScriptProvider_FsReadPathTraversalBlocked(t *testing.T) {
	wsRoot := t.TempDir()
	// Create a secret file OUTSIDE the workspace root
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP_SECRET"), 0644); err != nil {
		t.Fatal(err)
	}

	// Compute a relative path that escapes wsRoot to reach secretPath
	escapeRel, err := filepath.Rel(wsRoot, secretPath)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(t.TempDir(), "traversal_provider.lua")
	luaCode := fmt.Sprintf(`
provider = {
	name = "traversal-provider",
	complete = function(req)
		local content, err = fs.read("%s")
		if err ~= nil then
			return nil, err
		end
		return { text = content, stop_reason = "stop" }, nil
	end
}
`, escapeRel)
	if err := os.WriteFile(scriptPath, []byte(luaCode), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.ProviderConfig{
		Provider: "traversal-script",
		Extra:    map[string]any{"workspace_root": wsRoot},
	}
	sp, err := NewScriptProvider(cfg, scriptPath)
	if err != nil {
		t.Fatalf("NewScriptProvider failed: %v", err)
	}

	_, err = sp.Complete(context.Background(), CompleteRequest{User: "test"})
	if err == nil {
		t.Fatal("expected error for path traversal, got nil")
	}
	if !strings.Contains(err.Error(), "escapes workspace root") {
		t.Errorf("expected 'escapes workspace root' error, got: %v", err)
	}
}

func TestScriptProvider_FsNotAvailableWithoutWorkspaceRoot(t *testing.T) {
	scriptPath := filepath.Join(t.TempDir(), "no_fs_provider.lua")
	luaCode := `
provider = {
	name = "no-fs-provider",
	complete = function(req)
		-- fs should be nil when workspace_root is not configured
		if fs ~= nil then
			return nil, "fs should not be available"
		end
		return { text = "ok", stop_reason = "stop" }, nil
	end
}
`
	if err := os.WriteFile(scriptPath, []byte(luaCode), 0644); err != nil {
		t.Fatal(err)
	}

	// No workspace_root in Extra — fs module should not be registered
	sp, err := NewScriptProvider(&config.ProviderConfig{Provider: "no-fs-script"}, scriptPath)
	if err != nil {
		t.Fatalf("NewScriptProvider failed: %v", err)
	}

	resp, err := sp.Complete(context.Background(), CompleteRequest{User: "test"})
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("expected 'ok', got: %s", resp.Text)
	}
}
