package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/daybeam/vortex/config"
)

func TestLocalWorkspaceStore_WriteReadDelete(t *testing.T) {
	base := t.TempDir()
	store := NewLocalWorkspaceStore(base)

	data := []byte("hello world")
	if err := store.WriteFile("task-1", "output.txt", data); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	got, err := store.ReadFile("task-1", "output.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("expected %q, got %q", data, got)
	}

	files, err := store.ListFiles("task-1")
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if len(files) != 1 || files[0] != "output.txt" {
		t.Errorf("expected [output.txt], got %v", files)
	}

	if err := store.DeleteFile("task-1", "output.txt"); err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}

	if _, err := store.ReadFile("task-1", "output.txt"); !os.IsNotExist(err) {
		t.Errorf("expected NotExist after delete, got %v", err)
	}
}

func TestLocalWorkspaceStore_NestedPath(t *testing.T) {
	base := t.TempDir()
	store := NewLocalWorkspaceStore(base)

	if err := store.WriteFile("task-1", "sub/dir/file.txt", []byte("nested")); err != nil {
		t.Fatalf("WriteFile with nested path failed: %v", err)
	}
	got, err := store.ReadFile("task-1", "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("expected 'nested', got %q", got)
	}
}

func TestLocalWorkspaceStore_ListFiles_EmptyDir(t *testing.T) {
	base := t.TempDir()
	store := NewLocalWorkspaceStore(base)

	if err := os.MkdirAll(filepath.Join(base, "task-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := store.ListFiles("task-1")
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected empty list, got %v", files)
	}
}

func TestLocalEventPublisher_PublishSubscribe(t *testing.T) {
	logDir := mkdirTemp(t)
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	bus := NewEventBus()
	pub := NewLocalEventPublisher(logger, bus)

	ch, err := pub.Subscribe("test_topic")
	if err != nil {
		t.Fatalf("Subscribe failed: %v", err)
	}

	err = pub.Publish("test_topic", LogEvent{
		Event:  EventType("test_topic"),
		TaskID: "task-1",
		StepID: "step-1",
		Detail: map[string]any{"key": "value"},
	})
	if err != nil {
		t.Fatalf("Publish failed: %v", err)
	}

	select {
	case evt := <-ch:
		if evt.TaskID != "task-1" {
			t.Errorf("expected task-1, got %s", evt.TaskID)
		}
	default:
		t.Errorf("expected to receive event on channel")
	}
}

func TestLocalEventPublisher_NilBus(t *testing.T) {
	logDir := mkdirTemp(t)
	logger, _ := NewLogger(logDir, &config.SystemSettings{})
	pub := NewLocalEventPublisher(logger, nil)

	err := pub.Publish("topic", LogEvent{
		Event:  EventType("topic"),
		TaskID: "task-1",
	})
	if err != nil {
		t.Fatalf("Publish with nil bus should not fail: %v", err)
	}
}

func TestLocalPathLocker_ReadWriteLock(t *testing.T) {
	mgr := NewPathLockManager()
	locker := NewLocalPathLocker(mgr)

	ctx := context.Background()

	rd1, err := locker.AcquireReadLock(ctx, "file.txt")
	if err != nil {
		t.Fatalf("AcquireReadLock failed: %v", err)
	}
	rd2, err := locker.AcquireReadLock(ctx, "file.txt")
	if err != nil {
		t.Fatalf("second AcquireReadLock failed: %v", err)
	}
	_ = rd1.Close()
	_ = rd2.Close()

	wr, err := locker.AcquireWriteLock(ctx, "file.txt")
	if err != nil {
		t.Fatalf("AcquireWriteLock failed: %v", err)
	}
	_ = wr.Close()
}

func TestLocalPathLocker_CancelledContext(t *testing.T) {
	mgr := NewPathLockManager()
	locker := NewLocalPathLocker(mgr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := locker.AcquireReadLock(ctx, "file.txt")
	if err == nil {
		t.Errorf("expected error from cancelled context")
	}
	_, err = locker.AcquireWriteLock(ctx, "file.txt")
	if err == nil {
		t.Errorf("expected error from cancelled context")
	}
}

func TestExtensionInterfaces_CompileTimeChecks(t *testing.T) {
	var _ WorkspaceStore = (*LocalWorkspaceStore)(nil)
	var _ EventPublisher = (*LocalEventPublisher)(nil)
	var _ PathLocker = (*LocalPathLocker)(nil)
}
