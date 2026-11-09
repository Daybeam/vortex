package replay

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// segmentGap splits one task id into separate attempts. Long-lived repair
// sessions and ad-hoc admin tool calls on an old task id are not part of the
// original run.
const segmentGap = 30 * time.Minute

// liveWindow is how long a run may go without writing a new event before the
// loader stops calling it in flight. A killed process and a healthy one leave
// the same log tail, so recency is the only signal that separates "still
// running" from "interrupted".
const liveWindow = 3 * time.Minute

// dispatchMergeWindow collapses the two telemetry paths that describe a single
// re-dispatch: the scheduler logs step_retrying and then, a moment later, a
// fresh audit_entry with a new span id for the same step. Counting both would
// report the same retry twice. Telemetry timestamps are seconds.
const dispatchMergeWindow = 5.0

func buildRun(taskID string, files []logFile, now time.Time) (*RunDetail, error) {
	var raw []rawEvent
	for _, file := range files {
		raw = append(raw, parseFile(file)...)
	}
	if len(raw) == 0 {
		return nil, ErrNotFound
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].TS < raw[j].TS })
	segment, segmentCount, dates := pickSegment(raw, segmentGap)
	events := dedupe(segment)
	start := events[0].TS
	end := events[len(events)-1].TS
	index := aggregateSteps(events, start)
	completed, failed, retries := stepTotals(index.list)
	settled := hasEvent(events, "task_completed")
	failedTerminal := hasEvent(events, "task_failed")
	running := inFlight(events, index.list, settled, failedTerminal, now)
	prompt := runPrompt(events)
	provider, model := modelInfo(events)

	summary := RunSummary{
		TaskID:     taskID,
		Title:      runTitle(prompt, index.list, taskID),
		Task:       prompt,
		Status:     deriveStatus(index.list, settled, failedTerminal, running),
		Replayable: true,
		Settled:    settled || failedTerminal,
		Dates:      dates,
		Segments:   segmentCount,
		StartedAt:  rfc3339(start),
		EndedAt:    rfc3339(end),
		DurationMs: millisBetween(start, end),
		Steps:      len(index.list),
		Completed:  completed,
		Failed:     failed,
		Tools:      countTools(events),
		Retries:    retries,
		Events:     len(events),
		Provider:   provider,
		Model:      model,
		Error:      lastError(events, index.list),
	}
	detail := &RunDetail{
		RunSummary: summary,
		Steps:      index.list,
		Events:     buildTimeline(events, start, end, prompt, index),
		LogFiles:   logPaths(files),
	}
	applySubmittedDeps(detail.Steps, events)
	// Recover the explicit graph from the compact task_submitted snapshot.
	// This is a read-side change only: no new on-disk format, and historical
	// runs without a snapshot simply keep a nil Graph.
	detail.Graph = buildSubmittedGraph(detail.Steps, events)
	return detail, nil
}

// applySubmittedDeps recovers the explicit graph dependencies logged with the
// task_submitted event. Runs whose submission predates that snapshot keep an
// empty DependsOn list and render as a hub-only star — no edges are inferred.
func applySubmittedDeps(steps []Step, events []rawEvent) {
	if len(steps) == 0 {
		return
	}
	known := make(map[string]bool, len(steps))
	byID := make(map[string]*Step, len(steps))
	for i := range steps {
		known[steps[i].ID] = true
		byID[steps[i].ID] = &steps[i]
	}
	for _, event := range events {
		if event.Event != "task_submitted" {
			continue
		}
		raw, ok := event.Detail["deps"].(map[string]any)
		if !ok {
			continue
		}
		for id, value := range raw {
			target, ok := byID[id]
			if !ok {
				continue
			}
			items, ok := value.([]any)
			if !ok {
				continue
			}
			seen := make(map[string]bool, len(items))
			for _, item := range items {
				dep, ok := item.(string)
				if !ok || dep == id || !known[dep] || seen[dep] {
					continue
				}
				seen[dep] = true
				target.DependsOn = append(target.DependsOn, dep)
			}
			sort.Strings(target.DependsOn)
		}
	}
}

// buildSubmittedGraph rebuilds the explicit graph from the compact
// task_submitted payload. It returns nil when the submission carried no graph
// information at all, so viewers can tell "a known plan with no edges" apart
// from "no plan was ever recorded".
//
// Two snapshot keys exist:
//   - step_ids: every planned node, including dependency-free parallel roots.
//     New submissions always write it; older logs do not have it.
//   - deps: inbound edges, written only for nodes that declare them.
//
// When step_ids is absent, the executed step IDs become the node set so
// historical runs with deps still get a usable graph. With neither key we
// return nil instead of inventing nodes: such a run must stay on the
// time-overlap inference path.
func buildSubmittedGraph(steps []Step, events []rawEvent) *Graph {
	if len(steps) == 0 {
		return nil
	}
	var ids []string
	deps := make(map[string][]string)
	found := false
	for _, event := range events {
		if event.Event != "task_submitted" {
			continue
		}
		if rawIDs, ok := event.Detail["step_ids"].([]any); ok {
			list := make([]string, 0, len(rawIDs))
			seen := make(map[string]bool, len(rawIDs))
			for _, item := range rawIDs {
				id, ok := item.(string)
				if !ok || id == "" || seen[id] {
					continue
				}
				seen[id] = true
				list = append(list, id)
			}
			if len(list) > 0 {
				sort.Strings(list)
				ids = list
				found = true
			}
		}
		if rawDeps, ok := event.Detail["deps"].(map[string]any); ok {
			for id, value := range rawDeps {
				items, ok := value.([]any)
				if !ok {
					continue
				}
				seen := make(map[string]bool, len(items))
				list := make([]string, 0, len(items))
				for _, item := range items {
					dep, ok := item.(string)
					if !ok || dep == "" || dep == id || seen[dep] {
						continue
					}
					seen[dep] = true
					list = append(list, dep)
				}
				if len(list) > 0 {
					sort.Strings(list)
					deps[id] = list
					found = true
				}
			}
		}
	}
	if !found {
		return nil
	}
	// Planned nodes can include steps that never produced telemetry. A join
	// node cancelled before dispatch is the common case: run 390fbe51 logs
	// {"s6": ["s1".."s5"]} but only s1..s5 appear in the executed steps.
	// Dropping s6 would silently turn a five-way join into five unrelated
	// parallel roots, so every declared deps key belongs in the node set.
	nodeSet := make(map[string]bool, len(ids)+len(deps)+len(steps))
	for _, id := range ids {
		nodeSet[id] = true
	}
	for id := range deps {
		nodeSet[id] = true
	}
	if len(ids) == 0 {
		// Older logs have no step_ids: executed steps plus declared nodes are
		// the most honest reconstruction available. When step_ids IS present,
		// telemetry-only nodes (e.g. retries) stay out of the planned graph.
		for i := range steps {
			nodeSet[steps[i].ID] = true
		}
	}
	ids = make([]string, 0, len(nodeSet))
	for id := range nodeSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	known := nodeSet
	graph := &Graph{Steps: make([]GraphStep, 0, len(ids))}
	for _, id := range ids {
		node := GraphStep{ID: id}
		for _, dep := range deps[id] {
			if known[dep] {
				node.DependsOn = append(node.DependsOn, dep)
			}
		}
		graph.Steps = append(graph.Steps, node)
	}
	return graph
}

func logPaths(files []logFile) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.date+"/"+fileName(file.path))
	}
	return out
}

func fileName(path string) string {
	if idx := strings.LastIndexAny(path, `/\`); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

// pickSegment returns the attempt with the most step activity. Ties prefer the
// later attempt, which is usually the one a user remembers.
func pickSegment(events []rawEvent, gap time.Duration) ([]rawEvent, int, []string) {
	bounds := make([][2]int, 0, 4)
	begin := 0
	for i := 1; i < len(events); i++ {
		idle := time.Duration((events[i].TS - events[i-1].TS) * float64(time.Second))
		if idle > gap || events[i].Event == "task_submitted" {
			bounds = append(bounds, [2]int{begin, i})
			begin = i
		}
	}
	bounds = append(bounds, [2]int{begin, len(events)})

	best, bestScore := 0, -1
	for i, bound := range bounds {
		score := 0
		for _, event := range events[bound[0]:bound[1]] {
			if isStepEvent(event.Event) {
				score++
			}
		}
		if score >= bestScore {
			best, bestScore = i, score
		}
	}
	chosen := events[bounds[best][0]:bounds[best][1]]
	dateSet := map[string]bool{}
	for _, event := range chosen {
		if event.date != "" {
			dateSet[event.date] = true
		}
	}
	dates := make([]string, 0, len(dateSet))
	for date := range dateSet {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	return chosen, len(bounds), dates
}

func isStepEvent(name string) bool {
	return strings.HasPrefix(name, "step_") ||
		name == "task_completed" ||
		name == "task_failed" ||
		strings.HasPrefix(name, "EventRoleGeneration")
}

// dedupe merges the double-logged terminal events produced by the interceptor
// and scheduler writing the same step transition. The richer copy of each
// field is kept so status, error text and confidence all survive.
func dedupe(events []rawEvent) []rawEvent {
	seen := make(map[string]int, len(events))
	out := make([]rawEvent, 0, len(events))
	for _, event := range events {
		key := event.Event + "|" + event.StepID
		switch event.Event {
		case "step_completed", "step_failed":
			key += "|" + strconv.FormatInt(int64(math.Floor(event.TS)), 10)
		case "tool_call":
			// Admin tool calls log started/finished with the same timestamp;
			// they must not collapse into one record.
			key += "|" + detailString(event.Detail, "phase") + "|" + strconv.FormatFloat(event.TS, 'f', 6, 64)
		default:
			key += "|" + strconv.FormatFloat(event.TS, 'f', 6, 64)
		}
		if position, ok := seen[key]; ok {
			mergeEvent(&out[position], event)
			continue
		}
		seen[key] = len(out)
		out = append(out, event)
	}
	return out
}

func mergeEvent(target *rawEvent, extra rawEvent) {
	if target.Confidence == nil {
		target.Confidence = extra.Confidence
	}
	if target.StepID == "" {
		target.StepID = extra.StepID
	}
	if target.Detail == nil {
		target.Detail = extra.Detail
		return
	}
	for key, value := range extra.Detail {
		if _, ok := target.Detail[key]; !ok {
			target.Detail[key] = value
		}
	}
}

type stepAgg struct {
	starts      []float64
	dispatchAts []stepDispatch
	doneAt      float64
	failAt      float64
	lastEvent   float64
	role        string
	thought     string
	errText     string
	confidence  *float64
	tools       int
}

// stepDispatch is one re-dispatch of a step, with the error that caused it when
// telemetry recorded one.
type stepDispatch struct {
	at  float64
	err string
}

// appendDispatch records the instant a step was dispatched again, keeping one
// entry per re-dispatch even when both telemetry paths report it.
func (a *stepAgg) appendDispatch(at float64, err string) {
	if at <= 0 {
		return
	}
	for index, existing := range a.dispatchAts {
		if math.Abs(existing.at-at) < dispatchMergeWindow {
			if existing.err == "" {
				a.dispatchAts[index].err = err
			}
			return
		}
	}
	a.dispatchAts = append(a.dispatchAts, stepDispatch{at: at, err: err})
	sort.Slice(a.dispatchAts, func(i, j int) bool { return a.dispatchAts[i].at < a.dispatchAts[j].at })
}

func (a *stepAgg) retries() int {
	return len(a.dispatchAts)
}

func (a *stepAgg) execStart() float64 {
	if len(a.starts) > 1 {
		return a.starts[1]
	}
	if len(a.starts) == 1 {
		return a.starts[0]
	}
	return 0
}

func (a *stepAgg) firstStart() float64 {
	if len(a.starts) > 0 {
		return a.starts[0]
	}
	return 0
}

type stepIndex struct {
	list        []Step
	byID        map[string]*Step
	execStart   map[string]float64
	dispatchAts map[string][]stepDispatch
}

func aggregateSteps(events []rawEvent, runStart float64) stepIndex {
	order := make([]string, 0, 8)
	aggs := make(map[string]*stepAgg, 8)
	subtasks := make(map[string]string, 8)
	// One span id per dispatch, so a step that was sent out again shows up as a
	// second entry instead of silently extending the first attempt.
	spans := make(map[string]map[string]float64, 8)

	for _, event := range events {
		if event.StepID == "" {
			continue
		}
		if event.Event == "audit_entry" || event.Event == "EventRoleGenerationStarted" {
			if _, ok := subtasks[event.StepID]; !ok {
				task := detailString(event.Detail, "task")
				if task == "" {
					task = detailString(event.Detail, "task_description")
				}
				if task != "" {
					subtasks[event.StepID] = oneLine(stripReference(task), 400)
				}
			}
			if event.Event == "audit_entry" {
				span := detailString(event.Detail, "span_id")
				if span != "" {
					if spans[event.StepID] == nil {
						spans[event.StepID] = make(map[string]float64, 2)
					}
					if at, seen := spans[event.StepID][span]; !seen || event.TS < at {
						spans[event.StepID][span] = event.TS
					}
				}
			}
			continue
		}
		agg, ok := aggs[event.StepID]
		if !ok {
			agg = &stepAgg{}
			aggs[event.StepID] = agg
			order = append(order, event.StepID)
		}
		if event.TS > agg.lastEvent {
			agg.lastEvent = event.TS
		}
		applyStepEvent(agg, event)
	}

	sort.SliceStable(order, func(i, j int) bool {
		left := startOf(aggs[order[i]])
		right := startOf(aggs[order[j]])
		if left != right {
			return left < right
		}
		return order[i] < order[j]
	})

	for _, id := range order {
		heads := sortedSpanTimes(spans[id])
		// The first span is the original dispatch; everything after it is a
		// re-run of the same step.
		for _, at := range heads[min(1, len(heads)):] {
			aggs[id].appendDispatch(at, "")
		}
	}

	index := stepIndex{
		list:        make([]Step, 0, len(order)),
		byID:        make(map[string]*Step, len(order)),
		execStart:   make(map[string]float64, len(order)),
		dispatchAts: make(map[string][]stepDispatch, len(order)),
	}
	for position, id := range order {
		agg := aggs[id]
		step := buildStep(id, position+1, agg, subtasks[id], runStart)
		index.list = append(index.list, step)
		index.byID[id] = &index.list[len(index.list)-1]
		index.execStart[id] = stepTime(agg.execStart(), agg.firstStart(), agg.lastEvent)
		index.dispatchAts[id] = append([]stepDispatch(nil), agg.dispatchAts...)
	}
	return index
}

func sortedSpanTimes(heads map[string]float64) []float64 {
	if len(heads) == 0 {
		return nil
	}
	times := make([]float64, 0, len(heads))
	for _, at := range heads {
		times = append(times, at)
	}
	sort.Float64s(times)
	return times
}

func applyStepEvent(agg *stepAgg, event rawEvent) {
	switch event.Event {
	case "step_started":
		agg.starts = append(agg.starts, event.TS)
		if agg.role == "" {
			agg.role = detailString(event.Detail, "role")
		}
	case "EventEphemeralRoleGenerated":
		if agg.role == "" {
			agg.role = detailString(event.Detail, "role_id")
		}
	case "EventCoreToolCall", "EventMCPToolCall":
		agg.tools++
	case "tool_call":
		if detailString(event.Detail, "phase") == "finished" {
			agg.tools++
		}
	case "step_retrying":
		errText := oneLine(detailString(event.Detail, "error"), 200)
		agg.appendDispatch(event.TS, errText)
		if agg.errText == "" {
			agg.errText = errText
		}
	case "EventA3Compensation":
		agg.appendDispatch(event.TS, "compensation")
	case "step_completed":
		agg.doneAt = event.TS
		if event.Confidence != nil {
			agg.confidence = event.Confidence
		} else if value, ok := event.Detail["confidence"].(float64); ok {
			agg.confidence = &value
		}
	case "step_failed":
		agg.failAt = event.TS
		agg.errText = firstNonEmpty(
			oneLine(detailString(event.Detail, "error"), 200),
			oneLine(detailString(event.Detail, "details"), 200),
			agg.errText,
		)
	case "EventStepThought":
		if thought := detailString(event.Detail, "thought"); thought != "" {
			agg.thought = oneLine(thought, 240)
		}
	}
}

func buildStep(id string, position int, agg *stepAgg, task string, runStart float64) Step {
	startAt := stepTime(agg.execStart(), agg.firstStart(), agg.lastEvent)
	endAt := agg.lastEvent
	if agg.doneAt > 0 {
		endAt = agg.doneAt
	}
	if agg.failAt > 0 {
		endAt = agg.failAt
	}
	status := StepRunning
	if agg.doneAt > 0 && (agg.failAt == 0 || agg.doneAt >= agg.failAt) {
		status = StepCompleted
	} else if agg.failAt > 0 {
		status = StepFailed
	}
	return Step{
		ID:         id,
		Index:      position,
		Role:       agg.role,
		Task:       task,
		Status:     status,
		StartMs:    millisBetween(runStart, startAt),
		EndMs:      millisBetween(runStart, math.Max(startAt, endAt)),
		DurationMs: millisBetween(startAt, math.Max(startAt, endAt)),
		Tools:      agg.tools,
		Retries:    agg.retries(),
		Thought:    agg.thought,
		Error:      agg.errText,
		Confidence: agg.confidence,
	}
}

func startOf(agg *stepAgg) float64 {
	return stepTime(agg.execStart(), agg.firstStart(), agg.lastEvent)
}

func stepTime(candidates ...float64) float64 {
	for _, value := range candidates {
		if value > 0 {
			return value
		}
	}
	return 0
}

func stepTotals(steps []Step) (completed, failed, retries int) {
	for _, step := range steps {
		switch step.Status {
		case StepCompleted:
			completed++
		case StepFailed:
			failed++
		}
		retries += step.Retries
	}
	return completed, failed, retries
}

// countTools covers both step-scoped core tool calls and run-level admin tool
// calls, which carry no step id.
//
// EventMCPToolCall counts too. A sub-agent calling an MCP server is a tool call
// in every sense the reader cares about, and until that event existed this
// counter silently reported only the in-process half. Records written before
// the event existed are unaffected (they simply never contain it), so a replay
// of old jsonl yields the same number as before — but comparisons of this
// field across the change are no longer apples to apples, and should be
// grouped by whether EventMCPToolCall is present.
func countTools(events []rawEvent) int {
	tools := 0
	for _, event := range events {
		if event.Event == "EventCoreToolCall" || event.Event == "EventMCPToolCall" {
			tools++
			continue
		}
		if event.Event == "tool_call" && detailString(event.Detail, "phase") == "finished" {
			tools++
		}
	}
	return tools
}

func deriveStatus(steps []Step, settled, failedTerminal, running bool) string {
	completed, failed, _ := stepTotals(steps)
	switch {
	case settled:
		return StatusCompleted
	case failedTerminal:
		return StatusFailed
	case running:
		return StatusRunning
	case completed > 0 && failed > 0:
		return StatusPartial
	case completed > 0:
		return StatusCompleted
	case failed > 0:
		return StatusFailed
	default:
		return StatusInterrupted
	}
}

// inFlight reports whether the run is still being written. A run that logged
// its own terminal event is settled; a run whose every step already reached a
// terminal state is finished even if the task never logged one (the process
// died in the gap). Only a run with an open step and a fresh tail is live.
func inFlight(events []rawEvent, steps []Step, settled, failedTerminal bool, now time.Time) bool {
	if settled || failedTerminal || len(events) == 0 || !hasOpenStep(steps) {
		return false
	}
	return now.Sub(eventTime(events[len(events)-1].TS)) < liveWindow
}

// hasOpenStep is true when a step started but never reached a terminal state,
// or when the run has no step yet and is still being decomposed.
func hasOpenStep(steps []Step) bool {
	if len(steps) == 0 {
		return true
	}
	for _, step := range steps {
		if step.Status == StepRunning {
			return true
		}
	}
	return false
}

func runTitle(prompt string, steps []Step, taskID string) string {
	if prompt != "" {
		return oneLine(prompt, 90)
	}
	for _, step := range steps {
		if step.Task != "" {
			return oneLine(step.Task, 90)
		}
		if step.Role != "" {
			return oneLine(step.Role, 90)
		}
	}
	return taskID
}

func runPrompt(events []rawEvent) string {
	for _, event := range events {
		if event.Event == "smart_route" {
			if task := detailString(event.Detail, "task"); task != "" {
				return oneLine(stripReference(task), 400)
			}
		}
	}
	return ""
}

// stripReference cuts the semantic-precedent block the harness appends to
// prompts. It is retrieved evidence for the model, not part of the user's
// request, and it dominates every title that includes it.
func stripReference(text string) string {
	if idx := strings.Index(text, "[Reference:"); idx >= 0 {
		text = text[:idx]
	}
	return strings.TrimSpace(text)
}

func modelInfo(events []rawEvent) (provider, model string) {
	for _, event := range events {
		if event.Event != "provider_call_started" {
			continue
		}
		if value := detailString(event.Detail, "provider"); value != "" {
			provider = value
		}
		if value := detailString(event.Detail, "model"); value != "" {
			model = value
		}
	}
	return provider, model
}

func lastError(events []rawEvent, steps []Step) string {
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].Error != "" {
			return steps[i].Error
		}
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Event == "audit_exit_error" {
			return oneLine(detailString(events[i].Detail, "error"), 200)
		}
	}
	return ""
}

func hasEvent(events []rawEvent, name string) bool {
	for _, event := range events {
		if event.Event == name {
			return true
		}
	}
	return false
}

// buildTimeline maps raw telemetry onto the animation vocabulary. Terminal
// step events are emitted only for the step's final state so the replay never
// shows a node completing and then silently reverting.
func buildTimeline(events []rawEvent, start, end float64, prompt string, index stepIndex) []Event {
	out := make([]Event, 0, 96)
	seenRole := make(map[string]bool, len(index.list))
	seenStart := make(map[string]bool, len(index.list))
	finalAt := -1

	for _, event := range events {
		at := millisBetween(start, event.TS)
		step := event.StepID
		var stepStatus string
		if step != "" {
			if entry, ok := index.byID[step]; ok {
				stepStatus = entry.Status
			}
		}
		switch event.Event {
		case "task_submitted":
			out = append(out, Event{AtMs: at, Kind: KindSubmit, Text: prompt})
		case "EventEphemeralRoleGenerated":
			if step == "" || seenRole[step] {
				continue
			}
			seenRole[step] = true
			out = append(out, Event{AtMs: at, Kind: KindRole, StepID: step, Text: detailString(event.Detail, "role_id")})
		case "step_started":
			if step == "" || seenStart[step] {
				continue
			}
			if !sameInstant(event.TS, index.execStart[step]) {
				continue
			}
			seenStart[step] = true
			role := detailString(event.Detail, "role")
			if role == "" && step != "" {
				if entry, ok := index.byID[step]; ok {
					role = entry.Role
				}
			}
			out = append(out, Event{AtMs: at, Kind: KindStart, StepID: step, Text: role})
		case "EventCoreToolCall", "EventMCPToolCall":
			// Both are tool calls and both carry a "tool" field; the transport
			// difference (in-process core tool vs MCP server) matters for
			// attribution but not for the timeline, so they share one shape.
			out = append(out, Event{
				AtMs:   at,
				Kind:   KindTool,
				StepID: step,
				Tool:   detailString(event.Detail, "tool"),
			})
		case "tool_call":
			if detailString(event.Detail, "phase") != "finished" {
				continue
			}
			tool := detailString(event.Detail, "tool")
			out = append(out, Event{AtMs: at, Kind: KindTool, StepID: step, Tool: tool})
		case "decision_required":
			out = append(out, Event{AtMs: at, Kind: KindGate, StepID: step, Text: firstNonEmpty(detailString(event.Detail, "kind"), detailString(event.Detail, "question"), "decision required")})
		case "mount_policy_rejected":
			out = append(out, Event{AtMs: at, Kind: KindGate, StepID: step, Text: oneLine(detailString(event.Detail, "error"), 140)})
		case "step_completed":
			if stepStatus != StepCompleted {
				continue
			}
			out = append(out, Event{AtMs: at, Kind: KindDone, StepID: step, Text: detailString(event.Detail, "status")})
		case "step_failed":
			if stepStatus != StepFailed {
				continue
			}
			out = append(out, Event{AtMs: at, Kind: KindFail, StepID: step, Text: oneLine(detailString(event.Detail, "error"), 140)})
		case "task_completed":
			finalAt = len(out)
			out = append(out, Event{AtMs: at, Kind: KindFinal})
		case "task_failed":
			finalAt = len(out)
			out = append(out, Event{AtMs: at, Kind: KindFinal, Text: oneLine(detailString(event.Detail, "error"), 140)})
		}
	}
	if finalAt < 0 {
		out = append(out, Event{AtMs: millisBetween(start, end), Kind: KindFinal})
	}
	// Retry beats come from the dispatch tally rather than the event stream: a
	// re-dispatch can be logged as an audit span, a step_retrying, or a
	// compensation, and the tally already collapsed those into one entry each.
	for _, step := range index.list {
		for position, dispatch := range index.dispatchAts[step.ID] {
			text := dispatch.err
			if text == "" {
				text = "attempt " + strconv.Itoa(position+2)
			}
			out = append(out, Event{AtMs: millisBetween(start, dispatch.at), Kind: KindRetry, StepID: step.ID, Text: text})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AtMs < out[j].AtMs })
	return out
}

func sameInstant(left, right float64) bool {
	return math.Abs(left-right) < 1e-6
}

func detailString(detail map[string]any, key string) string {
	if detail == nil {
		return ""
	}
	switch value := detail[key].(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// eventTime converts a telemetry unix timestamp to a time.Time.
func eventTime(ts float64) time.Time {
	seconds := int64(ts)
	nanos := int64(math.Round((ts - float64(seconds)) * 1e9))
	return time.Unix(seconds, nanos)
}

func rfc3339(ts float64) string {
	return eventTime(ts).UTC().Format(time.RFC3339)
}

func millisBetween(from, to float64) int64 {
	value := int64(math.Round((to - from) * 1000))
	if value < 0 {
		return 0
	}
	return value
}
