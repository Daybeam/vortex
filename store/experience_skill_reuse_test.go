package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fixedEmbedClient returns one vector, optionally only for the query text.
type fixedEmbedClient struct {
	queryText string
	queryVec  []float32
	model     string
	failOther bool
}

func (c *fixedEmbedClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	if c.queryText == "" || text == c.queryText {
		return c.queryVec, c.model, nil
	}
	if c.failOther {
		return nil, "", errors.New("embed unavailable")
	}
	return c.queryVec, c.model, nil
}

// gatedEmbedClient blocks skill embeddings until release is closed, so a test
// can observe what the store does while an embedding call is in flight.
type gatedEmbedClient struct {
	queryText string
	vec       []float32
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (c *gatedEmbedClient) EmbedWithModel(ctx context.Context, text string) ([]float32, string, error) {
	if text == c.queryText {
		return c.vec, "m", nil
	}
	c.once.Do(func() { close(c.entered) })
	<-c.release
	return c.vec, "m", nil
}

// TestQueryRelevantSkills_NoIOOrWritesUnderReadLock reproduces the retrieval
// path holding es.Mu.RLock across an embedding round-trip: a concurrent
// ExperienceStore writer (here IncrementGeneratedSkillUsage) blocked until the
// network call finished, and the same code mutated gs.Embedding under the read
// lock. Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.1.
func TestQueryRelevantSkills_NoIOOrWritesUnderReadLock(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["gs1"] = &GeneratedSkill{
		ID:          "gs1",
		Description: "review kubernetes deployment manifests for privileged containers",
		Capability:  "security",
		SuccessRate: 1.0,
	}
	client := &gatedEmbedClient{
		queryText: "review kubernetes deployment manifests",
		vec:       []float32{1, 0},
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	es.SetEmbeddingClient(client)

	queryDone := make(chan []string, 1)
	go func() {
		queryDone <- es.QueryRelevantSkills(context.Background(), client.queryText, 2)
	}()

	select {
	case <-client.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("skill embedding was never attempted")
	}

	writeDone := make(chan struct{})
	go func() {
		es.IncrementGeneratedSkillUsage("gs1")
		close(writeDone)
	}()

	select {
	case <-writeDone:
	case <-time.After(500 * time.Millisecond):
		close(client.release)
		<-queryDone
		t.Fatal("store writer blocked while embedding was in flight: network I/O is happening under RLock")
	}
	close(client.release)
	<-queryDone
}

// TestQueryRelevantSkills_TaskLevelSkillRemainsRetrievable guards JIT
// reusability: dividing overlap by max(query, haystack) made a task-shaped
// JIT description (~20+ tokens) require an impossible 45% of its own length,
// so every long skill was silently unreachable even for the same task.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.2.
func TestQueryRelevantSkills_TaskLevelSkillRemainsRetrievable(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["jit_task"] = &GeneratedSkill{
		ID:          "jit_task",
		Description: "review kubernetes deployment manifests for security misconfigurations including privileged containers missing network policies and host path mounts in production clusters across staging and production namespaces with audit logging enabled and resource limits configured for each workload",
		Capability:  "security audit",
		SuccessRate: 1.0,
	}

	got := es.QueryRelevantSkills(context.Background(),
		"review kubernetes deployment manifests privileged containers network policies", 2)
	if len(got) != 1 || got[0] != "jit_task" {
		t.Fatalf("task-level skill with near-exact rerun must be retrievable, got %v", got)
	}
}

// TestQueryRelevantSkills_SemanticRecallWithoutLexicalOverlap covers the CJK /
// paraphrase case: tokenize yields no shared tokens, but a stored embedding
// makes the skill reusable. The old lexical pre-filter returned before any
// semantic comparison, so these skills were unreachable forever.
func TestQueryRelevantSkills_SemanticRecallWithoutLexicalOverlap(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["jit_cn"] = &GeneratedSkill{
		ID:             "jit_cn",
		Description:    "用浏览器抓取微信公众号文章正文并转换为 markdown",
		Capability:     "browser automation scraping",
		Embedding:      []float32{0.98, 0.2},
		EmbeddingModel: "m",
		SuccessRate:    1.0,
	}
	es.SetEmbeddingClient(&fixedEmbedClient{queryVec: []float32{1, 0}, model: "m"})

	got := es.QueryRelevantSkills(context.Background(), "抓取公众号文章", 2)
	if len(got) != 1 || got[0] != "jit_cn" {
		t.Fatalf("semantic-only match must be retrievable, got %v", got)
	}
}

// TestQueryRelevantSkills_UnrelatedCosineFiltered reproduces raw cosine being
// compared against the lexical threshold: an unrelated pair scoring 0.65
// passed the 0.15-0.45 gate. Cosine is now rescaled through
// skillSemanticFloor before it shares that threshold.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.3.
func TestQueryRelevantSkills_UnrelatedCosineFiltered(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["noise"] = &GeneratedSkill{
		ID:             "noise",
		Description:    "business logic question about article reactions and profile click throughs from trending developer posts for security",
		Capability:     "analyze",
		Embedding:      []float32{0.65, 0.76},
		EmbeddingModel: "m",
		SuccessRate:    1.0,
	}
	es.GeneratedSkills["match"] = &GeneratedSkill{
		ID:             "match",
		Description:    "security audit of authentication flow",
		Capability:     "security",
		Embedding:      []float32{0.99, 0.1},
		EmbeddingModel: "m",
		SuccessRate:    1.0,
	}
	es.SetEmbeddingClient(&fixedEmbedClient{queryVec: []float32{1, 0}, model: "m"})

	got := es.QueryRelevantSkills(context.Background(), "audit the authentication flow for security issues", 2)
	if len(got) != 1 || got[0] != "match" {
		t.Fatalf("unrelated high-cosine skill must be filtered, got %v", got)
	}
}

// TestQueryRelevantSkills_StaleEmbeddingModelNotUsed verifies vectors from a
// different embedding model are never compared with the query vector. Before
// the fix an identical stale vector scored cosine 1.0 and was injected.
func TestQueryRelevantSkills_StaleEmbeddingModelNotUsed(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["stale"] = &GeneratedSkill{
		ID:             "stale",
		Description:    "completely unrelated archival procedure for paper records and security retention",
		Capability:     "archival",
		Embedding:      []float32{1, 0},
		EmbeddingModel: "old-model",
		SuccessRate:    1.0,
	}
	es.SetEmbeddingClient(&fixedEmbedClient{
		queryText: "audit authentication flow security review",
		queryVec:  []float32{1, 0},
		model:     "new-model",
		failOther: true,
	})

	got := es.QueryRelevantSkills(context.Background(), "audit authentication flow security review", 2)
	if len(got) != 0 {
		t.Fatalf("stale-model embedding must not be used for scoring, got %v", got)
	}
}

// TestSkillExposureReachesUsageCredit is the reusability loop: retrieval
// attaches skills only to the spawn prompt, so StepRecord.Skills never saw
// them and UsageCount stayed 0 forever, making maybePromoteSkill's
// UsageCount>=3 gate structurally unreachable. The exposure handoff must
// credit usage exactly once.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.4.
func TestSkillExposureReachesUsageCredit(t *testing.T) {
	es := newTestStore(t)
	if err := es.AddGeneratedSkill(context.Background(), &GeneratedSkill{
		ID: "jit_x", Capability: "analyze", SuccessRate: 1.0,
	}); err != nil {
		t.Fatalf("AddGeneratedSkill: %v", err)
	}

	es.RecordSkillExposure("task_1", "step_1", []string{"jit_x"})
	exposed := es.TakeSkillExposures("task_1", "step_1")
	if len(exposed) != 1 || exposed[0] != "jit_x" {
		t.Fatalf("expected jit_x exposure, got %v", exposed)
	}
	if again := es.TakeSkillExposures("task_1", "step_1"); len(again) != 0 {
		t.Fatalf("exposure must be consumed once, got %v again", again)
	}

	rec := StepRecord{StepID: "step_1", Status: "ok", Skills: exposed}
	if err := es.RecordTaskCompletion(context.Background(), "task_1", []StepRecord{rec}, 0.9, nil, true, false); err != nil {
		t.Fatalf("RecordTaskCompletion: %v", err)
	}
	es.WaitAsyncSaves()

	gs, ok := es.GetGeneratedSkill("jit_x")
	if !ok {
		t.Fatal("jit_x disappeared")
	}
	if gs.UsageCount != 1 {
		t.Fatalf("exposure must credit UsageCount once, got %d", gs.UsageCount)
	}
}

// TestQueryRelevantSkills_FiltersExpiredJITTools is the dead-reference half of
// the JIT lifecycle: a tool's 24h TTL expires and the cleanup goroutine
// unregisters it, but its GeneratedSkill record survives forever. Retrieval
// kept returning that id, so spawn prompts advertised a tool that no longer
// existed and maybePromoteSkill could never succeed (PromotePermanent resolves
// the tool through GetMCP). The probe must drop only jit_* records whose tool
// is gone; live JIT records and non-JIT generated skills stay retrievable.
// Regression for JIT_SKILL_RETRIEVAL_NOISE_REMEDIATION.md §7.6.
func TestQueryRelevantSkills_FiltersExpiredJITTools(t *testing.T) {
	es := newTestStore(t)
	desc := "review kubernetes deployment manifests for privileged containers"
	for _, id := range []string{"jit_live", "jit_dead", "gen_manual"} {
		es.GeneratedSkills[id] = &GeneratedSkill{
			ID: id, Description: desc, Capability: "security", SuccessRate: 1.0,
		}
	}
	es.SetSkillLivenessProbe(func(id string) bool { return id != "jit_dead" })

	got := es.QueryRelevantSkills(context.Background(),
		"review kubernetes deployment manifests", 3)
	want := map[string]bool{"jit_live": true, "gen_manual": true}
	if len(got) != len(want) {
		t.Fatalf("expected live JIT + non-JIT skill, got %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("dead or unexpected skill %q returned; got %v", id, got)
		}
	}
}

// TestQuerySkillsBySignal_FiltersExpiredJITTools is the same dead-reference
// check on the retry path: QuerySkillsBySignal injects repair skills into the
// spawn prompt after a retryable failure, so a dead jit_* record there is just
// as unusable as one returned by QueryRelevantSkills.
func TestQuerySkillsBySignal_FiltersExpiredJITTools(t *testing.T) {
	es := newTestStore(t)
	es.GeneratedSkills["jit_dead"] = &GeneratedSkill{
		ID: "jit_dead", Capability: "repair",
		FailureSignal: "permission denied", SuccessRate: 1.0,
	}
	es.GeneratedSkills["jit_live"] = &GeneratedSkill{
		ID: "jit_live", Capability: "repair",
		FailureSignal: "permission denied", SuccessRate: 1.0,
	}
	es.SetSkillLivenessProbe(func(id string) bool { return id != "jit_dead" })

	got := es.QuerySkillsBySignal(context.Background(), "Access Denied: permission denied")
	if len(got) != 1 || got[0] != "jit_live" {
		t.Fatalf("expected only the live repair skill, got %v", got)
	}
}
