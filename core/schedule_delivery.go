package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// Schedule delivery — Phase 1 (webhook) and Phase 2 (session_current) of
// SCHEDULE_DELIVERY_TARGETS_DESIGN.md.
//
// Phase 1: zero model tokens — the task result is POSTed directly to the
// configured webhook URL. Does not touch the session wake/follow-up chain.
//
// Phase 2: the task result is delivered to the chat session that created the
// schedule, via a wake turn that bypasses the session-level follow-up depth
// cap. Per-job quota is enforced by Schedule.MaxRuns, not maxFollowUpDepth.

// SessionDeliverer delivers a task result to a chat session. Implemented by
// pkg/api.ChatHandler. The ScheduleManager calls this when a schedule's
// Deliver.Mode is "session_current".
type SessionDeliverer interface {
	DeliverToSession(ctx context.Context, sessionID, taskID, scheduleID string, result map[string]any) error
}

type SessionDeliveryError struct {
	Kind string
}

func (e *SessionDeliveryError) Error() string { return "session delivery: " + e.Kind }

var (
	ErrSessionMissing     = &SessionDeliveryError{Kind: "session missing"}
	ErrSessionBusy        = &SessionDeliveryError{Kind: "session busy"}
	ErrSessionUnavailable = &SessionDeliveryError{Kind: "session delivery unavailable"}
)

// webhookDeliveryTimeout caps the HTTP POST to a webhook endpoint.
const (
	webhookDeliveryTimeout = 10 * time.Second
	webhookDNSTimeout      = 3 * time.Second
)

var webhookLookupIP = net.DefaultResolver.LookupIP

// deliverWebhook POSTs the task result to the schedule's webhook URL.
// Returns nil on HTTP 2xx, an error otherwise. SSRF-protected: only https
// URLs are accepted, and private network ranges are rejected.
func (sm *ScheduleManager) deliverWebhook(ctx context.Context, s *schemas.Schedule, result map[string]any) error {
	target := s.Deliver
	if target.WebhookURL == "" {
		return fmt.Errorf("webhook delivery: no URL configured for schedule %s", s.ID)
	}
	if err := validateWebhookURL(target.WebhookURL); err != nil {
		return fmt.Errorf("webhook delivery: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"schedule_id":   s.ID,
		"schedule_name": s.Name,
		"task_result":   result,
		"delivered_at":  time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("webhook delivery: marshal payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, webhookDeliveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook delivery: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := *sm.httpClient
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DialContext = validatedWebhookDialContext
		client.Transport = transport
	} else if transport, ok := client.Transport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.DialContext = validatedWebhookDialContext
		client.Transport = clone
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if err := validateWebhookURL(req.URL.String()); err != nil {
			return err
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook delivery: POST %s: %w", target.WebhookURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook delivery: %s returned HTTP %d", target.WebhookURL, resp.StatusCode)
	}

	sm.logger.Log("EventWebhookDelivered", "", "", map[string]any{
		"schedule_id": s.ID,
		"url":         target.WebhookURL,
		"status_code": resp.StatusCode,
	})
	return nil
}

// validateWebhookURL enforces SSRF protection: only https URLs, no private
// network ranges, no localhost. This prevents a malicious schedule from
// probing internal services.
func validateWebhookURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("only https URLs are allowed, got %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("URL credentials are not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return validateWebhookIP(ip)
	}
	_, err = resolveWebhookIPs(context.Background(), host)
	return err
}

func resolveWebhookIPs(ctx context.Context, host string) ([]net.IP, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, webhookDNSTimeout)
		defer cancel()
	}
	addrs, err := webhookLookupIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("hostname %s resolved to no addresses", host)
	}
	for _, ip := range addrs {
		if err := validateWebhookIP(ip); err != nil {
			return nil, fmt.Errorf("hostname %s: %w", host, err)
		}
	}
	return addrs, nil
}

func validateWebhookIP(ip net.IP) error {
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return fmt.Errorf("non-public IP not allowed: %s", ip)
	}
	return nil
}

func validatedWebhookDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := resolveWebhookIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ip := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// deliverResult dispatches the task result to the configured delivery target.
// Phase 1: webhook. Phase 2: session_current. Phase 3: session_new (pending).
func (sm *ScheduleManager) deliverResult(ctx context.Context, s *schemas.Schedule, result map[string]any) error {
	if s.Deliver.Mode == "" || s.Deliver.Mode == "inbox" {
		return nil
	}
	switch s.Deliver.Mode {
	case "webhook":
		return sm.deliverWebhook(ctx, s, result)
	case "session_current":
		taskID, _ := result["task_id"].(string)
		if taskID == "" {
			return sm.downgradeSessionDelivery(s, result, "task_id missing")
		}
		if sm.sessionDeliverer == nil {
			return sm.downgradeSessionDelivery(s, result, "session deliverer not wired")
		}
		if err := sm.sessionDeliverer.DeliverToSession(ctx, s.Deliver.SessionID, taskID, s.ID, result); err != nil {
			if errors.Is(err, ErrSessionBusy) {
				return err
			}
			if errors.Is(err, ErrSessionMissing) || errors.Is(err, ErrSessionUnavailable) {
				return sm.downgradeSessionDelivery(s, result, err.Error())
			}
			return err
		}
		return nil
	default:
		return fmt.Errorf("unsupported schedule delivery mode %q", s.Deliver.Mode)
	}
}

func (sm *ScheduleManager) downgradeSessionDelivery(s *schemas.Schedule, result map[string]any, reason string) error {
	target := s.Deliver
	if target.OnMissing == "" {
		target.OnMissing = "inbox"
	}
	s.Deliver = target
	sm.logger.Log("schedule_delivery_downgraded", "", "", map[string]any{
		"schedule_id": s.ID, "reason": reason, "on_missing": target.OnMissing,
	})
	delivery := scheduleDeliveryFor(s)
	if target.OnMissing == "webhook" && sm.deliveryOutbox == nil {
		fallback := *s
		fallback.Deliver = schemas.DeliverTarget{Mode: "webhook", WebhookURL: target.WebhookURL}
		return sm.deliverWebhook(context.Background(), &fallback, result)
	}
	return sm.queueFallback(delivery, stringValue(result["task_id"]), result, reason)
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}
