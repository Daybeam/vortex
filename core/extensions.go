package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ─── Plug-and-Play Extension Points ─────────────────────────────────────────
// Design ref: docs/architecture/PLUG_AND_PLAY_EXTENSION_POINTS.md
//
// Three abstractions that future-proof the core for distributed/cloud evolution.
// Current implementations are local (filesystem, in-memory, sync.RWMutex).
// Future implementations can swap in S3, NATS, Etcd without touching callers.

// ─── 1. WorkspaceStore ──────────────────────────────────────────────────────

// WorkspaceStore abstracts task-scoped file storage.
// Local impl: LocalWorkspaceStore (os file ops under a base dir).
// Future impl: S3ObjectStore, MinIOStore, etc.
type WorkspaceStore interface {
	ReadFile(taskID, relPath string) ([]byte, error)
	WriteFile(taskID, relPath string, data []byte) error
	DeleteFile(taskID, relPath string) error
	ListFiles(taskID string) ([]string, error)
}

// LocalWorkspaceStore implements WorkspaceStore via the local filesystem.
// Files are stored under baseDir/<taskID>/<relPath>.
type LocalWorkspaceStore struct {
	baseDir string
}

func NewLocalWorkspaceStore(baseDir string) *LocalWorkspaceStore {
	return &LocalWorkspaceStore{baseDir: baseDir}
}

func (s *LocalWorkspaceStore) ReadFile(taskID, relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(s.baseDir, taskID, relPath))
}

func (s *LocalWorkspaceStore) WriteFile(taskID, relPath string, data []byte) error {
	full := filepath.Join(s.baseDir, taskID, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

func (s *LocalWorkspaceStore) DeleteFile(taskID, relPath string) error {
	return os.Remove(filepath.Join(s.baseDir, taskID, relPath))
}

func (s *LocalWorkspaceStore) ListFiles(taskID string) ([]string, error) {
	dir := filepath.Join(s.baseDir, taskID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

// ─── 2. EventPublisher ──────────────────────────────────────────────────────

// EventPublisher abstracts the event bus for state/telemetry broadcast.
// Local impl: LocalEventPublisher (wraps Logger).
// Future impl: NATSStreamPublisher, RedisStreamPublisher, etc.
type EventPublisher interface {
	Publish(topic string, event LogEvent) error
	Subscribe(topic string) (<-chan LogEvent, error)
}

// LocalEventPublisher implements EventPublisher via the in-memory Logger + EventBus.
type LocalEventPublisher struct {
	logger *Logger
	bus    *EventBus
}

func NewLocalEventPublisher(logger *Logger, bus *EventBus) *LocalEventPublisher {
	return &LocalEventPublisher{logger: logger, bus: bus}
}

func (p *LocalEventPublisher) Publish(topic string, event LogEvent) error {
	if p.logger != nil {
		p.logger.Log(event.Event, event.TaskID, event.StepID, event.Detail)
	}
	if p.bus != nil {
		p.bus.Publish(AgentEvent{
			EventType: topic,
			TaskID:    event.TaskID,
			StepID:    event.StepID,
			Payload:   event.Detail,
		})
	}
	return nil
}

func (p *LocalEventPublisher) Subscribe(topic string) (<-chan LogEvent, error) {
	ch := make(chan LogEvent, 64)
	handler := func(e AgentEvent) error {
		if topic != "*" && e.EventType != topic {
			return nil
		}
		select {
		case ch <- LogEvent{
			Event:  EventType(e.EventType),
			TaskID: e.TaskID,
			StepID: e.StepID,
			Detail: e.Payload,
		}:
		default:
		}
		return nil
	}
	if p.bus != nil {
		p.bus.Subscribe(topic, handler)
	}
	return ch, nil
}

// ─── 3. PathLocker ──────────────────────────────────────────────────────────

// PathLocker abstracts path-level concurrency control.
// Local impl: LocalPathLocker (wraps PathLockManager with sync.RWMutex).
// Future impl: EtcdDistributedLocker, RedisRedLock, etc.
type PathLocker interface {
	AcquireReadLock(ctx context.Context, path string) (io.Closer, error)
	AcquireWriteLock(ctx context.Context, path string) (io.Closer, error)
}

// LocalPathLocker implements PathLocker via the in-memory PathLockManager.
type LocalPathLocker struct {
	mgr *PathLockManager
}

func NewLocalPathLocker(mgr *PathLockManager) *LocalPathLocker {
	return &LocalPathLocker{mgr: mgr}
}

// lockCloser wraps an unlock func as an io.Closer.
type lockCloser struct {
	unlock func()
}

func (l *lockCloser) Close() error {
	l.unlock()
	return nil
}

func (l *LocalPathLocker) AcquireReadLock(ctx context.Context, path string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock := l.mgr.Acquire(path, false)
	return &lockCloser{unlock: unlock}, nil
}

func (l *LocalPathLocker) AcquireWriteLock(ctx context.Context, path string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unlock := l.mgr.Acquire(path, true)
	return &lockCloser{unlock: unlock}, nil
}

// Compile-time interface checks.
var _ WorkspaceStore = (*LocalWorkspaceStore)(nil)
var _ EventPublisher = (*LocalEventPublisher)(nil)
var _ PathLocker = (*LocalPathLocker)(nil)

// formatPath ensures consistent path joining for workspace operations.
func formatPath(base, taskID, relPath string) string {
	return filepath.Join(base, taskID, relPath)
}

// Ensure fmt is used (avoid unused import if interface checks evolve).
var _ = fmt.Sprintf
