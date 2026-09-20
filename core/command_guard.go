package core

import (
	"path/filepath"
	"strings"
)

// ─── Pager-immune environment ──────────────────────────────────────────────

// pagerEnvVars are injected into every spawned process by default.
// This prevents pager tools (less, more, git log, systemctl status)
// from blocking on TTY input when stdout is a pipe.
var pagerEnvVars = []string{
	"GIT_PAGER=cat",
	"PAGER=cat",
	"SYSTEMD_PAGER=cat",
	"MANPAGER=cat",
	"LESS=-F", // -F: quit if output fits on screen
}

// ─── TUI command guard ─────────────────────────────────────────────────────

// blockedInteractiveCommands lists tools that require a real TTY and
// will always hang in a pipe-based executor.
var blockedInteractiveCommands = map[string]string{
	"vim":    "vim is interactive. Use read_file/write_file/patch instead.",
	"vi":     "vi is interactive. Use read_file/write_file/patch instead.",
	"nvim":   "nvim is interactive. Use read_file/write_file/patch instead.",
	"nano":   "nano is interactive. Use read_file/write_file/patch instead.",
	"emacs":  "emacs is interactive. Use read_file/write_file/patch instead.",
	"top":    "top is interactive. Use 'top -b -n 1' for batch output.",
	"htop":   "htop is interactive. Use 'ps aux' for a static snapshot.",
	"btop":   "btop is interactive. Use 'ps aux' for a static snapshot.",
	"less":   "less is interactive. Use read_file or cat with head/tail.",
	"more":   "more is interactive. Use read_file or cat with head/tail.",
	"screen": "screen is interactive. Use terminal(background=true) for PTY mode.",
	"tmux":   "tmux is interactive. Use terminal(background=true) for PTY mode.",
	"telnet": "telnet is interactive. Use curl/wget for HTTP or a PTY session.",
	"ssh":    "ssh without -n can block on stdin. Add '-n' or use --no-pager.",
	"watch":  "watch is interactive. Use the command directly or in a loop.",
	"cowsay": "cowsay waits for input. Pipe input or use 'echo hi | cowsay'.",
}

// ─── Git auto-rewrite ──────────────────────────────────────────────────────

// gitSubcommandsThatPagedare git subcommands that commonly invoke a pager.
var gitSubcommandsThatPage = map[string]bool{
	"log": true, "diff": true, "show": true, "blame": true,
	"grep": true, "shortlog": true, "reflog": true, "cherry": true,
}

// ─── Guard struct ──────────────────────────────────────────────────────────

// CommandGuardResult holds the outcome of a pre-execution safety check.
type CommandGuardResult struct {
	// Block means the command was rejected. Message describes why.
	Block   bool
	Message string

	// RewrittenCommand and RewrittenArgs are the (possibly rewritten)
	// versions the caller should actually execute. If Block is false
	// and these are empty, use the originals.
	RewrittenCommand string
	RewrittenArgs    []string
}

// CheckCommand runs the full guard pipeline:
//  1. Blocklist TUI tools
//  2. Auto-inject --no-pager for git subcommands
func CheckCommand(command string, args []string) CommandGuardResult {
	cmdName := strings.ToLower(filepath.Base(command))
	cmdName = strings.TrimSuffix(cmdName, ".exe")

	// 1. TUI blocklist
	if reason, blocked := blockedInteractiveCommands[cmdName]; blocked {
		return CommandGuardResult{Block: true, Message: reason}
	}

	// 2. Git auto-rewrite: inject --no-pager if missing
	if cmdName == "git" && len(args) > 0 {
		hasNoPager := false
		for _, a := range args {
			if a == "--no-pager" {
				hasNoPager = true
				break
			}
		}
		if !hasNoPager && gitSubcommandsThatPage[args[0]] {
			newArgs := make([]string, 0, len(args)+1)
			newArgs = append(newArgs, "--no-pager")
			newArgs = append(newArgs, args...)
			return CommandGuardResult{
				RewrittenCommand: command,
				RewrittenArgs:    newArgs,
			}
		}
	}

	return CommandGuardResult{}
}

// PagerEnv returns the slice of environment variable strings that
// should be appended to os.Environ() when spawning a subprocess.
func PagerEnv() []string {
	out := make([]string, len(pagerEnvVars))
	copy(out, pagerEnvVars)
	return out
}
