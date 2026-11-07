package api

import (
	"github.com/daybeam/vortex/core/sieve"
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
// See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §change 1.
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
	// See docs/plan/SESSION_MEMORY_DELETION_DESIGN.md.
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
	// lastSubmitterAdmin lets a follow-up turn delegate with the same
	// authority as the turn that spawned it, instead of guessing.
	lastSubmitterAdmin sync.Map // sessionID → bool
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
// See docs/CHAT_GATEWAY_TIERS_DESIGN.md §3.
const defaultChatFrontDeskRole = "chat_front_desk"

// Shutdown waits for all in-flight chat goroutines to complete. Call before
// server close to prevent orphaned LLM provider calls (audit C4).
func (h *ChatHandler) Shutdown() {
	h.wg.Wait()
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

func NewChatHandler(app *tools.App) *ChatHandler {
	var chatBackend store.IChatBackend
	var outbox store.IChatOutbox
	if app.DB != nil {
		chatBackend = store.NewSQLiteChatBackend(app.DB)
		// Same DB, different job: sessions are history, the outbox is a queue.
		// Without a DB there is no queue, and transports keep delivering
		// inline (see TelegramChannel.deliversOutOfBand).
		outbox = store.NewSQLiteChatOutbox(app.DB)
	}
	// Use absolute path for session store when RootDir is set.
	// Without this, NewChatSessionStore("") defaults to "outputs/chat" (relative
	// to CWD), which changes between runs — the desktop binary in build/bin/ would
	// store sessions in build/bin/outputs/chat/ and lose them on recompile.
	sessionDir := ""
	if app.RootDir != "" {
		sessionDir = filepath.Join(app.RootDir, "outputs", "chat")
	}
	chatStore := chat.NewChatSessionStore(sessionDir, chatBackend)

	chatMemWindowSize := 10
	if app.Registry != nil && app.Registry.System.ChatMemoryWindowSize > 0 {
		chatMemWindowSize = app.Registry.System.ChatMemoryWindowSize
	}
	chatMem := chat.NewRollingWindowMemory(chatMemWindowSize)
	if app.Registry != nil && app.Registry.System.ChatMemoryMaxTokens > 0 {
		chatMem.MaxTokens = app.Registry.System.ChatMemoryMaxTokens
	}

	// Wire turn/token budgets from config (harness-bench report §4.1).
	// chat_max_turns is the chat-specific knob; max_tool_turns (shared with the
	// DAG path) stays as its fallback so the wiring added 2026-10-28 keeps
	// working. Default is chat.DefaultMaxTurns, raised 6 → 50 on 2026-11-04.
	chatMaxTurns := chat.DefaultMaxTurns
	if app.Registry != nil {
		if t := app.Registry.System.MaxToolTurns; t > 0 {
			chatMaxTurns = t
		}
		if t := app.Registry.System.ChatMaxTurns; t > 0 {
			chatMaxTurns = t
		}
	}
	chatMaxOutputTokens := 2048
	if app.Registry != nil && app.Registry.System.ChatMaxOutputTokens > 0 {
		chatMaxOutputTokens = app.Registry.System.ChatMaxOutputTokens
	}
	// SessionRoot from env var (harness-bench report §4.2). When set, core
	// tools resolve relative paths against the workspace root. Empty = legacy.
	chatSessionRoot := os.Getenv("HARNESSBENCH_WORKSPACE")

	harness := &chat.ChatHarness{
		Archive:           app.Archive,
		MemoryBank:        mbStoreForHarness(app),
		Embed:             app.EmbedClient,
		OutputBase:        "outputs",
		MaxTurns:          chatMaxTurns,
		MaxOutputTokens:   chatMaxOutputTokens,
		SessionRoot:       chatSessionRoot,
		Sieve:             sieve.NewSieve(100),
		Memory:            chatMem,
		ReadinessGateSoft: app.Registry.System.ReadinessGateSoft,
		// Delegate is assigned below, after the handler exists, so that it can
		// go through the single submission path (submitChatTask).
		// DelegateStatus closes the loop Delegate opens: without it the model
		// gets a task id it can never read back, while execute_code returns its
		// output in the same turn — which is why delegation measured 0/104.
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			if app.Scheduler == nil {
				return nil, false
			}
			if wait > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Second)
				defer cancel()
				return app.Scheduler.WaitTask(ctx, taskID, wait, "full")
			}
			return app.Scheduler.GetStatus(taskID, "full")
		},
	}

	// Resolve the configured default provider.
	if pc := app.Registry.ResolveProviderConfig(&config.Role{}); pc != nil {
		if p, err := providers.Get(pc, app.Registry.ExternalRuntimes); err == nil {
			harness.Provider = p
			harness.Model = pc.Model
			harness.MaxContextWindow = pc.MaxContextWindow
		}
	}

	handler := &ChatHandler{
		App:        app,
		Harness:    harness,
		Store:      chatStore,
		Outbox:     outbox,
		hookRunner: core.NewHookRunner(app.Registry.System.Hooks, app.Registry.ExternalRuntimes, app.Logger),
	}

	// Construction-time fallback Delegate (no request ctx): derive the tier
	// from the app itself (EffectiveTier falls back to app.Tier), so a
	// local/admin app isn't fail-closed on dynamic mounts. The request-scoped
	// copy (hCopy.Delegate) uses the real ctx tier.
	handler.Harness.Delegate = func(prompt string) (string, error) {
		isAdmin := tools.EffectiveTier(context.Background(), app) == tools.TierAdmin
		return handler.submitChatTask(
			[]schemas.StepInput{{ID: "s1", Task: prompt, SubmitterIsAdmin: isAdmin}}, "")
	}

	handler.WireTaskCompletionCallbacks()
	return handler
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

func (h *ChatHandler) SendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		SessionID      string           `json:"session_id"`
		MessageID      string           `json:"message_id"`
		Content        string           `json:"content"`
		ParentID       string           `json:"parent_id"`
		InputDir       string           `json:"input_dir"`
		ModelOverride  string           `json:"model_override"`
		RoleID         string           `json:"role_id"`
		DirectTask     bool             `json:"direct_task"`
		ReplyTo        string           `json:"reply_to"`
		ReplyToContent string           `json:"reply_to_content"`
		Attachments    []AttachmentSpec `json:"attachments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	resp := h.ProcessMessage(r.Context(), ChatRequest{
		SessionID:      body.SessionID,
		MessageID:      body.MessageID,
		Content:        body.Content,
		ParentID:       body.ParentID,
		InputDir:       body.InputDir,
		ModelOverride:  body.ModelOverride,
		RoleID:         body.RoleID,
		DirectTask:     body.DirectTask,
		CallerID:       tools.CallerIdentity(r.Context()),
		ReplyTo:        body.ReplyTo,
		ReplyToContent: body.ReplyToContent,
		Attachments:    body.Attachments,
	})

	if resp.StatusCode != 0 {
		http.Error(w, resp.Error, resp.StatusCode)
		return
	}
	if resp.Error != "" {
		writeJSON(w, map[string]any{"session_id": resp.SessionID, "error": resp.Error})
		return
	}
	writeJSON(w, resp)
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
// See docs/plan/CHAT_BRANCHING_AND_QUOTE_DESIGN.md §5-6.
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
	// See docs/plan/LIVE_COPILOT_SHARED_SESSION_DESIGN.md §2.3.
	if strings.HasPrefix(req.Content, "/live") {
		if handled, response := h.cmdLive(req.Content, session, sessionID, req.UserTier, req.CallerID); handled {
			session.AddEvent(chat.ChatEvent{Type: "slash_command", Data: response})
			session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: response}, "")
			h.persistSession(session)
			session.AddEvent(chat.ChatEvent{Type: "done"})
			return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, SlashCommand: true}, true
		}
	}
	if handled, response := h.handleSlashCommand(req.Content, session, sessionID, req.UserTier); handled {
		session.AddEvent(chat.ChatEvent{Type: "slash_command", Data: response})
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: response}, "")
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

	session := h.Store.GetOrCreate(sessionID)

	// Remember the submitter's authority for follow-up turns: a delegated task
	// that finishes after this turn ended is resumed by startFollowUpTurn,
	// which has no request context to re-derive it from. Without this it would
	// have to guess, and a wrong guess either escalates to admin or silently
	// loses the ability to delegate from the follow-up.
	h.lastSubmitterAdmin.Store(sessionID, chatIsAdmin)

	// Live Co-pilot: override AuthOwnerKey for guest users on shared sessions.
	// This routes their requests through the session owner's approval pipeline
	// (human_approval_required → checkApprovalAuthority → mount_policy).
	// See docs/plan/LIVE_COPILOT_SHARED_SESSION_DESIGN.md §2.2.
	if session.IsShared() && req.UserTier == TierGuest {
		if ownerID := session.GetOwnerID(); ownerID != "" {
			ctx = context.WithValue(ctx, tools.AuthOwnerKey, ownerID)
		}
	}

	if resp, ok := h.handleSlashCommandGate(req, session, sessionID); ok {
		return resp
	}
	h.autoDetectBranchMode(&req, session)
	if !session.AppendUserMessage(req.MessageID, req.Content, req.ParentID, req.ReplyTo) {
		return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, AlreadyProcessed: true}
	}
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
		harness = &chat.ChatHarness{
			Provider:          p,
			Model:             req.ModelOverride,
			Archive:           h.Harness.Archive,
			MemoryBank:        h.Harness.MemoryBank,
			Embed:             h.Harness.Embed,
			OutputBase:        h.Harness.OutputBase,
			Delegate:          h.Harness.Delegate,
			DelegateStatus:    h.Harness.DelegateStatus,
			MaxTurns:          h.Harness.MaxTurns,
			MaxOutputTokens:   h.Harness.MaxOutputTokens,
			SessionRoot:       h.Harness.SessionRoot,
			Sieve:             h.Harness.Sieve,
			Memory:            h.Harness.Memory,
			ReadinessGateSoft: h.Harness.ReadinessGateSoft,
			MaxContextWindow:  clone.MaxContextWindow,
		}
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
	// delegate_to_orchestrator. See docs/CHAT_GATEWAY_TIERS_DESIGN.md §3.
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
					harness = &chat.ChatHarness{
						Provider:          p,
						Model:             pc.Model,
						Role:              role,
						Archive:           h.Harness.Archive,
						MemoryBank:        h.Harness.MemoryBank,
						Embed:             h.Harness.Embed,
						OutputBase:        h.Harness.OutputBase,
						Delegate:          h.Harness.Delegate,
						DelegateStatus:    h.Harness.DelegateStatus,
						MaxTurns:          h.Harness.MaxTurns,
						MaxOutputTokens:   h.Harness.MaxOutputTokens,
						SessionRoot:       h.Harness.SessionRoot,
						Sieve:             h.Harness.Sieve,
						Memory:            h.Harness.Memory,
						ReadinessGateSoft: h.Harness.ReadinessGateSoft,
						MaxContextWindow:  pc.MaxContextWindow,
					}
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
					result := fmt.Sprintf("Hook-routed to orchestrator task %s (role=%s, reason=%s)", submitID, redirectRole, redirectReason)
					session.AddEvent(chat.ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": "delegate_to_orchestrator"}})
					session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: result}, "")
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
	hCopy.Delegate = func(prompt string) (string, error) {
		return h.submitChatTask(
			[]schemas.StepInput{{ID: "s1", Task: prompt, SubmitterIsAdmin: chatIsAdmin}},
			sessionID)
	}
	// Record which finished tasks this turn actually read. A follow-up wake
	// queued while this turn was in flight is then suppressed instead of
	// answered a second time — see chat_wake.go deliverPendingWake.
	hCopy.OnTerminalTaskSeen = func(taskID string) {
		h.noteTaskConsumed(sessionID, taskID)
	}
	// HelpProvider is per-request: it closes over the user's tier so the
	// chat model gets tier-filtered system manual info without execTool
	// needing to know about tiers.
	hCopy.HelpProvider = func(topic string) string {
		return core.BuildSystemManual(h.App.Registry, topic, req.UserTier)
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
		if (err == nil || chat.IsMaxTurnsPartial(err)) && final != "" {
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

			session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: final}, "")
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

func (h *ChatHandler) StreamHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		http.Error(w, "session_id required", http.StatusBadRequest)
		return
	}
	session := h.Store.Get(sessionID)
	if session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	lastSeq := parseSeq(r.Header.Get("Last-Event-ID"))
	// The Last-Event-ID header is authoritative on browser auto-reconnects
	// (it always carries the latest seen seq). The query param is only a
	// fallback for the initial connect, where no header exists yet — letting
	// it override would replay the whole history on every reconnect and
	// duplicate tool-call entries in the UI.
	if lastSeq == 0 {
		if q := r.URL.Query().Get("last_event_id"); q != "" {
			lastSeq = parseSeq(q)
		}
	}

	if chat.SinkEnabled() {
		h.streamHandlerWithSink(w, r, sessionID, lastSeq)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	lastPing := time.Now()
	for {
		events := session.EventsAfter(lastSeq)
		terminal := false
		for _, ev := range events {
			lastSeq = ev.Seq
			writeSSE(w, ev)
			if ev.Type == "done" || ev.Type == "error" {
				terminal = true
			}
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		if terminal {
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if time.Since(lastPing) > 15*time.Second {
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
				lastPing = time.Now()
			}
		}
	}
}
