package core

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Topic Segmentation (Chat Memory)
// ---------------------------------------------------------------------------

const (
	segmentStayThreshold   = 0.20 // Jaccard threshold for STAY
	segmentSealThreshold   = 0.05 // Jaccard threshold below which to SEAL
	segmentMaxItems        = 50   // max items per segment before forced seal
	segmentSummaryMaxChars = 200  // fallback summary max chars
)

// MemorySegment is a sealed topic segment containing multiple MemoryItems.
type MemorySegment struct {
	SegmentID  string    `json:"segment_id"`
	SourceType string    `json:"source_type"` // "chat"
	TaskScope  string    `json:"task_scope"`
	TopicTitle string    `json:"topic_title"`
	Summary    string    `json:"summary"`
	ItemIDs    []string  `json:"item_ids"`
	ItemCount  int       `json:"item_count"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	IsSealed   bool      `json:"is_sealed"`
	embedding  []float32
}

// SegmentManager manages topic segments for chat memory.
// It is thread-safe and persists sealed segments to a JSONL file.
type SegmentManager struct {
	mu           sync.RWMutex
	active       map[string]*MemorySegment // taskScope -> active segment
	sealedPath   string
	sealed       []MemorySegment
	segmentLinks map[string][]string
	summarizer   func(*MemorySegment) string
	embedClient  EmbeddingClient
}

func NewSegmentManager(sealedPath string) *SegmentManager {
	return &SegmentManager{
		active:     make(map[string]*MemorySegment),
		sealedPath: sealedPath,
	}
}

// AddItem processes a new MemoryItem and decides whether to keep it in the
// current segment or seal the current segment and start a new one.
func (m *SegmentManager) AddItem(item MemoryItem, taskScope string) *MemorySegment {
	m.mu.Lock()
	defer m.mu.Unlock()

	active := m.active[taskScope]
	if active == nil {
		active = m.openSegmentLocked(taskScope, item)
		m.active[taskScope] = active
		return active
	}

	sim := jaccardSimilarity(
		strings.ToLower(item.Intent)+" "+strings.ToLower(item.Summary),
		strings.ToLower(active.TopicTitle)+" "+strings.ToLower(active.Summary),
	)
	if len(item.Embedding) > 0 && len(active.embedding) > 0 {
		sim = cosineSimilarity(item.Embedding, active.embedding)
	}

	if len(active.ItemIDs) >= segmentMaxItems {
		m.sealSegmentLocked(active)
		active = m.openSegmentLocked(taskScope, item)
		m.active[taskScope] = active
		return active
	}

	if sim < segmentSealThreshold {
		m.sealSegmentLocked(active)
		active = m.openSegmentLocked(taskScope, item)
		m.active[taskScope] = active
		return active
	}

	// STAY
	active.ItemIDs = append(active.ItemIDs, item.NodeID)
	active.ItemCount++
	active.EndTime = item.Timestamp
	return active
}

// SealActive seals the active segment for the given taskScope.
func (m *SegmentManager) SealActive(taskScope string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seg, ok := m.active[taskScope]; ok {
		m.sealSegmentLocked(seg)
		delete(m.active, taskScope)
	}
}

// SealedCount returns the number of sealed segments.
func (m *SegmentManager) SealedCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sealed)
}

// ActiveSegment returns the current active segment for a taskScope, or nil.
func (m *SegmentManager) ActiveSegment(taskScope string) *MemorySegment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active[taskScope]
}

func (m *SegmentManager) openSegmentLocked(taskScope string, first MemoryItem) *MemorySegment {
	title := truncateStr(first.Intent, segmentSummaryMaxChars)
	if title == "" {
		title = truncateStr(first.Summary, segmentSummaryMaxChars)
	}
	return &MemorySegment{
		SegmentID:  fmt.Sprintf("seg_%s_%d", taskScope, time.Now().UnixNano()),
		SourceType: "chat",
		TaskScope:  taskScope,
		TopicTitle: title,
		Summary:    title,
		ItemIDs:    []string{first.NodeID},
		ItemCount:  1,
		StartTime:  first.Timestamp,
		EndTime:    first.Timestamp,
		IsSealed:   false,
		embedding:  first.Embedding,
	}
}

// SetSummarizer sets a custom LLM-based summarizer for segment sealing.
func (m *SegmentManager) SetSummarizer(fn func(*MemorySegment) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summarizer = fn
}

// SetEmbeddingClient sets the embedding client used for cross-segment linking.
func (m *SegmentManager) SetEmbeddingClient(client EmbeddingClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.embedClient = client
}

func (m *SegmentManager) sealSegmentLocked(seg *MemorySegment) {
	seg.IsSealed = true
	seg.EndTime = time.Now()

	// Fallback summary: first + last item intent
	if m.summarizer != nil {
		seg.Summary = m.summarizer(seg)
	} else if seg.ItemCount > 1 && len(seg.ItemIDs) > 0 {
		lastID := seg.ItemIDs[len(seg.ItemIDs)-1]
		seg.Summary = seg.TopicTitle + " \u2192 " + lastID
	}

	// Persist to JSONL
	if m.sealedPath != "" {
		data, err := json.Marshal(seg)
		if err != nil {
			return
		}
		m.appendSealedFile(data)
	}
	m.sealed = append(m.sealed, *seg)
	m.linkSealedSegmentLocked(seg)
}

// linkSealedSegmentLocked creates cross-segment links.
const crossSegmentLinkThreshold = 0.75

func (m *SegmentManager) linkSealedSegmentLocked(seg *MemorySegment) {
	if m.segmentLinks == nil {
		m.segmentLinks = make(map[string][]string)
	}
	start := 0
	if len(m.sealed) > 20 {
		start = len(m.sealed) - 20
	}
	for i := start; i < len(m.sealed); i++ {
		other := &m.sealed[i]
		if other.SegmentID == seg.SegmentID {
			continue
		}
		var sim float64
		if len(seg.embedding) > 0 && len(other.embedding) > 0 {
			sim = cosineSimilarity(seg.embedding, other.embedding)
		} else {
			sim = jaccardSimilarity(
				strings.ToLower(seg.TopicTitle)+" "+strings.ToLower(seg.Summary),
				strings.ToLower(other.TopicTitle)+" "+strings.ToLower(other.Summary),
			)
		}
		if sim >= crossSegmentLinkThreshold {
			m.segmentLinks[seg.SegmentID] = append(m.segmentLinks[seg.SegmentID], other.SegmentID)
			m.segmentLinks[other.SegmentID] = append(m.segmentLinks[other.SegmentID], seg.SegmentID)
		}
	}
}

// GetSegmentLinks returns cross-segment links for a segment ID (P2-12).
func (m *SegmentManager) GetSegmentLinks(segmentID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.segmentLinks[segmentID]
}

func (m *SegmentManager) appendSealedFile(data []byte) {
	dir := strings.ReplaceAll(m.sealedPath, "\\", "/")
	lastSlash := strings.LastIndex(dir, "/")
	if lastSlash != -1 {
		os.MkdirAll(dir[:lastSlash], 0755)
	}
	f, err := os.OpenFile(m.sealedPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(data, '\n'))
}

// ---------------------------------------------------------------------------
// Step Context Budget (Task Prompt Pruning)
// ---------------------------------------------------------------------------

const (
	defaultBudgetTokens = 6000
	minBudgetTokens     = 2000
	keepRecentSteps     = 3
	budgetTightenFactor = 0.7
	latencyBaselineSec  = 15.0
	latencyWindowSize   = 5
)

// StepContextBudget tracks the per-task context budget.
type StepContextBudget struct {
	TaskID       string    `json:"task_id"`
	BudgetTokens int       `json:"budget_tokens"`
	MinTokens    int       `json:"min_tokens"`
	KeepRecent   int       `json:"keep_recent"`
	LastAdjusted time.Time `json:"last_adjusted"`
}

// BudgetManager manages per-task context budgets with latency feedback.
type BudgetManager struct {
	mu            sync.RWMutex
	budgets       map[string]*StepContextBudget
	latencyWindow []float64
}

func NewBudgetManager() *BudgetManager {
	return &BudgetManager{
		budgets:       make(map[string]*StepContextBudget),
		latencyWindow: make([]float64, 0, latencyWindowSize),
	}
}

// GetBudget returns the current token budget for a task, or the default.
func (m *BudgetManager) GetBudget(taskID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if b, ok := m.budgets[taskID]; ok {
		return b.BudgetTokens
	}
	return defaultBudgetTokens
}

// RecordLatency records a step's latency and optionally tightens the budget.
func (m *BudgetManager) RecordLatency(taskID string, seconds float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.latencyWindow = append(m.latencyWindow, seconds)
	if len(m.latencyWindow) > latencyWindowSize {
		m.latencyWindow = m.latencyWindow[len(m.latencyWindow)-latencyWindowSize:]
	}

	avg := 0.0
	for _, s := range m.latencyWindow {
		avg += s
	}
	avg /= float64(len(m.latencyWindow))

	if avg > latencyBaselineSec {
		b, ok := m.budgets[taskID]
		if !ok {
			b = &StepContextBudget{
				TaskID: taskID, BudgetTokens: defaultBudgetTokens,
				MinTokens: minBudgetTokens, KeepRecent: keepRecentSteps,
			}
			m.budgets[taskID] = b
		}
		newBudget := int(float64(b.BudgetTokens) * budgetTightenFactor)
		if newBudget < b.MinTokens {
			newBudget = b.MinTokens
		}
		b.BudgetTokens = newBudget
		b.LastAdjusted = time.Now()
	}
}

// PruneStepContext replaces early items' summaries with micro-summaries.
func PruneStepContext(items []MemoryItem, budgetTokens, keepRecent int) ([]MemoryItem, int, int) {
	if len(items) <= keepRecent {
		total := 0
		for _, it := range items {
			total += estimateTokens(it.Summary)
		}
		return items, total, total
	}

	result := make([]MemoryItem, 0, len(items))
	earlyEnd := len(items) - keepRecent
	for _, it := range items[:earlyEnd] {
		pruned := it
		pruned.Summary = microSummary(it.Summary, 100)
		result = append(result, pruned)
	}
	result = append(result, items[earlyEnd:]...)

	origTokens := 0
	for _, it := range items {
		origTokens += estimateTokens(it.Summary)
	}
	compTokens := 0
	for _, it := range result {
		compTokens += estimateTokens(it.Summary)
	}
	return result, origTokens, compTokens
}

// ---------------------------------------------------------------------------
// Helper Functions
// ---------------------------------------------------------------------------

func jaccardSimilarity(a, b string) float64 {
	wordsA := toWordSet(strings.Fields(strings.ToLower(a)))
	wordsB := toWordSet(strings.Fields(strings.ToLower(b)))
	if len(wordsA) == 0 || len(wordsB) == 0 {
		return 0
	}
	intersection := 0
	for w := range wordsA {
		if wordsB[w] {
			intersection++
		}
	}
	union := len(wordsA) + len(wordsB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func toWordSet(words []string) map[string]bool {
	s := make(map[string]bool, len(words))
	for _, w := range words {
		s[w] = true
	}
	return s
}

func microSummary(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	half := maxChars / 2
	return s[:half] + " ... " + s[len(s)-half:]
}

func estimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}

func truncateStr(s string, maxChars int) string {
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars]
}
