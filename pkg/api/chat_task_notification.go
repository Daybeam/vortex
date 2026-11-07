package api

import (
	"fmt"
	"strings"

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

	steps, _ := statusData["steps"].(map[string]any)
	for id, sv := range steps {
		sm, ok := sv.(map[string]any)
		if !ok {
			continue
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
	if len(o.Files) == 0 {
		if o.ResultRef != "" {
			return " No output files; last result reference: " + o.ResultRef + "."
		}
		return " No output files were produced."
	}

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
	if len(names) == 0 {
		return " No output files were produced."
	}
	return " Output files: " + strings.Join(names, ", ") + "."
}
