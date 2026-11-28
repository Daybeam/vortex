package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// RemoteProvider implements SandboxProvider by forwarding execution requests
// to a remote HTTP sandbox service. This is the Android-first sandbox backend
// (per ANDROID_PORT_ANALYSIS.md §4) — local/docker/bubblewrap sandboxes are
// unavailable on Android, so remote is the only option.
//
// The remote sandbox protocol is intentionally simple:
//   POST /execute   → ExecRequest JSON → ExecResult JSON
//   POST /session/:id/close → 200 OK
//   GET  /health    → 200 OK
//
// Authentication: Bearer token in Authorization header.
//
// Desktop benefit: enables sandbox execution without local Docker install.
type RemoteProvider struct {
	baseURL string
	token   string
	client  *http.Client
}

// remoteExecuteRequest is the wire format for POST /execute.
type remoteExecuteRequest struct {
	Script    string            `json:"script"`
	Language  string            `json:"language"`
	SessionID string            `json:"session_id,omitempty"`
	TimeoutMs int64             `json:"timeout_ms,omitempty"`
	MemoryMB  uint64            `json:"memory_mb,omitempty"`
	EnvVars   map[string]string `json:"env_vars,omitempty"`
	Dir       string            `json:"dir,omitempty"`
}

// remoteExecuteResponse is the wire format for the /execute response.
type remoteExecuteResponse struct {
	SessionID string `json:"session_id,omitempty"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	DurationMs int64 `json:"duration_ms"`
}

func NewRemoteProvider(baseURL, token string) *RemoteProvider {
	return &RemoteProvider{
		baseURL: baseURL,
		token:   token,
		client: &http.Client{
			Timeout: 120 * time.Second, // align with webhookDeliveryTimeout pattern
		},
	}
}

func (p *RemoteProvider) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	startTime := time.Now()

	payload := remoteExecuteRequest{
		Script:    req.Script,
		Language:  req.Language,
		SessionID: req.SessionID,
		MemoryMB:  req.MemoryMB,
		EnvVars:   req.EnvVars,
		Dir:       req.Dir,
	}
	if req.Timeout > 0 {
		payload.TimeoutMs = req.Timeout.Milliseconds()
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("remote-sandbox: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("remote-sandbox: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("remote-sandbox: execute request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("remote-sandbox: server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result remoteExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("remote-sandbox: decode response: %w", err)
	}

	return &ExecResult{
		SessionID: result.SessionID,
		Stdout:    result.Stdout,
		Stderr:    result.Stderr,
		ExitCode:  result.ExitCode,
		Duration:  time.Since(startTime),
	}, nil
}

func (p *RemoteProvider) CloseSession(sessionID string) error {
	httpReq, err := http.NewRequest(http.MethodPost, p.baseURL+"/session/"+sessionID+"/close", nil)
	if err != nil {
		return fmt.Errorf("remote-sandbox: create close request: %w", err)
	}
	if p.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("remote-sandbox: close session request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote-sandbox: close session returned %d", resp.StatusCode)
	}
	return nil
}

func (p *RemoteProvider) HealthCheck(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("remote-sandbox: create health check request: %w", err)
	}
	if p.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.token)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("remote-sandbox: health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote-sandbox: health check returned %d", resp.StatusCode)
	}
	return nil
}

// ApplyExistingSandbox is not supported for remote sandboxes — resource
// limits are enforced by the remote service, not the local caller.
// Returns a no-op cleanup for interface compliance.
func (p *RemoteProvider) ApplyExistingSandbox(pid int, memoryMB uint64, cpuSecs uint64) (func(), error) {
	return func() {}, nil
}
