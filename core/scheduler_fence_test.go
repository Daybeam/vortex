package core

// Regression / behavior lock for O33, decided as Q2-2 on 2026-11-04: two engines
// pointed at the same outputs/ directory both adopt the same manifest, both run a
// loop, and both persist — so the last writer wins and the other keeps driving.
// Measured in B2-10's restart: the desktop engine adopted task_85b99bd4 and the
// pending decision's pd=1 survived, which only happens if it read the manifest the
// other process had been writing.
//
// operator's decision, verbatim: "Q2-2 的 A′（epoch/fence）能做，但要写清两点。
// 重读令牌再写 manifest 之间有 TOCTOU 窗口，所以它只能挡掉大部分冲突，不是强保证。
// ledger 行若能放进同一个 SQLite 事务里做比较再写，会更稳。" and
// "'输的一方退出执行器'要真的停掉 goroutine，只拒写不够，否则它会继续烧 token 和
// 触发外部 MCP 调用。"
//
// So the tests below pin three things: claiming preempts, the loser writes nothing
// (manifest, artifacts row AND ledger), and losing cancels the executor's context.
// The TOCTOU window is deliberately NOT tested — it is documented in
// core/scheduler_fence.go as the known limit, along with the SQLite compare-and-
// write that would close it.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// fenceSaveRegistry records ledger writes. The embedded nil interface makes any
// other registry method panic rather than return zeros, so a test that accidentally
// exercises a different path fails loudly.
type fenceSaveRegistry struct {
	store.TaskRegistry
	mu    sync.Mutex
	saves []string
}

func (r *fenceSaveRegistry) SaveTask(_ context.Context, taskID, _ string, _ []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saves = append(r.saves, taskID)
	return nil
}

func (r *fenceSaveRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.saves)
}

// newFenceEngine builds an engine whose outputBase is outDir, so two of them can
// be pointed at one shared workspace — the exact configuration O33 is about.
func newFenceEngine(t *testing.T, outDir string) *DirectedEngine {
	t.Helper()
	reg := &config.Registry{
		Roles:     make(map[string]*config.Role),
		Providers: make(map[string]*config.ProviderConfig),
		System:    config.SystemSettings{},
	}
	logger := mustNewLogger(t, t.TempDir(), &config.SystemSettings{})
	// Close the logger before t.TempDir() cleanup, or its async log-compression
	// goroutine races the directory removal (same hazard as newAbortTestEngine).
	t.Cleanup(func() { logger.Close() })
	s := NewDirectedEngine(reg, nil, nil, nil, logger, nil, outDir, outDir, nil)
	t.Cleanup(func() { s.Stop() })
	return s
}

func fenceRunningGraph(taskID string) *schemas.TaskGraph {
	return &schemas.TaskGraph{
		TaskID: taskID,
		Status: schemas.GraphRunning,
		Steps:  map[string]*schemas.Step{},
	}
}

// clearDebounce removes persistGraph's 200ms H-5 skip for one task, so a refused
// write is provably the fence and not the debounce.
func clearDebounce(s *DirectedEngine, taskID string) {
	s.lastPersistAt.Delete(taskID)
}

func TestFence_ClaimPreemptsAndOnlyTheLastClaimerHolds(t *testing.T) {
	out := t.TempDir()
	a := newFenceEngine(t, out)
	b := newFenceEngine(t, out)

	const taskID = "task_fence_preempt"
	a.claimFence(taskID)
	if !a.holdsFence(taskID) {
		t.Fatal("A should hold its own fresh claim")
	}
	if a.fenceOwner == b.fenceOwner {
		t.Fatalf("two engines share the owner token %q — a fence that cannot tell them apart is not a fence", a.fenceOwner)
	}

	b.claimFence(taskID) // adoption by the second engine is preemption, not refusal

	if b.holdsFence(taskID) == false {
		t.Error("B's claim must be the one that holds")
	}
	if a.holdsFence(taskID) {
		t.Error("A still believes it holds after B preempted it")
	}
	// The seq is the epoch: monotonic across claims, so a stale holder can never
	// match by accident.
	if v, ok := b.fences.Load(taskID); !ok || v.(graphFence).Seq < 2 {
		t.Errorf("B's claim seq = %+v, want >= 2", v)
	}
}

func TestFence_LoserWritesNeitherManifestNorLedger(t *testing.T) {
	out := t.TempDir()
	a := newFenceEngine(t, out)
	b := newFenceEngine(t, out)
	const taskID = "task_fence_refuse"

	graph := fenceRunningGraph(taskID)
	a.RegisterGraphForTest(graph)
	a.claimFence(taskID)

	// A persists normally first, so "nothing changed" below cannot be blamed on a
	// path that never worked.
	reg := &fenceSaveRegistry{}
	a.SetTaskRegistry(reg)
	clearDebounce(a, taskID)
	a.persistGraph(graph)
	if reg.count() != 1 {
		t.Fatalf("setup: A's first persist should write the ledger once, got %d", reg.count())
	}
	aManifest := filepath.Join(out, taskID, "manifest.json")
	writtenByA, err := os.ReadFile(aManifest)
	if err != nil {
		t.Fatalf("setup: A's first persist should write manifest.json: %v", err)
	}

	// The other engine adopts the same directory and drives the task somewhere A
	// has never seen: a step A's in-memory copy does not have, so a clobber by A is
	// a byte difference rather than a no-op.
	b.claimFence(taskID)
	holdersCopy := fenceRunningGraph(taskID)
	holdersCopy.Steps["s1"] = &schemas.Step{ID: "s1", Status: schemas.StepOK, Task: "written by the holder"}
	b.RegisterGraphForTest(holdersCopy)
	clearDebounce(b, taskID)
	b.persistGraph(holdersCopy)
	writtenByB, err := os.ReadFile(aManifest)
	if err != nil {
		t.Fatalf("read manifest after the holder persisted: %v", err)
	}
	if string(writtenByB) == string(writtenByA) {
		t.Fatal("setup: the two engines' manifests are byte-identical, so the clobber check proves nothing")
	}

	reg.mu.Lock()
	reg.saves = nil
	reg.mu.Unlock()

	clearDebounce(a, taskID)
	a.persistGraph(graph)

	if n := reg.count(); n != 0 {
		t.Errorf("loser wrote the ledger %d times after being preempted; want 0 (O33: the ledger row belongs to the holder)", n)
	}
	after, err := os.ReadFile(aManifest)
	if err != nil {
		t.Fatalf("manifest.json vanished: %v", err)
	}
	if string(after) != string(writtenByB) {
		t.Errorf("loser overwrote the holder's manifest.json (%d → %d bytes); want the holder's bytes kept", len(writtenByB), len(after))
	}
	if !b.holdsFence(taskID) {
		t.Error("B lost its own claim")
	}
}

func TestFence_LosingCancelsTheExecutorContext(t *testing.T) {
	out := t.TempDir()
	s := newFenceEngine(t, out)
	const taskID = "task_fence_stop"

	graph := fenceRunningGraph(taskID)
	s.RegisterGraphForTest(graph)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// startGraphExecutor stores the executor's own cancel here; injected directly
	// so this test does not also have to run a real step.
	s.fenceCancels.Store(taskID, cancel)

	s.claimFence(taskID)
	select {
	case <-ctx.Done():
		t.Fatal("executor cancelled while still holding the fence")
	default:
	}

	// Another holder wins the disk, and the next persist is where that is noticed.
	other, _ := json.Marshal(graphFence{Owner: "other-engine#pid-0", Seq: 99})
	if err := os.WriteFile(s.fencePath(taskID), other, 0o644); err != nil {
		t.Fatalf("write fence: %v", err)
	}
	clearDebounce(s, taskID)
	s.persistGraph(graph)

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("persist refused but the executor kept running — refusing writes alone " +
			"leaves it burning tokens and firing external MCP calls (Q2-2, second point)")
	}

	// Sticky: a refusal must not lapse back into permission without a fresh claim.
	s.claimFence(taskID)
	if !s.holdsFence(taskID) {
		t.Error("a fresh claim should re-establish holding")
	}
}

// TestFence_StartExecutorWritesNoFence pins the stamp point by its absence. Every
// site that starts driving a task goes through startGraphExecutor (8 call sites,
// audit L-N9), and none of them may write a fence: claiming there was measured to
// race the caller's directory — TestDispatchGroup_AcceptsOwnerID failed 3 and 6 of
// 40 runs with `TempDir RemoveAll cleanup: … The directory is not empty` while the
// claim ran inside the executor goroutine, and 0 of 80 once it did not.
func TestFence_StartExecutorWritesNoFence(t *testing.T) {
	out := t.TempDir()
	s := newFenceEngine(t, out)
	const taskID = "task_fence_start"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startGraphExecutor(ctx, taskID)
	time.Sleep(150 * time.Millisecond) // enough for the goroutine to have run

	if _, present := readGraphFence(s.fencePath(taskID)); present {
		t.Error("startGraphExecutor stamped a fence — the claim belongs to the first persist")
	}
	if _, err := os.Stat(filepath.Join(out, taskID)); err == nil {
		t.Error("startGraphExecutor created outputs/<taskID> just to publish a token")
	}
	if _, ok := s.fences.Load(taskID); ok {
		t.Error("startGraphExecutor recorded a local claim it never wrote")
	}
}

// TestFence_FirstPersistStampsTheClaim is the other half: the write that lands a
// manifest also lands the token, so a later engine driving the same directory has
// something to preempt, and the process that wrote first loses cleanly.
func TestFence_FirstPersistStampsTheClaim(t *testing.T) {
	out := t.TempDir()
	s := newFenceEngine(t, out)
	const taskID = "task_fence_stamp"

	graph := fenceRunningGraph(taskID)
	s.RegisterGraphForTest(graph)
	clearDebounce(s, taskID)
	s.persistGraph(graph)

	f, present := readGraphFence(s.fencePath(taskID))
	if !present {
		t.Fatal("the first persist did not stamp a claim — a later adopter would find no token to preempt")
	}
	if f.Owner != s.fenceOwner {
		t.Errorf("stamped claim owner = %q, want this engine %q", f.Owner, s.fenceOwner)
	}
	if _, err := os.Stat(filepath.Join(out, taskID, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing while a fence was stamped: %v", err)
	}

	// A stamp while already holding is a no-op, not a seq war.
	before := f
	clearDebounce(s, taskID)
	s.persistGraph(graph)
	if after, _ := readGraphFence(s.fencePath(taskID)); after != before {
		t.Errorf("repeat persist changed the holder's own seq (%+v → %+v); one claim per task, not per persist", before, after)
	}

	// And the stamped token is the thing an adopter takes over.
	b := newFenceEngine(t, out)
	b.claimFence(taskID)
	if s.holdsFence(taskID) {
		t.Error("the first writer still believes it holds after an adoption claimed the same task")
	}
	if !b.holdsFence(taskID) {
		t.Error("the adopter did not take the stamped fence")
	}
}

// TestFence_UnclaimedTaskIsNotPoliced pins the backward-compat half: a graph that
// never went through a claim keeps persisting exactly as before, so the fence can
// only ever stop a second executor, not freeze every existing writer out.
func TestFence_UnclaimedTaskIsNotPoliced(t *testing.T) {
	out := t.TempDir()
	s := newFenceEngine(t, out)
	const taskID = "task_fence_legacy"

	graph := fenceRunningGraph(taskID)
	s.RegisterGraphForTest(graph)
	reg := &fenceSaveRegistry{}
	s.SetTaskRegistry(reg)

	clearDebounce(s, taskID)
	s.persistGraph(graph)

	if !s.holdsFence(taskID) {
		t.Error("an unclaimed task must count as held (no fence file = no competing engine)")
	}
	if reg.count() != 1 {
		t.Errorf("unclaimed task persisted %d times, want 1 — the fence changed legacy behaviour", reg.count())
	}
	if _, err := os.Stat(filepath.Join(out, taskID, "manifest.json")); err != nil {
		t.Errorf("unclaimed task's manifest write failed: %v", err)
	}
}

func TestFence_DropFenceClearsTheStickyRefusal(t *testing.T) {
	out := t.TempDir()
	s := newFenceEngine(t, out)
	const taskID = "task_fence_drop"

	s.claimFence(taskID)
	other, _ := json.Marshal(graphFence{Owner: "other#2", Seq: 7})
	if err := os.WriteFile(s.fencePath(taskID), other, 0o644); err != nil {
		t.Fatalf("write fence: %v", err)
	}
	s.recordFenceLoss(taskID)
	if s.holdsFence(taskID) {
		t.Fatal("a recorded loss must hold the refusal")
	}

	s.dropFence(taskID)
	if !s.holdsFence(taskID) {
		t.Error("Clear/evict must forget the fence state, or a later adoption by another process is shadowed by a stale flag here")
	}
}
