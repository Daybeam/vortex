package core

import (
	"sort"
	"sync"
	"time"

	"github.com/daybeam/vortex/pkg/types"
)

// SignalLayer defines the priority levels for tasks in the swarm.
type SignalLayer = types.SignalLayer

// SignalIntensity holds the value and metadata of a signal in the field.
type SignalIntensity struct {
	Value       float64   `json:"value"`
	LastUpdated time.Time `json:"last_updated"`
}

type pair struct {
	id  string
	val float64
}

// SignalField implements the ACAIS hierarchical signal field with exponential decay.
type SignalField struct {
	Mu     sync.RWMutex
	Layers map[SignalLayer]map[string]*SignalIntensity // layer -> taskID -> intensity

	// Parameters
	DecayRate     float64       // e.g., 0.95 (5% decay per tick)
	DecayInterval time.Duration // e.g., 10 seconds
	BaseIntensity float64       // e.g., 1.0 (amount deposited)

	stopChan  chan struct{}
	stopOnce  sync.Once
	startOnce sync.Once // audit H2: prevent duplicate decay goroutines on double-Start
}

var signalWeights = map[SignalLayer]float64{
	types.LayerCritical:      1.0,
	types.LayerDependency:    0.6,
	types.LayerOpportunistic: 0.3,
	types.LayerExploration:   0.15,
}

func NewSignalField(decayRate float64, decayInterval time.Duration) *SignalField {
	sf := &SignalField{
		Layers: map[SignalLayer]map[string]*SignalIntensity{
			types.LayerCritical:      make(map[string]*SignalIntensity),
			types.LayerDependency:    make(map[string]*SignalIntensity),
			types.LayerOpportunistic: make(map[string]*SignalIntensity),
			types.LayerExploration:   make(map[string]*SignalIntensity),
		},
		DecayRate:     decayRate,
		DecayInterval: decayInterval,
		BaseIntensity: 1.0,
		stopChan:      make(chan struct{}),
	}
	return sf
}

// Start initiates the background decay goroutine. Idempotent — safe to call
// multiple times; only the first call launches a goroutine (audit H2).
func (sf *SignalField) Start() {
	sf.startOnce.Do(func() {
		go sf.decayLoop()
	})
}

// Stop halts the decay goroutine.
func (sf *SignalField) Stop() {
	sf.stopOnce.Do(func() { close(sf.stopChan) })
}

func (sf *SignalField) decayLoop() {
	ticker := time.NewTicker(sf.DecayInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sf.stopChan:
			return
		case <-ticker.C:
			sf.ApplyDecay()
		}
	}
}

// ApplyDecay reduces all signal intensities across all layers.
func (sf *SignalField) ApplyDecay() {
	sf.Mu.Lock()
	defer sf.Mu.Unlock()

	for _, tasks := range sf.Layers {
		for taskID, intensity := range tasks {
			// Exponential decay formula: I = I * decay_rate
			intensity.Value *= sf.DecayRate

			// Threshold to prune cold signals
			if intensity.Value < 0.01 {
				delete(tasks, taskID)
			}
		}
	}
}

// Deposit strengthens the signal for a specific task in a specific layer.
func (sf *SignalField) Deposit(layer SignalLayer, taskID string, value float64) {
	sf.Mu.Lock()
	defer sf.Mu.Unlock()

	if sf.Layers[layer] == nil {
		sf.Layers[layer] = make(map[string]*SignalIntensity)
	}

	if intensity, ok := sf.Layers[layer][taskID]; ok {
		intensity.Value += value
		intensity.LastUpdated = time.Now()
	} else {
		sf.Layers[layer][taskID] = &SignalIntensity{
			Value:       value,
			LastUpdated: time.Now(),
		}
	}
}

// GetState returns a snapshot of the current signal field state.
func (sf *SignalField) GetState() map[string]any {
	sf.Mu.RLock()
	defer sf.Mu.RUnlock()

	state := make(map[string]any)
	for layer, tasks := range sf.Layers {
		layerState := make(map[string]float64)
		for taskID, intensity := range tasks {
			layerState[taskID] = intensity.Value
		}
		state[string(layer)] = layerState
	}
	return state
}

// GetEffectiveSignal calculates the weighted signal strength for an agent.
func (sf *SignalField) GetEffectiveSignal(taskID string, agentLoad float64) float64 {
	sf.Mu.RLock()
	defer sf.Mu.RUnlock()
	return sf.getEffectiveSignalLocked(taskID, agentLoad)
}

func (sf *SignalField) getEffectiveSignalLocked(taskID string, agentLoad float64) float64 {
	// Formula: E = (Sum over layers: LayerWeight * Intensity) * (1 - AgentLoad)
	// We use hardcoded weights for now: Critical=1.0, Dependency=0.6, Opportunistic=0.3
	weights := signalWeights

	effective := 0.0
	for layer, weight := range weights {
		if tasks, ok := sf.Layers[layer]; ok {
			if intensity, ok := tasks[taskID]; ok {
				effective += weight * intensity.Value
			}
		}
	}

	return effective * (1.0 - agentLoad)
}

// Sensing returns the top task IDs with the strongest signals in a layer.
func (sf *SignalField) Sensing(layer SignalLayer, limit int) []string {
	sf.Mu.RLock()
	defer sf.Mu.RUnlock()

	tasks, ok := sf.Layers[layer]
	if !ok {
		return nil
	}

	// Sort by intensity descending (audit H1: was random map iteration order).
	type entry struct {
		id    string
		value float64
	}
	entries := make([]entry, 0, len(tasks))
	for id, intensity := range tasks {
		entries = append(entries, entry{id: id, value: intensity.Value})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].value > entries[j].value
	})

	if limit > len(entries) {
		limit = len(entries)
	}
	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = entries[i].id
	}
	return result
}

// SensingGlobal returns the top task IDs across all layers based on effective signal.
func (sf *SignalField) SensingGlobal(limit int, agentLoad float64) []string {
	sf.Mu.RLock()
	defer sf.Mu.RUnlock()

	// Collect all unique task IDs
	ids := make(map[string]bool)
	for _, tasks := range sf.Layers {
		for id := range tasks {
			ids[id] = true
		}
	}

	// Sort by effective signal descending (audit H1: was random map iteration order).
	type entry struct {
		id     string
		signal float64
	}
	entries := make([]entry, 0, len(ids))
	for id := range ids {
		entries = append(entries, entry{id: id, signal: sf.getEffectiveSignalLocked(id, agentLoad)})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].signal > entries[j].signal
	})

	if limit > len(entries) {
		limit = len(entries)
	}
	result := make([]string, limit)
	for i := 0; i < limit; i++ {
		result[i] = entries[i].id
	}
	return result
}
