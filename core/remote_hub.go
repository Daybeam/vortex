package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/pkg/types"
	"github.com/daybeam/vortex/schemas"
)

// RemoteTaskHub bridges a Worker node to a Hub node via HTTP.
type RemoteTaskHub struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Spawner    *Spawner
	notifyChan chan struct{}

	// Cache for currently claimed task data (protected by mu)
	mu           sync.Mutex
	currentStep  *schemas.Step
	currentGraph *schemas.TaskGraph

	// stopChan stops the background pulse goroutine (audit: was goroutine leak)
	stopChan chan struct{}
}

var _ interfaces.TaskHubInterface = (*RemoteTaskHub)(nil)

func NewRemoteTaskHub(baseURL, apiKey string, spawner *Spawner) *RemoteTaskHub {
	h := &RemoteTaskHub{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 100}},
		Spawner:    spawner,
		notifyChan: make(chan struct{}),
		stopChan:   make(chan struct{}),
	}
	// Background "pulse" to trigger sensing if Hub doesn't support push notifications yet
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[remote_hub] pulse goroutine panic: %v\n%s", r, debug.Stack())
			}
		}()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-h.stopChan:
				return
			case <-ticker.C:
				select {
				case h.notifyChan <- struct{}{}:
				default:
				}
			}
		}
	}()
	return h
}

// Stop terminates the background pulse goroutine (audit: was goroutine leak).
func (h *RemoteTaskHub) Stop() {
	close(h.stopChan)
}

func (h *RemoteTaskHub) GetSignalField() interfaces.SignalFieldInterface {
	return &RemoteSignalField{Hub: h}
}

func (h *RemoteTaskHub) ClaimTask(stepID string) bool {
	url := fmt.Sprintf("%s/api/swarm/claim", h.BaseURL)
	body, _ := json.Marshal(map[string]string{"step_id": stepID})

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Authorization", h.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() // audit M15: close body on all paths (was skipped on non-OK status)
	if resp.StatusCode != http.StatusOK {
		return false
	}

	var data struct {
		Graph *schemas.TaskGraph `json:"graph"`
		Step  *schemas.Step      `json:"step"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return false
	}

	h.mu.Lock()
	h.currentGraph = data.Graph
	h.currentStep = data.Step
	h.mu.Unlock()
	return true
}

func (h *RemoteTaskHub) ReleaseTask(stepID string) {
	url := fmt.Sprintf("%s/api/swarm/release", h.BaseURL)
	body, _ := json.Marshal(map[string]string{"step_id": stepID})

	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(body))
	req.Header.Set("Authorization", h.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.HTTPClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func (h *RemoteTaskHub) ExecuteTask(ctx context.Context, stepID string) (*schemas.SubagentOutput, error) {
	// Copy pointers under lock to avoid data race with ClaimTask (audit C5)
	h.mu.Lock()
	step := h.currentStep
	graph := h.currentGraph
	h.mu.Unlock()

	if step == nil || step.ID != stepID {
		return nil, fmt.Errorf("task %s not claimed or data missing", stepID)
	}

	// Construct SpawnRequest
	spawnReq := &SpawnRequest{
		TaskID:   graph.TaskID,
		StepID:   stepID,
		RoleID:   step.RoleID,
		Task:     step.Task,
		Metadata: nil, // Metadata type mismatch, skip for worker remote
		Hub:      NewContextHub(h.Spawner.registry, graph, h.Spawner.expStore),
	}

	// Execute locally using worker's spawner
	res, err := h.Spawner.Spawn(ctx, spawnReq)
	var output *schemas.SubagentOutput
	if err != nil || res == nil {
		output = &schemas.SubagentOutput{
			Status:     schemas.StatusFailed,
			Confidence: 0,
			Result:     map[string]any{"error": fmt.Sprintf("spawn failed: %v", err)},
		}
	} else {
		output = &res.Output
	}

	// Yield result back to Hub
	yieldURL := fmt.Sprintf("%s/api/swarm/yield", h.BaseURL)
	yieldBody, _ := json.Marshal(map[string]any{
		"step_id": stepID,
		"output":  output,
	})

	yReq, _ := http.NewRequest("POST", yieldURL, bytes.NewBuffer(yieldBody))
	yReq.Header.Set("Authorization", h.APIKey)
	yReq.Header.Set("Content-Type", "application/json")

	// audit M14: close response body to prevent connection pool exhaustion
	if yieldResp, err := h.HTTPClient.Do(yReq); err == nil {
		yieldResp.Body.Close()
	}

	return output, nil
}

func (h *RemoteTaskHub) GetNotifyChan() chan struct{} {
	return h.notifyChan
}

func (h *RemoteTaskHub) GetRoleAffinity(roleID, stepID string) float64 {
	// Worker nodes currently assume neutral affinity unless they have local history
	// Or we could implement an API call: GET /api/swarm/affinity?role_id=...&step_id=...
	return 0.5
}

// RemoteSignalField proxies signal sensing to the Hub.
type RemoteSignalField struct {
	Hub *RemoteTaskHub
}

var _ interfaces.SignalFieldInterface = (*RemoteSignalField)(nil)

func (f *RemoteSignalField) SensingGlobal(limit int, agentLoad float64) []string {
	url := fmt.Sprintf("%s/api/swarm/sensing", f.Hub.BaseURL)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", f.Hub.APIKey)

	resp, err := f.Hub.HTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close() // audit M16: close body on all paths (was skipped on non-OK status)
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var taskIDs []string
	json.NewDecoder(resp.Body).Decode(&taskIDs)
	return taskIDs
}

func (f *RemoteSignalField) Sensing(layer types.SignalLayer, limit int) []string {
	// For now, proxy to SensingGlobal or ignore layer
	return f.SensingGlobal(limit, 0)
}

func (f *RemoteSignalField) Deposit(layer types.SignalLayer, taskID string, value float64) {
	// Workers typically don't deposit signals directly to Hub's field via API yet
}

func (f *RemoteSignalField) GetEffectiveSignal(taskID string, agentLoad float64) float64 {
	return 1.0
}
