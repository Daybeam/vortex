package core

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

// audit P-10: shared transport for webhook dispatch — reuses TCP connections
// instead of creating a new transport (TLS handshake) per webhook call.
var webhookTransport = &http.Transport{
	MaxIdleConnsPerHost: 100,
	IdleConnTimeout:     90 * time.Second,
}

// audit fix C: bound concurrent webhook goroutines to prevent goroutine storms
// under burst event load. Each sendWebhook has a 10s timeout, so without a
// bound a notification burst with N routes spawns N concurrent HTTP goroutines
// with no backpressure. The semaphore caps in-flight dispatches.
var webhookSem = make(chan struct{}, 16) // max 16 concurrent webhook dispatches

func (s *DirectedEngine) dispatchNotifications(graph *schemas.TaskGraph, eventType EventType) {
	// audit L-1.4: snapshot graph.Status + GlobalWorkspace and determine
	// matching routes under RLock to prevent concurrent map iteration race
	// with MutateGraphTopology and field reads racing with handleStepSuccess.
	s.Mu.RLock()
	rec := NotificationRecord{
		TaskID:    graph.TaskID,
		Status:    string(graph.Status),
		Event:     string(eventType),
		Timestamp: time.Now(),
	}
	statusSnap := graph.Status
	globalWSSnap := graph.GlobalWorkspace

	cfg := s.registry.System.Notifications
	matchingRoutes := make([]config.NotificationRoute, 0)
	if cfg.Enabled {
		for _, route := range cfg.Routes {
			if matchesTrigger(route, eventType, graph) {
				matchingRoutes = append(matchingRoutes, route)
			}
		}
	}
	s.Mu.RUnlock()

	s.notifMu.Lock()
	s.notifications = append(s.notifications, rec)
	if len(s.notifications) > 100 {
		s.notifications = s.notifications[len(s.notifications)-100:]
	}
	s.notifMu.Unlock()

	if !cfg.Enabled || len(matchingRoutes) == 0 {
		return
	}

	payload := map[string]any{
		"task_id":      graph.TaskID,
		"status":       statusSnap,
		"event":        eventType,
		"completed_at": time.Now().Unix(),
		"artifacts":    globalWSSnap,
	}
	body, _ := json.Marshal(payload)

	for _, route := range matchingRoutes {
		go func(route config.NotificationRoute) {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[webhook] panic for %s: %v\n", route.TargetURL, r)
				}
			}()
			// audit fix C: acquire semaphore slot to bound concurrency. If all
			// slots are taken, this goroutine blocks until one frees — providing
			// backpressure instead of unbounded goroutine spawn.
			webhookSem <- struct{}{}
			defer func() { <-webhookSem }()
			sendWebhook(route.TargetURL, route.SecretEnv, body)
		}(route)
	}
}

func matchesTrigger(route config.NotificationRoute, eventType EventType, graph *schemas.TaskGraph) bool {
	matched := false
	for _, t := range route.TriggerOn {
		if t == string(eventType) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	if route.TaskType == "" || route.TaskType == "*" {
		return true
	}
	for _, step := range graph.Steps {
		if step != nil && step.RoleID == route.TaskType {
			return true
		}
	}
	return false
}

func sendWebhook(targetURL, secretEnv string, body []byte) {
	req, err := http.NewRequest("POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")

	if secretEnv != "" {
		if secret := os.Getenv(secretEnv); secret != "" {
			// HMAC-SHA256 webhook signature (Stripe-style signed payload).
			// Receiver verifies: hex(HMAC-SHA256(secret, ts + "." + body)) == v1.
			// This avoids transmitting the raw shared secret over the wire.
			ts := time.Now().Unix()
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(strconv.FormatInt(ts, 10)))
			mac.Write([]byte("."))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Webhook-Signature", "t="+strconv.FormatInt(ts, 10)+",v1="+sig)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: webhookTransport}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[webhook] dispatch failed for %s: %v\n", targetURL, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "[webhook] %s returned status %d\n", targetURL, resp.StatusCode)
	}
}

func (s *DirectedEngine) GetNotifications() []NotificationRecord {
	s.notifMu.RLock()
	defer s.notifMu.RUnlock()
	out := make([]NotificationRecord, len(s.notifications))
	copy(out, s.notifications)
	return out
}

func (s *DirectedEngine) MarkNotificationsRead() {
	s.notifMu.Lock()
	defer s.notifMu.Unlock()
	for i := range s.notifications {
		s.notifications[i].Read = true
	}
}
