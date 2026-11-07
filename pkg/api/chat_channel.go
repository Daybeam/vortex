package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/daybeam/vortex/core/obs"
	"github.com/daybeam/vortex/pkg/bootmetrics"
)

// ChatRequest is the transport-agnostic chat message request.
// HTTP, Telegram, and future input channels all produce this struct.
// User tier constants for Telegram auth (guest < authorized < admin).
const (
	TierGuest      = "guest"
	TierAuthorized = "authorized"
	TierAdmin      = "admin"
)

type ChatRequest struct {
	SessionID      string
	MessageID      string
	Content        string
	ParentID       string
	InputDir       string
	ModelOverride  string
	RoleID         string
	DirectTask     bool
	CallerID       string           // caller identity for task isolation (from tools.CallerIdentity)
	ReplyTo        string           // message ID being replied to (exact reference to old message)
	ReplyToContent string           // content of the referenced message (if already available, e.g. Telegram payload)
	Attachments    []AttachmentSpec // image/file attachments for VLM (relative paths under session workspace)
	UserTier       string           // authorization tier: guest | authorized | admin (default: guest)
}

// AttachmentSpec is a frontend-supplied attachment descriptor.
// Path is relative to the session workspace root (e.g. "uploads/seg1.png").
type AttachmentSpec struct {
	MimeType string `json:"mime_type"`
	Path     string `json:"path"`
	Label    string `json:"label"`
}

// ChatResponse is the immediate acknowledgment from ProcessMessage.
// The actual chat response flows asynchronously via session events
// (StreamHandler SSE or session.EventsAfter polling).
type ChatResponse struct {
	SessionID        string `json:"session_id,omitempty"`
	MessageID        string `json:"message_id,omitempty"`
	RoleID           string `json:"role_id,omitempty"`
	RoleName         string `json:"role_name,omitempty"`
	SlashCommand     bool   `json:"slash_command,omitempty"`
	AlreadyProcessed bool   `json:"already_processed,omitempty"`
	DirectTask       bool   `json:"direct_task,omitempty"`
	AutoRouted       bool   `json:"auto_routed,omitempty"`
	HookRouted       bool   `json:"hook_routed,omitempty"`
	Aborted          bool   `json:"aborted,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Error            string `json:"error,omitempty"`
	StatusCode       int    `json:"-"`
}

// ChatInputChannel is a pluggable chat entry point.
// HTTP is request-driven (Start registers routes); Telegram runs a
// background long-poll goroutine. Both route messages to
// ChatHandler.ProcessMessage and relay session events back to the user.
type ChatInputChannel interface {
	Name() string
	Start(ctx context.Context) error
	Stop()
}

// channelNoticeTimeout bounds the up/down notice so a slow or unreachable
// channel can never block startup or shutdown.
const channelNoticeTimeout = 5 * time.Second

// AdminNotifier is an optional capability: a channel that can push a message to
// its administrator(s) out-of-band (not in reply to a user message).
// Implemented by TelegramChannel (TELEGRAM_ADMIN_IDS). Keeping it an optional
// interface makes the orchestrator up/down notice channel-agnostic — a channel
// without an admin concept simply doesn't implement it, and adding a channel
// costs nothing here (this is the "not just Telegram" abstraction).
type AdminNotifier interface {
	NotifyAdmin(ctx context.Context, text string) error
}

// channelUpdownText renders the notice sent to a channel's administrator.
func channelUpdownText(channelName string, up bool) string {
	if up {
		return fmt.Sprintf("🟢 Orchestrator ONLINE — channel %q connected.", channelName)
	}
	return fmt.Sprintf("🔴 Orchestrator OFFLINE — shutting down (channel %q).", channelName)
}

// notifyChannelsAdmin pushes the up/down notice to every channel that can reach
// an administrator. Failures are logged, never returned: a notification must
// not break startup or shutdown.
func notifyChannelsAdmin(ctx context.Context, channels []ChatInputChannel, up bool) {
	for _, ch := range channels {
		an, ok := ch.(AdminNotifier)
		if !ok {
			continue
		}
		if err := an.NotifyAdmin(ctx, channelUpdownText(ch.Name(), up)); err != nil {
			slog.Warn("channel admin notice failed", "channel", ch.Name(), "up", up, "error", err)
		}
	}
}

// RegisterInputChannels wires all enabled chat input channels and returns
// them for lifecycle management. Channels are enabled by environment
// variables (Telegram: TELEGRAM_BOT_TOKEN).
func RegisterInputChannels(h *ChatHandler, mux *http.ServeMux, ctx context.Context) []ChatInputChannel {
	var channels []ChatInputChannel

	// HTTP channel — always enabled, registers REST routes.
	httpCh := NewHTTPChannel(h, mux)
	_ = httpCh.Start(ctx)
	channels = append(channels, httpCh)

	// Telegram channel — opt-in via TELEGRAM_BOT_TOKEN. Start probes the Bot
	// API (getMe with backoff retries), so it runs off the listener path: an
	// unreachable Telegram endpoint must not delay the console. The channel is
	// registered up front — Stop() is safe before Start() completes.
	if token := getEnv("TELEGRAM_BOT_TOKEN"); token != "" {
		tgCh := NewTelegramChannel(h, token)
		channels = append(channels, tgCh)
		go func() {
			if err := tgCh.Start(ctx); err != nil {
				// Log but don't fail — HTTP chat still works.
				// FIX (2026-10-27): surface to BOTH slog (daily stderr/log sink,
				// visible to operators watching logs/YYYY-MM-DD/_system.jsonl)
				// AND the structured logger (global_trajectory.jsonl). Previously
				// only the trajectory got the event, so the daily log showed no
				// sign the bot was down.
				slog.Error("telegram channel failed to start — bot is DOWN, HTTP chat still works", "error", err)
				if h.App != nil && h.App.Logger != nil {
					h.App.Logger.Log(obs.EventStepFailed, "telegram", "", map[string]any{"error": err.Error()})
				}
				return
			}
			slog.Info("telegram channel started — polling active", "bot", "telegram")
			if h.App != nil && h.App.Logger != nil {
				h.App.Logger.Log("EventChannelStarted", "telegram", "", map[string]any{"status": "ok"})
			}
		}()
	}

	// Orchestrator up/down notice (2026-10-23): the orchestrator has connected,
	// so tell whoever administers each channel. Best-effort, never fatal — and
	// kept off the startup path: a 5s-per-channel timeout used to delay the
	// listener whenever a channel endpoint was slow.
	go func() {
		noticeCtx, noticeCancel := context.WithTimeout(ctx, channelNoticeTimeout)
		defer noticeCancel()
		notifyChannelsAdmin(noticeCtx, channels, true)
	}()

	bootmetrics.Mark("channels")
	return channels
}

// StopAll stops all channels in reverse order.
func StopAll(channels []ChatInputChannel) {
	// Offline notice FIRST — the transports must still be alive to deliver it.
	ctx, cancel := context.WithTimeout(context.Background(), channelNoticeTimeout)
	notifyChannelsAdmin(ctx, channels, false)
	cancel()

	for i := len(channels) - 1; i >= 0; i-- {
		channels[i].Stop()
	}
}

func getEnv(key string) string { return os.Getenv(key) }
