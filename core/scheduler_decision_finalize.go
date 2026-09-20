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
	conf := graph.ComputeConfidence()
	graph.OverallConfidence = &conf
	now := time.Now()
	graph.CompletedAt = &now

	allOK := true
	for _, step := range graph.Steps {
		if step.Status == schemas.StepFailed || step.Status == schemas.StepBlocked {
			allOK = false
			break
		}
	}
	if !allOK {
		graph.Status = schemas.GraphFailed
	} else {
		allSkipped := true
		for _, step := range graph.Steps {
			if step.Status != schemas.StepSkipped {
				allSkipped = false
				break
			}
		}
		if allSkipped && len(graph.Steps) > 0 {
			graph.Status = schemas.GraphCompletedWithSkips
		} else {
			graph.Status = schemas.GraphCompleted
		}
	}

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
	_ = os.MkdirAll(outDir, 0755)
	if !graph.IsSmartRouted {
		manifest := graph.ToStatusDict()
		if data, err := json.MarshalIndent(manifest, "", "  "); err == nil {
			if werr := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0644); werr != nil {
				log.Printf("WARN: finalizeDecision: failed to write manifest.json for task %s: %v", graph.TaskID, werr)
			}
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
	for i := range graph.Artifacts {
		art := &graph.Artifacts[i]
		if art.Status == "published" {
			continue
		}

		// Find the corresponding OutputFile to get Deliver hint
		var deliverHint string
		for _, of := range graph.OutputFiles {
			if of.Path == art.Path {
				deliverHint = of.Deliver
				break
			}
		}

		if deliverHint == "" {
			continue
		}

		s.logger.Log("EventArtifactDeliveryStarted", graph.TaskID, art.OwnerStep, map[string]any{
			"path":    art.Path,
			"deliver": deliverHint,
		})

		// Implement actual delivery logic (e.g., upload to S3, send webhook, etc.)
		// For now, we simulate success for registered delivery channels.
		success := false
		switch {
		case strings.HasPrefix(deliverHint, "webhook:"):
			// Delivery not yet implemented — log simulated success.
			s.logger.Log("EventArtifactDeliverySimulated", graph.TaskID, art.OwnerStep, map[string]any{
				"path":    art.Path,
				"channel": "webhook",
				"target":  strings.TrimPrefix(deliverHint, "webhook:"),
			})
			success = true
		case strings.HasPrefix(deliverHint, "s3:"):
			// Delivery not yet implemented — log simulated success.
			s.logger.Log("EventArtifactDeliverySimulated", graph.TaskID, art.OwnerStep, map[string]any{
				"path":    art.Path,
				"channel": "s3",
				"target":  strings.TrimPrefix(deliverHint, "s3:"),
			})
			success = true
		case deliverHint == "local":
			success = true
		default:
			s.logger.Log("EventArtifactDeliverySkipped", graph.TaskID, art.OwnerStep, map[string]any{
				"path":   art.Path,
				"reason": "unknown delivery channel: " + deliverHint,
			})
		}

		if success {
			art.Status = "published"
			art.DeliveryStatus = "delivered"
			s.logger.Log("EventArtifactDelivered", graph.TaskID, art.OwnerStep, map[string]any{
				"path":    art.Path,
				"channel": deliverHint,
			})
		}
	}

	// Persist the updated delivery statuses
	s.persistGraph(graph)
}

func (s *DirectedEngine) reflect(graph *schemas.TaskGraph) {
	s.sieve.ClearHistory(graph.TaskID)
	s.spawner.ClearSieveHistory(graph.TaskID)
	if s.reflection != nil {
		s.goBackground(func() { s.reflection.ReflectOnTask(s.lifecycleCtx, graph) })
	}
}
