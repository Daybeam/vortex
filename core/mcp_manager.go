package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/client"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/safelimits"
	"github.com/daybeam/vortex/schemas"
)

// MCPConnectionManager isolates all stdio subprocess handling and JSON-RPC
// transport from the agent execution loop. Extracted from Spawner (Phase 2
// of SPAWNER_REFACTORING_PLAN.md) to improve maintainability and testability.
type MCPConnectionManager struct {
	mu                 sync.Mutex
	processCache       map[string]*client.MCPClient
	remoteSessionCache map[string]string
	mcpSpawnFailures   map[string]time.Time
	handshakeLocks     sync.Map // mcpID -> *sync.Mutex; per-ID handshake serialization (audit H4)
	discoveryLocks     sync.Map // mcpID -> *sync.Mutex; per-ID FullToolDefinitions serialization (audit H8)
	registry           *config.Registry
	logger             *Logger
	httpClient         *http.Client // shared for connection pooling (audit H4)
}

func NewMCPConnectionManager(registry *config.Registry, logger *Logger) *MCPConnectionManager {
	return &MCPConnectionManager{
		processCache:       make(map[string]*client.MCPClient),
		remoteSessionCache: make(map[string]string),
		mcpSpawnFailures:   make(map[string]time.Time),
		registry:           registry,
		logger:             logger,
		httpClient:         &http.Client{Timeout: 60 * time.Second},
	}
}

// discoveryLock returns a per-MCP-ID mutex for serializing access to
// FullToolDefinitions. This prevents concurrent discovery goroutines from
// racing on the same MCP's tool-definition slice (audit H8).
func (m *MCPConnectionManager) discoveryLock(mcpID string) *sync.Mutex {
	v, _ := m.discoveryLocks.LoadOrStore(mcpID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// EvictClient removes a cached MCP client, forcing a fresh spawn on next
// ensureMCPClient call. Used by the execution loop's reconnect-on-error path.
func (m *MCPConnectionManager) EvictClient(mcpID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cli, ok := m.processCache[mcpID]; ok {
		cli.Close()
		delete(m.processCache, mcpID)
	}
}

// CloseAll closes every cached MCP subprocess client. Call on shutdown to
// prevent orphaned subprocesses (audit finding C2).
func (m *MCPConnectionManager) CloseAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, cli := range m.processCache {
		cli.Close()
		delete(m.processCache, id)
	}
}

// mcpSpawnFailureTTL defines how long to keep an MCP in the failure ledger
// before retrying it.
const mcpSpawnFailureTTL = 300 * time.Second

func (m *MCPConnectionManager) markMCPSpawnFailed(mcpID string) {
	if m == nil || mcpID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordMCPStatusLocked(mcpID, false)
}

func (m *MCPConnectionManager) clearMCPSpawnFailed(mcpID string) {
	if m == nil || mcpID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordMCPStatusLocked(mcpID, true)
}

// recordMCPStatusLocked updates the failure ledger. Caller MUST hold m.mu.
func (m *MCPConnectionManager) recordMCPStatusLocked(mcpID string, ok bool) {
	if ok {
		delete(m.mcpSpawnFailures, mcpID)
		return
	}
	if m.mcpSpawnFailures == nil {
		m.mcpSpawnFailures = make(map[string]time.Time)
	}
	m.mcpSpawnFailures[mcpID] = time.Now()
}

// hasMCPSpawnFailed reports whether an MCP recently failed to spawn/handshake.
// Expired entries are lazily dropped.
func (m *MCPConnectionManager) hasMCPSpawnFailed(mcpID string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.mcpSpawnFailures[mcpID]
	if !ok {
		return false
	}
	if time.Since(t) > mcpSpawnFailureTTL {
		delete(m.mcpSpawnFailures, mcpID)
		return false
	}
	return true
}

func resolveMCPURL(mcp *config.MCPDef) (string, error) {
	if mcp.URL == "" {
		return "", fmt.Errorf("mcp %q has no URL", mcp.ID)
	}
	if mcp.URLAuthPlaceholder == "" {
		return mcp.URL, nil
	}
	if mcp.APIKeyEnv == "" {
		return "", fmt.Errorf("mcp %q has url_auth_placeholder set but no api_key_env configured", mcp.ID)
	}
	secret := os.Getenv(mcp.APIKeyEnv)
	if secret == "" {
		return "", fmt.Errorf("mcp %q requires env var %q for its URL auth placeholder, but it is unset or empty", mcp.ID, mcp.APIKeyEnv)
	}
	if !strings.Contains(mcp.URL, mcp.URLAuthPlaceholder) {
		return "", fmt.Errorf("mcp %q url_auth_placeholder %q not found in its configured url", mcp.ID, mcp.URLAuthPlaceholder)
	}
	return strings.ReplaceAll(mcp.URL, mcp.URLAuthPlaceholder, secret), nil
}

func (m *MCPConnectionManager) ensureRemoteMCPSession(ctx context.Context, mcp *config.MCPDef) string {
	m.mu.Lock()
	sid, cached := m.remoteSessionCache[mcp.ID]
	m.mu.Unlock()
	if cached {
		return sid
	}

	// Per-MCP-ID lock so concurrent first-access for the same remote MCP
	// doesn't trigger N duplicate handshakes (audit H4 TOCTOU race).
	v, _ := m.handshakeLocks.LoadOrStore(mcp.ID, &sync.Mutex{})
	idLock := v.(*sync.Mutex)
	idLock.Lock()
	defer idLock.Unlock()

	// Double-check after acquiring per-ID lock — another goroutine may
	// have completed the handshake while we were waiting.
	m.mu.Lock()
	sid, cached = m.remoteSessionCache[mcp.ID]
	m.mu.Unlock()
	if cached {
		return sid
	}

	sid = m.performRemoteMCPHandshake(ctx, mcp)

	m.mu.Lock()
	if m.remoteSessionCache == nil {
		m.remoteSessionCache = make(map[string]string)
	}
	m.remoteSessionCache[mcp.ID] = sid
	m.mu.Unlock()
	return sid
}

func (m *MCPConnectionManager) performRemoteMCPHandshake(ctx context.Context, mcp *config.MCPDef) string {
	resolvedURL, err := resolveMCPURL(mcp)
	if err != nil {
		return ""
	}

	applyAuth := func(req *http.Request) {
		if mcp.AuthHeaderName == "" || mcp.APIKeyEnv == "" {
			return
		}
		if secret := os.Getenv(mcp.APIKeyEnv); secret != "" {
			req.Header.Set(mcp.AuthHeaderName, mcp.AuthHeaderPrefix+secret)
		}
	}

	initBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "orchestrator-mcp-go", "version": "1.0.0"},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolvedURL, bytes.NewReader(initBody))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyAuth(req)

	httpClient := m.httpClient // audit H4: reuse shared client for connection pooling
	if httpClient == nil {
		httpClient = http.DefaultClient // fallback for bare struct literals
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		log.Printf("WARN: mcp_manager: failed to drain init response body: %v", err)
	}

	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		return ""
	}

	notifBody, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	notifReq, err := http.NewRequestWithContext(ctx, http.MethodPost, resolvedURL, bytes.NewReader(notifBody))
	if err == nil {
		notifReq.Header.Set("Content-Type", "application/json")
		notifReq.Header.Set("Accept", "application/json, text/event-stream")
		notifReq.Header.Set("Mcp-Session-Id", sid)
		applyAuth(notifReq)
		if notifResp, nerr := httpClient.Do(notifReq); nerr == nil {
			if _, err := io.Copy(io.Discard, notifResp.Body); err != nil {
				log.Printf("WARN: mcp_manager: failed to drain notification response body: %v", err)
			}
			notifResp.Body.Close()
		}
	}

	return sid
}

func (m *MCPConnectionManager) discoverRemoteMCPTools(ctx context.Context, mcp *config.MCPDef) ([]schemas.ToolDefinition, error) {
	resolvedURL, err := resolveMCPURL(mcp)
	if err != nil {
		return nil, fmt.Errorf("discoverRemoteMCPTools: %w", err)
	}

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolvedURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("discoverRemoteMCPTools: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if tid := GetTraceID(ctx); tid != "" {
		req.Header.Set("x-trace-id", tid)
	}
	if sid := GetSpanID(ctx); sid != "" {
		req.Header.Set("x-span-id", sid)
	}
	if sid := m.ensureRemoteMCPSession(ctx, mcp); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	if mcp.AuthHeaderName != "" {
		if mcp.APIKeyEnv == "" {
			return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q has auth_header_name set but no api_key_env configured", mcp.ID)
		}
		secret := os.Getenv(mcp.APIKeyEnv)
		if secret == "" {
			return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q requires env var %q for auth, but it is unset or empty", mcp.ID, mcp.APIKeyEnv)
		}
		req.Header.Set(mcp.AuthHeaderName, mcp.AuthHeaderPrefix+secret)
	}

	httpClient := m.httpClient // audit H4: reuse shared client
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discoverRemoteMCPTools: request failed: %w", err)
	}
	defer resp.Body.Close()

	// audit H1: cap response to prevent OOM from unbounded responses
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("discoverRemoteMCPTools: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q returned HTTP %d: %s", mcp.ID, resp.StatusCode, truncateForLog(string(respBody), 300))
	}

	var parsed map[string]any
	if err := json.Unmarshal(extractSSEBody(respBody), &parsed); err != nil {
		return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q returned non-JSON response: %w", mcp.ID, err)
	}

	resultObj := parsed
	if r, ok := parsed["result"].(map[string]any); ok {
		resultObj = r
	}
	if errObj, ok := parsed["error"]; ok {
		return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q returned JSON-RPC error: %v", mcp.ID, errObj)
	}

	toolsRaw, ok := resultObj["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("discoverRemoteMCPTools: mcp %q response had no \"tools\" array", mcp.ID)
	}

	var out []schemas.ToolDefinition
	for _, tRaw := range toolsRaw {
		t, ok := tRaw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := t["description"].(string)
		def := schemas.ToolDefinition{Name: name, Description: desc}
		if is, ok := t["inputSchema"].(map[string]any); ok {
			def.InputSchema = is
		}
		out = append(out, def)
	}
	return out, nil
}

func (m *MCPConnectionManager) callRemoteMCPTool(ctx context.Context, mcp *config.MCPDef, toolName string, arguments map[string]any) (any, error) {
	resolvedURL, err := resolveMCPURL(mcp)
	if err != nil {
		return nil, fmt.Errorf("callRemoteMCPTool: %w", err)
	}

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": arguments,
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resolvedURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("callRemoteMCPTool: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if tid := GetTraceID(ctx); tid != "" {
		req.Header.Set("x-trace-id", tid)
	}
	if sid := GetSpanID(ctx); sid != "" {
		req.Header.Set("x-span-id", sid)
	}
	if sid := m.ensureRemoteMCPSession(ctx, mcp); sid != "" {
		req.Header.Set("Mcp-Session-Id", sid)
	}

	if mcp.AuthHeaderName != "" {
		if mcp.APIKeyEnv == "" {
			return nil, fmt.Errorf("callRemoteMCPTool: mcp %q has auth_header_name set but no api_key_env configured", mcp.ID)
		}
		secret := os.Getenv(mcp.APIKeyEnv)
		if secret == "" {
			return nil, fmt.Errorf("callRemoteMCPTool: mcp %q requires env var %q for auth, but it is unset or empty", mcp.ID, mcp.APIKeyEnv)
		}
		req.Header.Set(mcp.AuthHeaderName, mcp.AuthHeaderPrefix+secret)
	}

	httpClient := m.httpClient // audit H4: reuse shared client
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("callRemoteMCPTool: request failed: %w", err)
	}
	defer resp.Body.Close()

	// audit H1: cap response to prevent OOM from unbounded responses
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, safelimits.MaxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("callRemoteMCPTool: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("callRemoteMCPTool: mcp %q tool %q returned HTTP %d: %s", mcp.ID, toolName, resp.StatusCode, truncateForLog(string(respBody), 300))
	}

	var parsed map[string]any
	if err := json.Unmarshal(extractSSEBody(respBody), &parsed); err != nil {
		return nil, fmt.Errorf("callRemoteMCPTool: mcp %q tool %q returned non-JSON response: %w", mcp.ID, toolName, err)
	}

	if errObj, ok := parsed["error"]; ok {
		return nil, fmt.Errorf("callRemoteMCPTool: mcp %q tool %q returned JSON-RPC error: %v", mcp.ID, toolName, errObj)
	}

	if result, ok := parsed["result"]; ok {
		return result, nil
	}
	return parsed, nil
}

func (m *MCPConnectionManager) ensureMCPClient(ctx context.Context, mcp *config.MCPDef, taskID string) (*client.MCPClient, error) {
	m.mu.Lock()
	cli, ok := m.processCache[mcp.ID]
	if ok && !cli.IsDead() {
		m.mu.Unlock()
	} else {
		if ok && cli.IsDead() {
			delete(m.processCache, mcp.ID)
		}
		m.mu.Unlock()

		cmd, args, mcpDir, mcpEnv := mcp.ResolveForPlatform()
		if mcpEnv == nil {
			mcpEnv = make(map[string]string)
		}
		if tid := GetTraceID(ctx); tid != "" {
			mcpEnv["VORTEX_TRACE_ID"] = tid
		}
		if sid := GetSpanID(ctx); sid != "" {
			mcpEnv["VORTEX_SPAN_ID"] = sid
		}

		_ = client.KillProcessTree(cmd)

		resolvedDir := mcpDir
		if resolvedDir != "" && !filepath.IsAbs(resolvedDir) {
			exePath, _ := os.Executable()
			baseDir := filepath.Dir(filepath.Clean(exePath))
			resolvedDir = filepath.Clean(filepath.Join(baseDir, resolvedDir))
		}

		var constraints *client.SandboxConstraints
		if mcp.Sandboxed {
			memMB := m.registry.System.SandboxedMemoryMB
			if memMB <= 0 {
				memMB = 256
			}
			cpuSecs := m.registry.System.SandboxedCPUSecs
			if cpuSecs <= 0 {
				cpuSecs = 30
			}
			constraints = &client.SandboxConstraints{
				MaxMemoryBytes: uint64(memMB) * 1024 * 1024,
				MaxCPUSeconds:  uint64(cpuSecs),
				KillOnClose:    true,
			}
		}

		newCli, err := client.NewMCPClient(cmd, args, resolvedDir, mcpEnv, constraints)
		if err != nil {
			m.markMCPSpawnFailed(mcp.ID)
			return nil, err
		}

		m.mu.Lock()
		if existing, exists := m.processCache[mcp.ID]; exists && !existing.IsDead() {
			newCli.Close()
			cli = existing
		} else {
			m.processCache[mcp.ID] = newCli
			cli = newCli
		}
		m.mu.Unlock()
	}

	if !cli.IsInitialized() {
		initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := cli.Initialize(initCtx, "2024-11-05"); err != nil {
			m.mu.Lock()
			if current, ok := m.processCache[mcp.ID]; ok && current == cli {
				delete(m.processCache, mcp.ID)
			}
			m.mu.Unlock()
			m.markMCPSpawnFailed(mcp.ID)
			cli.Close()
			return nil, fmt.Errorf("MCP %s handshake failed: %w", mcp.ID, err)
		}
		m.logger.Log("EventMCPHandshakeComplete", taskID, "", map[string]any{
			"mcp": mcp.ID,
		})
	}

	return cli, nil
}
