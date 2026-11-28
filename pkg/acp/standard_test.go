package acp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// eventMockEngine is a mock Engine that publishes obs events after Submit.
type eventMockEngine struct {
	taskID     string
	submitErr  error
	cancelErr  error
	cancelled  string
	lastPrompt string
	events     []obs.AgentEvent
	delay      time.Duration
}

func (m *eventMockEngine) Submit(inputs []schemas.StepInput) (string, error) {
	if len(inputs) > 0 {
		m.lastPrompt = inputs[0].Task
	}
	if m.submitErr != nil {
		return "", m.submitErr
	}
	id := m.taskID
	if id == "" {
		id = "std-task-1"
	}
	if len(m.events) > 0 {
		go func() {
			time.Sleep(m.delay)
			for _, ev := range m.events {
				obs.DefaultBus.Publish(ev)
			}
		}()
	}
	return id, nil
}

func (m *eventMockEngine) CancelTask(taskID string) error {
	m.cancelled = taskID
	return m.cancelErr
}

type reusableSessionEngine struct {
	mu        sync.Mutex
	next      int
	cancelled []string
	submitted chan string
}

func (m *reusableSessionEngine) Submit(inputs []schemas.StepInput) (string, error) {
	m.mu.Lock()
	m.next++
	n := m.next
	m.mu.Unlock()
	taskID := fmt.Sprintf("reuse-task-%d", n)
	m.submitted <- taskID
	if n == 2 {
		go func() {
			time.Sleep(20 * time.Millisecond)
			obs.DefaultBus.Publish(obs.NewAgentEvent(taskID, "", string(obs.EventTaskCompleted),
				map[string]any{"result": "second prompt completed"}))
		}()
	}
	return taskID, nil
}

func (m *reusableSessionEngine) CancelTask(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cancelled = append(m.cancelled, taskID)
	return nil
}

// parseAllLines parses all newline-delimited JSON from output into a slice
// of generic maps (works for both responses and notifications).
func parseAllLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("failed to parse line %q: %v", line, err)
		}
		result = append(result, m)
	}
	return result
}

// findResponseByID finds the JSON-RPC response with the given id.
func findResponseByID(t *testing.T, msgs []map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range msgs {
		rawID, ok := m["id"]
		if !ok {
			continue
		}
		var msgID string
		switch v := rawID.(type) {
		case float64:
			msgID = fmt.Sprintf("%v", int64(v))
		case string:
			msgID = v
		default:
			continue
		}
		if msgID == id {
			return m
		}
	}
	t.Fatalf("no response with id=%s found", id)
	return nil
}

// findNotifications filters messages that have a "method" field (notifications).
func findNotifications(msgs []map[string]any, method string) []map[string]any {
	var result []map[string]any
	for _, m := range msgs {
		if m["method"] == method {
			result = append(result, m)
		}
	}
	return result
}

// --- P0: Dialect Negotiation ---

func TestACP_StandardDialect_Initialize(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "1")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] == nil {
		t.Errorf("expected protocolVersion in result, got %v", result)
	}
	caps, ok := result["agentCapabilities"].(map[string]any)
	if !ok {
		t.Fatalf("expected agentCapabilities map, got %v", result["agentCapabilities"])
	}
	if caps["loadSession"] != false {
		t.Errorf("expected loadSession=false, got %v", caps["loadSession"])
	}
}

func TestACP_LegacyDialect_Initialize_StillWorks(t *testing.T) {
	// initialize WITHOUT protocolVersion must return legacy format.
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	resps := parseResponses(t, out)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("expected 1 success, got %+v", resps)
	}
	result := resps[0].Result.(map[string]any)
	if result["server"] != "orchestrator" {
		t.Errorf("expected server=orchestrator (legacy), got %v", result["server"])
	}
}

// --- P1: Session Lifecycle ---

func TestACP_StandardDialect_NewSession(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "2")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	sid, ok := result["sessionId"].(string)
	if !ok || sid == "" {
		t.Errorf("expected non-empty sessionId, got %v", result["sessionId"])
	}
}

func TestACP_StandardDialect_SessionNotFound(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"nonexistent","prompt":[{"type":"text","text":"hi"}]}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "2")
	if resp["error"] == nil {
		t.Fatalf("expected error for nonexistent session, got success")
	}
	errObj := resp["error"].(map[string]any)
	// ACP v1 spec: unknown session → -32002 Resource not found with data.sessionId
	if errObj["code"] != float64(-32002) {
		t.Errorf("expected code -32002 (Resource not found), got %v", errObj["code"])
	}
	data, ok := errObj["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data field with sessionId, got %v", errObj["data"])
	}
	if data["sessionId"] != "nonexistent" {
		t.Errorf("expected data.sessionId=nonexistent, got %v", data["sessionId"])
	}
}

func TestACP_StandardDialect_Prompt_EndToEnd(t *testing.T) {
	mock := &eventMockEngine{
		taskID: "std-task-1",
		delay:  20 * time.Millisecond,
		events: []obs.AgentEvent{
			obs.NewAgentEvent("std-task-1", "", string(obs.EventTaskCompleted),
				map[string]any{"status": "completed"}),
		},
	}
	out := runStandardACPWithPipe(t, mock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`+"\n",
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"SESSION_ID","prompt":[{"type":"text","text":"hello"}]}}`+"\n",
		500*time.Millisecond,
	)
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "3")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["stopReason"] != string(StopReasonEndTurn) {
		t.Errorf("expected stopReason=end_turn, got %v", result["stopReason"])
	}
	if mock.lastPrompt != "hello" {
		t.Errorf("engine got prompt %q, want %q", mock.lastPrompt, "hello")
	}
}

// --- P2: Streaming ---

func TestACP_StandardDialect_Streaming(t *testing.T) {
	mock := &eventMockEngine{
		taskID: "std-task-1",
		delay:  20 * time.Millisecond,
		events: []obs.AgentEvent{
			obs.NewAgentEvent("std-task-1", "step-1", string(obs.EventStepStarted), nil),
			obs.NewAgentEvent("std-task-1", "step-1", string(obs.EventToolCall), map[string]any{"tool": "read_file"}),
			obs.NewAgentEvent("std-task-1", "step-1", string(obs.EventStepCompleted), nil),
			obs.NewAgentEvent("std-task-1", "", string(obs.EventTaskCompleted), map[string]any{"status": "completed"}),
		},
	}
	out := runStandardACPWithPipe(t, mock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`+"\n",
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"SESSION_ID","prompt":[{"type":"text","text":"hello"}]}}`+"\n",
		500*time.Millisecond,
	)
	msgs := parseAllLines(t, out)

	// Verify session/update notifications were received.
	updates := findNotifications(msgs, "session/update")
	if len(updates) < 2 {
		t.Errorf("expected at least 2 session/update notifications, got %d", len(updates))
	}

	// Verify the prompt response was received.
	resp := findResponseByID(t, msgs, "3")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["stopReason"] != string(StopReasonEndTurn) {
		t.Errorf("expected stopReason=end_turn, got %v", result["stopReason"])
	}
}

// --- P3: Cancel ---

func TestACP_StandardDialect_Cancel(t *testing.T) {
	mock := &eventMockEngine{
		taskID: "std-task-1",
		// No events — prompt blocks until cancelled.
	}
	out := runStandardACPWithPipe(t, mock,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp"}}`+"\n",
		`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"SESSION_ID","prompt":[{"type":"text","text":"hello"}]}}`+"\n"+
			`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"SESSION_ID"}}`+"\n",
		500*time.Millisecond,
	)
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "3")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["stopReason"] != string(StopReasonCancelled) {
		t.Errorf("expected stopReason=cancelled, got %v", result["stopReason"])
	}
	if mock.cancelled != "std-task-1" {
		t.Errorf("engine.CancelTask not called or wrong taskID: %q", mock.cancelled)
	}
}

// --- P4: Error Codes ---

func TestACP_StandardDialect_AgentInfo(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "1")
	result := resp["result"].(map[string]any)
	info, ok := result["agentInfo"].(map[string]any)
	if !ok {
		t.Fatalf("expected agentInfo map, got %v", result["agentInfo"])
	}
	if info["name"] != "orchestrator" {
		t.Errorf("expected agentInfo.name=orchestrator, got %v", info["name"])
	}
	if info["version"] == nil || info["version"] == "" {
		t.Errorf("expected non-empty agentInfo.version, got %v", info["version"])
	}
}

func TestACP_StandardDialect_ProtocolVersionNegotiation(t *testing.T) {
	// Client requests version 5 (higher than our supported version 1).
	// Agent should respond with its latest supported version (1).
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":5}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "1")
	result := resp["result"].(map[string]any)
	pv, ok := result["protocolVersion"].(float64)
	if !ok {
		t.Fatalf("expected protocolVersion number, got %v", result["protocolVersion"])
	}
	if pv != float64(StandardProtocolVersion) {
		t.Errorf("expected negotiated version=%d, got %v", StandardProtocolVersion, pv)
	}
}

func TestACP_VORTEX_ACP_DIALECT_StandardForced(t *testing.T) {
	// VORTEX_ACP_DIALECT=standard forces standard mode even without protocolVersion.
	old := os.Getenv("VORTEX_ACP_DIALECT")
	os.Setenv("VORTEX_ACP_DIALECT", "standard")
	defer os.Setenv("VORTEX_ACP_DIALECT", old)

	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "1")
	result := resp["result"].(map[string]any)
	// Standard mode returns protocolVersion; legacy mode returns server/version.
	if result["protocolVersion"] == nil {
		t.Errorf("expected standard mode (protocolVersion present) with VORTEX_ACP_DIALECT=standard, got %v", result)
	}
	if result["server"] != nil {
		t.Errorf("did not expect legacy format (server field) with VORTEX_ACP_DIALECT=standard, got %v", result)
	}
}

func TestACP_VORTEX_ACP_DIALECT_LegacyForced(t *testing.T) {
	// VORTEX_ACP_DIALECT=legacy forces legacy mode even with protocolVersion.
	old := os.Getenv("VORTEX_ACP_DIALECT")
	os.Setenv("VORTEX_ACP_DIALECT", "legacy")
	defer os.Setenv("VORTEX_ACP_DIALECT", old)

	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n")
	resps := parseResponses(t, out)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("expected 1 success, got %+v", resps)
	}
	result := resps[0].Result.(map[string]any)
	// Legacy mode returns server/version; standard mode returns protocolVersion.
	if result["server"] != "orchestrator" {
		t.Errorf("expected legacy mode (server=orchestrator) with VORTEX_ACP_DIALECT=legacy, got %v", result)
	}
	if result["protocolVersion"] != nil {
		t.Errorf("did not expect standard format (protocolVersion field) with VORTEX_ACP_DIALECT=legacy, got %v", result)
	}
}

func TestACP_StandardDialect_UnknownMethod(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"frobnicate"}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "2")
	if resp["error"] == nil {
		t.Fatalf("expected error for unknown method")
	}
	errObj := resp["error"].(map[string]any)
	if errObj["code"] != float64(-32601) {
		t.Errorf("expected code -32601, got %v", errObj["code"])
	}
}

func TestACP_StandardDialect_PingInStandardMode(t *testing.T) {
	out := runStandardACP(t, &eventMockEngine{},
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"method":"ping"}`+"\n")
	msgs := parseAllLines(t, out)
	resp := findResponseByID(t, msgs, "2")
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["pong"] != true {
		t.Errorf("expected pong=true, got %v", result["pong"])
	}
}

// Regression for docs/plan/O47_O35_Q3_ADJUSTMENT_PLAN.md §3.5: the engine's
// terminal answer is payload.result, not payload.status.
func TestACP_StandardDialect_FinalResultPayload(t *testing.T) {
	var out safeBuffer
	srv := newStandardServer(nil, func(v any) {
		data, _ := json.Marshal(v)
		out.Write(append(data, '\n'))
	}, make(chan struct{}))
	defer srv.close()

	srv.emitFinalChunk("session-1", obs.NewAgentEvent("task-1", "", string(obs.EventTaskCompleted),
		map[string]any{"result": "final answer", "status": "completed"}))

	updates := findNotifications(parseAllLines(t, out.String()), "session/update")
	if len(updates) != 1 {
		t.Fatalf("expected one final update, got %d", len(updates))
	}
	params := updates[0]["params"].(map[string]any)
	update := params["update"].(map[string]any)
	content := update["content"].(map[string]any)
	if content["text"] != "final answer" {
		t.Fatalf("final chunk text = %v, want final answer", content["text"])
	}
}

type resolvingEventEngine struct {
	eventMockEngine
	result string
}

func (m *resolvingEventEngine) ResolveTaskResult(string) string { return m.result }

// Regression for docs/plan/O47_O35_Q3_ADJUSTMENT_PLAN.md §3.5: successful
// tasks can reach the terminal event without an inline result. QwenPaw's real
// ACP client then receives an empty agent_message_chunk unless the server
// resolves the persisted step result.
func TestACP_StandardDialect_FinalResultFromEngine(t *testing.T) {
	engine := &resolvingEventEngine{result: "persisted final answer"}
	var out safeBuffer
	srv := newStandardServer(engine, func(v any) {
		data, _ := json.Marshal(v)
		out.Write(append(data, '\n'))
	}, make(chan struct{}))
	defer srv.close()

	srv.emitFinalChunk("session-1", obs.NewAgentEvent("task-1", "", string(obs.EventTaskCompleted),
		map[string]any{"status": "completed"}))

	updates := findNotifications(parseAllLines(t, out.String()), "session/update")
	if len(updates) != 1 {
		t.Fatalf("expected one final update, got %d", len(updates))
	}
	params := updates[0]["params"].(map[string]any)
	update := params["update"].(map[string]any)
	content := update["content"].(map[string]any)
	if content["text"] != "persisted final answer" {
		t.Fatalf("final chunk text = %v, want persisted final answer", content["text"])
	}
}

// Regression for docs/plan/O47_O35_Q3_ADJUSTMENT_PLAN.md §3.5: cancellation
// belongs to one prompt turn and must not poison later prompts in the session.
func TestACP_StandardDialect_SessionReusableAfterCancel(t *testing.T) {
	engine := &reusableSessionEngine{submitted: make(chan string, 2)}
	var out safeBuffer
	srv := newStandardServer(engine, func(v any) {
		data, _ := json.Marshal(v)
		out.Write(append(data, '\n'))
	}, make(chan struct{}))
	defer srv.close()

	newResult, rpcErr := srv.handleNewSession(json.RawMessage(`{"cwd":"/tmp"}`))
	if rpcErr != nil {
		t.Fatalf("session/new failed: %+v", rpcErr)
	}
	sessionID := newResult.SessionID
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.handlePrompt(json.RawMessage(fmt.Sprintf(
			`{"sessionId":%q,"prompt":[{"type":"text","text":"first"}]}`, sessionID)), json.RawMessage(`1`))
	}()
	select {
	case <-engine.submitted:
	case <-time.After(time.Second):
		t.Fatal("first prompt was not submitted")
	}
	srv.handleCancel(json.RawMessage(fmt.Sprintf(`{"sessionId":%q}`, sessionID)))
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled first prompt did not return")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		srv.handlePrompt(json.RawMessage(fmt.Sprintf(
			`{"sessionId":%q,"prompt":[{"type":"text","text":"second"}]}`, sessionID)), json.RawMessage(`2`))
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second prompt did not complete after cancellation")
	}

	msgs := parseAllLines(t, out.String())
	first := findResponseByID(t, msgs, "1")["result"].(map[string]any)
	if first["stopReason"] != string(StopReasonCancelled) {
		t.Fatalf("first stopReason = %v, want cancelled", first["stopReason"])
	}
	second := findResponseByID(t, msgs, "2")["result"].(map[string]any)
	if second["stopReason"] != string(StopReasonEndTurn) {
		t.Fatalf("second stopReason = %v, want end_turn", second["stopReason"])
	}
	engine.mu.Lock()
	cancelled := append([]string(nil), engine.cancelled...)
	engine.mu.Unlock()
	if len(cancelled) != 1 || cancelled[0] != "reuse-task-1" {
		t.Fatalf("cancelled tasks = %v, want only reuse-task-1", cancelled)
	}
}

// --- Helpers ---

// runStandardACP runs ServeACP with a string reader (for simple tests that
// don't involve blocking session/prompt).
func runStandardACP(t *testing.T, engine Engine, input string) string {
	t.Helper()
	var stdout bytes.Buffer
	if err := ServeACP(engine, strings.NewReader(input), &stdout); err != nil {
		t.Fatalf("ServeACP error: %v", err)
	}
	return stdout.String()
}

// runStandardACPWithPipe runs ServeACP with a pipe so that session/prompt
// can block without the scanner exiting prematurely. preInput is sent first,
// then the sessionId is extracted and substituted into postInput (replacing
// the literal "SESSION_ID"), then postInput is sent. After waitDelay, the
// pipe is closed and the full output is returned.
func runStandardACPWithPipe(t *testing.T, engine Engine, preInput, postInput string, waitDelay time.Duration) string {
	t.Helper()
	pr, pw := io.Pipe()
	var stdout safeBuffer
	done := make(chan struct{})
	go func() {
		ServeACP(engine, pr, &stdout)
		close(done)
	}()

	// Send preInput (initialize + session@new).
	pw.Write([]byte(preInput))
	time.Sleep(100 * time.Millisecond)

	// Extract sessionId from stdout.
	msgs := parseAllLines(t, stdout.String())
	resp := findResponseByID(t, msgs, "2")
	result := resp["result"].(map[string]any)
	sessionID := result["sessionId"].(string)

	// Substitute SESSION_ID and send postInput.
	finalInput := strings.ReplaceAll(postInput, "SESSION_ID", sessionID)
	pw.Write([]byte(finalInput))
	time.Sleep(waitDelay)

	// Close pipe and wait for ServeACP to finish.
	pw.Close()
	<-done

	return stdout.String()
}
