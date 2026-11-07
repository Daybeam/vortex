package api

// webhook_notifier.go
//
// Pushes orchestrator task lifecycle events to an external webhook endpoint
// (e.g. the Hermes webhook platform) with GitHub-compatible HMAC signing.
//
// Design notes:
//   - Fire-and-forget by contract: Send/SendAgent never propagate an error to
//     the caller, so a callback endpoint being down can never block or fail a
//     task. Failures are logged only.
//   - Each send mints a fresh X-GitHub-Delivery id. The receiver treats that
//     header as an idempotency key with a 1h TTL, so reusing one would make it
//     return {"status":"duplicate"} and silently drop the later event.
//   - Orchestrator's own task_completed payload is just {"confidence": x}, so
//     status/summary are synthesised here; otherwise the webhook prompt
//     template would leave {summary} unresolved.
//   - No third-party dependency added: net/http only, local URL, disabled
//     unless WEBHOOK_CALLBACK_URL is set.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/obs"
)

const (
	webhookDefaultSecret   = "«REDACTED»"
	webhookDefaultTimeout  = 10 * time.Second
	webhookMaxPayloadBytes = 64 * 1024
	webhookNotifyRoute     = "callback-notify"
	webhookAgentRoute      = "callback-agent"
	webhookMaxSummaryRunes = 300
)

// webhookNotifier delivers obs.AgentEvent lifecycle events to a webhook endpoint.
type webhookNotifier struct {
	url        string // base URL, no /webhooks/<route> suffix
	secret     string
	route      string
	agentRoute string
	timeout    time.Duration
	client     *http.Client

	// useAgent routes task events to the agent route (which triggers an LLM
	// run and replies) instead of the notify route (deliver_only, zero LLM).
	// See the doc comment on SubscribeTaskEvents for the timeout implication.
	useAgent bool

	idMu sync.Mutex
	seq  int64

	// Unsubscribe funcs from SubscribeTaskEvents. Nil when unwired.
	unsubscribers []func()

	// routes from config.NotificationConfig.Routes. When non-empty,
	// SubscribeTaskEvents uses config-driven 3-layer filtering instead of
	// the hardcoded 8-event subscription. Each route is sent to
	// independently (see matchRoute).
	routes []config.NotificationRoute
}

// NewWebhookNotifier builds a notifier from a base URL (e.g.
// "http://127.0.0.1:8644"). An empty base disables the notifier: every Send
// becomes a no-op, which is the safe default when the endpoint is unset.
func NewWebhookNotifier(base string) *webhookNotifier {
	n := &webhookNotifier{
		url:        strings.TrimRight(base, "/"),
		secret:     webhookDefaultSecret,
		route:      webhookNotifyRoute,
		agentRoute: webhookAgentRoute,
		timeout:    webhookDefaultTimeout,
	}
	n.applyEnv()
	n.client = &http.Client{Timeout: n.timeout}
	return n
}

// applyEnv lets deployment override the endpoint without a code change.
func (n *webhookNotifier) applyEnv() {
	if base := getEnv("WEBHOOK_CALLBACK_URL"); base != "" {
		n.url = strings.TrimRight(base, "/")
	}
	if s := getEnv("WEBHOOK_SECRET"); s != "" {
		n.secret = s
	}
	if r := getEnv("WEBHOOK_ROUTE"); r != "" {
		n.route = r
	}
	if a := getEnv("WEBHOOK_AGENT_ROUTE"); a != "" {
		n.agentRoute = a
	}
	if d := getEnv("WEBHOOK_TIMEOUT"); d != "" {
		if ms, err := strconv.Atoi(d); err == nil && ms > 0 {
			n.timeout = time.Duration(ms) * time.Millisecond
			n.client = &http.Client{Timeout: n.timeout}
		}
	}
	// WEBHOOK_USE_AGENT=true routes task events to the agent route so the
	// receiver runs an LLM turn instead of pushing a bare notification. The
	// agent route answers 202 immediately (the run is queued server-side), so
	// this does not add to the client's blocking time on the synchronous
	// Publish path.
	n.useAgent = strings.EqualFold(getEnv("WEBHOOK_USE_AGENT"), "true") ||
		strings.EqualFold(getEnv("WEBHOOK_USE_AGENT"), "1")
}

func (n *webhookNotifier) enabled() bool { return n.url != "" }

// SetRoutes configures config-driven 3-layer filtering. When routes are
// non-empty, SubscribeTaskEvents subscribes to the union of all route
// TriggerOn events and dispatches each event only to matching routes.
// Each route is sent to independently (no merging).
func (n *webhookNotifier) SetRoutes(routes []config.NotificationRoute) {
	n.routes = routes
}

// matchRoute checks all 3 filter layers for a single route.
// Returns true when the event should be delivered to this route.
func matchRoute(ev obs.AgentEvent, route config.NotificationRoute, scheduler *core.DirectedEngine) bool {
	if !route.Enabled {
		return false
	}
	if !matchTriggerOn(ev.EventType, route.TriggerOn) {
		return false
	}
	if !matchGlob(ev.TaskID, route.TaskIDGlob) {
		return false
	}
	if !matchGlob(ev.StepID, route.StepIDGlob) {
		return false
	}
	if !matchTaskType(ev, route.TaskType, scheduler) {
		return false
	}
	return true
}

// matchTriggerOn checks the event-type layer. Empty TriggerOn means Tier 0
// default (task_completed + task_failed).
func matchTriggerOn(eventType string, triggerOn []string) bool {
	if len(triggerOn) == 0 {
		return eventType == string(obs.EventTaskCompleted) ||
			eventType == string(obs.EventTaskFailed)
	}
	for _, t := range triggerOn {
		if t == eventType {
			return true
		}
	}
	return false
}

// matchGlob checks a value against a glob pattern. Empty or "*" pattern
// matches everything.
func matchGlob(value, pattern string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	matched, _ := filepath.Match(pattern, value)
	return matched
}

// matchTaskType checks the task-type layer. Empty or "*" matches all.
// On lookup failure (task not found in scheduler), defaults to allow
// (don't block on missing data).
func matchTaskType(ev obs.AgentEvent, taskType string, scheduler *core.DirectedEngine) bool {
	if taskType == "" || taskType == "*" {
		return true
	}
	// Fast path: check payload for role_id.
	if roleID, ok := ev.Payload["role_id"].(string); ok {
		return roleID == taskType
	}
	// Slow path: reverse-lookup scheduler.
	if scheduler != nil {
		if data, ok := scheduler.GetStatus(ev.TaskID, "full"); ok && data != nil {
			if roleID, ok := data["role_id"].(string); ok {
				return roleID == taskType
			}
		}
	}
	// Conservative: allow on lookup failure (don't silently drop events).
	return true
}

// nextDeliveryID returns a unique monotonic id for one delivery.
func (n *webhookNotifier) nextDeliveryID() string {
	n.idMu.Lock()
	defer n.idMu.Unlock()
	n.seq++
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatInt(n.seq, 10)
}

// webhooksStatusForEvent maps an orchestrator EventType to a compact status
// string suitable for the webhook payload. Non-terminal events (step in
// flight, decision waiting) return "" so the receiver treats the message
// as informational rather than final.
func webhooksStatusForEvent(ev obs.AgentEvent) string {
	switch ev.EventType {
	case string(obs.EventTaskFailed), string(obs.EventStepFailed):
		return "failed"
	case string(obs.EventTaskCompleted), string(obs.EventStepCompleted):
		return "completed"
	case string(obs.EventStepPartial):
		return "partial"
	case string(obs.EventTaskSubmitted), string(obs.EventStepStarted):
		return "running"
	case string(obs.EventStepRetrying):
		return "retrying"
	case string(obs.EventDecisionRequired):
		return "awaiting_decision"
	case string(obs.EventDecisionSubmitted):
		return "decision_resolved"
	case string(obs.EventTaskSuspended):
		return "suspended"
	case string(obs.EventTaskResumed):
		return "resumed"
	default:
		return ""
	}
}

// webhooksSummaryForEvent renders a human-readable summary line for a
// webhook payload. It delegates to buildPayload's task-status branch for
// the two legacy terminal events so existing receiver prompt templates
// keep getting byte-identical summaries; other events get a compact
// "event_type: task_id/step_id" line.
func webhooksSummaryForEvent(ev obs.AgentEvent) string {
	status := webhooksStatusForEvent(ev)
	switch status {
	case "completed", "failed":
		// Preserve the legacy summary shape so consumers that branch on
		// "orchestrator task completed"/"orchestrator task failed" continue to work.
		summary := "orchestrator task failed"
		if status == "completed" {
			summary = "orchestrator task completed"
			if c, ok := ev.Payload["confidence"]; ok {
				summary = fmt.Sprintf("orchestrator task completed (confidence=%v)", c)
			}
		}
		if desc, ok := ev.Payload["task"].(string); ok && desc != "" {
			summary = fmt.Sprintf("task %q %s", truncateRunes(desc, 120), doneWord(status))
		}
		return summary
	case "partial":
		return fmt.Sprintf("step partially completed (%s)", shortRef(ev))
	case "running":
		return fmt.Sprintf("execution started (%s)", shortRef(ev))
	case "retrying":
		return fmt.Sprintf("retrying (%s)", shortRef(ev))
	case "awaiting_decision":
		return fmt.Sprintf("awaiting decision (%s)", shortRef(ev))
	case "decision_resolved":
		return fmt.Sprintf("decision submitted (%s)", shortRef(ev))
	case "suspended":
		return fmt.Sprintf("task suspended (%s)", shortRef(ev))
	case "resumed":
		return fmt.Sprintf("task resumed (%s)", shortRef(ev))
	default:
		return fmt.Sprintf("%s (%s)", ev.EventType, shortRef(ev))
	}
}

// shortRef renders "task_id[+step_id]" for summary lines without the two
// fields being empty.
func shortRef(ev obs.AgentEvent) string {
	if ev.StepID != "" {
		return ev.TaskID + "/" + ev.StepID
	}
	return ev.TaskID
}

// buildPayload maps an orchestrator AgentEvent to the flat payload shape the
// webhook prompt template expects.
func (n *webhookNotifier) buildPayload(ev obs.AgentEvent) ([]byte, error) {
	status := webhooksStatusForEvent(ev)
	summary := webhooksSummaryForEvent(ev)

	out := map[string]any{
		"event_type":      ev.EventType,
		"task_id":         ev.TaskID,
		"step_id":         ev.StepID,
		"status":          status,
		"summary":         truncateRunes(summary, webhookMaxSummaryRunes),
		"context_summary": "orchestrator task lifecycle event; see payload for execution details.",
		"payload":         ev.Payload,
		"event_id":        ev.EventID,
		"timestamp":       ev.Timestamp,
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if len(b) > webhookMaxPayloadBytes {
		// Receiver caps bodies at 1MB; keep well under it and keep the message
		// readable instead of shipping a truncated JSON blob.
		out["payload"] = map[string]any{
			"truncated":    true,
			"original_len": len(b),
		}
		if b, err = json.Marshal(out); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// doneWord renders the terminal verb for a status.
func doneWord(status string) string {
	if status == "failed" {
		return "execution failed"
	}
	return "execution completed"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Send delivers to the notification route (deliver_only: the rendered prompt is
// the message body, no LLM round trip).
func (n *webhookNotifier) Send(ev obs.AgentEvent) error {
	return n.doSend(n.route, ev)
}

// SendAgent delivers to the agent route (the LLM processes the payload and
// replies with the result).
func (n *webhookNotifier) SendAgent(ev obs.AgentEvent) error {
	return n.doSend(n.agentRoute, ev)
}

func (n *webhookNotifier) doSend(route string, ev obs.AgentEvent) error {
	if !n.enabled() {
		return nil
	}
	body, err := n.buildPayload(ev)
	if err != nil {
		log.Printf("WARN webhook: build payload for %s: %v", ev.TaskID, err)
		return err
	}
	url := n.url
	if i := strings.Index(url, "/webhooks/"); i >= 0 {
		// URL was already given with a route suffix; rewrite that route so
		// SendAgent can target the agent route without duplicating the path.
		url = url[:i+len("/webhooks/")] + route
	} else {
		url = url + "/webhooks/" + route
	}
	if err := n.post(url, body); err != nil {
		log.Printf("WARN webhook: delivery to %s failed (task %s): %v", route, ev.TaskID, err)
	}
	return err
}

// post performs the signed POST. Transport errors are returned so the caller can
// log them; any HTTP response that completes is treated as accepted — 200 means
// the target accepted the message, 202 means the run was queued.
func (n *webhookNotifier) post(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", n.nextDeliveryID())
	req.Header.Set("X-Hub-Signature-256", SignedWebhookHeader(body, n.secret))

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	return nil
}

// SignedWebhookHeader returns the GitHub-style "sha256=<hex>" HMAC-SHA256 of
// body under secret, matching the header format the receiver validates.
func SignedWebhookHeader(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// SubscribeTaskEvents wires this notifier into the global EventBus for task
// terminal events. It is a SEPARATE subscription from ChatHandler's
// WireTaskCompletionCallbacks on purpose: that handler bails out early for
// tasks without a chat session, which would silently swallow every callback
// fired by a non-chat task (CLI, cron, API caller).
//
// Fire-and-forget: the push never affects task status. Subscribe is synchronous
// (EventBus.Publish dispatches inline), but post() is bounded by client.Timeout
// (default 10s), so a wedged endpoint delays a task's terminal event by at most
// that long rather than hanging the scheduler.
//
// Callers own the returned unsubscribe funcs; see webhookNotifier.unwire.
func (n *webhookNotifier) SubscribeTaskEvents(scheduler *core.DirectedEngine) {
	if !n.enabled() || scheduler == nil {
		return
	}

	// Config-driven path: when routes are configured, subscribe to the
	// union of all enabled routes' TriggerOn events and dispatch each
	// event only to matching routes (3-layer filter).
	if hasEnabledRoutes(n.routes) {
		n.subscribeWithRoutes(scheduler)
		return
	}

	send := func(ev obs.AgentEvent) error {
		if ev.TaskID == "" {
			return nil
		}
		// Enrich from the scheduler so the message names the task instead of
		// just its id, and carries the terminal status.
		if data, ok := scheduler.GetStatus(ev.TaskID, "full"); ok && data != nil {
			if desc, ok := data["task"].(string); ok && desc != "" {
				ev.Payload["task"] = desc
			}
			if st, ok := data["status"].(string); ok && st != "" {
				ev.Payload["status"] = st
			}
		}
		if n.useAgent {
			// Agent route: the receiver renders the prompt template, runs an
			// LLM turn, and delivers the reply to the configured chat. Answers
			// 202 immediately, so this blocks the Publish path no more than the
			// notify route does.
			return n.SendAgent(ev)
		}
		return n.Send(ev)
	}
	// Subscribe to Tier 0 (terminal) and Tier 1 (lifecycle) events.
	// Tier 2 (debug/governance) events are excluded by default to avoid
	// webhook spam; they can be added by extending this slice.
	eventTypes := []string{
		string(obs.EventTaskCompleted),
		string(obs.EventTaskFailed),
		string(obs.EventStepCompleted),
		string(obs.EventStepFailed),
		string(obs.EventStepRetrying),
		string(obs.EventDecisionRequired),
		string(obs.EventTaskSuspended),
		string(obs.EventTaskResumed),
	}
	n.unsubscribers = make([]func(), 0, len(eventTypes))
	for _, evt := range eventTypes {
		n.unsubscribers = append(n.unsubscribers,
			obs.DefaultBus.SubscribeWithCancel(evt, send))
	}
}

// hasEnabledRoutes returns true when at least one route is Enabled.
func hasEnabledRoutes(routes []config.NotificationRoute) bool {
	for _, r := range routes {
		if r.Enabled {
			return true
		}
	}
	return false
}

// subscribeWithRoutes subscribes to the union of all enabled routes' event
// types and dispatches each event only to matching routes. Each matching
// route is sent to independently (no merging).
func (n *webhookNotifier) subscribeWithRoutes(scheduler *core.DirectedEngine) {
	// Collect the union of event types from all enabled routes.
	eventSet := make(map[string]bool)
	for _, r := range n.routes {
		if !r.Enabled {
			continue
		}
		if len(r.TriggerOn) == 0 {
			// Tier 0 default.
			eventSet[string(obs.EventTaskCompleted)] = true
			eventSet[string(obs.EventTaskFailed)] = true
		}
		for _, t := range r.TriggerOn {
			eventSet[t] = true
		}
	}

	send := func(ev obs.AgentEvent) error {
		if ev.TaskID == "" {
			return nil
		}
		// Enrich from scheduler (same as hardcoded path).
		if data, ok := scheduler.GetStatus(ev.TaskID, "full"); ok && data != nil {
			if desc, ok := data["task"].(string); ok && desc != "" {
				ev.Payload["task"] = desc
			}
			if st, ok := data["status"].(string); ok && st != "" {
				ev.Payload["status"] = st
			}
		}
		// Dispatch to each matching route independently.
		for _, r := range n.routes {
			if matchRoute(ev, r, scheduler) {
				n.sendToRoute(r, ev)
			}
		}
		return nil
	}

	n.unsubscribers = make([]func(), 0, len(eventSet))
	for evt := range eventSet {
		n.unsubscribers = append(n.unsubscribers,
			obs.DefaultBus.SubscribeWithCancel(evt, send))
	}
}

// sendToRoute delivers an event to a specific route. Fire-and-forget: errors
// are logged only. Dispatches to webhook (default) or telegram based on
// route.Channel (admin lifecycle §3.1 — channel seam).
func (n *webhookNotifier) sendToRoute(route config.NotificationRoute, ev obs.AgentEvent) {
	switch route.Channel {
	case "telegram":
		n.sendToTelegram(route, ev)
	default:
		n.sendToWebhook(route, ev)
	}
}

// sendToTelegram delivers an event via the Telegram Bot API sendMessage.
// The bot token comes from route.SecretEnv (never the URL), chat_id from
// route.ChatID. Fire-and-forget: errors are logged only.
func (n *webhookNotifier) sendToTelegram(route config.NotificationRoute, ev obs.AgentEvent) {
	token := getEnv(route.SecretEnv)
	if token == "" || route.ChatID == "" {
		log.Printf("WARN telegram: route %s missing bot token (%s) or chat_id", route.Name, route.SecretEnv)
		return
	}
	text := fmt.Sprintf("[%s] task=%s step=%s", ev.EventType, ev.TaskID, ev.StepID)
	if ev.Payload != nil {
		if summary, ok := ev.Payload["summary"].(string); ok && summary != "" {
			text += "\n" + summary
		}
	}
	text = truncateRunes(text, 4000)
	body, _ := json.Marshal(map[string]string{"chat_id": route.ChatID, "text": text})
	url := "https://api.telegram.org/bot" + token + "/sendMessage"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN telegram: build request for route %s: %s", route.Name, tgRedact(token, err.Error()))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// Same reason as tgCall: this error quotes the URL, and the URL is
		// "https://api.telegram.org/bot<token>/sendMessage".
		log.Printf("WARN telegram: delivery to route %s failed: %s", route.Name, tgRedact(token, err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("WARN telegram: route %s returned status %d", route.Name, resp.StatusCode)
	}
}

// sendToWebhook delivers an event to a route's TargetURL with route-specific
// HMAC signing. Fire-and-forget: errors are logged only.
func (n *webhookNotifier) sendToWebhook(route config.NotificationRoute, ev obs.AgentEvent) {
	body, err := n.buildPayload(ev)
	if err != nil {
		log.Printf("WARN webhook: build payload for route %s: %v", route.Name, err)
		return
	}
	secret := n.secret
	if route.SecretEnv != "" {
		if s := getEnv(route.SecretEnv); s != "" {
			secret = s
		}
	}
	req, err := http.NewRequest(http.MethodPost, route.TargetURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("WARN webhook: build request for route %s: %v", route.Name, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Delivery", n.nextDeliveryID())
	req.Header.Set("X-Hub-Signature-256", SignedWebhookHeader(body, secret))

	resp, err := n.client.Do(req)
	if err != nil {
		log.Printf("WARN webhook: delivery to route %s failed (task %s): %v", route.Name, ev.TaskID, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		log.Printf("WARN webhook: route %s returned status %d", route.Name, resp.StatusCode)
	}
}

// UnsubscribeTaskEvents tears down the task-event subscriptions. Safe to call
// even when nothing was wired.
func (n *webhookNotifier) UnsubscribeTaskEvents() {
	for _, unsub := range n.unsubscribers {
		unsub()
	}
	n.unsubscribers = nil
}

// WireTaskWebhook wires the process-wide task webhook callback from the
// environment and returns a teardown func. It returns nil when
// WEBHOOK_CALLBACK_URL is unset or no scheduler is available — the safe
// default-off path.
//
// routes from config.NotificationConfig.Routes enable config-driven 3-layer
// filtering (event_type / task_id_glob / step_id_glob / task_type). When
// routes is empty or none are Enabled, the notifier falls back to the
// hardcoded 8-event subscription (backward compatible).
//
// Ownership rule: call this EXACTLY ONCE per process, and defer the returned
// func. ChatHandler does not subscribe on its own, so this is the single source
// of truth for every build:
//   - the default stdio/MCP build (main.go) has no ChatHandler at all;
//   - the web/desktop builds (main_web.go, main_desktop.go) create one, but they
//     are a different binary under build tags, so there is no overlap.
//
// EventBus.Subscribe has no teardown, so a second call would accumulate a second
// subscriber and double-deliver. A single call site is the only guarantee
// against that.
func WireTaskWebhook(scheduler *core.DirectedEngine, routes []config.NotificationRoute) (unsubscribe func()) {
	base := getEnv("WEBHOOK_CALLBACK_URL")
	if base == "" || scheduler == nil {
		return nil
	}
	n := NewWebhookNotifier(base)
	n.SetRoutes(routes)
	n.SubscribeTaskEvents(scheduler)
	return n.UnsubscribeTaskEvents
}
