package core

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/daybeam/vortex/schemas"
)

// scheduler_decision_finalize.go — Task finalization, artifact delivery, and reflection.
// Extracted from scheduler_decision.go on 2026-09-15.
// Contains: finalize, deliverArtifacts, reflect.

func (s *DirectedEngine) finalize(graph *schemas.TaskGraph) {
	// audit C-5/C-8: protect graph field writes + map iteration from concurrent
	// MutateGraphTopology (which adds steps under Mu.Lock) and persistGraph
	// (which marshals the graph under Mu.Lock).
	s.Mu.Lock()
	conf := graph.ComputeConfidence()
	graph.OverallConfidence = &conf // audit C-8
	now := time.Now()
	graph.CompletedAt = &now // audit C-8

	allOK := true
	for _, step := range graph.Steps { // audit C-5: iterate under Lock
		if step.Status == schemas.StepFailed || step.Status == schemas.StepBlocked {
			allOK = false
			break
		}
	}
	// Determine final status, then atomically set it via setGraphStatus
	// (prevents data race with run()'s reader).
	var finalStatus schemas.GraphStatus
	if !allOK {
		finalStatus = schemas.GraphFailed
	} else {
		allSkipped := true
		for _, step := range graph.Steps { // audit C-5
			if step.Status != schemas.StepSkipped {
				allSkipped = false
				break
			}
		}
		if allSkipped && len(graph.Steps) > 0 {
			finalStatus = schemas.GraphCompletedWithSkips
		} else {
			finalStatus = schemas.GraphCompleted
		}
	}
	s.Mu.Unlock()
	s.setGraphStatus(graph, finalStatus)

	s.persistGraph(graph)

	// Publish graph-level terminal event for consumers
	if allOK {
		s.publishEvent(graph.TaskID, "", EventTaskCompleted, map[string]any{"confidence": conf})
	} else {
		s.publishEvent(graph.TaskID, "", EventTaskFailed, map[string]any{"confidence": conf})
	}

	// Notify waiters
	s.broadcastDone(graph.TaskID)

	// Write manifest
	outDir := filepath.Join(s.outputBase, graph.TaskID)
	if err := os.MkdirAll(outDir, 0755); err != nil { // audit L-3.1: log MkdirAll failure
		log.Printf("WARN: finalizeDecision: failed to create output dir %s: %v", outDir, err)
	}
	manifest := graph.ToStatusDict()
	if data, err := json.Marshal(manifest); err == nil { // P-1.8: compact marshal on hot path
		if werr := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0644); werr != nil {
			log.Printf("WARN: finalizeDecision: failed to write manifest.json for task %s: %v", graph.TaskID, werr)
		}
	}

	event := EventTaskCompleted
	if graph.Status == schemas.GraphFailed {
		event = EventTaskFailed
	}
	s.logger.LogWithConfidence(event, graph.TaskID, "", conf, map[string]any{
		"output_files": len(graph.OutputFiles),
	})

	// Reflect — extract experience records
	s.goBackground(func() { s.reflect(graph) }) // audit H6: use goBackground so Stop() drains

	// Delivery (Step 6 of PROJECT_MAP.md)
	s.goBackground(func() { s.deliverArtifacts(graph) }) // audit H6: use goBackground so Stop() drains

	// Notification dispatch (webhook routing)
	s.goBackground(func() { s.dispatchNotifications(graph, event) }) // audit H6: use goBackground so Stop() drains
}

func (s *DirectedEngine) deliverArtifacts(graph *schemas.TaskGraph) {
	// audit P-MED-11: Snapshot artifacts + output files under Lock (O(a+o)),
	// then do the O(a×o) matching + logging + delivery simulation outside the
	// lock, then re-acquire Lock for the brief status write-back.
	// (audit L-1.3: Lock still protects snapshot consistency and write-back
	// against persistGraph and handleStepSuccess.)
	type artifactSnap struct {
		index     int
		path      string
		status    string
		ownerStep string
	}
	type outputSnap struct {
		path    string
		deliver string
	}

	s.Mu.Lock()
	arts := make([]artifactSnap, len(graph.Artifacts))
	for i := range graph.Artifacts {
		a := &graph.Artifacts[i]
		arts[i] = artifactSnap{i, a.Path, a.Status, a.OwnerStep}
	}
	ofs := make([]outputSnap, len(graph.OutputFiles))
	for i, of := range graph.OutputFiles {
		ofs[i] = outputSnap{of.Path, of.Deliver}
	}
	s.Mu.Unlock()

	// O(artifacts×outputs) matching + logging + delivery — outside lock
	type statusUpdate struct {
		index int
	}
	var updates []statusUpdate

	for _, a := range arts {
		if a.status == "published" {
			continue
		}

		var deliverHint string
		for _, of := range ofs {
			if of.path == a.path {
				deliverHint = of.deliver
				break
			}
		}

		if deliverHint == "" {
			continue
		}

		s.logger.Log("EventArtifactDeliveryStarted", graph.TaskID, a.ownerStep, map[string]any{
			"path":    a.path,
			"deliver": deliverHint,
		})

		success := false
		switch {
		case strings.HasPrefix(deliverHint, "webhook:"):
			s.logger.Log("EventArtifactDeliverySimulated", graph.TaskID, a.ownerStep, map[string]any{
				"path":    a.path,
				"channel": "webhook",
				"target":  strings.TrimPrefix(deliverHint, "webhook:"),
			})
			success = true
		case strings.HasPrefix(deliverHint, "s3:"):
			s.logger.Log("EventArtifactDeliverySimulated", graph.TaskID, a.ownerStep, map[string]any{
				"path":    a.path,
				"channel": "s3",
				"target":  strings.TrimPrefix(deliverHint, "s3:"),
			})
			success = true
		case deliverHint == "local":
			success = true
		default:
			s.logger.Log("EventArtifactDeliverySkipped", graph.TaskID, a.ownerStep, map[string]any{
				"path":   a.path,
				"reason": "unknown delivery channel: " + deliverHint,
			})
		}

		if success {
			updates = append(updates, statusUpdate{a.index})
			s.logger.Log("EventArtifactDelivered", graph.TaskID, a.ownerStep, map[string]any{
				"path":    a.path,
				"channel": deliverHint,
			})
		}
	}

	// Brief write-back under Lock
	s.Mu.Lock()
	for _, u := range updates {
		graph.Artifacts[u.index].Status = "published"
		graph.Artifacts[u.index].DeliveryStatus = "delivered"
	}
	s.Mu.Unlock()

	// Persist the updated delivery statuses
	s.persistGraph(graph)
}

func (s *DirectedEngine) reflect(graph *schemas.TaskGraph) {
	s.sieve.ClearHistory(graph.TaskID)
	if s.spawner != nil {
		s.spawner.ClearSieveHistory(graph.TaskID)
	}
	if s.reflection != nil {
		s.goBackground(func() { s.reflection.ReflectOnTask(s.lifecycleCtx, graph) })
	}
}
