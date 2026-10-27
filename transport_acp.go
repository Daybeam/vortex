//go:build !web && !dev && !desktop

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/schemas"
)

// acpEngine is the minimal interface serveACP needs from the engine.
// *core.DirectedEngine satisfies this implicitly.
type acpEngine interface {
	Submit(inputs []schemas.StepInput) (string, error)
	CancelTask(taskID string) error
}

// --- JSON-RPC 2.0 wire types ---

type acpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type acpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *acpError       `json:"error,omitempty"`
}

type acpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type acpNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// --- ACP method params ---

type acpRunTaskParams struct {
	Prompt string `json:"prompt"`
	RoleID string `json:"role_id,omitempty"`
}

type acpCancelParams struct {
	TaskID string `json:"task_id"`
}

// serveACP runs the orchestrator as an ACP (Agent Client Protocol) server
// over stdio (newline-delimited JSON-RPC 2.0). Purely additive transport —
// zero core changes. See docs/plan/ACP_SERVER_TRANSPORT_DESIGN.md.
//
// Methods: initialize, ping, run_task, cancel.
// Notifications (server→client): task_completed, task_failed.
func serveACP(engine acpEngine, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024) // 4 MB max line

	var mu sync.Mutex // serialize writes to stdout (event goroutine + main loop)
	writeJSON := func(v any) {
		data, err := json.Marshal(v)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		_, _ = stdout.Write(append(data, '\n'))
	}

	// Event bridge: forward task terminal events as ACP notifications.
	unsub1 := core.DefaultBus.SubscribeWithCancel(string(core.EventTaskCompleted), func(ev core.AgentEvent) error {
		writeJSON(acpNotification{JSONRPC: "2.0", Method: "task_completed", Params: map[string]any{
			"task_id": ev.TaskID, "payload": ev.Payload,
		}})
		return nil
	})
	unsub2 := core.DefaultBus.SubscribeWithCancel(string(core.EventTaskFailed), func(ev core.AgentEvent) error {
		writeJSON(acpNotification{JSONRPC: "2.0", Method: "task_failed", Params: map[string]any{
			"task_id": ev.TaskID, "payload": ev.Payload,
		}})
		return nil
	})
	defer unsub1()
	defer unsub2()

	for scanner.Scan() {
		var req acpRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeJSON(acpResponse{JSONRPC: "2.0", ID: req.ID, Error: &acpError{Code: -32700, Message: "parse error"}})
			continue
		}

		var result any
		var e *acpError
		switch req.Method {
		case "initialize":
			result = map[string]any{"server": "orchestrator", "version": "1.0", "capabilities": []string{"run_task", "cancel", "streaming"}}
		case "ping":
			result = map[string]any{"pong": true}
		case "run_task":
			var p acpRunTaskParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				e = &acpError{Code: -32602, Message: "invalid params"}
				break
			}
			taskID, err := engine.Submit([]schemas.StepInput{{ID: "acp-1", Task: p.Prompt, RoleID: p.RoleID}})
			if err != nil {
				e = &acpError{Code: -32000, Message: err.Error()}
				break
			}
			result = map[string]any{"task_id": taskID}
		case "cancel":
			var p acpCancelParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				e = &acpError{Code: -32602, Message: "invalid params"}
				break
			}
			if err := engine.CancelTask(p.TaskID); err != nil {
				e = &acpError{Code: -32000, Message: err.Error()}
				break
			}
			result = map[string]any{"cancelled": true}
		default:
			e = &acpError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
		}
		writeJSON(acpResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: e})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[acp] scanner error: %v", err)
		return err
	}
	return nil
}
