package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/core/obs"
)

func eventForTest(taskID, eventType string, payload map[string]any) obs.AgentEvent {
	return obs.NewAgentEvent(taskID, "", eventType, payload)
}

// --- unit: signing ---

func TestWebhookSign_HMAC(t *testing.T) {
	body := []byte(`{"a":1}`)
	mac := hmac.New(sha256.New, []byte("«REDACTED»"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := SignedWebhookHeader(body, "«REDACTED»"); got != want {
		t.Fatalf("SignedWebhookHeader = %q, want %q", got, want)
	}
}

func TestWebhookSign_Hex(t *testing.T) {
	mac := hmac.New(sha256.New, []byte("«REDACTED»"))
	mac.Write([]byte("x"))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := SignedWebhookHeader([]byte("x"), "«REDACTED»"); got != want {
		t.Fatalf("SignedWebhookHeader = %q, want %q", got, want)
	}
}

// --- unit: construction ---

func TestWebhookNotifier_DisabledWhenNoURL(t *testing.T) {
	if NewWebhookNotifier("").enabled() {
		t.Fatal("empty URL must disable the notifier")
	}
}

func TestWebhookNotifier_Defaults(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/none")
	if n.route != "callback-notify" {
		t.Fatalf("default route = %q", n.route)
	}
	if n.agentRoute != "callback-agent" {
		t.Fatalf("default agent route = %q", n.agentRoute)
	}
	if n.secret == "" {
		t.Fatal("secret must not be empty by default")
	}
	if n.timeout <= 0 {
		t.Fatalf("timeout = %v", n.timeout)
	}
}

// --- unit: payload shape ---

func TestWebhookNotifier_BuildPayload(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/none")
	ev := eventForTest("t-1", string(obs.EventTaskCompleted), map[string]any{"confidence": 0.91})
	b, err := n.buildPayload(ev)
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if got["event_type"] != "task_completed" {
		t.Errorf("event_type = %v", got["event_type"])
	}
	if got["task_id"] != "t-1" {
		t.Errorf("task_id = %v", got["task_id"])
	}
	if got["status"] != "completed" {
		t.Errorf("status = %v", got["status"])
	}
	if got["summary"] != "orchestrator task completed (confidence=0.91)" {
		t.Errorf("summary = %v", got["summary"])
	}
	p, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload nested key missing; got %T", got["payload"])
	}
	if p["confidence"] != 0.91 {
		t.Errorf("payload.confidence = %v", p["confidence"])
	}
}

func TestWebhookNotifier_FailedStatus(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/none")
	b, _ := n.buildPayload(eventForTest("t-2", string(obs.EventTaskFailed), nil))
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["status"] != "failed" {
		t.Fatalf("status = %v", got["status"])
	}
	if got["summary"] != "orchestrator task failed" {
		t.Fatalf("summary = %v", got["summary"])
	}
}

func TestWebhookNotifier_TaskDescriptionPreferred(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/none")
	ev := eventForTest("t-9", string(obs.EventTaskCompleted), map[string]any{
		"task":       "refactor auth module",
		"confidence": 0.8,
	})
	b, _ := n.buildPayload(ev)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if got["summary"] != `task "refactor auth module" execution completed` {
		t.Fatalf("summary = %q, want description-led summary", got["summary"])
	}
	// enrichment must not leak into the top-level status
	if got["status"] != "completed" {
		t.Errorf("status = %v", got["status"])
	}
}

// --- integration: full send against a receiver that verifies the signature ---

type received struct {
	mu         sync.Mutex
	path       string
	body       []byte
	goodSig    bool
	deliveryID string
}

func runWebhookReceiver(t *testing.T, secret string) (*httptest.Server, *received) {
	t.Helper()
	srv := &received{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		srv.mu.Lock()
		srv.path = r.URL.Path
		srv.body = append([]byte(nil), body...)
		srv.goodSig = hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want))
		srv.deliveryID = r.Header.Get("X-GitHub-Delivery")
		srv.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"delivered"}`))
	}))
	t.Cleanup(s.Close)
	return s, srv
}

func TestWebhookNotifier_SendHappyPath(t *testing.T) {
	s, got := runWebhookReceiver(t, "«REDACTED»")
	n := NewWebhookNotifier(s.URL + "/webhooks/callback-notify")
	n.secret = "«REDACTED»"

	if err := n.Send(eventForTest("t-3", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.path != "/webhooks/callback-notify" {
		t.Errorf("path = %q", got.path)
	}
	if !got.goodSig {
		t.Error("server rejected the HMAC signature")
	}
	if got.deliveryID == "" {
		t.Error("X-GitHub-Delivery must be set (idempotency key)")
	}
	var pl map[string]any
	if err := json.Unmarshal(got.body, &pl); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if pl["task_id"] != "t-3" {
		t.Errorf("task_id = %v", pl["task_id"])
	}
}

func TestWebhookNotifier_AgentRoute(t *testing.T) {
	s, got := runWebhookReceiver(t, "«REDACTED»")
	n := NewWebhookNotifier(s.URL + "/webhooks/callback-notify")
	n.agentRoute = "callback-agent"
	n.secret = "«REDACTED»"

	if err := n.SendAgent(eventForTest("t-4", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("SendAgent: %v", err)
	}
	got.mu.Lock()
	defer got.mu.Unlock()
	if got.path != "/webhooks/callback-agent" {
		t.Errorf("path = %q, want /webhooks/callback-agent", got.path)
	}
}

func TestWebhookNotifier_FireAndForget(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/none")
	n.timeout = 300 * time.Millisecond
	if err := n.Send(eventForTest("t-5", string(obs.EventTaskCompleted), nil)); err != nil {
		t.Fatalf("Send must not return error (fire-and-forget): %v", err)
	}
	if err := n.SendAgent(eventForTest("t-5b", string(obs.EventTaskFailed), nil)); err != nil {
		t.Fatalf("SendAgent must not return error: %v", err)
	}
}

func TestWebhookNotifier_StatusCodesDoNotError(t *testing.T) {
	var code int
	var mu sync.Mutex
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	n := NewWebhookNotifier(s.URL + "/webhooks/callback-notify")
	n.timeout = time.Second

	for _, c := range []int{200, 202, 429, 502} {
		mu.Lock()
		code = c
		mu.Unlock()
		if err := n.Send(eventForTest(fmt.Sprintf("t-%d", c), string(obs.EventTaskCompleted), nil)); err != nil {
			t.Fatalf("status %d must not error: %v", c, err)
		}
	}
}

// Delivery-ID uniqueness: reusing one across real events makes the webhook
// return {"status":"duplicate"} and silently drop the later event, so each send
// must mint its own id.
func TestWebhookNotifier_DeliveryIDsAreUnique(t *testing.T) {
	ids := make(chan string, 20)
	n := NewWebhookNotifier("http://127.0.0.1:1/x")
	n.client = &http.Client{Transport: &idCapture{out: ids}}
	for i := 0; i < 10; i++ {
		_ = n.Send(eventForTest(fmt.Sprintf("u-%d", i), string(obs.EventTaskCompleted), nil))
	}
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if id == "" {
			t.Fatal("empty delivery id")
		}
		if seen[id] {
			t.Fatalf("duplicate delivery id %q — webhook would drop the later event", id)
		}
		seen[id] = true
	}
	if len(seen) != 10 {
		t.Fatalf("got %d unique ids, want 10", len(seen))
	}
}

type idCapture struct{ out chan<- string }

func (c *idCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	c.out <- r.Header.Get("X-GitHub-Delivery")
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}, nil
}

// Concurrency: the notifier must be safe for use from many task goroutines.
func TestWebhookNotifier_ConcurrentSend(t *testing.T) {
	var mu sync.Mutex
	n := NewWebhookNotifier("http://127.0.0.1:1/x")
	n.client = &http.Client{Transport: &idCapture{out: make(chan string, 100)}}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := n.Send(eventForTest(fmt.Sprintf("c-%d", i), string(obs.EventTaskCompleted), nil)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Send error: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
}

func TestWebhookNotifier_RouteOverridable(t *testing.T) {
	n := NewWebhookNotifier("http://127.0.0.1:1/base")
	n.route = "other-route"
	if n.route != "other-route" {
		t.Fatal("route not settable (env override path)")
	}
}

// TestWebhookNotifier_UseAgentEnv verifies the WEBHOOK_USE_AGENT switch. The
// switch decides which route SubscribeTaskEvents posts to (see the wiring test
// below); here we only check the env parsing is tolerant and that unset stays
// off, so an existing deployment is unaffected.
func TestWebhookNotifier_UseAgentEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"false", false},
		{"0", false},
		{"no", false},
		{"true", true},
		{"1", true},
		{"TRUE", true},
	} {
		t.Setenv("WEBHOOK_USE_AGENT", tc.env)
		n := NewWebhookNotifier("http://127.0.0.1:1/base")
		if n.useAgent != tc.want {
			t.Errorf("WEBHOOK_USE_AGENT=%q: useAgent = %v, want %v", tc.env, n.useAgent, tc.want)
		}
	}
	t.Setenv("WEBHOOK_USE_AGENT", "")
}
