package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/schemas"
)

// cloneHarnessForRequest builds the per-request copy of a chat harness.
//
// It exists because SendHandler cannot simply do `*src`: ChatHarness carries a
// sync.Mutex (SteerLock), so a struct copy is a vet failure. The workaround was
// an explicit field list, and that list has silently dropped fields three times
// (ReadinessGateSoft, MaxContextWindow, Segments). Most recently DelegateStatus:
// the tool table and the receipt came from two different harnesses, so the tool
// existed while the receipt behaved as if the feature were unwired — an
// end-to-end run showed "next: the result arrives as a task_callback event"
// instead of the follow-up call.
//
// Delegate is deliberately NOT copied: the caller replaces it with a
// session-scoped closure. Everything else must be, and
// TestCloneHarnessForRequest_ListsEveryField keeps the list honest.
// cloneHarnessForRequest returns a per-request copy of the shared harness.
//
// The copy is a pointer for a reason that has nothing to do with style:
// ChatHarness carries a sync.Mutex in SteerLock, and returning it by value is
// exactly what vet's copylocks flags ("return copies lock value"). Callers hold
// the copy and take its address anyway (harness = hCopy), so the pointer is
// what they already wanted.
func cloneHarnessForRequest(src *chat.ChatHarness) *chat.ChatHarness {
	h := chat.ChatHarness{
		Provider:          src.Provider,
		Model:             src.Model,
		Role:              src.Role,
		Archive:           src.Archive,
		MemoryBank:        src.MemoryBank,
		Embed:             src.Embed,
		OutputBase:        src.OutputBase,
		MaxTurns:          src.MaxTurns,
		MaxOutputTokens:   src.MaxOutputTokens,
		SessionRoot:       src.SessionRoot,
		Sieve:             src.Sieve,
		Memory:            src.Memory,
		Attachments:       src.Attachments,
		ReadinessGateSoft: src.ReadinessGateSoft,
		DelegateStatus:    src.DelegateStatus,
		Segments:          src.Segments,
		MaxContextWindow:  src.MaxContextWindow,
	}
	// projectResolver is unexported, so it cannot be copied field-by-field from
	// this package. Forwarding through the base harness's public ProjectOf
	// keeps the per-request copy resolving the same project space; without it
	// ProjectOf falls back to "default" on the copy and cross-project recall
	// silently degrades to the default scope — compiled fine, behaved wrong.
	// Caught by TestCloneHarnessForRequest_ClassifiesEveryField, which exists
	// precisely because this class of omission is invisible at build time.
	h.SetProjectResolver(src.ProjectOf)
	return &h
}

// chatWorkspaceRoot is the workspace root a chat-initiated orchestrator task
// must be submitted with.
//
// It exists because "the workspace" is one concept with five submission paths
// (construction-time Delegate, direct_task, the hook redirect, the per-request
// Delegate, and the MCP tool), and every one of them used to pass "" — so the
// scheduled task's graph carried WorkspaceRoot="", the spawned step agent built
// its SafePathValidator with an empty SessionRoot, and its core tools fell back
// to outputs/<taskID>. A delegated subagent therefore could not see the files
// its parent had just been reading. Returning "" here keeps the legacy
// behaviour for deployments that never configure a workspace root.
func chatWorkspaceRoot(h *chat.ChatHarness) string {
	if h != nil {
		return h.SessionRoot
	}
	return ""
}

// handleDirectTask submits req.Content as a one-step orchestrator task and
// returns the immediate response. The submission runs in a tracked goroutine so
// the caller is not blocked on scheduler persistence.
func (h *ChatHandler) handleDirectTask(req ChatRequest, session *chat.ChatSession, sessionID string, chatIsAdmin bool) ChatResponse {
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
			"direct_task": true,
			"role":        req.RoleID,
		}})
		var submitID string
		var err error
		if req.InputDir != "" {
			wsRoot, _ := filepath.Abs(filepath.Join("outputs", "chat_sessions", sessionID))
			submitID, err = h.App.Scheduler.SubmitWithSessionIR(
				[]schemas.StepInput{{
					ID:               "s1",
					Task:             req.Content,
					RoleID:           req.RoleID,
					InputDir:         req.InputDir,
					SubmitterIsAdmin: chatIsAdmin,
				}},
				nil, nil, nil, "", sessionID, wsRoot, 0, 0, req.CallerID,
			)
		} else {
			submitID, err = h.submitChatTask([]schemas.StepInput{{
				ID:               "s1",
				Task:             req.Content,
				RoleID:           req.RoleID,
				SubmitterIsAdmin: chatIsAdmin,
			}}, sessionID)
		}
		if err != nil {
			session.AddEvent(chat.ChatEvent{Type: "tool_error", Data: err.Error(), Meta: map[string]any{"tool": "delegate_to_orchestrator"}})
			session.AddEvent(chat.ChatEvent{Type: "error", Data: err.Error()})
			return
		}
		result := fmt.Sprintf("Direct task submitted to orchestrator: task %s (role=%s)", submitID, req.RoleID)
		session.AddEvent(chat.ChatEvent{Type: "tool_result", Data: result, Meta: map[string]any{"tool": "delegate_to_orchestrator"}})
		session.AppendMessage(chat.ChatMessage{Role: "assistant", Content: result}, "")
		h.persistSession(session)
		session.AddEvent(chat.ChatEvent{Type: "done"})
	}()
	return ChatResponse{SessionID: sessionID, MessageID: req.MessageID, DirectTask: true}
}
