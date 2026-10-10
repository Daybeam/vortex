package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJaccardSimilarity(t *testing.T) {
	tests := []struct {
		a, b string
		want float64
	}{
		{"hello world", "hello world", 1.0},
		{"hello world", "goodbye universe", 0.0},
		{"the cat sat", "the dog sat", 2.0 / 4.0},
		{"", "anything", 0.0},
		{"a b c d", "b c d e", 3.0 / 5.0},
	}
	for _, tt := range tests {
		got := jaccardSimilarity(tt.a, tt.b)
		if abs(got-tt.want) > 0.001 {
			t.Errorf("jaccard(%q, %q) = %.3f, want %.3f", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestMicroSummary(t *testing.T) {
	s := "hello"
	if got := microSummary(s, 200); got != s {
		t.Errorf("microSummary short: got %q", got)
	}
	long := strings.Repeat("a", 500)
	got := microSummary(long, 100)
	if len(got) > 110 {
		t.Errorf("microSummary long: got len %d, want <= 110", len(got))
	}
	if !strings.Contains(got, "...") {
		t.Errorf("microSummary should contain '...' marker")
	}
}

func TestContextSegEstimateTokens(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcdefgh", 2},
		{"hello world", 3},
	}
	for _, tt := range tests {
		got := estimateTokens(tt.s)
		if got != tt.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestSegmentManager_BasicFlow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "segments.jsonl")
	sm := NewSegmentManager(path)

	taskScope := "chat_session1"
	now := time.Now()

	item1 := MemoryItem{
		TaskID: taskScope, NodeID: "n1",
		Intent: "how to configure LSP for Go", Summary: "you need to install gopls",
		Timestamp: now,
	}
	seg := sm.AddItem(item1, taskScope)
	if seg == nil {
		t.Fatal("expected non-nil segment")
	}
	if seg.TopicTitle == "" {
		t.Error("topic title should be non-empty")
	}
	if seg.ItemCount != 1 {
		t.Errorf("item count = %d, want 1", seg.ItemCount)
	}
	if seg.IsSealed {
		t.Error("new segment should not be sealed")
	}

	item2 := MemoryItem{
		TaskID: taskScope, NodeID: "n2",
		Intent: "how to install gopls", Summary: "run go install golang.org/x/tools/gopls",
		Timestamp: now.Add(time.Second),
	}
	seg2 := sm.AddItem(item2, taskScope)
	if seg2.ItemCount != 2 {
		t.Errorf("item count = %d, want 2", seg2.ItemCount)
	}
	if seg2.SegmentID != seg.SegmentID {
		t.Error("second item should stay in same segment")
	}

	item3 := MemoryItem{
		TaskID: taskScope, NodeID: "n3",
		Intent: "what is the capital of France", Summary: "Paris is the capital",
		Timestamp: now.Add(2 * time.Second),
	}
	seg3 := sm.AddItem(item3, taskScope)
	if seg3.ItemCount != 1 {
		t.Errorf("new segment item count = %d, want 1", seg3.ItemCount)
	}
	if sm.SealedCount() != 1 {
		t.Errorf("sealed count = %d, want 1", sm.SealedCount())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read segments file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("segments file should not be empty")
	}
}

func TestSegmentManager_MultipleScopes(t *testing.T) {
	sm := NewSegmentManager("")
	now := time.Now()

	sm.AddItem(MemoryItem{NodeID: "n1", Intent: "configure LSP", Timestamp: now}, "chat_a")
	sm.AddItem(MemoryItem{NodeID: "n2", Intent: "python script", Timestamp: now}, "chat_b")

	segA := sm.ActiveSegment("chat_a")
	segB := sm.ActiveSegment("chat_b")
	if segA == nil || segB == nil {
		t.Fatal("both scopes should have active segments")
	}
	if segA.SegmentID == segB.SegmentID {
		t.Error("different scopes should have different segments")
	}
}

func TestSegmentManager_ForcedSeal(t *testing.T) {
	sm := NewSegmentManager("")
	now := time.Now()

	taskScope := "chat_forced"
	for i := 0; i < segmentMaxItems+1; i++ {
		item := MemoryItem{
			TaskID: taskScope, NodeID: fmt.Sprintf("n%d", i),
			Intent: "how to configure LSP for Go", Summary: "you need gopls",
			Timestamp: now.Add(time.Duration(i) * time.Second),
		}
		sm.AddItem(item, taskScope)
	}

	if sm.SealedCount() < 1 {
		t.Error("forced seal should have occurred")
	}
}

func TestBudgetManager_DefaultBudget(t *testing.T) {
	bm := NewBudgetManager()
	got := bm.GetBudget("task1")
	if got != defaultBudgetTokens {
		t.Errorf("default budget = %d, want %d", got, defaultBudgetTokens)
	}
}

func TestBudgetManager_LatencyFeedback(t *testing.T) {
	bm := NewBudgetManager()

	// Record high latencies (20s each) for 5 steps - all above baseline (15s).
	// Each RecordLatency call tightens budget by budgetTightenFactor (0.7).
	// 6000 -> 4200 -> 2940 -> 2058 -> 1440 -> 2000 (clamped to minBudgetTokens)
	for i := 0; i < latencyWindowSize; i++ {
		bm.RecordLatency("task1", latencyBaselineSec+5)
	}

	got := bm.GetBudget("task1")
	// After 5 tightenings: 6000*0.7^5 = 798, clamped to minBudgetTokens=2000
	if got != minBudgetTokens {
		t.Errorf("budget after 5 high-latency records = %d, want %d (clamped to min)", got, minBudgetTokens)
	}
}

func TestBudgetManager_LowLatency(t *testing.T) {
	bm := NewBudgetManager()
	for i := 0; i < latencyWindowSize; i++ {
		bm.RecordLatency("task1", 2.0)
	}
	got := bm.GetBudget("task1")
	if got != defaultBudgetTokens {
		t.Errorf("budget after low latency = %d, want %d", got, defaultBudgetTokens)
	}
}

func TestBudgetManager_MinClamp(t *testing.T) {
	bm := NewBudgetManager()
	for i := 0; i < 20; i++ {
		bm.RecordLatency("task1", latencyBaselineSec+10)
	}
	got := bm.GetBudget("task1")
	if got < minBudgetTokens {
		t.Errorf("budget = %d, should be >= minBudgetTokens %d", got, minBudgetTokens)
	}
}

func TestPruneStepContext_NoPruning(t *testing.T) {
	items := []MemoryItem{
		{NodeID: "n1", Summary: "step 1 result"},
		{NodeID: "n2", Summary: "step 2 result"},
	}
	pruned, orig, comp := PruneStepContext(items, defaultBudgetTokens, keepRecentSteps)
	if len(pruned) != len(items) {
		t.Errorf("pruned len = %d, want %d", len(pruned), len(items))
	}
	if orig != comp {
		t.Errorf("orig = %d, comp = %d, should be equal when no pruning", orig, comp)
	}
}

func TestPruneStepContext_WithPruning(t *testing.T) {
	items := make([]MemoryItem, 10)
	for i := range items {
		items[i] = MemoryItem{
			NodeID:  fmt.Sprintf("n%d", i),
			Summary: strings.Repeat("x", 200),
		}
	}

	pruned, orig, comp := PruneStepContext(items, 1000, 3)

	for i := 0; i < 7; i++ {
		if len(pruned[i].Summary) > 110 {
			t.Errorf("pruned item %d: summary len = %d, should be <= 110", i, len(pruned[i].Summary))
		}
	}
	for i := 7; i < 10; i++ {
		if len(pruned[i].Summary) != 200 {
			t.Errorf("item %d: summary len = %d, want 200", i, len(pruned[i].Summary))
		}
	}
	if orig <= comp {
		t.Errorf("orig = %d, comp = %d, orig should be > comp after pruning", orig, comp)
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		a, b   []float32
		want   float64
		name   string
	}{
		{[]float32{1, 0}, []float32{1, 0}, 1.0, "same"},
		{[]float32{1, 0}, []float32{0, 1}, 0, "orthogonal"},
		{[]float32{1, 0}, []float32{-1, 0}, -1.0, "opposite"},
		{[]float32{}, []float32{1, 2}, 0, "empty"},
	}
	for _, tt := range tests {
		got := cosineSimilarity(tt.a, tt.b)
		if abs(got-tt.want) > 0.01 {
			t.Errorf("%s: cosineSim = %.4f, want %.4f", tt.name, got, tt.want)
		}
	}
}

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("hello", 10); got != "hello" {
		t.Errorf("truncateStr short: got %q", got)
	}
	if got := truncateStr("hello world", 5); got != "hello" {
		t.Errorf("truncateStr long: got %q, want %q", got, "hello")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
