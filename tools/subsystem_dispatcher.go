package tools

import (
	"context"
	"sync"
)

// SubsystemDispatcher manages modular subsystems for orchestrator_invoke,
// replacing monolithic switch-case dispatching with clean namespace routing.
type SubsystemDispatcher struct {
	mu       sync.RWMutex
	handlers map[string]SubsystemHandler
}

var (
	defaultDispatcher = &SubsystemDispatcher{
		handlers: make(map[string]SubsystemHandler),
	}
)

// RegisterSubsystem registers a modular subsystem handler.
func RegisterSubsystem(h SubsystemHandler) {
	defaultDispatcher.mu.Lock()
	defer defaultDispatcher.mu.Unlock()
	defaultDispatcher.handlers[h.SubsystemName()] = h
}

// Dispatch routes an orchestrator_invoke request to the corresponding subsystem.
func Dispatch(ctx context.Context, app *App, subsystem, action string, args map[string]any) (any, error) {
	defaultDispatcher.mu.RLock()
	handler, ok := defaultDispatcher.handlers[subsystem]
	defaultDispatcher.mu.RUnlock()

	if !ok {
		// Fallback to legacy registry if subsystem not migrated yet (Zero-breaking-change)
		return legacyInvokeFallback(ctx, app, subsystem, action, args)
	}

	return handler.HandleAction(ctx, app, action, args)
}

// legacyInvokeFallback preserves compatibility with un-migrated subsystems.
// It delegates to the legacy SubsystemRegistry.Invoke which handles permission
// checks, timeout, logging, and usage tracking.
func legacyInvokeFallback(ctx context.Context, app *App, subsystem, action string, args map[string]any) (any, error) {
	return registry.Invoke(ctx, app, subsystem, action, args)
}
