package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/core"
)

// noteTerminalTask reports a task state the model has just been shown, but only
// when it is terminal: a running task that the model polled once has not been
// "delivered", and suppressing the follow-up for it would re-introduce the
// silent loss this signal exists to prevent.
func (h *ChatHarness) noteTerminalTask(id string, st map[string]any) {
	if h == nil || h.OnTerminalTaskSeen == nil || st == nil {
		return
	}
	if s, _ := st["status"].(string); isTerminalChatStatus(s) {
		h.OnTerminalTaskSeen(id)
	}
}

// delegateReceipt is what the model sees right after delegating. It answers
// three questions: what was created, is it running, and what do I do next.
func (h *ChatHarness) delegateReceipt(id string) string {
	var b strings.Builder
	b.WriteString("delegated task_id=" + id + " status=accepted\n")
	b.WriteString("The orchestrator is running it in the background, as its own task with its own plan.\n")
	if h.DelegateStatus != nil {
		if st, ok := h.DelegateStatus(id, 0); ok && st != nil {
			if s, _ := st["status"].(string); s != "" {
				b.WriteString("current status: " + s + "\n")
			}
			h.noteTerminalTask(id, st)
		}
		b.WriteString("next: reply to the user now with the state you have — this session is resumed " +
			"automatically when the task finishes, and the result is delivered then. ")
		if h.pd.pollWakeMode() {
			// The wake arm locks every poll after the first, so the receipt must
			// not suggest one; suggesting a call that will be refused teaches the
			// model that the tool is broken.
			b.WriteString("Do not poll orchestrator_task_status: a first check returns immediately at most once, " +
				"and later ones are locked. Polling cannot make the task finish sooner.\n")
		} else {
			b.WriteString("If the answer is useless without the result, call orchestrator_task_status(task_id=\"" + id +
				"\", wait_seconds=20) instead; call it at most a couple of times and never loop on it.\n")
		}
	} else {
		b.WriteString("next: the result arrives as a task_callback event in this session.\n")
	}
	return b.String()
}

func (h *ChatHarness) execTool(ctx context.Context, name string, args map[string]any, taskID string) (string, error) {
	if name == "delegate_to_orchestrator" {
		if h.Delegate == nil {
			return "", fmt.Errorf("delegate is not available")
		}
		prompt, ok := args["prompt"].(string)
		if !ok || prompt == "" {
			return "", fmt.Errorf("delegate_to_orchestrator: missing or empty 'prompt' argument")
		}
		if len(h.declaredArtifacts) > 0 && h.SessionRoot != "" {
			if _, missing := core.WorkspaceArtifactsComplete(h.SessionRoot, h.declaredArtifacts); missing == 0 &&
				len(core.WorkspaceArtifactDefects(h.SessionRoot, h.declaredFields)) == 0 {
				return core.MarkerDelegationSkipped, nil
			}
		}
		prompt = h.delegatedPromptWithSpec(prompt)
		id, err := h.Delegate(prompt)
		if err != nil {
			return "", err
		}
		// Once a task is delegated, the rest of this run is about collecting
		// its result, not about doing the work here (§9.11.2). Recorded on
		// every arm, including off: the off arm is the baseline for
		// calls_after_deleg, and a baseline read off a different binary is not
		// a baseline (iron law 223). Recording a string cannot change
		// behaviour — blockedBy() and the hard-arm filter both return early
		// when the arm is off.
		//
		// One call, three updates: the session-wide id set the poll lever
		// matches (a turn can hand off twice), the last id the messages quote,
		// and the per-turn flag the lockout reads.
		h.pd.recordDelegation(id)
		return h.delegateReceipt(id), nil
	}
	if name == "orchestrator_task_status" {
		if h.DelegateStatus == nil {
			return "", fmt.Errorf("orchestrator_task_status: task status is not wired in this deployment")
		}
		id, _ := args["task_id"].(string)
		if strings.TrimSpace(id) == "" {
			return "", fmt.Errorf("orchestrator_task_status: 'task_id' is required")
		}
		wait := 15.0
		if w, ok := args["wait_seconds"].(float64); ok && w > 0 {
			wait = w
			if wait > 60 {
				wait = 60
			}
		}
		// Wake arm: the one allowed check answers immediately instead of
		// blocking for the caller's wait_seconds. A blocking first poll is the
		// foreground cost this lever exists to remove (§9.12.5).
		wake := h.pd.isWakePoll(id)
		if wake {
			wait = 0
		}
		if st, ok := h.DelegateStatus(id, time.Duration(wait*float64(time.Second))); ok && st != nil {
			h.noteTerminalTask(id, st)
			out := formatTaskStatus(id, st, h.declaredArtifacts)
			if s, _ := st["status"].(string); wake && !isTerminalChatStatus(s) {
				out += "\n" + postDelegWakeNote
			}
			h.pd.notePoll(id)
			return out, nil
		}
		return "", fmt.Errorf("orchestrator_task_status: task %s not found", id)
	}
	if name == "orchestrator_help" {
		if h.HelpProvider == nil {
			return "", fmt.Errorf("orchestrator_help: system manual is not wired in this deployment")
		}
		topic, _ := args["topic"].(string)
		return h.HelpProvider(topic), nil
	}
	if name == "search_archive" {
		query, _ := args["query"].(string)
		limit := 5
		if l, ok := args["limit"].(float64); ok && int(l) > 0 && int(l) <= 10 {
			limit = int(l)
		}
		if query == "" {
			return "", fmt.Errorf("search_archive: query is required")
		}
		if h.Archive == nil {
			return "", fmt.Errorf("search_archive: archive not available")
		}
		forest, err := h.Archive.SearchForest(query, h.Embed, limit, 0.9, taskID, h.ProjectOf(taskID), taskID)
		if err != nil {
			return "", fmt.Errorf("search_archive: %v", err)
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d matching items:\n\n", len(forest.Nodes)))
		for i, n := range forest.Nodes {
			sb.WriteString(fmt.Sprintf("## %d. [%s] %s\n", i+1, n.NodeID, n.Intent))
			if n.Summary != "" {
				sb.WriteString(n.Summary + "\n\n")
			}
			if len(n.SourceKeys) > 0 {
				sb.WriteString(fmt.Sprintf("File: %s (use read_file to access)\n\n", n.SourceKeys[0]))
			}
		}
		return sb.String(), nil
	}
	if h.SessionRoot != "" {
		taskDir := filepath.Join(h.OutputBase, taskID)
		validator := core.NewSafePathValidator(taskDir, h.SessionRoot, []string{h.SessionRoot})
		return core.HandleCoreTool(ctx, name, args, h.OutputBase, taskID, validator)
	}
	return core.HandleCoreTool(ctx, name, args, h.OutputBase, taskID)
}
