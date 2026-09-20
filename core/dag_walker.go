package core

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
	"github.com/daybeam/vortex/store"
)

// DAGGraphWalker is the pure graph-theory component of the DirectedEngine.
// It owns no I/O and no Spawner references — only graph topology queries,
// dependency-edge analysis, and registry lookups.
//
// Extracted from DirectedEngine per docs/completed/GOD_CLASS_REFACTORING_MASTER_PLAN.md
// Phase 2. Holds references to shared state owned by DirectedEngine (the graph
// map and mutex are shared, not copied).
type DAGGraphWalker struct {
	graphs     map[string]*schemas.TaskGraph
	mu         *sync.RWMutex
	outputBase string
	logger     *Logger
	registry   *config.Registry
}

// NewDAGGraphWalker creates a walker that shares the given graph map and mutex
// with its owning DirectedEngine.
func NewDAGGraphWalker(graphs map[string]*schemas.TaskGraph, mu *sync.RWMutex, outputBase string, logger *Logger, registry *config.Registry) *DAGGraphWalker {
	return &DAGGraphWalker{
		graphs:     graphs,
		mu:         mu,
		outputBase: outputBase,
		logger:     logger,
		registry:   registry,
	}
}

// GetAllReadySteps collects all ready steps across all running graphs.
func (w *DAGGraphWalker) GetAllReadySteps() []ReadyStepPair {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var pairs []ReadyStepPair
	for _, graph := range w.graphs {
		if graph.Status != schemas.GraphRunning {
			continue
		}
		ready := graph.ReadySteps()
		for _, step := range ready {
			pairs = append(pairs, ReadyStepPair{Graph: graph, Step: step})
		}
	}
	return pairs
}

// DetectReadDeps scans a step's tool-call trace for file reads and matches
// them to producing steps' output files, building ReadDep edges.
func (w *DAGGraphWalker) DetectReadDeps(graph *schemas.TaskGraph, stepID string, trace []store.ToolInteraction, snap *SnapshotIndex) []schemas.ReadDep {
	var deps []schemas.ReadDep
	if len(trace) == 0 {
		return deps
	}
	outputFileMap := make(map[string]string)
	for _, of := range graph.OutputFiles {
		if of.Path != "" && of.StepID != stepID {
			outputFileMap[of.Path] = of.StepID
		}
	}

	buildDep := func(prodStep, path, evidence string) schemas.ReadDep {
		dep := schemas.ReadDep{
			StepID:   prodStep,
			FilePath: path,
			Evidence: evidence,
		}
		if snap != nil {
			dep.SnapshotID = snap.ID
			rel, err := filepath.Rel(filepath.Join(w.outputBase, graph.TaskID), path)
			if err == nil {
				rel = filepath.ToSlash(rel)
				if meta, ok := snap.Files[rel]; ok {
					dep.SourceSHA256 = meta.SHA256
				}
			}
		}
		return dep
	}

	for _, ti := range trace {
		switch ti.ToolName {
		case "read_file":
			if path, ok := ti.Arguments["path"].(string); ok && path != "" {
				if prodStep, exists := outputFileMap[path]; exists {
					deps = append(deps, buildDep(prodStep, path, fmt.Sprintf("tool:%s arg:path", ti.ToolName)))
				}
			}
		case "web_extract":
		case "read_file_lines":
			if path, ok := ti.Arguments["path"].(string); ok && path != "" {
				if prodStep, exists := outputFileMap[path]; exists {
					deps = append(deps, buildDep(prodStep, path, fmt.Sprintf("tool:%s arg:path", ti.ToolName)))
				}
			}
		default:
			for key, val := range ti.Arguments {
				if strVal, ok := val.(string); ok && strVal != "" {
					if strings.Contains(strVal, "/") || strings.Contains(strVal, "\\") ||
						strings.HasSuffix(strVal, ".json") || strings.HasSuffix(strVal, ".md") ||
						strings.HasSuffix(strVal, ".txt") || strings.HasSuffix(strVal, ".csv") ||
						strings.HasSuffix(strVal, ".log") || strings.HasSuffix(strVal, ".yaml") ||
						strings.HasSuffix(strVal, ".yml") || strings.HasSuffix(strVal, ".go") {
						if prodStep, exists := outputFileMap[strVal]; exists {
							deps = append(deps, buildDep(prodStep, strVal, fmt.Sprintf("tool:%s arg:%s", ti.ToolName, key)))
						}
					}
				}
			}
		}
	}
	return deps
}

// RecordCoordinationEdge appends a coordination edge to the graph and logs it.
func (w *DAGGraphWalker) RecordCoordinationEdge(graph *schemas.TaskGraph, source, target, edgeType string, payload int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	edge := schemas.CoordinationEdge{
		SourceID:  source,
		TargetID:  target,
		Type:      edgeType,
		Payload:   payload,
		Timestamp: float64(time.Now().UnixNano()) / 1e9,
	}
	graph.CoordinationEdges = append(graph.CoordinationEdges, edge)

	w.logger.Log("EventCoordinationEdge", graph.TaskID, target, map[string]any{
		"source":  source,
		"type":    edgeType,
		"payload": payload,
	})
}

// GetCrossFamilyVerifier returns a provider ID from a different model family
// than the given provider. Falls back to "" when unknown or unavailable.
func (w *DAGGraphWalker) GetCrossFamilyVerifier(providerID string) string {
	if w.registry == nil {
		return ""
	}
	// audit H1: Providers is mutated in place by the config file-watcher
	// reload; an unlocked read/range races and can fatal-panic. Take the
	// read lock for the whole lookup (DAGGraphWalker has no lock of its own
	// that would deadlock here).
	w.registry.Mu.RLock()
	defer w.registry.Mu.RUnlock()
	current := w.registry.Providers[providerID]
	if current == nil {
		return ""
	}
	for id, cfg := range w.registry.Providers {
		if id == providerID {
			continue
		}
		if cfg.Family != "" && cfg.Family != current.Family {
			return id
		}
	}
	return ""
}
