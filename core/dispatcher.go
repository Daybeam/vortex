package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

func (s *DirectedEngine) dispatchNotifications(graph *schemas.TaskGraph, eventType EventType) {
	rec := NotificationRecord{
		TaskID:    graph.TaskID,
		Status:    string(graph.Status),
		Event:     string(eventType),
		Timestamp: time.Now(),
	}
	s.notifMu.Lock()
	s.notifications = append(s.notifications, rec)
	if len(s.notifications) > 100 {
		s.notifications = s.notifications[len(s.notifications)-100:]
	}
	s.notifMu.Unlock()

	cfg := s.registry.System.Notifications
	if !cfg.Enabled || len(cfg.Routes) == 0 {
		return
	}

	payload := map[string]any{
		"task_id":      graph.TaskID,
		"status":       graph.Status,
		"event":        eventType,
		"completed_at": time.Now().Unix(),
		"artifacts":    graph.GlobalWorkspace,
	}
	body, _ := json.Marshal(payload)

	for _, route := range cfg.Routes {
		if matchesTrigger(route, eventType, graph) {
			go sendWebhook(route.TargetURL, route.SecretEnv, body)
		}
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
			req.Header.Set("X-Webhook-Secret", secret)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
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
