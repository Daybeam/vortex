package api

import (
	"fmt"
	"strings"

	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// taskOutcome is what a finished delegated task reports to the session that
// asked for it.
//
// The text carries the whole verdict because its three consumers are not
// equally informed: the SSE listener also reads ChatEvent.Meta, the next LLM
// turn reads only this string from the history tree, and Telegram forwards only
// this string (forwardEvents sends ev.Data and drops Meta). A binary
// "completed successfully"/"failed" verdict therefore reached the human reader
// as a lie whenever the graph terminated "completed" on top of a step that had
// reported itself partial -- measured on 2026-11-06 with task_75f448f3, which
// finished status=partial, confidence 0.30, no output files, and was announced
// as "completed successfully".
type taskOutcome struct {
	Desc      string
	ID        string
	Status    string
	EventType string

	ResultRef string
	LastError string

	// GapStep names the weakest step that ended "partial" (empty when none
	// did); GapConf is that step's self-reported confidence, GapNotes what it
	// said it had not verified.
	GapStep  string
	GapConf  *float64
	GapNotes string

	Files []map[string]any

	// WorkspaceRoot is where the step agents' core tools resolved relative
	// paths (ToStatusDictView "full"). Empty means the legacy per-task layout,
	// whose declared names cannot be resolved without guessing.
	WorkspaceRoot string
	// Declared is the completion contract parsed out of the steps' task text,
	// first-seen order. It exists because graph.OutputFiles is appended only
	// from a step's structured output_ref (scheduler_decision_success.go:126):
	// a delegated agent that writes its deliverable with file tools contributes
	// nothing there, so an empty Files list says nothing about what landed on
	// disk. formatTaskStatus has used the same contract since it was written.
	Declared []string
	// Contracts is the structured artifact contract list (schemas.ArtifactContract),
	// carrying field types and optionality alongside the Declared names.
	Contracts []schemas.ArtifactContract
}

// collectTaskOutcome reads what this notification needs out of the scheduler's
// "full" status view (ToStatusDictView, schemas/task.go).
func collectTaskOutcome(taskID, taskDesc, evType string, statusData map[string]any) taskOutcome {
	o := taskOutcome{
		Desc:      taskDesc,
		ID:        taskID,
		EventType: evType,
	}
	o.Status, _ = statusData["status"].(string)
	o.Files, _ = statusData["output_files"].([]map[string]any)
	o.WorkspaceRoot, _ = statusData["workspace_root"].(string)

	seenDeclared := make(map[string]struct{})
	steps, _ := statusData["steps"].(map[string]any)
	for id, sv := range steps {
		sm, ok := sv.(map[string]any)
		if !ok {
			continue
		}
		// A chat delegation is one step whose Task is the whole delegated
		// prompt; a multi-step task may carry the contract in any step. Only
		// the section is read, so unioning harmless steps adds nothing.
		if task, _ := sm["task"].(string); task != "" {
			for _, name := range core.ParseDeclaredContract(task).Create {
				if _, dup := seenDeclared[name]; dup {
					continue
				}
				seenDeclared[name] = struct{}{}
				o.Declared = append(o.Declared, name)
			}
		}
		if rr, _ := sm["result_ref"].(string); rr != "" {
			o.ResultRef = rr
		}
		if le, _ := sm["last_error"].(string); le != "" {
			o.LastError = le
		}
		// A step can end "partial" while the graph still reaches "completed";
		// that gap is the part the reader needs to know about.
		if st, _ := sm["status"].(string); st != string(schemas.StepPartial) {
			continue
		}
		conf, _ := sm["confidence"].(*float64)
		if o.selectsGap(conf) {
			o.GapStep, o.GapConf = id, conf
			if missing, _ := sm["missing_context"].([]string); len(missing) > 0 {
				o.GapNotes = strings.Join(missing, "; ")
			}
		}
	}
	return o
}

// selectsGap reports whether a partial step at confidence cf is the one to name.
// The lowest known confidence is the honest summary; steps are visited in map
// order, so a step that reported no confidence only holds the slot until one
// that did turns up.
func (o taskOutcome) selectsGap(cf *float64) bool {
	if o.GapStep == "" {
		return true
	}
	if cf == nil {
		return false
	}
	return o.GapConf == nil || *cf < *o.GapConf
}

// summary renders the notification line the user sees.
func (o taskOutcome) summary() string {
	prefix := fmt.Sprintf("[Task Notification] Delegated orchestrator task '%s' (%s)", o.Desc, o.ID)

	if !o.reachedCompletion() {
		msg := prefix + " failed during execution."
		if o.LastError != "" {
			msg += " Error: " + o.LastError
		}
		return msg
	}

	if o.GapStep != "" {
		conf := "confidence unreported"
		if o.GapConf != nil {
			conf = fmt.Sprintf("confidence %.2f", *o.GapConf)
		}
		msg := fmt.Sprintf("%s finished with gaps: step %s reported status=partial, %s.",
			prefix, o.GapStep, conf)
		if o.GapNotes != "" {
			msg += " Unverified: " + truncateRunes(o.GapNotes, 300)
		}
		return msg + o.filesClause()
	}

	return prefix + " completed successfully." + o.filesClause()
}

// reachedCompletion mirrors the event the callback was fired for: a graph that
// terminated "completed" (with or without skipped steps), or an explicit
// EventTaskCompleted. Everything else -- failed, cancelled, orphaned -- is not
// a success and must not be announced as one.
func (o taskOutcome) reachedCompletion() bool {
	switch schemas.GraphStatus(o.Status) {
	case schemas.GraphCompleted, schemas.GraphCompletedWithSkips:
		return true
	}
	return o.EventType == string(obs.EventTaskCompleted)
}

// filesClause names the deliverable, because "completed" without a path is not
// something the reader can check.
func (o taskOutcome) filesClause() string {
	if len(o.Files) > 0 {
		var names []string
		for _, f := range o.Files {
			path, _ := f["path"].(string)
			if path == "" {
				continue
			}
			primary, _ := f["is_primary"].(bool)
			if primary {
				path += " (primary)"
			}
			names = append(names, path)
		}
		if len(names) > 0 {
			return " Output files: " + strings.Join(names, ", ") + "."
		}
	}

	// A delegated agent's file-tool writes never reach graph.OutputFiles (that
	// list is appended only from a step's structured output_ref), so an empty
	// Files list is not evidence that nothing was produced. When the prompt
	// carried a completion contract, check the declared names under the
	// workspace root -- the same check formatTaskStatus already makes.
	if len(o.Declared) > 0 && o.WorkspaceRoot != "" {
		missing := core.MissingArtifacts(o.WorkspaceRoot, o.Declared)
		absent := make(map[string]bool, len(missing))
		for _, name := range missing {
			absent[name] = true
		}
		var present []string
		for _, name := range o.Declared {
			if !absent[name] {
				present = append(present, name)
			}
		}
		switch {
		case len(present) > 0:
			clause := " Artifacts present in the workspace: " + strings.Join(present, ", ") + "."
			if len(missing) > 0 {
				clause += " Still missing: " + strings.Join(missing, ", ") + "."
			}
			return clause
		default:
			return fmt.Sprintf(" No declared artifacts were produced (%d declared: %s).",
				len(o.Declared), strings.Join(o.Declared, ", "))
		}
	}

	if o.ResultRef != "" {
		return " No output files; last result reference: " + o.ResultRef + "."
	}
	if o.WorkspaceRoot != "" {
		// Without a contract there is no name to check, and claiming "none"
		// would repeat the lie this clause exists to stop: the agent may have
		// written files with tools the graph never sees.
		return " No output files were reported; the delegated agent's workspace is " + o.WorkspaceRoot + "."
	}
	return " No output files were produced."
}
