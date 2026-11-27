package core

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/schemas"
)

func newTestLogger(t *testing.T) *obs.Logger {
	t.Helper()
	l, _ := obs.NewLogger(t.TempDir(), nil)
	t.Cleanup(l.Close)
	return l
}

type mockTransport struct {
	fn func(*http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.fn(req)
}

func TestValidateWebhookURL_HTTPSOnly(t *testing.T) {
	if err := validateWebhookURL("http://example.com/hook"); err == nil {
		t.Error("http URL should be rejected")
	}
}

func TestValidateWebhookURL_LoopbackIP(t *testing.T) {
	if err := validateWebhookURL("https://127.0.0.1/hook"); err == nil {
		t.Error("loopback IP should be rejected")
	}
}

func TestValidateWebhookURL_PrivateIP(t *testing.T) {
	for _, ip := range []string{
		"https://10.0.0.1/hook",
		"https://192.168.1.1/hook",
		"https://172.16.0.1/hook",
	} {
		if err := validateWebhookURL(ip); err == nil {
			t.Errorf("private IP %s should be rejected", ip)
		}
	}
}

func TestValidateWebhookURL_EmptyHost(t *testing.T) {
	if err := validateWebhookURL("https:///hook"); err == nil {
		t.Error("empty host should be rejected")
	}
}

func TestValidateWebhookURL_InvalidURL(t *testing.T) {
	if err := validateWebhookURL("://broken"); err == nil {
		t.Error("invalid URL should be rejected")
	}
}

func TestDeliverWebhook_Success(t *testing.T) {
	var received map[string]any
	sm := &ScheduleManager{
		logger: newTestLogger(t),
		httpClient: &http.Client{Transport: &mockTransport{fn: func(req *http.Request) (*http.Response, error) {
			json.NewDecoder(req.Body).Decode(&received)
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		}}},
	}
	s := &schemas.Schedule{
		ID:   "sched_test",
		Name: "test schedule",
		Deliver: schemas.DeliverTarget{
			Mode:       "webhook",
			WebhookURL: "https://192.0.2.1/hook",
		},
	}
	result := map[string]any{"task_id": "task_123", "status": "completed"}

	err := sm.deliverWebhook(context.Background(), s, result)
	if err != nil {
		t.Fatalf("deliverWebhook failed: %v", err)
	}
	if received["schedule_id"] != "sched_test" {
		t.Errorf("payload schedule_id = %v, want sched_test", received["schedule_id"])
	}
	if received["task_result"] == nil {
		t.Error("payload should include task_result")
	}
}

func TestDeliverWebhook_Non2xx(t *testing.T) {
	sm := &ScheduleManager{
		logger: newTestLogger(t),
		httpClient: &http.Client{Transport: &mockTransport{fn: func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
		}}},
	}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode:       "webhook",
			WebhookURL: "https://192.0.2.1/hook",
		},
	}

	err := sm.deliverWebhook(context.Background(), s, map[string]any{})
	if err == nil {
		t.Error("deliverWebhook should fail on HTTP 500")
	}
}

func TestDeliverWebhook_NoURL(t *testing.T) {
	sm := &ScheduleManager{logger: newTestLogger(t)}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode: "webhook",
		},
	}
	err := sm.deliverWebhook(context.Background(), s, map[string]any{})
	if err == nil {
		t.Error("deliverWebhook should fail when no URL configured")
	}
}

func TestDeliverWebhook_SSRFRejected(t *testing.T) {
	sm := &ScheduleManager{logger: newTestLogger(t)}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode:       "webhook",
			WebhookURL: "https://127.0.0.1/hook",
		},
	}
	err := sm.deliverWebhook(context.Background(), s, map[string]any{})
	if err == nil {
		t.Error("deliverWebhook should reject SSRF URL")
	}
}

func TestDeliverResult_NoOpWhenModeEmpty(t *testing.T) {
	sm := &ScheduleManager{logger: newTestLogger(t)}
	s := &schemas.Schedule{
		ID:      "sched_test",
		Deliver: schemas.DeliverTarget{},
	}
	sm.deliverResult(context.Background(), s, map[string]any{})
}

func TestDeliverResult_UnknownModeNoOp(t *testing.T) {
	sm := &ScheduleManager{logger: newTestLogger(t)}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode: "session_new",
		},
	}
	if err := sm.deliverResult(context.Background(), s, map[string]any{}); err == nil {
		t.Error("session_new must report not implemented rather than silently succeeding")
	}
}

type mockSessionDeliverer struct {
	called     bool
	sessionID  string
	taskID     string
	scheduleID string
	result     map[string]any
	err        error
}

func (m *mockSessionDeliverer) DeliverToSession(ctx context.Context, sessionID, taskID, scheduleID string, result map[string]any) error {
	m.called = true
	m.sessionID = sessionID
	m.taskID = taskID
	m.scheduleID = scheduleID
	m.result = result
	return m.err
}

func TestDeliverResult_SessionCurrent_CallsDeliverer(t *testing.T) {
	deliverer := &mockSessionDeliverer{}
	sm := &ScheduleManager{
		logger:           newTestLogger(t),
		sessionDeliverer: deliverer,
	}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode:      "session_current",
			SessionID: "sess_123",
		},
	}
	result := map[string]any{"task_id": "task_456", "status": "completed"}

	sm.deliverResult(context.Background(), s, result)

	if !deliverer.called {
		t.Fatal("DeliverToSession was not called")
	}
	if deliverer.sessionID != "sess_123" {
		t.Errorf("sessionID = %s, want sess_123", deliverer.sessionID)
	}
	if deliverer.taskID != "task_456" {
		t.Errorf("taskID = %s, want task_456", deliverer.taskID)
	}
	if deliverer.scheduleID != "sched_test" {
		t.Errorf("scheduleID = %s, want sched_test", deliverer.scheduleID)
	}
}

func TestDeliverResult_SessionCurrent_NoDelivererWired(t *testing.T) {
	sm := &ScheduleManager{logger: newTestLogger(t)}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode:      "session_current",
			SessionID: "sess_123",
		},
	}
	result := map[string]any{"task_id": "task_456", "status": "completed"}

	if err := sm.deliverResult(context.Background(), s, result); err == nil {
		t.Error("missing session deliverer must leave a durable fallback error")
	}
}

func TestDeliverResult_SessionCurrent_EmptyTaskID(t *testing.T) {
	deliverer := &mockSessionDeliverer{}
	sm := &ScheduleManager{
		logger:           newTestLogger(t),
		sessionDeliverer: deliverer,
	}
	s := &schemas.Schedule{
		ID: "sched_test",
		Deliver: schemas.DeliverTarget{
			Mode:      "session_current",
			SessionID: "sess_123",
		},
	}
	result := map[string]any{"status": "completed"}

	if err := sm.deliverResult(context.Background(), s, result); err == nil {
		t.Error("missing task_id must leave a durable fallback error")
	}

	if deliverer.called {
		t.Fatal("DeliverToSession should not be called when task_id is empty")
	}
}
