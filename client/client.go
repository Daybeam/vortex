package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// MCPClient manages a local stdio MCP server process.
type MCPClient struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	scanner        *bufio.Scanner
	stderrBuf      bytes.Buffer
	stderrMu       sync.Mutex
	writeMu        sync.Mutex
	pending        sync.Map // map[string]chan rpcResponse, keyed by request id
	nextID         atomic.Int64
	done           chan struct{}
	readErr        atomic.Value
	sandboxCleanup func() // released in Close(); nil if not sandboxed or ApplySandbox failed
	initialized    atomic.Bool
	closeOnce      sync.Once // ensures Close() is idempotent (audit M5)
}

type rpcResponse struct {
	data []byte
	err  error
}

// applyWindowsNpxShimEnv scopes the npx.exe ENOENT workaround (see F20 in
// orchestrator-mcp-go-debug-playbook-addendum-2026-07-03.md) to only the
// MCP subprocess being spawned right here, without touching the machine's
// PATH or Claude Desktop's own environment at all. If a built npx.exe shim
// exists at tools/win-shims/npx-shim/npx.exe next to the running
// orchestrator binary, that directory is prepended to PATH for this one
// child process (and its own children, e.g. lsmcp's internal spawn('npx')
// call). No-op on non-Windows. No-op (leaves cmd.Env nil, i.e. Go's default
// full-environment inheritance) if the shim hasn't been built yet -- this
// must never be the reason a working MCP subprocess fails to start.
func applyWindowsNpxShimEnv(cmd *exec.Cmd) {
	if runtime.GOOS != "windows" {
		return
	}
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	shimDir := filepath.Join(filepath.Dir(exePath), "tools", "win-shims", "npx-shim")
	if _, err := os.Stat(filepath.Join(shimDir, "npx.exe")); err != nil {
		return // shim not built yet -- do nothing
	}

	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	found := false
	for i, kv := range env {
		// Windows env var names are case-insensitive ("Path" vs "PATH" both
		// show up depending on the process that set them); match by prefix
		// case-insensitively rather than assuming one specific casing.
		if len(kv) >= 5 && strings.EqualFold(kv[:5], "PATH=") {
			env[i] = "PATH=" + shimDir + string(os.PathListSeparator) + kv[5:]
			found = true
			break
		}
	}
	if !found {
		env = append(env, "PATH="+shimDir)
	}
	cmd.Env = env
}

func NewMCPClient(command string, args []string, dir string, env map[string]string, constraints *SandboxConstraints) (*MCPClient, error) {
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		e := os.Environ()
		for k, v := range env {
			e = append(e, k+"="+v)
		}
		cmd.Env = e
	}
	applyWindowsNpxShimEnv(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var sandboxCleanup func()
	if constraints != nil {
		// Apply sandbox constraints to the child process. A failure here is
		// logged to stderr but is not fatal to spawning the client -- the
		// caller asked for a JIT/untrusted tool to run sandboxed, and refusing
		// to start it at all on a sandbox-setup error would be a bigger
		// availability regression than running one unsandboxed instance while
		// the failure is visible in logs for follow-up.
		cleanup, sbErr := ApplySandbox(cmd.Process.Pid, *constraints)
		if sbErr != nil {
			fmt.Fprintf(os.Stderr, "[jit-sandbox] WARNING: failed to sandbox pid=%d: %v (tool will run unsandboxed)\n", cmd.Process.Pid, sbErr)
		} else {
			sandboxCleanup = cleanup
		}
	}

	c := &MCPClient{
		cmd:            cmd,
		stdin:          stdin,
		scanner:        bufio.NewScanner(stdout),
		done:           make(chan struct{}),
		sandboxCleanup: sandboxCleanup,
	}
	// Allow up to 1MB per line for large tool responses
	c.scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	// Capture stderr in a buffer (thread-safe via stderrMu)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				c.stderrMu.Lock()
				c.stderrBuf.Write(buf[:n])
				c.stderrMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// FIX (2026-06-30, re-applying the 2026-06-24 readLoop/sync.Map fix
	// that had silently regressed back to the old per-call-goroutine
	// pattern -- see client.go.bak_readloop_reapply_20260630 for the
	// regressed version this replaces).
	//
	// A single dedicated goroutine is the only reader of c.scanner for
	// the lifetime of this client. Responses are dispatched to whichever
	// SendRequest call is waiting on the matching request id via a
	// sync.Map of channels. This eliminates the previous pattern, where
	// each SendRequest spawned its own ad-hoc goroutine calling
	// c.scanner.Scan(): if a request timed out while its scan goroutine
	// was still blocked waiting for a slow tool call (a goroutine leak),
	// a subsequent request's new scan goroutine could end up racing with
	// the leaked one on the same shared, non-concurrency-safe
	// bufio.Scanner -- silently cross-wiring a response meant for one
	// request to a completely different, unrelated request.
	go c.readLoop()

	return c, nil
}

// stderrString returns the accumulated stderr output in a thread-safe manner.
func (c *MCPClient) stderrString() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return c.stderrBuf.String()
}

func (c *MCPClient) readLoop() {
	defer close(c.done)

	for c.scanner.Scan() {
		line := make([]byte, len(c.scanner.Bytes()))
		copy(line, c.scanner.Bytes())

		var idProbe struct {
			ID any `json:"id"`
		}
		_ = json.Unmarshal(line, &idProbe)
		key := fmt.Sprintf("%v", idProbe.ID)

		if chVal, ok := c.pending.Load(key); ok {
			ch := chVal.(chan rpcResponse)
			select {
			case ch <- rpcResponse{data: line}:
			default:
			}
		}
		// If nothing is pending for this id (e.g. the caller already timed
		// out and was cleaned up), the line is intentionally dropped.
	}

	err := c.scanner.Err()
	if err == nil {
		err = fmt.Errorf("MCP server closed stdout unexpectedly (stderr: %s)", c.stderrString())
	} else {
		err = fmt.Errorf("read from stdout: %w (stderr: %s)", err, c.stderrString())
	}
	c.readErr.Store(err)

	// Wake up any requests still waiting so they don't hang until their
	// context times out for no reason once the reader is dead.
	c.pending.Range(func(_, chVal any) bool {
		ch := chVal.(chan rpcResponse)
		select {
		case ch <- rpcResponse{err: err}:
		default:
		}
		return true
	})
}

// SendRequest sends a JSON-RPC request and reads the response with context-based timeout.
func (c *MCPClient) SendRequest(ctx context.Context, method string, params any) (any, error) {
	// If the reader is already dead (process crashed/exited before this
	// call started), fail fast with the real cause instead of silently
	// hanging until ctx times out with a generic timeout error.
	select {
	case <-c.done:
		if v := c.readErr.Load(); v != nil {
			return nil, v.(error)
		}
		return nil, fmt.Errorf("MCP client closed (stderr: %s)", c.stderrString())
	default:
	}

	id := c.nextID.Add(1)
	idKey := fmt.Sprintf("%v", id)
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
		"id":      id,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ch := make(chan rpcResponse, 1)
	c.pending.Store(idKey, ch)
	defer c.pending.Delete(idKey)

	// Write request + newline delimiter. writeMu only protects
	// interleaving of concurrent writes to stdin; it is held only for the
	// duration of the write itself, not for the whole round trip, so a
	// slow response from one request never blocks another request's
	// write.
	c.writeMu.Lock()
	_, werr := c.stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("write to stdin: %w (stderr: %s)", werr, c.stderrString())
	}

	var line []byte
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		line = res.data
	case <-c.done:
		if v := c.readErr.Load(); v != nil {
			return nil, v.(error)
		}
		return nil, fmt.Errorf("MCP client closed while waiting for response (stderr: %s)", c.stderrString())
	case <-ctx.Done():
		return nil, fmt.Errorf("MCP request timed out or cancelled: %w (stderr: %s)", ctx.Err(), c.stderrString())
	}

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  any    `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w (raw: %s)", err, string(line))
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}

	return resp.Result, nil
}

// IsDead reports whether the underlying MCP subprocess's read loop has
// already exited (e.g. the process crashed, or its stdout closed because
// the command/args were wrong -- a ModuleNotFoundError, missing binary,
// etc). Once dead, a client can never successfully service another
// request; SendRequest will keep returning the same stored readErr
// immediately without attempting IO.
//
// Callers that cache MCPClient instances by MCP ID (see core/spawner.go's
// processCache) should check this before reusing a cached entry and evict
// it if true. The process cache has no other invalidation path -- it is
// not cleared by config.reload/Registry.Load(), so without this check a
// client that failed once (e.g. because of a since-corrected command/args
// typo in config_windows.json) would be reused forever until the whole
// orchestrator process restarts, silently discarding any config fix.
// ADDED (2026-07-19).
func (c *MCPClient) IsDead() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// IsInitialized reports whether the initialize handshake has already been
// performed for this client.
// ADDED (2026-08-15).
func (c *MCPClient) IsInitialized() bool {
	return c.initialized.Load()
}

// Initialize performs the MCP initialize handshake (initialize request ->
// notifications/initialized) against the local stdio server. This MUST be
// called successfully before any other JSON-RPC methods (list_tools,
// tools/call, etc) are attempted, or the server may return -32602
// (Invalid request parameters).
// ADDED (2026-08-15).
func (c *MCPClient) Initialize(ctx context.Context, protocolVersion string) error {
	if c.initialized.Load() {
		return nil
	}

	initReq := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "orchestrator-mcp-go", "version": "1.0.0"},
	}

	_, err := c.SendRequest(ctx, "initialize", initReq)
	if err != nil {
		return fmt.Errorf("initialize request failed: %w", err)
	}

	// notifications/initialized is a one-way notification per spec
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal initialized notification: %w", err)
	}

	c.writeMu.Lock()
	_, err = c.stdin.Write(append(data, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("write initialized notification failed: %w (stderr: %s)", err, c.stderrString())
	}

	c.initialized.Store(true)
	return nil
}

// IsConnectionError reports whether the given error returned by SendRequest
// indicates a transport-level failure (crashed process, broken pipe, EOF)
// that warrants a reconnection attempt.
// ADDED (2026-08-04).
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Match common stdio failure signals
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "eof") ||
		strings.Contains(msg, "closed unexpectedly") ||
		strings.Contains(msg, "closed stdout unexpectedly") ||
		strings.Contains(msg, "client closed") ||
		strings.Contains(msg, "io: read/write on closed pipe")
}

func (c *MCPClient) Close() {
	c.closeOnce.Do(func() {
		c.stdin.Close()
		// Kill the entire process tree by PID, not just the parent — prevents
		// orphaned child processes (e.g., Chrome spawned by an MCP server).
		// Falls back to plain Process.Kill if PID is unavailable (audit H2).
		if c.cmd.Process != nil {
			_ = KillProcessTreeByPID(c.cmd.Process.Pid)
		}
		_ = c.cmd.Wait()
		if c.sandboxCleanup != nil {
			c.sandboxCleanup()
		}
	})
}

// KillProcessTreeByPID forcefully terminates a process and all its descendants
// by PID. This is the safe, targeted replacement for pattern-based pkill that
// avoids collateral kills of unrelated processes sharing the same name (audit H2).
//
// Platform behavior:
//   - Windows: taskkill /PID pid /T /F  (native recursive tree kill)
//   - Unix:    pkill -P pid (children) + kill -TERM pid (parent)
func KillProcessTreeByPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	if runtime.GOOS == "windows" {
		// /T = kill child processes recursively, /F = force
		return exec.Command("taskkill", "/PID", fmt.Sprintf("%d", pid), "/T", "/F").Run()
	}
	// Unix: kill children by parent PID, then the parent itself.
	// pkill -P pid kills all processes whose parent is pid.
	_ = exec.Command("pkill", "-P", fmt.Sprintf("%d", pid)).Run()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// KillProcessTree forcefully terminates all processes matching the given pattern.
// Useful for cleaning up orphan child processes (e.g., Chrome) spawned by an MCP server.
//
// WARNING: pattern-based killing can cause collateral damage (any process whose
// command line contains the pattern is killed). Prefer KillProcessTreeByPID when
// the PID is known. This function is kept for pre-start orphan cleanup only.
func KillProcessTree(pattern string) error {
	if pattern == "" {
		return nil
	}
	if runtime.GOOS == "windows" {
		// Windows: use taskkill /IM <pattern> /F to kill by image name.
		// Append .exe if no extension is present (common convention).
		img := pattern
		if filepath.Ext(img) == "" {
			img += ".exe"
		}
		return exec.Command("taskkill", "/IM", img, "/F").Run()
	}
	// Unix: use pkill -f to kill processes by full command line pattern
	return exec.Command("pkill", "-f", pattern).Run()
}
