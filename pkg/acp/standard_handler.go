package acp

import (
	"encoding/json"
	"sync"

	"github.com/google/uuid"
	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

// standardServer handles Zed-standard ACP requests after dialect negotiation
// detects protocolVersion in the initialize params. It manages session<->task
// mapping, streams obs events as session/update notifications, and blocks
// session/prompt until the task reaches a terminal state.
//
// See docs/plan/STANDARD_ACP_SERVER_SUPPORT_DESIGN.md for the full design.
type standardServer struct {
	engine    Engine
	writeJSON func(any)
	done      chan struct{} // closed when ServeACP scanner exits

	sessionsMu sync.Mutex
	sessions   map[string]*sessionInfo

	// Terminal event buffer: taskID -> terminal event. Installed at server
	// startup via global obs subscription to avoid the "task completes before
	// wait registers" race (design doc section 2.3).
	terminalMu  sync.Mutex
	terminalBuf map[string]obs.AgentEvent
	waitChs     map[string]chan struct{}

	unsubTerminal func() // unsubscribes terminal event handlers
}

// sessionInfo tracks one ACP session and its associated task.
type sessionInfo struct {
	id        string
	cwd       string
	taskID    string // set once after engine.Submit; protected by sessionsMu
	cancelCh  chan struct{}
	cancelled bool
}

func newStandardServer(engine Engine, writeJSON func(any), done chan struct{}) *standardServer {
	s := &standardServer{
		engine:      engine,
		writeJSON:   writeJSON,
		done:        done,
		sessions:    make(map[string]*sessionInfo),
		terminalBuf: make(map[string]obs.AgentEvent),
		waitChs:     make(map[string]chan struct{}),
	}
	unsub1 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskCompleted), s.bufferTerminal)
	unsub2 := obs.DefaultBus.SubscribeWithCancel(string(obs.EventTaskFailed), s.bufferTerminal)
	s.unsubTerminal = func() { unsub1(); unsub2() }
	return s
}

func (s *standardServer) close() {
	if s.unsubTerminal != nil {
		s.unsubTerminal()
	}
}

// bufferTerminal stores terminal events and signals any waiting prompt.
func (s *standardServer) bufferTerminal(ev obs.AgentEvent) error {
	s.terminalMu.Lock()
	defer s.terminalMu.Unlock()
	s.terminalBuf[ev.TaskID] = ev
	if ch, ok := s.waitChs[ev.TaskID]; ok {
		close(ch)
		delete(s.waitChs, ev.TaskID)
	}
	return nil
}

// handleInitialize returns the standard ACP initialize response.
func (s *standardServer) handleInitialize(clientVersion int) StandardInitializeResult {
	// Protocol version negotiation: if client supports our version, echo it;
	// otherwise respond with our latest supported version.
	negotiated := StandardProtocolVersion
	if clientVersion > 0 && clientVersion < StandardProtocolVersion {
		negotiated = clientVersion
	}
	return StandardInitializeResult{
		ProtocolVersion: negotiated,
		AgentCapabilities: AgentCapabilities{
			LoadSession: false,
			PromptCapabilities: PromptCapabilities{
				Image:           false,
				Audio:           false,
				EmbeddedContext: false,
			},
		},
		AgentInfo: ImplementationInfo{
			Name:    "orchestrator",
			Title:   "Orchestrator MCP",
			Version: "1.0.0",
		},
	}
}

// handleNewSession creates a new ACP session.
func (s *standardServer) handleNewSession(params json.RawMessage) (NewSessionResult, *rpcError) {
	var p NewSessionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return NewSessionResult{}, &rpcError{Code: -32602, Message: "invalid params"}
	}
	sid := uuid.New().String()
	s.sessionsMu.Lock()
	s.sessions[sid] = &sessionInfo{id: sid, cwd: p.Cwd, cancelCh: make(chan struct{})}
	s.sessionsMu.Unlock()
	return NewSessionResult{SessionID: sid}, nil
}

// handlePrompt submits a task, streams session/update notifications, and
// blocks until the task reaches a terminal state or the session is cancelled.
// Writes the response (PromptResult or error) directly via writeJSON.
func (s *standardServer) handlePrompt(params json.RawMessage, reqID json.RawMessage) {
	var p PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Error: &rpcError{Code: -32602, Message: "invalid params"}})
		return
	}

	s.sessionsMu.Lock()
	session, ok := s.sessions[p.SessionID]
	s.sessionsMu.Unlock()
	if !ok {
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Error: &rpcError{
			Code: -32002, Message: "Resource not found",
			Data: map[string]string{"sessionId": p.SessionID},
		}})
		return
	}

	promptText := ""
	for _, b := range p.Prompt {
		if b.Type == "text" {
			promptText += b.Text
		}
	}

	// Subscribe to intermediate events for streaming (before Submit).
	unsub1 := s.subscribeStreamEvent(obs.EventStepStarted, session, p.SessionID, "plan", "")
	unsub2 := s.subscribeStreamEvent(obs.EventToolCall, session, p.SessionID, "tool_call", "in_progress")
	unsub3 := s.subscribeStreamEvent(obs.EventStepCompleted, session, p.SessionID, "tool_call_update", "completed")
	defer unsub1()
	defer unsub2()
	defer unsub3()

	taskID, err := s.engine.Submit([]schemas.StepInput{{ID: "acp-1", Task: promptText}})
	if err != nil {
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Error: &rpcError{Code: -32603, Message: err.Error()}})
		return
	}

	s.sessionsMu.Lock()
	session.taskID = taskID
	s.sessionsMu.Unlock()

	// Check if terminal event already arrived (race: task completed before
	// we registered the wait channel).
	waitCh := make(chan struct{})
	s.terminalMu.Lock()
	if ev, ok := s.terminalBuf[taskID]; ok {
		delete(s.terminalBuf, taskID)
		s.terminalMu.Unlock()
		s.emitFinalChunk(p.SessionID, ev)
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Result: PromptResult{StopReason: StopReasonEndTurn}})
		return
	}
	s.waitChs[taskID] = waitCh
	s.terminalMu.Unlock()

	select {
	case <-waitCh:
		s.terminalMu.Lock()
		ev := s.terminalBuf[taskID]
		delete(s.terminalBuf, taskID)
		delete(s.waitChs, taskID)
		s.terminalMu.Unlock()
		s.emitFinalChunk(p.SessionID, ev)
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Result: PromptResult{StopReason: StopReasonEndTurn}})
	case <-session.cancelCh:
		s.engine.CancelTask(taskID)
		s.writeJSON(Response{JSONRPC: "2.0", ID: reqID, Result: PromptResult{StopReason: StopReasonCancelled}})
	case <-s.done:
		// Scanner loop exited (client disconnected); stop waiting.
	}
}

// subscribeStreamEvent subscribes to an intermediate obs event type and
// forwards matching events as session/update notifications.
func (s *standardServer) subscribeStreamEvent(evType obs.EventType, session *sessionInfo, sessionID, updateType, status string) func() {
	return obs.DefaultBus.SubscribeWithCancel(string(evType), func(ev obs.AgentEvent) error {
		s.sessionsMu.Lock()
		match := session.taskID == ev.TaskID
		s.sessionsMu.Unlock()
		if match {
			s.writeJSON(notification{
				JSONRPC: "2.0",
				Method:  "session/update",
				Params: SessionUpdateParams{
					SessionID: sessionID,
					Update:    SessionUpdate{SessionUpdate: updateType, Status: status},
				},
			})
		}
		return nil
	})
}

// emitFinalChunk sends the terminal agent_message_chunk with the result text.
func (s *standardServer) emitFinalChunk(sessionID string, ev obs.AgentEvent) {
	text := ""
	if ev.Payload != nil {
		if msg, ok := ev.Payload["message"].(string); ok {
			text = msg
		} else if status, ok := ev.Payload["status"].(string); ok {
			text = status
		}
	}
	s.writeJSON(notification{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params: SessionUpdateParams{
			SessionID: sessionID,
			Update: SessionUpdate{
				SessionUpdate: "agent_message_chunk",
				Content:       &ContentBlock{Type: "text", Text: text},
			},
		},
	})
}

// handleCancel processes a session/cancel notification (no response expected).
func (s *standardServer) handleCancel(params json.RawMessage) {
	var p StandardCancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.sessionsMu.Lock()
	session, ok := s.sessions[p.SessionID]
	if ok && !session.cancelled {
		session.cancelled = true
		close(session.cancelCh)
	}
	s.sessionsMu.Unlock()
}

// hasProtocolVersion checks if initialize params contain protocolVersion,
// which is the dialect discriminator (standard ACP vs legacy DSH).
func hasProtocolVersion(params json.RawMessage) bool {
	if len(params) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return false
	}
	_, ok := m["protocolVersion"]
	return ok
}

// extractProtocolVersion reads the protocolVersion integer from initialize
// params. Returns 0 if absent or unparseable.
func extractProtocolVersion(params json.RawMessage) int {
	if len(params) == 0 {
		return 0
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(params, &m); err != nil {
		return 0
	}
	raw, ok := m["protocolVersion"]
	if !ok {
		return 0
	}
	var v int
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0
	}
	return v
}
