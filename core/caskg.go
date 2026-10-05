package core

import (
	"encoding/json"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SkillCausalEdge records the transition success rates between skills.
type SkillCausalEdge struct {
	SourceSkillID string    `json:"source_skill_id"`
	TargetSkillID string    `json:"target_skill_id"`
	SuccessCount  int       `json:"success_count"`
	TotalCount    int       `json:"total_count"`
	CausalLift    float64   `json:"causal_lift"`
	LastUpdated   time.Time `json:"last_updated,omitempty"` // Last activation/reinforcement timestamp
}

// CausalSkillGraph is the persistent structure for causal reasoning.
type CausalSkillGraph struct {
	Nodes map[string]int                         `json:"nodes"` // skill_id -> total executions
	Edges map[string]map[string]*SkillCausalEdge `json:"edges"` // source -> target -> edge
}

// CaSKGManager handles the autonomous updates and scoring for causal skill graphs.
type CaSKGManager struct {
	mu            sync.RWMutex
	Graph         *CausalSkillGraph
	PersistPath   string
	taskTracking  map[string][]string // taskID -> ordered list of skills
	lastPruneTime time.Time           // last metabolic pruning timestamp (throttle)
}

const maxTaskTrackingEntries = 1000 // cap on taskTracking map; clearing is safe (loses transitions for abandoned tasks)

// NewCaSKGManager initializes a manager with a specific persistence path.
func NewCaSKGManager(persistPath string) *CaSKGManager {
	m := &CaSKGManager{
		Graph: &CausalSkillGraph{
			Nodes: make(map[string]int),
			Edges: make(map[string]map[string]*SkillCausalEdge),
		},
		PersistPath:   persistPath,
		taskTracking:  make(map[string][]string),
		lastPruneTime: time.Now(), // initialize so first Save() doesn't immediately prune
	}
	m.load()
	return m
}

func (m *CaSKGManager) load() {
	data, err := os.ReadFile(m.PersistPath)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, m.Graph); err != nil {
		log.Printf("WARN: CaSKGManager: failed to unmarshal causal graph from %s: %v", m.PersistPath, err)
	}
}

func (m *CaSKGManager) Save() error {
	// Metabolic pruning: evict stale and low-performing edges periodically.
	// Throttled to avoid pruning on every Save() call (which fires per task
	// event). This prevents evicting edges that are actively being recorded.
	m.mu.Lock()
	if time.Since(m.lastPruneTime) > pruneInterval {
		m.pruneStaleEdgesLocked(defaultStaleEdgeMaxAge, defaultMinSuccessRate)
		m.lastPruneTime = time.Now()
	}
	m.mu.Unlock()

	m.mu.RLock()
	data, err := json.MarshalIndent(m.Graph, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	// Ensure parent directory exists (auto-init: data/ may not exist on first run).
	// Regression: without this, os.WriteFile fails with "no such file or directory"
	// and causal learning is silently lost across restarts.
	if dir := filepath.Dir(m.PersistPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(m.PersistPath, data, 0644)
}

// HookEventBus registers the manager to listen for execution lifecycle events.
func (m *CaSKGManager) HookEventBus(bus *EventBus) {
	// Track steps as they complete
	bus.Subscribe(string(EventStepCompleted), func(ev AgentEvent) error {
		skillID, _ := ev.Payload["role"].(string) // Often RoleID represents the skill
		if skillID == "" {
			skillID, _ = ev.Payload["skill"].(string)
		}
		if skillID != "" {
			m.mu.Lock()
			m.taskTracking[ev.TaskID] = append(m.taskTracking[ev.TaskID], skillID)
			// Cap the map to prevent unbounded growth from abandoned tasks.
			// audit L-2: previously cleared the ENTIRE map, losing tracking for
			// all active tasks. Now evicts ~25% of entries, preserving the current
			// task and most others. Random map iteration ≈ random eviction.
			if len(m.taskTracking) > maxTaskTrackingEntries {
				target := maxTaskTrackingEntries * 3 / 4
				for k := range m.taskTracking {
					if len(m.taskTracking) <= target {
						break
					}
					if k != ev.TaskID {
						delete(m.taskTracking, k)
					}
				}
			}
			m.mu.Unlock()
		}
		return nil
	})

	// Finalize and record transitions on task success
	bus.Subscribe(string(EventTaskCompleted), func(ev AgentEvent) error {
		m.mu.Lock()
		history := m.taskTracking[ev.TaskID]
		delete(m.taskTracking, ev.TaskID)

		if len(history) >= 2 {
			for i := 0; i < len(history)-1; i++ {
				source := history[i]
				target := history[i+1]
				m.recordTransition(source, target, true)
			}
		}
		m.mu.Unlock()

		if err := m.Save(); err != nil {
			log.Printf("WARN: CaSKGManager: failed to save after task completion: %v", err)
		}
		return nil
	})

	// Record failed transitions (penalty)
	bus.Subscribe(string(EventTaskFailed), func(ev AgentEvent) error {
		m.mu.Lock()
		history := m.taskTracking[ev.TaskID]
		delete(m.taskTracking, ev.TaskID)

		if len(history) >= 2 {
			// The last transition is likely the failure point
			source := history[len(history)-2]
			target := history[len(history)-1]
			m.recordTransition(source, target, false)
		}
		m.mu.Unlock()

		if err := m.Save(); err != nil {
			log.Printf("WARN: CaSKGManager: failed to save after task failure: %v", err)
		}
		return nil
	})
}

func (m *CaSKGManager) recordTransition(source, target string, success bool) {
	m.Graph.Nodes[source]++
	m.Graph.Nodes[target]++

	if m.Graph.Edges[source] == nil {
		m.Graph.Edges[source] = make(map[string]*SkillCausalEdge)
	}

	edge, ok := m.Graph.Edges[source][target]
	if !ok {
		edge = &SkillCausalEdge{
			SourceSkillID: source,
			TargetSkillID: target,
		}
		m.Graph.Edges[source][target] = edge
	}

	edge.TotalCount++
	if success {
		edge.SuccessCount++
	}
	edge.LastUpdated = time.Now()

	// Calculate Causal Lift: P(Success | A->B) / P(Success | B)
	// For simplicity in this implementation, we use: SuccessCount / TotalCount
	// Adjusted for base success rate of the target node.

	pSuccessGivenA := float64(edge.SuccessCount) / float64(edge.TotalCount)

	// Base success rate of target (across all sources) is harder to track precisely
	// without a global Success counter per node.
	// For now, we use a conservative lift:
	edge.CausalLift = pSuccessGivenA
}

// Default metabolic pruning parameters. Edges unused for 7 days or with
// success rate below 30% (after ≥5 observations) are evicted periodically.
const (
	defaultStaleEdgeMaxAge      = 7 * 24 * time.Hour
	defaultMinSuccessRate       = 0.3
	staleEdgeMinTotalForPruning = 5  // don't prune edges with too few observations
	pruneInterval               = 1 * time.Hour // throttle: prune at most once per hour
)

// PruneStaleEdges removes stale and low-performing edges from the causal skill
// graph, mirroring ExperienceStore's metabolic ROI lifecycle.
//
// Rule 1 (time-decay): Edges whose LastUpdated exceeds maxAge are evicted.
// Rule 2 (low-performing): Edges with TotalCount ≥ 5 and success rate below
// minSuccessRate are evicted.
//
// Edges with zero LastUpdated (legacy data loaded from old JSON) are spared
// from Rule 1 — they get stamped on the next recordTransition.
func (m *CaSKGManager) PruneStaleEdges(maxAge time.Duration, minSuccessRate float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneStaleEdgesLocked(maxAge, minSuccessRate)
}

// pruneStaleEdgesLocked is the lock-free inner logic of PruneStaleEdges.
// Caller MUST hold m.mu write lock.
func (m *CaSKGManager) pruneStaleEdgesLocked(maxAge time.Duration, minSuccessRate float64) {
	now := time.Now()
	for source, targets := range m.Graph.Edges {
		for target, edge := range targets {
			// Rule 1: Time-decay eviction (stale paths unused beyond TTL).
			// Zero LastUpdated = legacy edge from old JSON; spare it.
			if !edge.LastUpdated.IsZero() && now.Sub(edge.LastUpdated) > maxAge {
				delete(targets, target)
				continue
			}
			// Rule 2: Low-performing eviction (metabolic ROI equivalent).
			if edge.TotalCount >= staleEdgeMinTotalForPruning &&
				float64(edge.SuccessCount)/float64(edge.TotalCount) < minSuccessRate {
				delete(targets, target)
			}
		}
		if len(targets) == 0 {
			delete(m.Graph.Edges, source)
		}
	}
}

// GetCausalBoost returns the multiplier for a candidate skill given the predecessor.
func (m *CaSKGManager) GetCausalBoost(predecessorSkillID, candidateSkillID string) float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if predecessorSkillID == "" {
		return 1.0
	}

	targets, ok := m.Graph.Edges[predecessorSkillID]
	if !ok {
		return 1.0
	}

	edge, ok := targets[candidateSkillID]
	if !ok || edge.TotalCount < 3 { // Cold-start threshold
		return 1.0
	}

	// α = 0.5 (scaling factor)
	alpha := 0.5
	// Boost = 1 + alpha * (CausalLift - 0.5)
	// (Using 0.5 as neutral base since Lift is currently just P(Success|A->B))
	boost := 1.0 + alpha*(edge.CausalLift-0.5)

	return math.Max(0.1, boost) // Never prune completely, just suppress
}
