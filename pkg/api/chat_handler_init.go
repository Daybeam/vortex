package api

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/core/sieve"
	"github.com/daybeam/vortex/pkg/chat"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
)

func NewChatHandler(app *tools.App) *ChatHandler {
	var chatBackend store.IChatBackend
	var outbox store.IChatOutbox
	if app.DB != nil {
		chatBackend = store.NewSQLiteChatBackend(app.DB)
		outbox = store.NewSQLiteChatOutbox(app.DB)
	}
	sessionDir := ""
	if app.RootDir != "" {
		sessionDir = filepath.Join(app.RootDir, "outputs", "chat")
	}
	chatStore := chat.NewChatSessionStore(sessionDir, chatBackend)

	chatMemWindowSize := 10
	if app.Registry != nil && app.Registry.System.ChatMemoryWindowSize > 0 {
		chatMemWindowSize = app.Registry.System.ChatMemoryWindowSize
	}
	chatMem := chat.NewRollingWindowMemory(chatMemWindowSize)
	if app.Registry != nil && app.Registry.System.ChatMemoryMaxTokens > 0 {
		chatMem.MaxTokens = app.Registry.System.ChatMemoryMaxTokens
	}
	// Wire MemoryBank for settled-preference reads (replaces synchronous
	// FactCache extraction). Spec: docs/completed/2026-11-27/USER_PROFILE_INCREMENTAL_INDEXER.md §5.2
	chatMem.MemoryBank = mbStoreForHarness(app)

	chatMaxTurns := chat.DefaultMaxTurns
	if app.Registry != nil {
		if t := app.Registry.System.MaxToolTurns; t > 0 {
			chatMaxTurns = t
		}
		if t := app.Registry.System.ChatMaxTurns; t > 0 {
			chatMaxTurns = t
		}
	}
	chatMaxOutputTokens := 2048
	if app.Registry != nil && app.Registry.System.ChatMaxOutputTokens > 0 {
		chatMaxOutputTokens = app.Registry.System.ChatMaxOutputTokens
	}
	wakeReserve := chat.DefaultWakeReserve
	wakeReserveRatio := chat.DefaultWakeReserveRatio
	if app.Registry != nil {
		if v := app.Registry.System.WakeReserve; v > 0 {
			wakeReserve = v
		}
		if v := app.Registry.System.WakeReserveRatio; v > 0 {
			wakeReserveRatio = v
		}
	}
	chatSessionRoot := os.Getenv("HARNESSBENCH_WORKSPACE")

	_ = wakeContractMode()

	harness := &chat.ChatHarness{
		Archive:           app.Archive,
		MemoryBank:        mbStoreForHarness(app),
		Embed:             app.EmbedClient,
		OutputBase:        "outputs",
		MaxTurns:          chatMaxTurns,
		WakeReserve:       wakeReserve,
		WakeReserveRatio:  wakeReserveRatio,
		MaxOutputTokens:   chatMaxOutputTokens,
		SessionRoot:       chatSessionRoot,
		Sieve:             sieve.NewSieve(100),
		Memory:            chatMem,
		ReadinessGateSoft: app.Registry.System.ReadinessGateSoft,
		SkillQuickPath: func(query string) *chat.QuickPathMatch {
			if app.ExpStore == nil || query == "" {
				return nil
			}
			qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer qcancel()
			ids := app.ExpStore.QueryRelevantSkills(qctx, query, 1)
			if len(ids) == 0 {
				return nil
			}
			gs, ok := app.ExpStore.GetGeneratedSkill(ids[0])
			if !ok || !gs.IsVerified {
				return nil
			}
			return &chat.QuickPathMatch{
				SkillID:   gs.ID,
				ToolName:  gs.ID,
				Signature: gs.Signature,
			}
		},
		DelegateStatus: func(taskID string, wait time.Duration) (map[string]any, bool) {
			if app.Scheduler == nil {
				return nil, false
			}
			if wait > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), wait+2*time.Second)
				defer cancel()
				return app.Scheduler.WaitTask(ctx, taskID, wait, "full")
			}
			return app.Scheduler.GetStatus(taskID, "full")
		},
	}

	if pc := app.Registry.ResolveProviderConfig(&config.Role{}); pc != nil {
		if p, err := providers.Get(pc, app.Registry.ExternalRuntimes); err == nil {
			harness.Provider = p
			harness.Model = pc.Model
			harness.MaxContextWindow = pc.MaxContextWindow
		}
	}

	handler := &ChatHandler{
		App:            app,
		Harness:        harness,
		Store:          chatStore,
		Outbox:         outbox,
		hookRunner:     core.NewHookRunner(app.Registry.System.Hooks, app.Registry.ExternalRuntimes, app.Logger),
		delegWatchStop: make(chan struct{}),
	}

	// Wire UserProfileIndexer for async preference extraction.
	// Spec: docs/completed/2026-11-27/USER_PROFILE_INCREMENTAL_INDEXER.md §2
	if mbStore := mbStoreForHarness(app); mbStore != nil {
		threshold := app.Registry.System.UserProfileIndexerThreshold
		interval := parseUserProfileIndexerInterval(app.Registry.System.UserProfileIndexerInterval)
		handler.profileIndexer = chat.NewUserProfileIndexer(mbStore, threshold, interval)
		go handler.profileIndexer.Start(app.LifecycleCtx)
	}

	handler.Harness.Delegate = func(prompt string, maxTurnsCap int) (string, error) {
		isAdmin := tools.EffectiveTier(context.Background(), app) == tools.TierAdmin
		return handler.submitChatTask(
			[]schemas.StepInput{{ID: "s1", Task: prompt, SubmitterIsAdmin: isAdmin, MaxTurnsCap: maxTurnsCap}}, "")
	}

	handler.WireTaskCompletionCallbacks()
	return handler
}

// parseUserProfileIndexerInterval parses the config string (e.g. "1h", "30m")
// into a time.Duration. Empty or invalid → default 1h.
func parseUserProfileIndexerInterval(s string) time.Duration {
	if s == "" {
		return chat.DefaultUserProfileIndexerInterval
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return chat.DefaultUserProfileIndexerInterval
	}
	return d
}
