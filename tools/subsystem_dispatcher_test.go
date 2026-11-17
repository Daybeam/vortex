package tools

import (
	"context"
	"errors"
	"testing"
)

type mockSubsystemHandler struct {
	name      string
	actionErr error
	result    any
	called    bool
}

func (m *mockSubsystemHandler) SubsystemName() string { return m.name }

func (m *mockSubsystemHandler) HandleAction(ctx context.Context, app *App, action string, args map[string]any) (any, error) {
	m.called = true
	if m.actionErr != nil {
		return nil, m.actionErr
	}
	return m.result, nil
}

func TestDispatch_RegisteredHandler(t *testing.T) {
	handler := &mockSubsystemHandler{name: "test_dispatch", result: map[string]any{"ok": true}}
	defaultDispatcher.mu.Lock()
	defaultDispatcher.handlers["test_dispatch"] = handler
	defaultDispatcher.mu.Unlock()
	t.Cleanup(func() {
		defaultDispatcher.mu.Lock()
		delete(defaultDispatcher.handlers, "test_dispatch")
		defaultDispatcher.mu.Unlock()
	})

	result, err := Dispatch(context.Background(), &App{}, "test_dispatch", "some_action", nil)
	if err != nil {
		t.Fatalf("Dispatch returned error: %v", err)
	}
	if !handler.called {
		t.Fatal("handler was not called")
	}
	if m, ok := result.(map[string]any); !ok || !m["ok"].(bool) {
		t.Errorf("expected result {ok: true}, got %v", result)
	}
}

func TestDispatch_RegisteredHandlerReturnsError(t *testing.T) {
	expectedErr := errors.New("handler failure")
	handler := &mockSubsystemHandler{name: "test_dispatch_err", actionErr: expectedErr}
	defaultDispatcher.mu.Lock()
	defaultDispatcher.handlers["test_dispatch_err"] = handler
	defaultDispatcher.mu.Unlock()
	t.Cleanup(func() {
		defaultDispatcher.mu.Lock()
		delete(defaultDispatcher.handlers, "test_dispatch_err")
		defaultDispatcher.mu.Unlock()
	})

	_, err := Dispatch(context.Background(), &App{}, "test_dispatch_err", "some_action", nil)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected error %v, got %v", expectedErr, err)
	}
}

func TestDispatch_UnregisteredFallback(t *testing.T) {
	_, err := Dispatch(context.Background(), &App{}, "nonexistent_subsystem_xyz", "some_action", nil)
	if err == nil {
		t.Fatal("expected error for unregistered subsystem, got nil")
	}
}

func TestRegisterSubsystem(t *testing.T) {
	handler := &mockSubsystemHandler{name: "test_register", result: "registered"}
	RegisterSubsystem(handler)
	t.Cleanup(func() {
		defaultDispatcher.mu.Lock()
		delete(defaultDispatcher.handlers, "test_register")
		defaultDispatcher.mu.Unlock()
	})

	defaultDispatcher.mu.RLock()
	h, ok := defaultDispatcher.handlers["test_register"]
	defaultDispatcher.mu.RUnlock()
	if !ok {
		t.Fatal("handler not found in dispatcher after RegisterSubsystem")
	}
	if h.SubsystemName() != "test_register" {
		t.Errorf("expected SubsystemName 'test_register', got %q", h.SubsystemName())
	}
}
