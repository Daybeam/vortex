package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

func TestRegisterFastPathSubsystem(t *testing.T) {
	r := NewSubsystemRegistry()
	app := &App{}
	RegisterFastPathSubsystem(r, app)

	s, ok := r.Subsystems["fastpath"]
	if !ok {
		t.Fatal("expected 'fastpath' subsystem to be registered")
	}
	if s.Name != "fastpath" {
		t.Errorf("Name = %q, want %q", s.Name, "fastpath")
	}
	if s.Description == "" {
		t.Error("expected non-empty Description")
	}
	a, ok := s.Actions["execute_contract"]
	if !ok {
		t.Fatal("expected 'execute_contract' action to be registered")
	}
	if a.Name != "execute_contract" {
		t.Errorf("Action Name = %q, want %q", a.Name, "execute_contract")
	}
	if a.Handler == nil {
		t.Error("expected non-nil Handler")
	}
	if len(a.Parameters) == 0 {
		t.Error("expected non-empty Parameters")
	}
}

func TestRegisterFastPathSubsystem_ExecuteContractMissingPath(t *testing.T) {
	r := NewSubsystemRegistry()
	app := &App{}
	RegisterFastPathSubsystem(r, app)

	s := r.Subsystems["fastpath"]
	_, err := s.Actions["execute_contract"].Handler(context.Background(), app, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("error should mention path, got: %v", err)
	}
}

func TestRegisterEditionSubsystem(t *testing.T) {
	app := &App{}
	mock := &mockEditionSubsystem{name: "test-sub"}
	app.RegisterEditionSubsystem(mock)
	if len(app.editionSubsystems) != 1 {
		t.Fatalf("expected 1 subsystem, got %d", len(app.editionSubsystems))
	}
	if app.editionSubsystems[0].Name() != "test-sub" {
		t.Errorf("Name = %q, want %q", app.editionSubsystems[0].Name(), "test-sub")
	}
}

func TestInitEditionSubsystems_Success(t *testing.T) {
	app := &App{}
	mock := &mockEditionSubsystem{name: "ok-sub", initErr: nil}
	app.RegisterEditionSubsystem(mock)
	if err := app.InitEditionSubsystems(); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !mock.initCalled {
		t.Error("expected Init to be called")
	}
}

func TestInitEditionSubsystems_Error(t *testing.T) {
	app := &App{}
	mock := &mockEditionSubsystem{name: "fail-sub", initErr: errors.New("init failed")}
	app.RegisterEditionSubsystem(mock)
	err := app.InitEditionSubsystems()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "fail-sub") {
		t.Errorf("error should contain subsystem name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "init failed") {
		t.Errorf("error should contain init error, got: %v", err)
	}
}

func TestInitEditionSubsystems_StopsOnError(t *testing.T) {
	app := &App{}
	failMock := &mockEditionSubsystem{name: "fail", initErr: errors.New("boom")}
	okMock := &mockEditionSubsystem{name: "ok"}
	app.RegisterEditionSubsystem(failMock)
	app.RegisterEditionSubsystem(okMock)
	_ = app.InitEditionSubsystems()
	if okMock.initCalled {
		t.Error("expected second subsystem Init to NOT be called after first fails")
	}
}

func TestSetupSubsystems(t *testing.T) {
	srv := server.NewMCPServer("test", "1.0.0")
	app := &App{}
	err := app.SetupSubsystems(srv)
	if err != nil {
		t.Fatalf("SetupSubsystems failed: %v", err)
	}
	if _, ok := registry.Subsystems["fastpath"]; !ok {
		t.Error("expected 'fastpath' to be registered in global registry")
	}
	if app.Promoter == nil {
		t.Error("expected Promoter to be initialized by promoterSubsystem.Init")
	}
}

func TestDiscoverActions_AdminTier(t *testing.T) {
	r := NewSubsystemRegistry()
	r.Register(&Subsystem{
		Name:        "test",
		Description: "Test subsystem",
		Actions: map[string]Action{
			"action1": {Name: "action1", Description: "First action"},
			"action2": {Name: "action2", Description: "Second action"},
		},
	})
	out, err := r.discoverActions("test", TierAdmin)
	if err != nil {
		t.Fatalf("discoverActions failed: %v", err)
	}
	if !strings.Contains(out, "test") {
		t.Error("expected output to contain subsystem name")
	}
	if !strings.Contains(out, "action1") {
		t.Error("expected output to contain action1")
	}
	if !strings.Contains(out, "action2") {
		t.Error("expected output to contain action2")
	}
}

func TestDiscoverActions_PublicTierFilters(t *testing.T) {
	r := NewSubsystemRegistry()
	r.Register(&Subsystem{
		Name:        "config",
		Description: "Config subsystem",
		Actions: map[string]Action{
			"get_config":  {Name: "get_config", Description: "Get config"},
			"set_config":  {Name: "set_config", Description: "Set config"},
			"list_roles":  {Name: "list_roles", Description: "List roles"},
		},
	})
	out, err := r.discoverActions("config", TierPublic)
	if err != nil {
		t.Fatalf("discoverActions failed: %v", err)
	}
	if !strings.Contains(out, "get_config") {
		t.Error("expected output to contain whitelisted action 'get_config'")
	}
	if !strings.Contains(out, "list_roles") {
		t.Error("expected output to contain whitelisted action 'list_roles'")
	}
	if strings.Contains(out, "set_config") {
		t.Error("expected output to NOT contain non-whitelisted action 'set_config'")
	}
}

func TestDiscoverActions_NotFound(t *testing.T) {
	r := NewSubsystemRegistry()
	_, err := r.discoverActions("nonexistent", TierAdmin)
	if err == nil {
		t.Fatal("expected error for nonexistent subsystem")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should contain subsystem name, got: %v", err)
	}
}

type mockEditionSubsystem struct {
	name       string
	initErr    error
	initCalled bool
}

func (m *mockEditionSubsystem) Name() string { return m.name }
func (m *mockEditionSubsystem) Init(app *App) error {
	m.initCalled = true
	return m.initErr
}
