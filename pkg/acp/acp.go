package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"

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
	Data    any    `json:"data,omitempty"`
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

	// Standard ACP state (lazy-initialized on first standard initialize)
	var stdServer *standardServer
	var promptWg sync.WaitGroup
	done := make(chan struct{})
	var stdMode atomic.Bool

	// VORTEX_ACP_DIALECT env: "standard" forces standard mode, "legacy" forces
	// legacy mode, "auto" (default) detects via protocolVersion in initialize.
	dialectOverride := os.Getenv("VORTEX_ACP_DIALECT")

	// Event bridge: forward task terminal events as ACP notifications.
	// Suppressed in standard mode — the standardServer handles terminal
	// events via its own buffer + session/update streaming.
	unsub1 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskCompleted), func(ev obs.AgentEvent) error {
		if stdMode.Load() {
			return nil
		}
		writeJSON(notification{JSONRPC: "2.0", Method: "task_completed", Params: map[string]any{
			"task_id": ev.TaskID, "payload": ev.Payload,
		}})
		return nil
	})
	unsub2 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskFailed), func(ev obs.AgentEvent) error {
		if stdMode.Load() {
			return nil
		}
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

		// Standard ACP dialect detection.
		// VORTEX_ACP_DIALECT=standard forces standard; =legacy forces legacy;
		// auto/empty detects via protocolVersion in initialize params.
		useStandard := false
		if req.Method == "initialize" {
			switch dialectOverride {
			case "standard":
				useStandard = true
			case "legacy":
				useStandard = false
			default: // "auto" or empty
				useStandard = hasProtocolVersion(req.Params)
			}
		}

		if useStandard {
			stdMode.Store(true)
			if stdServer == nil {
				stdServer = newStandardServer(engine, writeJSON, done)
				defer stdServer.close()
			}
			clientVersion := extractProtocolVersion(req.Params)
			writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Result: stdServer.handleInitialize(clientVersion)})
			continue
		}

		// Standard ACP method routing.
		if stdServer != nil {
			switch req.Method {
			case "session/new":
				result, errResp := stdServer.handleNewSession(req.Params)
				if errResp != nil {
					writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Error: errResp})
				} else {
					writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Result: result})
				}
			case "session/prompt":
				promptWg.Add(1)
				go func() {
					defer promptWg.Done()
					stdServer.handlePrompt(req.Params, req.ID)
				}()
			case "session/cancel":
				stdServer.handleCancel(req.Params)
			case "ping":
				writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"pong": true}})
			default:
				writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}})
			}
			continue
		}

		// Legacy DSH dialect.
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
				e = &rpcError{Code: -32603, Message: err.Error()}
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
				e = &rpcError{Code: -32603, Message: err.Error()}
				break
			}
			result = map[string]any{"cancelled": true}
		default:
			e = &rpcError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
		}
		writeJSON(Response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: e})
	}

	close(done)
	promptWg.Wait()

	if err := scanner.Err(); err != nil {
		log.Printf("[acp] scanner error: %v", err)
		return err
	}
	return nil
}
