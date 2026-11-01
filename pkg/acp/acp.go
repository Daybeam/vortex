package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// Engine is the minimal interface ServeACP needs from the engine.
// *core.DirectedEngine satisfies this implicitly.
type Engine interface {
	Submit(inputs []schemas.StepInput) (string, error)
	CancelTask(taskID string) error
}

// --- JSON-RPC 2.0 wire types ---

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the JSON-RPC 2.0 response envelope. Exported for test helpers
// that parse and assert on server output.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// --- ACP method params ---

type runTaskParams struct {
	Prompt string `json:"prompt"`
	RoleID string `json:"role_id,omitempty"`
}

type cancelParams struct {
	TaskID string `json:"task_id"`
}

// ServeACP runs the orchestrator as an ACP (Agent Client Protocol) server
// over stdio (newline-delimited JSON-RPC 2.0). Purely additive transport —
// zero core changes. See docs/completed/2026-10-28/ACP_SERVER_TRANSPORT_DESIGN.md.
//
// Methods: initialize, ping, run_task, cancel.
// Notifications (server→client): task_completed, task_failed.
func ServeACP(engine Engine, stdin io.Reader, stdout io.Writer) error {
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
	unsub1 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskCompleted), func(ev obs.AgentEvent) error {
		writeJSON(notification{JSONRPC: "2.0", Method: "task_completed", Params: map[string]any{
			"task_id": ev.TaskID, "payload": ev.Payload,
		}})
		return nil
	})
	unsub2 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskFailed), func(ev obs.AgentEvent) error {
		writeJSON(notification{JSONRPC: "2.0", Method: "task_failed", Params: map[string]any{
			"task_id": ev.TaskID, "payload": ev.Payload,
		}})
		return nil
	})
	defer unsub1()
	defer unsub2()

	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}

		var result any
		var e *rpcError
		switch req.Method {
		case "initialize":
			result = map[string]any{"server": "orchestrator", "version": "1.0", "capabilities": []string{"run_task", "cancel", "streaming"}}
		case "ping":
			result = map[string]any{"pong": true}
		case "run_task":
			var p runTaskParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				e = &rpcError{Code: -32602, Message: "invalid params"}
				break
			}
			taskID, err := engine.Submit([]schemas.StepInput{{ID: "acp-1", Task: p.Prompt, RoleID: p.RoleID}})
			if err != nil {
				e = &rpcError{Code: -32000, Message: err.Error()}
				break
			}
			result = map[string]any{"task_id": taskID}
		case "cancel":
			var p cancelParams
			if err := json.Unmarshal(req.Params, &p); err != nil {
				e = &rpcError{Code: -32602, Message: "invalid params"}
				break
			}
			if err := engine.CancelTask(p.TaskID); err != nil {
				e = &rpcError{Code: -32000, Message: err.Error()}
				break
			}
			result = map[string]any{"cancelled": true}
		default:
			e = &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
		}
		writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: e})
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[acp] scanner error: %v", err)
		return err
	}
	return nil
}
