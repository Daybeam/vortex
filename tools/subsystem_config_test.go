package tools

import (
	"context"
	"testing"
)

func TestConfigSubsystem_SubsystemName(t *testing.T) {
	s := &ConfigSubsystem{}
	if name := s.SubsystemName(); name != "config" {
		t.Errorf("expected SubsystemName 'config', got %q", name)
	}
}

func TestConfigSubsystem_HandleAction_DelegatesToRegistry(t *testing.T) {
	cs := &ConfigSubsystem{}

	_, err := cs.HandleAction(context.Background(), &App{}, "nonexistent_action_xyz", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent action, got nil")
	}
}

func TestConfigSubsystem_HandleAction_RegisteredInDispatcher(t *testing.T) {
	defaultDispatcher.mu.RLock()
	_, ok := defaultDispatcher.handlers["config"]
	defaultDispatcher.mu.RUnlock()
	if !ok {
		t.Fatal("ConfigSubsystem not registered in defaultDispatcher (init() should have registered it)")
	}
}
