package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

// mbStoreForHarness returns the MemoryBankStore when MemoryBankEnabled is
// true, nil otherwise. This gates ChatHarness injection + ReflectionEngine
// extraction via a single config toggle.
// See docs/completed/2026-10-17/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 1.
func mbStoreForHarness(app *tools.App) *store.MemoryBankStore {
	if app.MemoryBankStore == nil {
		return nil
	}
	if app.Registry != nil && !app.Registry.System.MemoryBankEnabledEffective() {
		return nil
	}
	return app.MemoryBankStore
}

// ChatHandler serves the standalone chat entry point (not MCP):
// SSE + POST with Last-Event-ID resume.
type ChatHandler struct {
	App     *tools.App
	Harness *chat.ChatHarness
	Store   *chat.ChatSessionStore

	// Outbox is the durable queue outbound notifications go through, so a
	// notification produced here can be delivered by whichever hub can reach
	// the user -- possibly after this process is gone. Nil without a DB, which
	// leaves transports on their inline delivery path.
	Outbox store.IChatOutbox

	// hookRunner executes chat-track hooks (chat_habit, chat_pre_turn,
	// chat_post_turn). Nil-safe: set in NewChatHandler when hooks are configured.
	hookRunner *core.HookRunner

	// wg tracks in-flight chat goroutines so Shutdown can wait for them
	// to drain instead of orphaning LLM calls on server stop (audit C4).
	wg sync.WaitGroup

	// channelWg tracks input-channel goroutines (e.g. Telegram start) so
	// Shutdown can wait for them to exit (audit R3).
	channelWg sync.WaitGroup

	// cancelFuncs maps sessionID → context.CancelFunc for true interrupt
	// support. When a new request arrives for a session with an in-flight
	// run, the previous cancel func is invoked to stop wasted token spend.
	cancelFuncs sync.Map

	// lastThinking stores the last thinking/reasoning content per sessionID.
	// Used by the /thinking slash command to expand the full thinking that
	// was truncated in the Telegram display.
	lastThinking sync.Map

	// clearConfirmations tracks pending /clear confirmations per sessionID.
	// When a user issues /clear, a confirmation token is stored here. The
	// user must re-issue /clear <sessionID> confirm to actually clear.
	// Token expires after 30 seconds to prevent stale confirmations.
	clearConfirmations sync.Map // sessionID → int64 (unix deadline)

	// forgetConfirmations tracks pending /forget confirmations.
	// Key: sessionID + "|" + scope + "|" + args. Value: int64 (unix deadline).
	// 60s window (2× /clear) because /forget is irreversible cross-session.
	// See docs/completed/2026-10-21/SESSION_MEMORY_DELETION_DESIGN.md.
	forgetConfirmations sync.Map // "session|scope|args" → int64 (unix deadline)

	// progressLast throttles round-level progress events per task.
	// Key: taskID. Value: time.Time of the last emitted "still running" tick.
	// See chat_progress.go — the delegated task runs in the background and the
	// chat stream would otherwise show nothing between "done" and the terminal
	// task_callback.
	progressLast sync.Map // taskID → time.Time

	// processInfoDisabled tracks per-session process-info visibility.
	// Key: sessionID. Value: true (process info hidden).
	// Default: absent = process info shown (enabled by default).
	// Toggled via /process on|off slash command (2026-10-27).
	// When disabled, thinking/tool_call/tool_result/trajectory events
	// are suppressed in forwardEvents; essential events (delta, error,
	// decision_needed, done) are always sent.
	processInfoDisabled sync.Map // sessionID → bool

	// ── Follow-up turns (async delegation, segment ④) ────────────────
	// A delegated task usually outlives the turn that started it. When it
	// finishes after that turn ended, nobody is left to turn its result into an
	// answer — task_callback only appends an event. startFollowUpTurn runs one
	// more turn for the session; these three maps are its bookkeeping.
	// See chat_followup.go.
	followUpSeen  sync.Map // "sessionID|taskID|reason" → struct{} (dedupe per cause)
	followUpDepth sync.Map // sessionID → int (chained follow-ups, capped)

	// Wake budget for pushed task states (see chat_wake.go). Keyed by taskID:
	// the goal is "one task does not turn the chat into a log stream", and a
	// task is the unit that blocks, not the session.
	wakeSeen   sync.Map // "sessionID|taskID|dedupeKey" → struct{}
	wakeStates sync.Map // taskID → *wakeState

	// pendingWake holds wakes that were deferred because the session was busy.
	// A wake carries either a decision notice or a finished task's result; both
	// are data the model cannot obtain on its own, so this is a FIFO per session
	// and not a single slot. finishTurn drains it after the current turn ends.
	// Key: sessionID. Value: *wakeQueue. See chat_wake.go.
	pendingWake sync.Map // sessionID → *wakeQueue

	// consumedTasks records "sessionID|taskID" when the running turn was shown
	// that task's terminal state (OnTerminalTaskSeen). A queued wake whose
	// result that turn already delivered is dropped instead of answered twice.
	// Cleared when the session's next turn claims the slot.
	consumedTasks sync.Map // "sessionID|taskID" → struct{}
	// readFiles records "sessionID|basename" for every file the running turn read
	// successfully (chat.ChatHarness.OnFileRead). consumedTasks says "a terminal
	// state was shown"; this says "content was actually read", which is the fact
	// the wake rule needs — measured over 283 archived firings, the marker alone
	// was wrong in 137–169 of them (pkg/api/chat_wake.go wakeDropDecision).
	// Cleared when the session's next turn claims the slot, same as consumedTasks.
	readFiles sync.Map // "sessionID|basename" → struct{}
	// delegatedTasks records "sessionID|taskID" for every task a turn in this
	// session handed off. The poll lever needs the whole SET, not the latest id
	// (a turn can delegate twice), and the wake turn needs it too — but the
	// harness that delegated is gone by then, so the ledger lives here.
	// See chat_delegated_tasks.go and post_deleg_lockout.go.
	// Value: *delegWatch when the watchdog is armed (chat_deleg_watchdog.go),
	// struct{}{} otherwise — the key set is what the poll lever reads, and it is
	// identical either way.
	delegatedTasks sync.Map // "sessionID|taskID" → *delegWatch | struct{}{}
	// delegWatchOnce / delegWatchStop own the watchdog goroutine. Both are
	// inert unless VORTEX_DELEG_WATCHDOG is set: the goroutine is started on the
	// first delegation, not at construction, so an idle handler runs nothing.
	// The channel is created in the constructor rather than inside the Once so
	// Shutdown can never race a nil channel and then block in wg.Wait on a
	// goroutine it failed to stop.
	delegWatchOnce sync.Once
	delegStopOnce  sync.Once
	delegWatchStop chan struct{}
	// sessionHarness keeps the harness this session's last request ran on.
	// A role-bound request builds a NEW harness (role's provider/model/system
	// prompt) instead of mutating the shared one, so `h.Harness` never carries
	// a role; a wake turn that clones it would resume on the default identity.
	// See chat_session_harness.go for the measurement that found this.
	sessionHarness sync.Map // sessionID → *chat.ChatHarness
	// lastSubmitterAdmin lets a follow-up turn delegate with the same
	// authority as the turn that spawned it, instead of guessing.
	lastSubmitterAdmin sync.Map // sessionID → bool

	// profileIndexer asynchronously extracts user preferences from chat
	// messages. Nil when MemoryBank is disabled or no store is configured.
	// Spec: docs/completed/2026-11-27/USER_PROFILE_INCREMENTAL_INDEXER.md
	profileIndexer *chat.UserProfileIndexer
}

// IsProcessInfoDisabled returns true if process info is hidden for the session.
// Default is false (process info shown). Thread-safe.
func (h *ChatHandler) IsProcessInfoDisabled(sessionID string) bool {
	v, ok := h.processInfoDisabled.Load(sessionID)
	return ok && v.(bool)
}

// SetProcessInfoDisabled sets the process-info visibility for a session.
// Thread-safe.
func (h *ChatHandler) SetProcessInfoDisabled(sessionID string, disabled bool) {
	if disabled {
		h.processInfoDisabled.Store(sessionID, true)
	} else {
		h.processInfoDisabled.Delete(sessionID)
	}
}

// defaultChatFrontDeskRole is the role ID used as the default chat front desk
// when the user hasn't explicitly selected a role. This implements the three-tier
// chat gateway: the front desk model (medium, fast) classifies complexity and
// either answers directly, asks clarifying questions, or escalates to the
// planner (strong model) via delegate_to_orchestrator.
// See docs/completed/2026-10-11/CHAT_GATEWAY_TIERS_DESIGN.md §3.
const defaultChatFrontDeskRole = "chat_front_desk"

// Shutdown waits for all in-flight chat goroutines and channel goroutines to
// complete. Call before server close to prevent orphaned LLM provider calls
// (audit C4) and channel goroutine leaks (audit R3).
func (h *ChatHandler) Shutdown() {
	// Stop the profile indexer before waiting for chat goroutines.
	if h.profileIndexer != nil {
		h.profileIndexer.Stop()
	}
	// Stop the delegation watchdog before waiting: it holds a wg slot, so
	// closing after Wait would deadlock for the life of the process. Closed
	// through a Once because Shutdown is reachable from more than one path.
	h.delegStopOnce.Do(func() {
		if h.delegWatchStop != nil {
			close(h.delegWatchStop)
		}
	})
	h.wg.Wait()
	h.channelWg.Wait()
}

// CleanupSession removes all sync.Map entries for a session to prevent
// unbounded memory growth from completed sessions (audit H8). Handles both
// simple-keyed maps (sessionID) and composite-keyed maps ("sessionID|...").
func (h *ChatHandler) CleanupSession(sessionID string) {
	// Simple-keyed maps: direct delete.
	h.cancelFuncs.Delete(sessionID)
	h.lastThinking.Delete(sessionID)
	h.clearConfirmations.Delete(sessionID)
	h.processInfoDisabled.Delete(sessionID)
	h.followUpDepth.Delete(sessionID)
	h.pendingWake.Delete(sessionID)
	h.sessionHarness.Delete(sessionID)
	h.lastSubmitterAdmin.Delete(sessionID)

	// Composite-keyed maps: iterate and delete entries with matching prefix.
	prefix := sessionID + "|"
	deleteByPrefix := func(m *sync.Map) {
		m.Range(func(k, _ any) bool {
			if s, ok := k.(string); ok && len(s) > len(prefix) && s[:len(prefix)] == prefix {
				m.Delete(k)
			}
			return true
		})
	}
	deleteByPrefix(&h.forgetConfirmations)
	deleteByPrefix(&h.followUpSeen)
	deleteByPrefix(&h.wakeSeen)
	deleteByPrefix(&h.consumedTasks)
	deleteByPrefix(&h.delegatedTasks)
}

// StoreLastThinking saves the last thinking/reasoning content for a session.
// Used by the /thinking slash command to expand truncated thinking display.
func (h *ChatHandler) StoreLastThinking(sessionID, thinking string) {
	h.lastThinking.Store(sessionID, thinking)
}

// GetLastThinking retrieves the last thinking/reasoning content for a session.
// Returns empty string if no thinking has been stored.
func (h *ChatHandler) GetLastThinking(sessionID string) string {
	if v, ok := h.lastThinking.Load(sessionID); ok {
		return v.(string)
	}
	return ""
}

// WireTaskCompletionCallbacks hooks the ChatHandler into the global EventBus so
// a task that finishes, blocks on a decision, or reports a heartbeat reaches the
// chat session that owns it. The handlers themselves are in chat_outbox.go:
// every one of them produces a notification, and a notification now has two
// audiences -- the client that is watching, and the queue that a hub which
// cannot reach the user still writes to.
func (h *ChatHandler) WireTaskCompletionCallbacks() {
	if h.App == nil || h.App.Scheduler == nil {
		return
	}

	obs.DefaultBus.Subscribe(string(obs.EventTaskCompleted), h.notifyTaskTerminal)
	obs.DefaultBus.Subscribe(string(obs.EventTaskFailed), h.notifyTaskTerminal)

	obs.DefaultBus.Subscribe("EventHeartbeatSleep", h.notifyHeartbeat)
	obs.DefaultBus.Subscribe("EventHeartbeatComplete", h.notifyHeartbeat)
	obs.DefaultBus.Subscribe("EventHeartbeatFail", h.notifyHeartbeat)

	obs.DefaultBus.Subscribe(string(obs.EventDecisionRequired), h.notifyDecisionRequired)

	// ── Background-task progress (ADDED 2026-10-30) ───────────────
	// The VCP heartbeat above is model-driven and only fires when the model
	// writes a [[Heartbeat::…]] marker -- a protocol no prompt teaches, so it
	// fires zero times in practice. This subscription is the model-independent
	// half: the engine's own step/round events become "progress" SSE events so
	// a waiting user sees status between "done" and "task_callback".
	// See chat_progress.go.
	h.wireTaskProgress()
}

// persistSession saves the chat session and logs any failure. Persistence is
// best-effort (the in-memory session stays authoritative for this process),
// but silently discarding the error would lose chat history with no signal.
// Extracted so every caller uses the same logging (was 7 bare `_ =` sites).
func (h *ChatHandler) persistSession(session *chat.ChatSession) {
	if err := h.Store.Persist(session); err != nil {
		log.Printf("WARN: chat: failed to persist session %s: %v", session.ID, err)
	}
}

// submitChatTask is the only way a chat-initiated orchestrator task is
// submitted from this package.
//
// It exists because there were four submission sites — the construction-time
// Delegate, the direct_task gate, the pre-turn hook redirect, and the
// per-request Delegate — and every one of them built its own call with
// workspaceRoot="". An empty root is silent: the task runs, the delegated step
// agent writes files, and nothing in the transcript says they went to
// outputs/<taskID> instead of the tree the chat agent had just been reading.
// Delegation therefore meant "hand the work to an agent that cannot see the
// work", which is indistinguishable from the model choosing badly.
// See harness-bench D3, T2-STALL-RESUME-2026-10-30.md §12.7.
//
// The root comes from h.Harness rather than from a parameter: a parameter is
// something a call site can get wrong, and all four of them did. The
// per-request harness is a field-for-field copy of this one (SessionRoot
// included, pinned by TestCloneHarnessForRequest_CarriesEveryField), so the
// copy and the original agree by construction.
func (h *ChatHandler) submitChatTask(steps []schemas.StepInput, sessionID string) (string, error) {
	if h == nil || h.App == nil || h.App.Scheduler == nil {
		return "", fmt.Errorf("orchestrator scheduler is not available")
	}
	return h.App.Scheduler.SubmitWithWorkspace(steps, sessionID, chatWorkspaceRoot(h.Harness))
}

// ProcessMessage is the transport-agnostic chat message processor.
// It handles slash commands, direct task submission, model/role resolution,
// hook gates, light routing, and the harness LLM loop. The actual response
// flows asynchronously via session events (SSE or polling).
// ctx is the request context (used for hook timeouts and cancellation).
// chatSubmitterIsAdmin resolves whether a chat-submitted task is admin for the
// mount gate (core.mount_gate.go). Channel requests (e.g. Telegram) carry the
// caller's tier on req.UserTier and never place it in ctx; we fall back to the
// context/app tier only when the request carries no channel tier (web/stdio).
// Extracted so the "guest must not be treated as admin" invariant is testable.
// @decision:DECISION-TELEGRAM-TIER
func chatSubmitterIsAdmin(ctx context.Context, app *tools.App, reqTier string) bool {
	if reqTier != "" {
		return reqTier == tools.TierAdmin
	}
	return tools.EffectiveTier(ctx, app) == tools.TierAdmin
}

// autoDetectBranchMode (Mode 3, 2026-10-24): if the user didn't explicitly
// choose fork (parent_id) or quote (reply_to), auto-detect the best mode.
// See docs/completed/2026-10-26/CHAT_BRANCHING_AND_QUOTE_DESIGN.md §5-6.
func (h *ChatHandler) autoDetectBranchMode(req *ChatRequest, session *chat.ChatSession) {
	if req.ParentID != "" || req.ReplyTo != "" {
		return // explicit mode chosen — respect it
	}
	decision := chat.ClassifyBranchIntent(req.Content, session)
	log.Printf("[chat-branch] auto-detect: %s — %s", decision.Mode, decision.Reasoning)
	switch decision.Mode {
	case "fork":
		req.ParentID = decision.QuoteTargetID
	case "quote":
		req.ReplyTo = decision.QuoteTargetID
	}
}

// handleSlashCommandGate checks for a slash command and, if handled, emits the
// events + persists + returns the response. Extracted from ProcessMessage to
// keep it within the size ratchet.
func (h *ChatHandler) handleSlashCommandGate(req ChatRequest, session *chat.ChatSession, sessionID string) (ChatResponse, bool) {
	// Live Co-pilot /live commands need CallerID (for OwnerID), which
	// handleSlashCommand doesn't receive. Handle /live here before falling
	// through to the general slash command handler.
	// See docs/completed/2026-10-30/LIVE_COPILOT_SHARED_SESSION_DESIGN.md §2.3.
	if strings.HasPrefix(req.Content, "/live") {
		liveOwner := session.GetOwnerID()
		if liveOwner == "" {
			liveOwner = req.OwnerID // authorized local/admin claim of an ownerless session
		}
		if liveOwner == "" {
			liveOwner = req.CallerID // internal/test callers without stable auth context
		}
		if handled, response := h.cmdLive(req.Content, session, sessionID, req.UserTier, liveOwner); handled {
			session.AddEvent(chat.ChatEvent{Type: "slash_command", Data: response})
			session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: response, ClientID: "asst_" + req.MessageID}, "")
			h.persistSession(session)
			session.AddEvent(chat.ChatEvent{Type: "done"})
			return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, SlashCommand: true}, true
		}
	}
	if handled, response := h.handleSlashCommandForOwner(req.Content, session, sessionID, req.UserTier, req.OwnerID); handled {
		session.AddEvent(chat.ChatEvent{Type: "slash_command", Data: response})
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: response, ClientID: "asst_" + req.MessageID}, "")
		h.persistSession(session)
		session.AddEvent(chat.ChatEvent{Type: "done"})
		return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, SlashCommand: true}, true
	}
	return ChatResponse{}, false
}

func (h *ChatHandler) ProcessMessage(ctx context.Context, req ChatRequest) ChatResponse {
	if req.Content == "" {
		return ChatResponse{Error: "content is required", StatusCode: http.StatusBadRequest}
	}

	chatIsAdmin := chatSubmitterIsAdmin(ctx, h.App, req.UserTier)
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	req.OwnerID = chatRequestOwner(ctx, req)
	session, turnCtx, err := h.prepareChatSession(ctx, sessionID, req.OwnerID, req.UserTier, chatIsAdmin)
	if err != nil {
		return ChatResponse{Error: "session not found", StatusCode: http.StatusNotFound}
	}
	ctx = turnCtx

	// Remember the submitter's authority for follow-up turns: a delegated task
	// that finishes after this turn ended is resumed by startFollowUpTurn,
	// which has no request context to re-derive it from. Without this it would
	// have to guess, and a wrong guess either escalates to admin or silently
	// loses the ability to delegate from the follow-up.
	h.lastSubmitterAdmin.Store(sessionID, chatIsAdmin)

	if resp, ok := h.handleSlashCommandGate(req, session, sessionID); ok {
		return resp
	}
	h.autoDetectBranchMode(&req, session)
	if !session.AppendUserMessage(req.MessageID, req.Content, req.ParentID, req.ReplyTo) {
		return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, AlreadyProcessed: true}
	}
	// Feed the user message to the async profile indexer for preference
	// extraction. Spec: docs/completed/2026-11-27/USER_PROFILE_INCREMENTAL_INDEXER.md §2
	if h.profileIndexer != nil {
		h.profileIndexer.OnUserMessage(chat.ChatMessage{
			Role:    "user",
			Content: req.Content,
		})
	}
	// Mark this turn as awaiting a reply for crash recovery.
	// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md §3.4.
	session.SetAwaitingReply(&chat.AwaitingReplyInfo{
		MessageID: req.MessageID,
		Content:   req.Content,
		StartedAt: time.Now(),
	})
	h.persistSession(session)

	// ── Direct Task Submission Gate ───────────────────────────────────
	if req.DirectTask {
		return h.handleDirectTask(req, session, sessionID, chatIsAdmin)
	}

	if h.Harness.Provider == nil {
		return ChatResponse{SessionID: sessionID, Error: "no provider configured"}
	}

	// Resolve the harness for this request. If model_override is set,
	// clone the default provider config, swap the model, and resolve via
	// providers.Get() (cached — repeated requests with the same model reuse
	// the same provider instance). Falls back to the default harness on error.
	harness := h.Harness
	if req.ModelOverride != "" && req.ModelOverride != harness.Model {
		// audit NEW-1/NEW-2: restrict model_override to models already configured
		// in the registry. This bounds the provider cache and prevents cost/privilege bypass.
		if !h.App.Registry.IsModelConfigured(req.ModelOverride) {
			return ChatResponse{Error: "model_override rejected: model not configured in registry", StatusCode: http.StatusBadRequest}
		}
		pc := h.App.Registry.ResolveProviderConfig(&config.Role{})
		if pc == nil {
			return ChatResponse{Error: "model_override failed: no provider config resolved", StatusCode: http.StatusInternalServerError}
		}
		clone := *pc
		clone.Model = req.ModelOverride
		p, perr := providers.Get(&clone, h.App.Registry.ExternalRuntimes)
		if perr != nil {
			return ChatResponse{Error: fmt.Sprintf("model_override failed: %v", perr), StatusCode: http.StatusInternalServerError}
		}
		harness = harnessForResolvedProvider(h.Harness, p, req.ModelOverride, nil, clone.MaxContextWindow)
	}

	// Role selection: when role_id is provided, resolve the role from the
	// registry and build a role-bound harness with the role's provider, model,
	// and system prompt (Instruction). When role_id is empty, fall back to
	// the session's active role for continuity across reconnects.
	effectiveRoleID := req.RoleID
	if effectiveRoleID == "" {
		effectiveRoleID = session.GetActiveRoleID()
	}
	// Three-tier gateway: default to front desk role when no role is specified
	// and no model override is requested. The front desk model (medium, fast)
	// classifies complexity and escalates to the planner (strong model) via
	// delegate_to_orchestrator. See docs/completed/2026-10-11/CHAT_GATEWAY_TIERS_DESIGN.md §3.
	if (effectiveRoleID == "" || effectiveRoleID == "default") && req.ModelOverride == "" && h.App.Registry != nil {
		h.App.Registry.Mu.RLock()
		_, frontDeskExists := h.App.Registry.Roles[defaultChatFrontDeskRole]
		h.App.Registry.Mu.RUnlock()
		if frontDeskExists {
			effectiveRoleID = defaultChatFrontDeskRole
		}
	}
	if effectiveRoleID != "" && effectiveRoleID != "default" {
		h.App.Registry.Mu.RLock()
		rolePtr := h.App.Registry.Roles[effectiveRoleID]
		var role *config.Role
		if rolePtr != nil {
			rc := *rolePtr
			role = &rc
		}
		h.App.Registry.Mu.RUnlock()
		if role != nil {
			pc := h.App.Registry.ResolveProviderConfig(role)
			if pc != nil {
				if p, err := providers.Get(pc, h.App.Registry.ExternalRuntimes); err == nil {
					harness = harnessForResolvedProvider(h.Harness, p, pc.Model, role, pc.MaxContextWindow)
					if req.RoleID != "" {
						session.SetActiveRoleID(req.RoleID)
					}
				}
			}
		}
	}

	taskID := "chat_" + sessionID
	history := session.SnapshotMessages()

	// ── Exact Reference Injection (reply-to) ───────────────────────────
	// If the user replied to a specific old message, inject its content as
	// a context block so the model sees the referenced text. For Telegram,
	// ReplyToContent is already populated from the update payload. For web,
	// look up by internal message ID from the session tree.
	if req.ReplyTo != "" && req.ReplyToContent == "" {
		if msg := session.GetMessage(req.ReplyTo); msg != nil {
			req.ReplyToContent = msg.Content
		}
	}
	if req.ReplyToContent != "" {
		refMsg := chat.ChatMessage{
			Role:    "user",
			Content: "[Referenced message]\n" + req.ReplyToContent,
		}
		// The new user message was already appended to the session above
		// (AppendUserMessage), so SnapshotMessages ends with it. Insert the
		// quotation BEFORE it: the model must read the quote as context for
		// the reply that follows, not as a follow-up turn. This also keeps
		// ChatHarness.Run's `query` (last user message = the system-prompt
		// subject) pointed at the user's actual message — appending after it
		// made the system prompt describe the QUOTED text instead.
		// See CHAT_BRANCHING_AND_QUOTE_DESIGN.md §4: "Inject as a quotation
		// before the new message".
		if n := len(history); n > 0 && history[n-1].Role == "user" {
			history = append(history[:n-1], refMsg, history[n-1])
		} else {
			history = append(history, refMsg)
		}
	}

	// ── Chat Hook Gate (chat_habit + chat_pre_turn) ─────────────────────
	if h.hookRunner != nil {
		hookCtx := core.HookContext{
			Point:     core.HookChatPreTurn,
			SessionID: sessionID,
			RoleID:    effectiveRoleID,
			Input: map[string]any{
				"user_message": req.Content,
				"history":      history,
			},
		}

		// audit R-MED-1: track habit hook goroutine with h.wg — previously
		// orphaned on server shutdown, violating the documented invariant.
		h.wg.Add(1)
		go func() {
			// audit R-1: recover prevents panic in habit hook from crashing the process.
			defer func() {
				if r := recover(); r != nil {
					log.Printf("PANIC in habit hook: %v", r)
				}
			}()
			defer h.wg.Done()
			habitCtx, habitCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer habitCancel()
			h.hookRunner.Run(habitCtx, core.HookChatHabit, hookCtx)
		}()

		hookCtx2, hookCancel := context.WithTimeout(ctx, 2*time.Second)
		preResult := h.hookRunner.Run(hookCtx2, core.HookChatPreTurn, hookCtx)
		hookCancel()
		switch preResult.Action {
		case "redirect_to_orchestrator":
			redirectRole, _ := preResult.Data["role_id"].(string)
			redirectMCPsStr, _ := preResult.Data["mcps"].(string)
			redirectReason, _ := preResult.Data["reason"].(string)
			redirectMCPs := []string{}
			if redirectMCPsStr != "" {
				redirectMCPs = strings.Split(redirectMCPsStr, ",")
			}
			if redirectRole != "" {
				h.wg.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Fprintf(os.Stderr, "[chat-handlers] goroutine panic: %v\n%s", r, debug.Stack())
						}
					}()
					defer h.wg.Done()
					session.AddEvent(chat.ChatEvent{Type: "tool_call", Data: "delegate_to_orchestrator", Meta: map[string]any{
						"name":        "delegate_to_orchestrator",
						"args":        req.Content,
						"hook_routed": true,
						"role":        redirectRole,
						"mcps":        redirectMCPs,
						"reason":      redirectReason,
					}})
					submitID, err := h.submitChatTask([]schemas.StepInput{{
						ID:               "s1",
						Task:             req.Content,
						RoleID:           redirectRole,
						AdditionalMCPs:   redirectMCPs,
						SubmitterIsAdmin: chatIsAdmin,
					}}, sessionID)
					if err != nil {
						session.AddEvent(chat.ChatEvent{Type: "error", Data: err.Error()})
						return
					}
					// `task_id`, not `submit_id`: the value is `task_<hex>`
					// (schemas.NewTaskID), i.e. the same identifier the stream
					// already publishes as `meta.task_id` on progress and
					// task_callback. The whole point of stamping it here is to
					// let a consumer join this delegation to that task, and a
					// join needs one spelling, not two.
					result := fmt.Sprintf("Hook-routed to orchestrator task %s (role=%s, reason=%s)", submitID, redirectRole, redirectReason)
					session.AddEvent(chat.ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": "delegate_to_orchestrator", "task_id": submitID}})
					session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: result, ClientID: "asst_" + req.MessageID}, "")
					h.persistSession(session)
					session.AddEvent(chat.ChatEvent{Type: "done"})
				}()
				return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, HookRouted: true}
			}
			session.AddEvent(chat.ChatEvent{Type: "hook_warning", Data: "redirect_to_orchestrator requested without role_id — ignored"})
		case "abort":
			reason, _ := preResult.Data["reason"].(string)
			session.AddEvent(chat.ChatEvent{Type: "hook_abort", Data: reason})
			return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, Aborted: true, Reason: reason}
		case "inject_context":
			if ctxText, _ := preResult.Data["context"].(string); ctxText != "" {
				session.AddEvent(chat.ChatEvent{Type: "hook_context_injected", Data: ctxText})
			}
		}
	}

	// Light Routing Gate REMOVED (2026-10-21, audit R2/D4). It auto-delegated
	// to a hardcoded vendor role/MCP set based on substrings in the message
	// ("generate", "image", ...), bypassing authorization. Callers must route
	// explicitly (role_id / additional_mcps) or rely on the planner.

	// Ensure the harness Delegate propagates sessionID so task completion
	// callbacks can find this chat session. Create a per-request copy with
	// shared pointer fields and a fresh mutex (vet-safe: avoids copying
	// the sync.Mutex in ChatHarness.SteerLock).
	hCopy := cloneHarnessForRequest(harness)
	// Remember WHICH harness answered this turn (role-bound or default) so the
	// wake turn that this turn may schedule resumes as the same agent. Set
	// before Run: the completion callback can fire while the turn is still
	// executing, and a wake turn that starts first would otherwise read nothing.
	h.noteSessionHarness(sessionID, harness)
	hCopy.Delegate = func(prompt string, maxTurnsCap int) (string, error) {
		id, err := h.submitChatTask(
			[]schemas.StepInput{{ID: "s1", Task: prompt, SubmitterIsAdmin: chatIsAdmin, MaxTurnsCap: maxTurnsCap}},
			sessionID)
		// The ledger, not the harness, is what a later wake turn reads: this
		// harness is discarded when the turn ends. Recorded only on success —
		// an id that was never handed off must not unlock (or lock) anything.
		if err == nil {
			h.noteDelegatedTask(sessionID, id)
		}
		return id, err
	}
	// Record that this turn was shown a finished task's terminal state, and what
	// it actually read. A follow-up wake queued while this turn was in flight is
	// then suppressed only if the evidence says the result already reached the
	// model — see chat_wake.go deliverPendingWake and wakeDropDecision.
	hCopy.OnTerminalTaskSeen = func(taskID string) {
		h.noteTaskConsumed(sessionID, taskID)
	}
	hCopy.OnFileRead = func(basename string) {
		h.noteFileRead(sessionID, basename)
	}
	// HelpProvider is per-request: it closes over the user's tier so the
	// chat model gets tier-filtered system manual info without execTool
	// needing to know about tiers.
	hCopy.HelpProvider = func(topic string) string {
		return core.BuildSystemManual(h.App.Registry, topic, req.UserTier)
	}
	// ScheduleExec uses the caller's stable owner, not the session owner that
	// prepareChatSession may substitute for shared-session approval routing.
	scheduleOwnerID := req.OwnerID
	hCopy.ScheduleExec = func(action string, args map[string]any) (string, error) {
		tier := tools.TierPublic
		if req.UserTier == tools.TierAdmin {
			tier = tools.TierAdmin
		}
		// Lifecycle-aware context: derive from app.LifecycleCtx so the
		// schedule callback is cancelled on shutdown (audit C-1).
		// Falls back to Background when LifecycleCtx is not wired.
		baseCtx := h.App.LifecycleCtx
		if baseCtx == nil {
			baseCtx = context.Background()
		}
		ctx := context.WithValue(baseCtx, tools.AuthTierKey, tier)
		ctx = context.WithValue(ctx, tools.AuthOwnerKey, scheduleOwnerID)
		result, err := tools.InvokeSubsystem(ctx, h.App, "schedule", action, args)
		if err != nil {
			return "", err
		}
		data, _ := json.Marshal(result)
		return string(data), nil
	}
	// Resolve VLM attachments: convert relative paths (e.g. "uploads/seg.png")
	// to absolute paths under the session workspace root.
	if len(req.Attachments) > 0 {
		wsRoot, _ := filepath.Abs(filepath.Join("outputs", "chat_sessions", sessionID))
		resolved := make([]schemas.Attachment, 0, len(req.Attachments))
		for _, a := range req.Attachments {
			absPath := filepath.Join(wsRoot, a.Path)
			resolved = append(resolved, schemas.Attachment{
				MimeType: a.MimeType,
				Path:     absPath,
				Label:    a.Label,
			})
		}
		hCopy.Attachments = resolved
	}
	harness = hCopy

	// Cancel any in-flight run for this session (true interrupt support).
	if oldCancel, ok := h.cancelFuncs.LoadAndDelete(sessionID); ok {
		oldCancel.(context.CancelFunc)()
	}

	h.wg.Add(1)
	go func() {
		// terminalSent tracks whether this turn emitted a terminal event
		// ("done" or "error"). StreamHandler only returns on a terminal event,
		// so a turn that dies without one leaves the client hanging until its
		// own deadline — see finishTurn.
		terminalSent := false
		// Registered first so it runs last (defers are LIFO): Shutdown() must
		// not stop waiting until finishTurn has emitted its event.
		defer h.wg.Done()
		defer func() {
			h.finishTurn(session, terminalSent, recover())
		}()
		// detach from request ctx: the chat turn goroutine outlives the HTTP
		// response; bounded by the turn budget + h.cancelFuncs. The budget is
		// env-overridable because the turn clock and the container clock are
		// different clocks (see chat_turn_timeout.go).
		runCtx, cancel := context.WithTimeout(context.Background(), chatTurnTimeout())
		defer cancel()
		h.cancelFuncs.Store(sessionID, cancel)
		defer h.cancelFuncs.Delete(sessionID)
		// Consumed markers describe "the turn that just ended". This turn is a
		// new one, so any leftover marker must not suppress its wakes.
		h.clearConsumedTasks(sessionID)
		h.clearReadFiles(sessionID)
		final, err := harness.Run(runCtx, taskID, history, func(ev chat.ChatEvent) error {
			if harness.Role != nil {
				if ev.Meta == nil {
					ev.Meta = map[string]any{}
				}
				if _, ok := ev.Meta["role_id"]; !ok {
					ev.Meta["role_id"] = harness.Role.ID
					ev.Meta["role_name"] = harness.Role.Name
				}
			}
			if ev.Type == "done" || ev.Type == "error" {
				terminalSent = true
			}
			session.AddEvent(ev)
			return nil
		})
		if (err == nil || chat.IsPartialRun(err)) && final != "" {
			if err != nil {
				// Partial (turn ceiling or exploration ceiling): the run stopped
				// deliberately but still produced output. Log it so the partial
				// is traceable.
				fmt.Fprintf(os.Stderr, "[chat] session=%s task=%s turn ended with partial result: %v\n",
					sessionID, taskID, err)
			}
			if err == nil && h.hookRunner != nil {
				postResult := h.hookRunner.Run(runCtx, core.HookChatPostTurn, core.HookContext{
					Point:     core.HookChatPostTurn,
					SessionID: sessionID,
					RoleID:    effectiveRoleID,
					Input: map[string]any{
						"user_message": req.Content,
					},
					Output: map[string]any{
						"response": final,
					},
				})
				for _, ann := range postResult.Annotations {
					session.AddEvent(chat.ChatEvent{Type: "hook_annotation", Data: ann.Text, Meta: map[string]any{"annotation_type": ann.Type}})
				}
			}

			session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: final, ClientID: "asst_" + req.MessageID}, "")

			// Enqueue chat_reply to outbox for durable delivery (fault recovery).
			// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md §3.3.
			if h.Outbox != nil && final != "" {
				// audit FIX (2026-11-21): was context.Background() with no
				// timeout — a slow SQLite write could block the chat turn
				// indefinitely. 5s bounds the durability write.
				enqCtx, enqCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer enqCancel()
				if added, err := h.Outbox.Enqueue(enqCtx, store.OutboxItem{
					SessionID: sessionID,
					Kind:      kindChatReply,
					DedupeKey: "chat_reply:" + sessionID + ":" + req.MessageID,
					Text:      final,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "[chat] WARN: failed to enqueue chat_reply for %s: %v\n", sessionID, err)
				} else if added {
					h.logOutbox(eventNotificationQueued, map[string]any{
						"kind": kindChatReply, "dedupe_key": "chat_reply:" + sessionID + ":" + req.MessageID,
						"session_id": sessionID,
					})
				}
			}

			// Clear the pending reply mark — the turn is complete.
			// See docs/completed/2026-11-21/CHAT_REPLY_FAULT_RECOVERY_PLAN.md §3.5.
			session.ClearAwaitingReply()
			h.persistSession(session)

			// ── Semantic Recall: index this turn in ContextArchive ───
			// buildSystem's SearchForest already scopes by taskID =
			// "chat_"+sessionID, so indexing here makes past turns
			// retrievable for in-session semantic recall.
			// Q5-3: a ceiling-truncated partial stays in the transcript but must
			// never become retrievable knowledge.
			if err == nil && !chat.IsTruncated(final) && harness.Archive != nil && harness.Embed != nil {
				emb, embErr := harness.Embed.Embed(runCtx, req.Content)
				if embErr != nil {
					fmt.Fprintf(os.Stderr, "[chat] WARN: embedding failed for turn %s: %v\n", req.MessageID, embErr)
				}
				// audit fix L3: surface Archive.Append failure — previously swallowed,
				// so semantic recall silently missed this turn with no signal.
				if aerr := harness.Archive.Append(core.MemoryItem{
					TaskID:    taskID,
					NodeID:    req.MessageID,
					Intent:    req.Content,
					Summary:   final,
					Embedding: emb,
					Timestamp: time.Now(),
				}); aerr != nil {
					fmt.Fprintf(os.Stderr, "[chat] WARN: failed to index turn in ContextArchive: %v\n", aerr)
				}
			}
		} else if err != nil {
			// Non-partial error (e.g. context deadline exceeded, provider
			// failure). Previously this was silently dropped — the turn
			// produced no output and no log line, so the only trace was a
			// hanging SSE stream. The RotatingWriter now captures stderr,
			// but an explicit log line makes the cause searchable.
			fmt.Fprintf(os.Stderr, "[chat] session=%s task=%s turn failed: %v\n",
				sessionID, taskID, err)
		}
	}()

	resp := ChatResponse{SessionID: sessionID, MessageID: req.MessageID}
	if harness.Role != nil {
		resp.RoleID = harness.Role.ID
		resp.RoleName = harness.Role.Name
	}
	return resp
}

// finishTurn guarantees that a chat turn leaves a terminal event in the
// session before its goroutine exits.
//
// StreamHandler loops until it sees "done" or "error"; it has no other exit
// path besides the client disconnecting or the process dying. Any turn that
// returns (or dies) without one therefore leaves the caller waiting for its
// own deadline. That is not theoretical: on 2026-10-30 the delegated run of
// harness-bench task 026 finished its model work at t=22 s, then the JSON
// skeletonizer panicked on a valid read_file payload, the panic was recovered
// and logged with nothing else emitted, and the round sat idle for the
// remaining 1137 s of its 1200 s budget before being scored as a model error.
// The panic itself is fixed at the source; this is the containment that keeps
// the next one cheap.
//
// panicVal is the value returned by recover() (nil when there was no panic).
// terminalSent reports whether Run already emitted "done"/"error".
func (h *ChatHandler) finishTurn(session *chat.ChatSession, terminalSent bool, panicVal any) {
	if session == nil {
		return
	}
	if panicVal != nil {
		fmt.Fprintf(os.Stderr, "[chat-handlers] goroutine panic: %v\n%s", panicVal, debug.Stack())
	}
	if !terminalSent {
		data := "chat turn ended without producing a terminal event"
		meta := map[string]any{"no_terminal": true}
		if panicVal != nil {
			data = fmt.Sprintf("chat turn aborted by internal error: %v", panicVal)
			meta = map[string]any{"panic": true}
		}
		session.AddEvent(chat.ChatEvent{Type: "error", Data: data, Meta: meta})
		h.persistSession(session)
	}
	// Deliver a queued wake if one was deferred while this turn was in flight.
	// The cancelFuncs slot is already freed by the time finishTurn runs (it's
	// the first defer, finishTurn is the third), so startFollowUpTurnWithTrigger
	// sees the session as idle and can start a follow-up turn.
	h.deliverPendingWake(session.ID)
}
