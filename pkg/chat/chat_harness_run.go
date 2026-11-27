package chat

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/stream"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// maxContractDefectSteers bounds defect feedback independently of the one-shot
// completion steer, so fixing a reported defect can still produce a done steer.
const maxContractDefectSteers = 2

// chatRun is the state of one ChatHarness.Run invocation. Keeping it separate
// from ChatHarness preserves the harness's immutable, concurrent-use contract
// while letting the loop's preparation, provider, and tool phases stay small.
type chatRun struct {
	h   *ChatHarness
	ctx context.Context

	taskID string
	emit   func(ChatEvent) error

	maxTurns        int
	maxOutputTokens int
	query           string
	system          string
	messages        []ChatMessage

	declaredArtifacts    []string
	completionContract   core.CompletionContractMode
	contractSteers       int
	contractDefectSteers int

	finalText       strings.Builder
	recentSigs      []string
	toolFailErrors  map[string][]string
	noTextTurns     int
	echoGuard       *core.EchoGuard
	noTextNudged    bool
	lastTurnToolsOK bool
	overflowRetried bool
	lastToolMeta    *store.ToolMetadata

	stats *runStats

	execGuardMode   codeExecGuardMode
	execStreakLimit int
	eg              exploreGuard
}

// prepareRun performs the work that precedes the measured agent loop. A Tier-3
// auto-delegation returns before loop statistics and archival defers are armed,
// exactly as the original inline ordering did.
func (h *ChatHarness) prepareRun(ctx context.Context, taskID string, history []ChatMessage, emit func(ChatEvent) error) (*chatRun, string, bool) {
	maxTurns := h.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	maxOutputTokens := h.MaxOutputTokens
	if maxOutputTokens <= 0 {
		maxOutputTokens = 2048
	}

	query := ""
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			query = history[i].Content
			break
		}
	}

	h.contract.wakeSeeded = false
	declaredArtifacts := h.initContract(query)
	h.declaredArtifacts = declaredArtifacts
	h.taskPrompt = query
	LogDelegationSpecOnce()
	h.initDeclaredFields(query)

	r := &chatRun{
		h:                  h,
		ctx:                ctx,
		taskID:             taskID,
		emit:               emit,
		maxTurns:           maxTurns,
		maxOutputTokens:    maxOutputTokens,
		query:              query,
		declaredArtifacts:  declaredArtifacts,
		completionContract: core.CompletionContractModeFromEnv(),
	}
	core.LogCompletionContractOnce()

	r.system = h.applyQuickPath(query, h.buildSystem(), emit)

	r.messages = make([]ChatMessage, 0, len(history)+maxTurns)
	if h.Memory != nil {
		history = h.Memory.Process(ctx, taskID, history)
	}
	r.messages = append(r.messages, history...)

	// Retrieval and time context are tail-only additions: the provider's cached
	// prefix remains byte-identical across turns.
	if ctxBlock := h.buildTurnContext(query, taskID); ctxBlock != "" {
		for i := len(r.messages) - 1; i >= 0; i-- {
			if r.messages[i].Role == "user" {
				r.messages[i].Content += ctxBlock
				break
			}
		}
	}
	appendTimeContext(r.messages, history)

	if result, delegated := r.mountOversizedInput(); delegated {
		return nil, result, true
	}
	return r, "", false
}

// mountOversizedInput applies Tier 1 (file mount) and Tier 3 (auto-delegation)
// before the first provider request. Tier 2 retry remains in providerTurn.
func (r *chatRun) mountOversizedInput() (string, bool) {
	h := r.h
	if h.MaxContextWindow <= 0 || h.OutputBase == "" {
		return "", false
	}
	totalChars := 0
	for _, m := range r.messages {
		totalChars += len(m.Content)
	}
	estimatedTokens := totalChars / 4
	if estimatedTokens <= h.MaxContextWindow/2 {
		return "", false
	}

	for i := len(r.messages) - 1; i >= 0; i-- {
		if r.messages[i].Role != "user" || len(r.messages[i].Content) <= 4000 {
			continue
		}
		content := r.messages[i].Content

		fileDir := filepath.Join(h.OutputBase, r.taskID, "inputs")
		if err := os.MkdirAll(fileDir, 0755); err != nil {
			log.Printf("WARN: chat_harness: failed to create input dir: %v", err)
		}
		filename := fmt.Sprintf("large_input_%d.txt", time.Now().UnixMilli())
		filePath := filepath.Join(fileDir, filename)
		if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
			log.Printf("WARN: chat_harness: failed to write large input file: %v", err)
		}

		lines := strings.Split(content, "\n")
		headEnd := 20
		if headEnd > len(lines) {
			headEnd = len(lines)
		}
		tailStart := len(lines) - 20
		if tailStart < headEnd {
			tailStart = headEnd
		}
		summary := fmt.Sprintf("[Large input mounted: %d chars, %d lines, ~%d tokens. Saved as: %s]\n",
			len(content), len(lines), estimatedTokens, filename)
		summary += "--- First 20 lines ---\n" + strings.Join(lines[:headEnd], "\n") + "\n"
		if tailStart < len(lines) && tailStart > headEnd {
			summary += fmt.Sprintf("... [%d lines omitted] ...\n", tailStart-headEnd)
			summary += "--- Last 20 lines ---\n" + strings.Join(lines[tailStart:], "\n")
		}
		summary += fmt.Sprintf("\n[Use read_file path=%s start_line=N end_line=M to read specific sections]", filename)

		if estimatedTokens > h.MaxContextWindow && h.Delegate != nil {
			intent := content
			if len(intent) > 500 {
				intent = intent[:250] + " ... " + intent[len(intent)-250:]
			}
			delegatePrompt := fmt.Sprintf(
				"Read and process the file at %s.\nUser intent (preview): %s\nFile summary: %s\n"+
					"Use read_file with start_line/end_line to read in batches. Analyze and respond to the user's request.",
				filePath, intent, summary)
			delTaskID, delErr := h.Delegate(delegatePrompt, h.SubAgentBudgetCap())
			if delErr == nil {
				r.emit(ChatEvent{Type: "tool_call", Data: "auto_delegated_to_orchestrator", Meta: map[string]any{
					"task_id": delTaskID,
					"reason":  "input exceeds context window",
					"tokens":  estimatedTokens,
				}})
				result := fmt.Sprintf(
					"Your input is large (~%d tokens, exceeds model context window of %d).\n"+
						"I've saved it to %s and delegated analysis to the orchestrator.\n"+
						"Task: %s\nUse /status %s to check progress.",
					estimatedTokens, h.MaxContextWindow, filename, delTaskID, delTaskID)
				r.emit(ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": "auto_delegate"}})
				r.emit(ChatEvent{Type: "done"})
				return result, true
			}
		}

		r.messages[i].Content = summary
		r.emit(ChatEvent{Type: "steer", Data: fmt.Sprintf(
			"large input (%d tokens) mounted as file %s, message replaced with summary",
			estimatedTokens, filename)})
		break
	}
	return "", false
}

func (r *chatRun) initLoop() {
	r.recentSigs = make([]string, 0, 5)
	r.toolFailErrors = make(map[string][]string)
	r.echoGuard = core.NewEchoGuard(0)
	r.stats = newRunStats(len(r.declaredArtifacts), len(r.h.declaredFields))
	r.execGuardMode, r.execStreakLimit = codeExecGuardSettings()
	r.eg = exploreGuard{writeTargets: make(map[string]struct{}), writeCounts: make(map[string]int)}
	log.Printf("[chat-harness] task=%s sieve_nil=%v turns=%d", r.taskID, r.h.Sieve == nil, r.maxTurns)
}

// execute owns the bounded loop. Recovery and tool phases return to this
// orchestrator so cancellation, retry-turn accounting, and terminal ordering
// remain visible in one place.
func (r *chatRun) execute() (string, error) {
	for turn := 0; turn < r.maxTurns; turn++ {
		if r.ctx.Err() != nil {
			r.emit(ChatEvent{Type: "error", Data: fmt.Sprintf("context cancelled: %v", r.ctx.Err())})
			return r.finalText.String(), r.ctx.Err()
		}

		prevLen := r.finalText.Len()
		r.h.currentTurn = turn + 1
		r.stats.turns++

		req := providers.CompleteRequest{
			System:      r.system,
			User:        flattenChatMessages(r.messages),
			Model:       r.h.Model,
			MaxTokens:   r.maxOutputTokens,
			MCPServers:  []schemas.MCPServerDef{{Name: "core", Tools: r.stats.offerTools(r.h)}},
			Attachments: r.h.Attachments,
		}

		resp, retry, turnDelta, err := r.providerTurn(req, prevLen)
		if err != nil {
			return r.finalText.String(), err
		}
		if retry {
			turn += turnDelta
			continue
		}
		if len(resp.ToolCalls) == 0 {
			if r.finishNoToolTurn(prevLen) {
				return r.finalText.String(), nil
			}
			continue
		}

		r.observeToolTurn(resp, prevLen)
		turnToolsOK := false
		for i := range resp.ToolCalls {
			outcome := r.executeToolCall(i, resp.ToolCalls)
			if outcome.toolsOK {
				turnToolsOK = true
			}
			if outcome.skipped {
				continue
			}
			if outcome.rule.stopBatch && !r.h.batchHasWrite(resp.ToolCalls[i+1:]) {
				skipRemainingCalls(resp.ToolCalls[i+1:], r.emit, &r.messages)
				break
			}
			if outcome.rule.endRun {
				r.stats.exitReason = "exploration_exhausted"
				r.emit(ChatEvent{Type: "error", Data: fmt.Sprintf("exploration ceiling: %d calls without a write", r.eg.streak), Meta: map[string]any{"exit_reason": "exploration_exhausted", "cap": r.eg.streak}})
				return markExplorationTruncated(r.finalText.String(), r.eg.streak),
					fmt.Errorf("exploration ceiling reached (%d calls without a write): %w", r.eg.streak, ErrExplorationExhausted)
			}
		}
		r.lastTurnToolsOK = turnToolsOK
	}

	r.stats.exitReason = "max_turns"
	r.emit(ChatEvent{Type: "error", Data: fmt.Sprintf("max turns (%d) exceeded", r.maxTurns), Meta: map[string]any{"exit_reason": "max_turns", "cap": r.maxTurns}})
	return markTruncated(r.finalText.String(), r.maxTurns), fmt.Errorf("max turns (%d) exceeded: %w", r.maxTurns, ErrMaxTurnsExceeded)
}

// providerTurn streams one response and applies the three original recovery
// paths in their original order: repetition steer, one-shot overflow retry, and
// terminal timeout/provider errors.
func (r *chatRun) providerTurn(req providers.CompleteRequest, prevLen int) (*providers.ProviderResponse, bool, int, error) {
	resp, err := r.h.streamTurn(r.ctx, req, &r.finalText, r.emit)
	if err == nil {
		return resp, false, 0, nil
	}
	if errors.Is(err, stream.ErrRepetitiveOutput) {
		if r.finalText.Len() > prevLen {
			text := r.finalText.String()[:prevLen]
			r.finalText.Reset()
			r.finalText.WriteString(text)
		}
		r.emit(ChatEvent{Type: "loop_guard", Data: "repetitive streaming output detected"})
		r.emit(ChatEvent{Type: "steer", Data: "repetitive output detected, refocus"})
		r.messages = append(r.messages, ChatMessage{Role: "user", Content: core.MarkerRepetitiveOutput})
		return nil, true, 0, nil
	}
	if core.IsContextLimitError(err) && !r.overflowRetried {
		r.overflowRetried = true
		r.emit(ChatEvent{Type: "steer", Data: "context overflow detected, truncating input and retrying"})
		for i := len(r.messages) - 1; i >= 0; i-- {
			if r.messages[i].Role == "user" && len(r.messages[i].Content) > 4000 {
				content := r.messages[i].Content
				half := 2000
				if half > len(content)/2 {
					half = len(content) / 2
				}
				r.messages[i].Content = content[:half] +
					fmt.Sprintf("\n[... truncated: %d chars omitted due to context overflow ...]\n", len(content)-half*2) +
					content[len(content)-half:]
				break
			}
		}
		return nil, true, -1, nil
	}
	providerName := r.h.Provider.Name()
	if emitTimeoutError(r.emit, err, r.ctx, providerName) {
		return nil, false, 0, err
	}
	emitRunError(r.emit, err, providerName)
	return nil, false, 0, err
}

// finishNoToolTurn applies the echo guard before the normal done event. It
// returns true for both a clean answer and an echo kill, and false for a hint.
func (r *chatRun) finishNoToolTurn(prevLen int) bool {
	turnText := r.finalText.String()[prevLen:]
	switch r.echoGuard.Check(turnText) {
	case core.EchoHint:
		r.emit(ChatEvent{Type: "loop_guard", Data: "text echo detected"})
		r.emit(ChatEvent{Type: "steer", Data: core.EchoHintText})
		r.messages = append(r.messages, ChatMessage{Role: "user", Content: core.EchoHintText})
		return false
	case core.EchoKill:
		r.emit(ChatEvent{Type: "loop_guard", Data: "text echo: killed after grace"})
		r.stats.exitReason = "text_echo_killed"
		return true
	}
	r.stats.exitReason = "done"
	r.emit(ChatEvent{Type: "done"})
	return true
}

// observeToolTurn contains the pre-execution guards: no-text convergence,
// truncation inspection, and call-level repetition detection.
func (r *chatRun) observeToolTurn(resp *providers.ProviderResponse, prevLen int) {
	if r.finalText.Len()-prevLen == 0 {
		r.noTextTurns++
	} else {
		r.noTextTurns = 0
	}
	if r.noTextTurns >= 3 {
		n := r.noTextTurns
		r.noTextTurns = 0
		if !r.noTextNudged && !r.lastTurnToolsOK {
			r.noTextNudged = true
			r.stats.noTextSteers++
			r.emit(ChatEvent{Type: "steer", Data: fmt.Sprintf("no text output for %d consecutive turns", n)})
			r.messages = append(r.messages, ChatMessage{Role: "user", Content: core.MarkerNoTextResponse})
		}
	}

	if r.finalText.Len()-prevLen > 0 && r.h.Sieve != nil && r.lastToolMeta != nil {
		newText := r.finalText.String()[prevLen:]
		if ok, reason := r.h.Sieve.InspectTruncationArtifact(r.lastToolMeta, newText); !ok {
			r.emit(ChatEvent{Type: "truncation_violation", Data: reason})
		}
	}

	if r.h.Sieve != nil {
		interactions := make([]store.ToolInteraction, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			interactions[i] = store.ToolInteraction{ToolName: tc.Name, Arguments: tc.Arguments}
		}
		if ok, reason := r.h.Sieve.InspectToolCalls(r.taskID, "", interactions); !ok {
			r.emit(ChatEvent{Type: "loop_guard", Data: reason})
		}
		return
	}

	sig := toolCallSignature(resp.ToolCalls)
	if sig == "" {
		return
	}
	r.recentSigs = append(r.recentSigs, sig)
	if len(r.recentSigs) > 5 {
		r.recentSigs = r.recentSigs[len(r.recentSigs)-5:]
	}
	count := 0
	for _, recent := range r.recentSigs {
		if recent == sig {
			count++
		}
	}
	if count >= 3 {
		r.emit(ChatEvent{Type: "loop_guard", Data: "identical tool calls repeated (windowed fallback)"})
	}
}
