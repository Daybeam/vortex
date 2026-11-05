package core

// The graph persist fence (register O33, decision Q2-2, 2026-11-04).
//
// What it is for: two engines pointed at the same outputs/ directory both load
// the same manifest.json, both run a loop for it, and both persist — so the last
// writer silently wins and the other one keeps driving. Measured, not inferred:
// when the desktop engine restarted at 13:24 it adopted task_85b99bd4 and a
// pending decision's pd=1 survived the adoption (orphaned → running), which is
// only possible if it read the manifest the other process had been writing.
//
// operator chose the epoch/fence shape over an ownership lock, with the reasoning
// recorded verbatim in docs/plan/O_BATCH_REMEDIATION_2026-11-04.md: same dir and
// same DB means the data is already single-writer-by-overwrite, so what is needed
// is not arbitration over "who may adopt" but a way for the one that loses to
// stop. That also removes the Windows OpenProcess dependency entirely.
//
// Semantics, in one line each:
//   - Claiming is preemption, never refusal: a claimer writes its own token at
//     seq+1 and starts driving. The previous holder is not consulted.
//   - Writing requires holding: persistGraph and finalizeDecision's manifest
//     write re-read the fence first and skip every write when it is not theirs.
//   - The stamp point is the write, not the executor start: persistGraph claims
//     for its task only after a manifest actually landed (stampFence). See the
//     two measured reasons there — a stale token is not a live rival, and a
//     background write at start races whoever owns the directory.
//   - Losing means stopping, not just muting: detection cancels the loser's own
//     executor context, so the run loop returns and executeStep/Spawner.Spawn
//     (which take that ctx) abort in-flight work.
//
// The two limits the user asked to have written down, in the user's own framing:
//
//  1. TOCTOU. Re-reading the token and writing the manifest are two syscalls, so
//     a claim that lands between them still loses the race. This blocks *most*
//     conflicts — enough that a stale holder stops within one persist — but it is
//     not a strong guarantee. The stronger shape would be a compare-and-write
//     inside one SQLite transaction on the ledger row (store/task_backend_sqlite
//     .go:177 SaveTask is an unconditional upsert today, so that needs a backend
//     change, not a call-site change). Recorded as the known next step, not done.
//
//  2. Refusing writes alone would not be enough. A muted-but-running executor
//     still burns tokens and still fires external MCP calls, which are side
//     effects no fence can take back. That is why the loss path cancels.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// fenceFileName is a sibling of manifest.json, deliberately not a field of the
// graph: manifest.json is written by two different serializations (persistGraph
// marshals schemas.TaskGraph, scheduler_decision_finalize.go:121 marshals
// graph.ToStatusDict()), and they are not field-for-field equal — a fence riding
// inside the manifest would be dropped by one of them. It is also coordination
// state, not task state, so it should not appear in resume output or the API.
const fenceFileName = "fence.json"

// graphFence is the content of fence.json: who holds the right to persist this
// task, and which claim number that was.
type graphFence struct {
	Owner string `json:"owner"`
	Seq   int64  `json:"seq"`
}

// fenceOwnerToken names this process for the lifetime of the engine. pid alone
// is not enough: two engines in containers can share a pid, and a restart reuses
// one, so the boot timestamp keeps an old holder's token from matching a new
// process. The counter is what makes two engines built inside one process (which
// tests and any future in-process multi-workspace host do) distinguishable even
// when the clock does not tick between them. hostname is what makes the token
// legible in a log about a shared network mount.
var fenceTokenSeq atomic.Int64

func fenceOwnerToken() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknownhost"
	}
	return fmt.Sprintf("%s-%d-%d-%d", host, os.Getpid(), time.Now().UnixNano(), fenceTokenSeq.Add(1))
}

func (s *DirectedEngine) fencePath(taskID string) string {
	return filepath.Join(s.outputBase, taskID, fenceFileName)
}

// readGraphFence returns the on-disk token. A missing or unparseable file is
// reported as "absent", which callers treat as unclaimed: a task written before
// this code existed keeps behaving exactly as it did, rather than being frozen
// out by the fence that was supposed to fix double-writers.
func readGraphFence(path string) (graphFence, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return graphFence{}, false
	}
	var f graphFence
	if err := json.Unmarshal(data, &f); err != nil || f.Owner == "" {
		return graphFence{}, false
	}
	return f, true
}

// writeGraphFence publishes a claim. tmp+rename is the atomic step: on both NTFS
// (MoveFileEx with REPLACE_EXISTING) and POSIX rename is atomic within a
// directory, so a reader never sees a half-written token. The window this closes
// is write-tearing, not the claim race — see limit 1 above.
//
// The rename is retried because on Windows replacing a file that another engine
// created milliseconds earlier fails outright with a sharing violation — 实测
// `rename …\fence.json.tmp …\fence.json: The process cannot access the file
// because it is being used by another process`, from a test where two engines in
// one process stamped the same task. A held-open destination is exactly the
// transient state a claim must survive, and one short retry is enough in every
// run measured here.
func writeGraphFence(path string, f graphFence) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	var rerr error
	for attempt := 0; attempt < 3; attempt++ {
		if rerr = os.Rename(tmp, path); rerr == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return rerr
}

// hasFenceToken reports whether this engine may fence at all. An engine built
// without NewDirectedEngine has no owner token, and for it the fence stays off:
// production has exactly one construction site (core/scheduler.go:236), so the
// engines without a token are the several dozen test DirectedEngine literals that
// hand-build the struct. Making those self-cancel on a claim they cannot even
// name would be a regression the fence has no business causing — measured, not
// assumed: the first version of this code did exactly that, and
// TestApprovePlan_GoroutineTrackedAndContextDerived plus
// TestResumeSuspendedTask_SwarmModeRestartsWatchdog went red.
func (s *DirectedEngine) hasFenceToken() bool { return s.fenceOwner != "" }

// claimFence preempts the holder of taskID and records the token as ours. It
// re-reads after the rename to confirm we won; if someone claimed after us, we
// take their seq and try again, so two claimers converge on one holder instead
// of both believing they hold. That convergence is the whole of what this buys
// against limit 1 — it narrows the window, it does not close it.
//
// A claim that cannot be written still returns normally: on a backend where
// outputs/ is read-only the engine must keep doing what it did before this fence
// existed, and a persist that cannot write has bigger problems than double-writers.
//
// Two lost attempts means another process is actively contending for the same
// task, so the claim is recorded as lost: this executor stops rather than joins
// the fight with a second writer.
func (s *DirectedEngine) claimFence(taskID string) {
	if !s.hasFenceToken() {
		return // unfenced engine (see hasFenceToken)
	}
	path := s.fencePath(taskID)
	// A claim is the only thing that lifts a recorded loss: this engine is being
	// told to drive the task again, so the sticky refusal has served its purpose.
	s.fenceBroken.Delete(taskID)

	for attempt := 0; attempt < 2; attempt++ {
		mine := graphFence{Owner: s.fenceOwner, Seq: 1}
		if onDisk, present := readGraphFence(path); present {
			mine.Seq = onDisk.Seq + 1
		}
		if err := writeGraphFence(path, mine); err != nil {
			// A claim that never reached disk must not be believed locally either:
			// holdsFence would compare the phantom against the previous holder's
			// token, find a mismatch, and freeze this engine's own persists on a
			// backend where nothing was ever claimed. Forgetting the claim is what
			// "no fence here" has always meant.
			s.fences.Delete(taskID)
			return
		}
		after, _ := readGraphFence(path)
		s.fences.Store(taskID, after)
		if after.Owner == mine.Owner && after.Seq == mine.Seq {
			return // held
		}
		// Lost the claim race: loop, taking the winner's seq as our base.
	}
	s.recordFenceLoss(taskID)
}

// stampFence makes the on-disk token ours if it is not already, and is what
// persistGraph calls right after a successful manifest write. Two reasons it is
// the stamp point rather than executor start:
//
//  1. A token left on disk does not mean a live rival. After any crash or kill the
//     previous holder's fence.json is stale, and refusing to start on that evidence
//     would break the resume path this fence was added to protect.
//  2. 实测: a write at executor start is a write that races whoever built the
//     engine. TestDispatchGroup_AcceptsOwnerID failed 3 and 6 times out of 40 with
//     `TempDir RemoveAll cleanup: unlinkat …: The directory is not empty` once
//     startGraphExecutor claimed, and 0 of 80 with only that write suppressed — the
//     claim goroutine creates outputs/<taskID>/ after the test body has returned.
//     Stamping with the state the claim protects has no such window: persistGraph
//     already writes that directory synchronously.
func (s *DirectedEngine) stampFence(taskID string) {
	if !s.hasFenceToken() {
		return
	}
	if cur, ok := s.fences.Load(taskID); ok {
		if mine, isFence := cur.(graphFence); isFence {
			if onDisk, present := readGraphFence(s.fencePath(taskID)); present && onDisk == mine {
				return // already published and still ours
			}
		}
	}
	s.claimFence(taskID)
}

// holdsFence is the compare half. Unfenced tasks (no claim by this process — a
// single-step graph that never started an executor, or a test that put a graph
// in s.graphs directly) are allowed: the fence exists to stop a second executor,
// not to police every writer that predates it.
func (s *DirectedEngine) holdsFence(taskID string) bool {
	if _, broken := s.fenceBroken.Load(taskID); broken {
		return false // sticky until a fresh claim; see recordFenceLoss
	}
	cur, ok := s.fences.Load(taskID)
	if !ok {
		return true // no claim by this process, which also covers an untokened engine
	}
	onDisk, present := readGraphFence(s.fencePath(taskID))
	if !present {
		return true // deleted out from under us: nothing here says another engine took over
	}
	mine := cur.(graphFence)
	return onDisk.Owner == mine.Owner && onDisk.Seq == mine.Seq
}

// recordFenceLoss is the stop half. Marking the task broken makes the refusal
// survive a token that later flips back (a re-claim is the only way to clear it,
// which startGraphExecutor does when this engine re-adopts), and cancelling the
// executor's own context is what makes it a stop rather than a mute.
func (s *DirectedEngine) recordFenceLoss(taskID string) {
	if _, already := s.fenceBroken.LoadOrStore(taskID, struct{}{}); already {
		return
	}
	held := ""
	if v, ok := s.fences.Load(taskID); ok {
		mine := v.(graphFence)
		held = fmt.Sprintf("%s#%d", mine.Owner, mine.Seq)
	}
	onDisk, present := readGraphFence(s.fencePath(taskID))
	winner := ""
	if present {
		winner = fmt.Sprintf("%s#%d", onDisk.Owner, onDisk.Seq)
	}
	if s.logger != nil {
		s.logger.Log("GraphFenceLost", taskID, "", map[string]any{
			"held":     held,
			"on_disk":  winner,
			"action":   "persist refused and executor cancelled",
			"register": "O33/Q2-2",
		})
	}
	if c, ok := s.fenceCancels.Load(taskID); ok {
		if cancel, ok := c.(context.CancelFunc); ok {
			cancel()
		}
	}
}

// dropFence forgets every trace of a task's fence state. Called when this engine
// stops driving a task (Clear/evict), so a later adoption by another process is
// not shadowed by a stale sticky flag in this one.
func (s *DirectedEngine) dropFence(taskID string) {
	s.fences.Delete(taskID)
	s.fenceBroken.Delete(taskID)
	s.fenceCancels.Delete(taskID)
}
