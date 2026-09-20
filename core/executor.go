package core

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// StdinPattern defines a pattern that indicates a process is waiting for input.
type StdinPattern struct {
	Pattern     *regexp.Regexp
	Description string
}

var DefaultStdinPatterns = []StdinPattern{
	{Pattern: regexp.MustCompile(`\?\s*$`), Description: "Question mark suffix"},
	{Pattern: regexp.MustCompile(`\(y/n\)`), Description: "Yes/No confirmation"},
	{Pattern: regexp.MustCompile(`password:`), Description: "Password prompt"},
	{Pattern: regexp.MustCompile(`Press any key`), Description: "Key wait"},
	{Pattern: regexp.MustCompile(`>\s*$`), Description: "REPL prompt"},
	{Pattern: regexp.MustCompile(`Enter .+: `), Description: "Input request"},
}

// ExecutionResult holds the outcome of a controlled execution.
type ExecutionResult struct {
	Stdout      []byte
	Stderr      []byte
	ExitCode    int
	Status      string // ok, error, timeout, stdin_required
	Error       error
	StdinPrompt string
	Diagnostic  *DiagnosticResult // ADDED (2026-08-16)
}

// DiagnosticResult contains information from dual-path verification.
type DiagnosticResult struct {
	TransportDamage bool
	ScriptStdout    []byte
	ScriptStderr    []byte
	ScriptExitCode  int
	FixAttempted    bool
}

// ControlledExecutor runs processes with monitoring for timeouts and interactive prompts.
type ControlledExecutor struct {
	IdleTimeout            time.Duration
	TotalTimeout           time.Duration
	Patterns               []StdinPattern
	OutputEncoding         string // "" or "utf8" = default; "gbk" = decode Windows GBK output to UTF-8
	DisableStdinMonitoring bool
	DiagnosticMode         bool // ADDED (2026-08-16): Enable dual-path verification
}

func NewControlledExecutor() *ControlledExecutor {
	return &ControlledExecutor{
		IdleTimeout:  15 * time.Second,
		TotalTimeout: 300 * time.Second,
		Patterns:     DefaultStdinPatterns,
	}
}

func (e *ControlledExecutor) Run(ctx context.Context, command string, args []string, cwd string, input []byte) (*ExecutionResult, error) {
	// Command guard: block TUI tools, auto-inject git --no-pager
	guard := CheckCommand(command, args)
	if guard.Block {
		return &ExecutionResult{
			Status: "error",
			Error:  fmt.Errorf("blocked interactive command: %s", guard.Message),
		}, nil
	}
	if guard.RewrittenCommand != "" {
		command = guard.RewrittenCommand
	}
	if guard.RewrittenArgs != nil {
		args = guard.RewrittenArgs
	}

	// Create context with total timeout
	execCtx, cancel := context.WithTimeout(ctx, e.TotalTimeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, command, args...)
	// Inject pager-immunity env vars so child processes never spawn a pager
	cmd.Env = append(os.Environ(), PagerEnv()...)
	if cwd != "" {
		cmd.Dir = cwd
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	result := &ExecutionResult{Status: "ok", ExitCode: 0}
	var stdoutBuf, stderrBuf bytes.Buffer

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Write initial input if provided
	if len(input) > 0 {
		go func() {
			if _, err := stdinPipe.Write(input); err != nil {
				fmt.Fprintf(os.Stderr, "executor: stdin write failed: %v\n", err)
			}
			if err := stdinPipe.Close(); err != nil {
				fmt.Fprintf(os.Stderr, "executor: stdin close failed: %v\n", err)
			}
		}()
	} else {
		stdinPipe.Close()
	}

	// Channels for signaling
	done := make(chan error, 1)
	var stdinDetected chan string
	if !e.DisableStdinMonitoring {
		stdinDetected = make(chan string, 1)
	}

	idleTimer := time.NewTimer(e.IdleTimeout)
	defer idleTimer.Stop()

	var wg sync.WaitGroup
	wg.Add(2)

	// Monitor Stdout for data and patterns.
	// A pattern match alone is not sufficient evidence the process is blocked on stdin:
	// the matched text may simply be normal output that happens to contain the literal
	// substrings we scan for (e.g. source code containing "(y/n)" or "Press any key" as
	// string literals). We only treat a match as a genuine stdin wait once the process
	// has gone quiet (no further output) for a short grace period after the match. This
	// avoids killing processes whose legitimate output momentarily resembles a prompt.
	const stdinConfirmDelay = 1500 * time.Millisecond
	var lastReadAt atomic.Int64
	lastReadAt.Store(time.Now().UnixNano())
	go func() {
		defer wg.Done()
		reader := bufio.NewReader(stdoutPipe)
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				stdoutBuf.Write(buf[:n])
				idleTimer.Reset(e.IdleTimeout)
				lastReadAt.Store(time.Now().UnixNano())

				// Check patterns
				if !e.DisableStdinMonitoring {
					output := string(buf[:n])
					for _, p := range e.Patterns {
						if p.Pattern.MatchString(output) {
							matchedAt := time.Now().UnixNano()
							desc := p.Description + ": " + output
							go func(matchedAt int64, desc string) {
								timer := time.NewTimer(stdinConfirmDelay)
								defer timer.Stop()
								<-timer.C
								// Only fire if no newer output has arrived since this
								// match — i.e. the process is still genuinely idle.
								if lastReadAt.Load() <= matchedAt {
									select {
									case stdinDetected <- desc:
									default:
									}
								}
							}(matchedAt, desc)
						}
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Monitor Stderr
	go func() {
		defer wg.Done()
		io.Copy(&stderrBuf, stderrPipe)
	}()

	// Wait for process completion
	go func() {
		done <- cmd.Wait()
	}()

	// Main event loop
	select {
	case <-execCtx.Done():
		result.Status = "timeout"
		result.Error = execCtx.Err()
		_ = cmd.Process.Kill()
	case prompt := <-stdinDetected:
		result.Status = "stdin_required"
		result.StdinPrompt = prompt
		_ = cmd.Process.Kill() // Terminate for now, as we don't support true interactive resume yet
	case <-idleTimer.C:
		result.Status = "timeout"
		result.Error = fmt.Errorf("idle timeout exceeded (%v)", e.IdleTimeout)
		_ = cmd.Process.Kill()
	case err := <-done:
		if err != nil {
			result.Status = "error"
			result.Error = err
			if exitError, ok := err.(*exec.ExitError); ok {
				result.ExitCode = exitError.ExitCode()
			}
		}
	}

	wg.Wait()
	result.Stdout = e.decodeOutput(stdoutBuf.Bytes())
	result.Stderr = e.decodeOutput(stderrBuf.Bytes())

	// ─── Dual-Path Verification (ADDED 2026-08-16) ───────────────
	if e.DiagnosticMode && result.Status != "timeout" {
		if shellType, scriptBody := IdentifyShellTask(command, args); shellType != "" {
			e.verifyRobustness(ctx, result, shellType, scriptBody, cwd)
		}
	}

	return result, nil
}

// IdentifyShellTask checks if the command is a nested shell call and extracts the script body.
func IdentifyShellTask(command string, args []string) (shellType, scriptBody string) {
	cmdName := strings.ToLower(filepath.Base(command))
	cmdName = strings.TrimSuffix(cmdName, ".exe")

	// PowerShell
	if cmdName == "powershell" || cmdName == "pwsh" {
		for i, arg := range args {
			if strings.EqualFold(arg, "-Command") || strings.EqualFold(arg, "/Command") {
				if i+1 < len(args) {
					return "powershell", args[i+1]
				}
			}
		}
	}

	// Bash / Sh
	if cmdName == "bash" || cmdName == "sh" {
		for i, arg := range args {
			if arg == "-c" && i+1 < len(args) {
				return "bash", args[i+1]
			}
		}
	}

	// CMD
	if cmdName == "cmd" {
		for i, arg := range args {
			if (strings.EqualFold(arg, "/c") || strings.EqualFold(arg, "/k")) && i+1 < len(args) {
				// CMD is tricky because everything after /c is often treated as the command
				return "cmd", strings.Join(args[i+1:], " ")
			}
		}
	}

	return "", ""
}

func (e *ControlledExecutor) verifyRobustness(ctx context.Context, directRes *ExecutionResult, shellType, body, cwd string) {
	// Use a separate context for the verification path to avoid interfering with main path timing
	verifyCtx, cancel := context.WithTimeout(ctx, e.TotalTimeout)
	defer cancel()

	scriptRes, err := e.RunViaTempScript(verifyCtx, shellType, body, cwd)
	if err != nil {
		return
	}

	directRes.Diagnostic = &DiagnosticResult{
		ScriptStdout:   scriptRes.Stdout,
		ScriptStderr:   scriptRes.Stderr,
		ScriptExitCode: scriptRes.ExitCode,
	}

	// Compare outputs
	stdoutMismatch := !bytes.Equal(directRes.Stdout, scriptRes.Stdout)
	exitCodeMismatch := directRes.ExitCode != scriptRes.ExitCode

	if stdoutMismatch || exitCodeMismatch {
		directRes.Diagnostic.TransportDamage = true
		// LOGGING: In a real environment, we'd log this event.
		// For now, we signal damage to the result.

		// Auto-fix: if script path succeeded and direct failed, prefer script result
		if scriptRes.ExitCode == 0 && directRes.ExitCode != 0 {
			directRes.Stdout = scriptRes.Stdout
			directRes.Stderr = scriptRes.Stderr
			directRes.ExitCode = scriptRes.ExitCode
			directRes.Status = "ok"
			directRes.Diagnostic.FixAttempted = true
			if directRes.Error != nil {
				directRes.Error = nil
			}
		}
	}
}

func (e *ControlledExecutor) RunViaTempScript(ctx context.Context, shellType, body, cwd string) (*ExecutionResult, error) {
	ext := ".sh"
	if shellType == "powershell" {
		ext = ".ps1"
	}

	tmpFile, err := os.CreateTemp("", "orch_verify_*"+ext)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	if _, err := tmpFile.WriteString(body); err != nil {
		return nil, err
	}
	tmpFile.Close()

	var cmd string
	var args []string

	switch shellType {
	case "powershell":
		cmd = "powershell"
		args = []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", tmpFile.Name()}
	case "bash":
		cmd = "bash"
		args = []string{tmpFile.Name()}
	}

	// Create a new executor for the script path, but disable diagnostic mode to avoid recursion
	scriptExec := &ControlledExecutor{
		IdleTimeout:            e.IdleTimeout,
		TotalTimeout:           e.TotalTimeout,
		Patterns:               e.Patterns,
		OutputEncoding:         e.OutputEncoding,
		DisableStdinMonitoring: true, // Scripts usually don't need pattern monitoring
		DiagnosticMode:         false,
	}

	return scriptExec.Run(ctx, cmd, args, cwd, nil)
}

// decodeOutput converts output bytes to UTF-8 if OutputEncoding is set to "gbk".
// On Windows, if no encoding is specified, it also tries GBK as a heuristic fallback.
func (e *ControlledExecutor) decodeOutput(data []byte) []byte {
	enc := e.OutputEncoding
	if enc == "" && runtime.GOOS == "windows" {
		enc = "auto" // attempt heuristic on Windows
	}
	switch enc {
	case "gbk":
		decoded, err := decodeGBK(data)
		if err == nil {
			return decoded
		}
	case "auto":
		// Only transcode if the output looks like it could be GBK
		// (contains bytes in the 0x80-0xFF range, which are invalid UTF-8)
		if !isValidUTF8(data) {
			decoded, err := decodeGBK(data)
			if err == nil {
				return decoded
			}
		}
	}
	return data
}

func decodeGBK(data []byte) ([]byte, error) {
	decoder := simplifiedchinese.GBK.NewDecoder()
	reader := transform.NewReader(bytes.NewReader(data), decoder)
	return io.ReadAll(reader)
}

func isValidUTF8(data []byte) bool {
	for i := 0; i < len(data); {
		if data[i] < 0x80 {
			i++
			continue
		}
		// Multi-byte sequence
		switch {
		case data[i]>>5 == 0b110 && i+1 < len(data) && data[i+1]>>6 == 0b10:
			i += 2
		case data[i]>>4 == 0b1110 && i+2 < len(data) && data[i+1]>>6 == 0b10 && data[i+2]>>6 == 0b10:
			i += 3
		case data[i]>>3 == 0b11110 && i+3 < len(data) && data[i+1]>>6 == 0b10 && data[i+2]>>6 == 0b10 && data[i+3]>>6 == 0b10:
			i += 4
		default:
			return false
		}
	}
	return true
}
