package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ── ACP (Agent Client Protocol) Client ──────────────────────────────────────
//
// A lightweight JSON-RPC 2.0 client over an exec.Cmd stdio pipe. It lets the
// orchestrator delegate a DAG step to an external ACP-compliant coding agent
// (e.g. DeepSeek-Reasonix, DSH) while retaining orchestrator's L1-L3
// verification pipeline on the returned diff.
//
// See docs/completed/2026-10-28/ACP_CLIENT_SPAWNER_INTEGRATION_DESIGN.md.

// acpClientMessage is the union wire type for JSON-RPC responses and
// notifications. A message is a response when ID is non-empty; a notification
// when Method is non-empty and ID is absent.
type acpClientMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpClientError `json:"error,omitempty"`
}

type acpClientError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// acpRunTaskParams matches the server-side acpRunTaskParams in transport_acp.go.
type acpRunTaskParams struct {
	Prompt string `json:"prompt"`
	RoleID string `json:"role_id,omitempty"`
}

// ACPClient manages a JSON-RPC 2.0 session with an external ACP worker
// process over stdio. It is safe for sequential use (one request at a time).
type ACPClient struct {
	cmd   *exec.Cmd
	stdin interface {
		Write([]byte) (int, error)
		Close() error
	}
	stdout interface {
		Read([]byte) (int, error)
	}

	writeMu sync.Mutex // serialize writes to stdin
	idMu    sync.Mutex
	nextID  int64

	msgCh    chan *acpClientMessage // buffered channel from reader goroutine
	done     chan struct{}          // closed when reader goroutine exits
	quit     chan struct{}          // closed by Close to unblock readLoop send (audit R-28-1)
	quitOnce sync.Once              // guards quit from double-close panic
	readErr  error                  // set by reader goroutine before close(done)
	waitWg   sync.WaitGroup         // audit CRITICAL-005: tracks in-flight goroutines to prevent leaks

}

// NewACPClient starts an external ACP worker process and returns a client
// ready for Initialize. The caller must Close() when done.
func NewACPClient(binaryPath string, args []string, workDir string) (*ACPClient, error) {
	cmd := exec.Command(binaryPath, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	return acpClientFromCmd(cmd)
}

// acpClientFromCmd creates an ACPClient from a configured (not-yet-started)
// exec.Cmd. Used by NewACPClient and tests (which need custom env vars for
// the mock-server re-exec pattern).
func acpClientFromCmd(cmd *exec.Cmd) (*ACPClient, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // forward child stderr for debugging

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start process: %w", err)
	}

	c := &ACPClient{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		msgCh:  make(chan *acpClientMessage, 32),
		done:   make(chan struct{}),
		quit:   make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// readLoop reads newline-delimited JSON-RPC messages from stdout and
// dispatches them to msgCh. Exits when stdout is closed (child exited).
func (c *ACPClient) readLoop() {
	defer close(c.done)
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024) // 4 MB max line
	for scanner.Scan() {
		var msg acpClientMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue // skip malformed lines
		}
		// Audit R-28-1: select on quit so Close() can unblock us when
		// msgCh is full (cap 32) and no consumer is reading. Without this,
		// a plain send deadlocks Close() <-c.done if the child floods >32
		// notifications after the caller stops consuming.
		select {
		case c.msgCh <- &msg:
		case <-c.quit:
			return
		}
	}
	c.readErr = scanner.Err()
}

// sendRequest sends a JSON-RPC request and blocks until the matching response
// arrives. Notifications received while waiting are forwarded to onNotify.
func (c *ACPClient) sendRequest(
	ctx context.Context,
	method string,
	params any,
	onNotify func(*acpClientMessage),
) (*acpClientMessage, error) {
	c.idMu.Lock()
	c.nextID++
	id := c.nextID
	c.idMu.Unlock()

	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", id, method, params}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	c.writeMu.Lock()
	_, err = c.stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Wait for our response, dispatching any notifications along the way.
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-c.msgCh:
			if !ok {
				if c.readErr != nil {
					return nil, fmt.Errorf("connection closed: %w", c.readErr)
				}
				return nil, fmt.Errorf("connection closed")
			}
			if len(msg.ID) > 0 {
				var respID int64
				if json.Unmarshal(msg.ID, &respID) == nil && respID == id {
					return msg, nil
				}
				// Not our response — ignore (shouldn't happen in sequential mode).
				continue
			}
			if msg.Method != "" && onNotify != nil {
				onNotify(msg)
			}
		}
	}
}

// Initialize performs the ACP handshake. Must be called before ExecuteTask.
func (c *ACPClient) Initialize(ctx context.Context) error {
	resp, err := c.sendRequest(ctx, "initialize", map[string]any{
		"client":  "orchestrator",
		"version": "1.0",
	}, nil)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("initialize: [%d] %s", resp.Error.Code, resp.Error.Message)
	}
	return nil
}

// ExecuteTask sends a run_task request and waits for the task_completed or
// task_failed notification. Intermediate notifications are forwarded to
// onEvent. Returns the completion payload on success.
func (c *ACPClient) ExecuteTask(
	ctx context.Context,
	task, roleID string,
	onEvent func(method string, params map[string]any),
) (map[string]any, error) {
	// Phase 1: dispatch the task — server responds immediately with task_id.
	resp, err := c.sendRequest(ctx, "run_task", acpRunTaskParams{
		Prompt: task,
		RoleID: roleID,
	}, nil)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("run_task: [%d] %s", resp.Error.Code, resp.Error.Message)
	}

	// Phase 2: wait for task_completed / task_failed notification.
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case msg, ok := <-c.msgCh:
			if !ok {
				if c.readErr != nil {
					return nil, fmt.Errorf("connection closed while awaiting result: %w", c.readErr)
				}
				return nil, fmt.Errorf("connection closed while awaiting result")
			}
			switch msg.Method {
			case "task_completed":
				return decodeParams(msg.Params), nil
			case "task_failed":
				p := decodeParams(msg.Params)
				if e, ok := p["error"].(string); ok && e != "" {
					return nil, fmt.Errorf("task_failed: %s", e)
				}
				return nil, fmt.Errorf("task_failed")
			default:
				if onEvent != nil && msg.Method != "" {
					onEvent(msg.Method, decodeParams(msg.Params))
				}
			}
		}
	}
}

// decodeParams unmarshals JSON-RPC params into a map. Returns empty map on
// error or nil input — never nil.
func decodeParams(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

// Close shuts down the client: signals readLoop to stop, closes stdin, reaps
// the child process (with a 5 s grace period before killing), and ensures the
// reader goroutine has exited.
func (c *ACPClient) Close() error {
	// Audit R-28-1: signal readLoop to stop blocking on a full msgCh. If
	// msgCh is full (cap 32) and the caller has stopped consuming, readLoop
	// is blocked on the channel send — closing stdin or killing the child
	// cannot unblock it (it's not in scanner.Scan). Only quit can.
	c.quitOnce.Do(func() { close(c.quit) })

	// Close stdin to signal the child to exit gracefully.
	c.writeMu.Lock()
	_ = c.stdin.Close()
	c.writeMu.Unlock()

	// Reap the child with a 5 s grace period before killing. Killing the
	// child closes stdout, which unblocks readLoop's Scan if it was waiting
	// for data. (readLoop may also have exited already via quit.) We reap
	// in a goroutine so the timeout can trigger a kill; StdoutPipe uses an
	// *os.File so Wait does not race with readLoop's reads.
	waitErr := make(chan error, 1)
	c.waitWg.Add(1) // audit CRITICAL-005: track goroutine to prevent leak
	go func() {
		defer c.waitWg.Done()
		waitErr <- c.cmd.Wait()
	}()
	var err error
	select {
	case err = <-waitErr:
	case <-time.After(5 * time.Second):
		_ = c.cmd.Process.Kill()
		err = <-waitErr
	}

	// Ensure readLoop has exited so its goroutine doesn't leak. By now the
	// child has exited (stdout closed → Scan returns false) or readLoop
	// exited via quit; either way done is closed.
	<-c.done

	// audit CRITICAL-005: wait for all tracked goroutines to complete
	// before returning, ensuring no goroutine leak.
	c.waitWg.Wait()

	return err
}
