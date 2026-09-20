package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/schemas"
)

type FileExperienceBackend struct {
	dir string
}

func (b *FileExperienceBackend) Save(ctx context.Context, data map[string]any) error {
	for k, v := range data {
		path := filepath.Join(b.dir, k+".json")
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, b, 0644); err != nil {
			return err
		}
	}
	return nil
}

func (b *FileExperienceBackend) Load(ctx context.Context) (map[string]any, error) {
	data := make(map[string]any)
	return data, nil
}

func (b *FileExperienceBackend) SaveDecisionOutcome(ctx context.Context, do *DecisionOutcome) error {
	return nil
}

func (b *FileExperienceBackend) SaveTaskPattern(ctx context.Context, p *TaskPattern) error {
	return nil
}

func (b *FileExperienceBackend) SaveRoleProfile(ctx context.Context, rp *RoleProfile) error {
	return nil
}

func (b *FileExperienceBackend) SaveRouteWeight(ctx context.Context, rw *RouteWeight) error {
	return nil
}

func (b *FileExperienceBackend) SaveGeneratedSkill(ctx context.Context, gs *GeneratedSkill) error {
	return nil
}

func (b *FileExperienceBackend) SaveSkillAffinity(ctx context.Context, sa *SkillAffinity) error {
	return nil
}

func (b *FileExperienceBackend) SaveEnvironmentIssue(ctx context.Context, ei *EnvironmentIssue) error {
	return nil
}

func (b *FileExperienceBackend) SaveRoleAffinity(ctx context.Context, roleID, taskType string, score float64) error {
	return nil
}

func (b *FileExperienceBackend) SaveDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error {
	return nil
}

func (b *FileExperienceBackend) SaveStatePotential(ctx context.Context, sp *StatePotential) error {
	return nil
}

func (b *FileExperienceBackend) SaveCooccurrence(ctx context.Context, c *CooccurrenceEntry) error {
	return nil
}

func (b *FileExperienceBackend) GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*CooccurrenceEntry, error) {
	return nil, nil
}

func (b *FileExperienceBackend) GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*CooccurrenceEntry, error) {
	return nil, nil
}

func (b *FileExperienceBackend) SaveExperienceNode(ctx context.Context, node *ExperienceNode) error {
	return nil
}

func (b *FileExperienceBackend) SaveExperienceEdge(ctx context.Context, edge ExperienceEdge) error {
	return nil
}

func (b *FileExperienceBackend) LoadExperienceNodes(ctx context.Context) (map[string]*ExperienceNode, error) {
	return nil, nil
}

func (b *FileExperienceBackend) LoadExperienceEdges(ctx context.Context) ([]ExperienceEdge, error) {
	return nil, nil
}

func (b *FileExperienceBackend) SavePromotionAuditLog(ctx context.Context, log *PromotionAuditLog) error {
	return nil
}

func (b *FileExperienceBackend) LoadPromotionAuditLogs(ctx context.Context) ([]PromotionAuditLog, error) {
	return nil, nil
}

// ExperienceStore manages experience data using a backend.
type ExperienceStore struct {
	Mu               sync.RWMutex
	dir              string
	backend          IExperienceBackend
	taskStore        ITaskStore
	System           *config.SystemSettings
	TaskPatterns     map[string]TaskPattern
	RoleProfiles     map[string]*RoleProfile
	DecisionOutcomes []*DecisionOutcome
	SkillAffinities  map[string]*SkillAffinity
	GeneratedSkills  map[string]*GeneratedSkill
	ArchivedSkills   map[string]*GeneratedSkill
	// RoutingMatrix indices: [RoleID][ModelID][Capability][SkillID]
	RoutingMatrix      map[string]map[string]map[string]map[string]*RouteWeight
	EnvironmentIssues  map[string]*EnvironmentIssue
	DecisionPrecedents map[string]*schemas.DecisionNode // ADDED (2026-08-26)
	// AntiPatternStore persists structural dev pitfalls for subagent injection.
	AntiPatternStore *AntiPatternStore // ADDED (2026-08-27)

	// Experience Graph Nodes (ADDED 2026-09-06 for FMC)
	Nodes map[string]*ExperienceNode `json:"nodes"`
	Edges []ExperienceEdge           `json:"edges"`

	// JIT Promotion Audit (ADDED 2026-09-06)
	JITCandidates      map[string]*JITCandidate `json:"jit_candidates"`
	PromotionAuditLogs []PromotionAuditLog      `json:"promotion_audit_logs"`

	// queryRelevantAntiPatterns is a helper on the store struct that scans
	// the AntiPatternStore for precedents matching the task intent keywords.
	RoleAffinities map[string]map[string]float64

	// StatePotentials: empirical success rate per environment state.
	// ADDED (2026-09-08) for State-Potential Dynamic Routing.
	StatePotentials map[string]*StatePotential

	embeddingClient IEmbeddingClient

	// persistInFlight prevents unbounded goroutine spawn from
	// RecordTaskCompletion's fire-and-forget persist. If a persist
	// is already running, the next one is skipped — the subsequent
	// persist will pick up all accumulated changes.
	persistInFlight atomic.Bool

	// saveWg tracks fire-and-forget DB save goroutines (StatePotential,
	// Cooccurrence) so shutdown can wait for them to drain before closing
	// the backend (audit H10).
	saveWg sync.WaitGroup
}

// WaitAsyncSaves blocks until all fire-and-forget DB save goroutines
// (StatePotential, Cooccurrence) have completed. Call before closing
// the backend to prevent "database is closed" errors (audit H10).
func (es *ExperienceStore) WaitAsyncSaves() {
	es.saveWg.Wait()
}

type EnvironmentIssue struct {
	Command    string    `json:"command"`
	Message    string    `json:"message"`
	Count      int       `json:"count"`
	LastSeen   time.Time `json:"last_seen"`
	IsResolved bool      `json:"is_resolved"`
}

func NewExperienceStore(dir string, ts ITaskStore, sys *config.SystemSettings, expBackend IExperienceBackend, apBackend IAntiPatternBackend) (*ExperienceStore, error) {
	backend := expBackend
	if backend == nil {
		backend = &FileExperienceBackend{dir: dir}
	}
	es := &ExperienceStore{
		dir:                dir,
		backend:            backend,
		taskStore:          ts,
		System:             sys,
		TaskPatterns:       make(map[string]TaskPattern),
		RoleProfiles:       make(map[string]*RoleProfile),
		SkillAffinities:    make(map[string]*SkillAffinity),
		GeneratedSkills:    make(map[string]*GeneratedSkill),
		ArchivedSkills:     make(map[string]*GeneratedSkill),
		RoutingMatrix:      make(map[string]map[string]map[string]map[string]*RouteWeight),
		EnvironmentIssues:  make(map[string]*EnvironmentIssue),
		DecisionPrecedents: make(map[string]*schemas.DecisionNode),
		AntiPatternStore:   NewAntiPatternStore(nil, apBackend),
		Nodes:              make(map[string]*ExperienceNode),
		Edges:              make([]ExperienceEdge, 0),
		JITCandidates:      make(map[string]*JITCandidate),
		PromotionAuditLogs: make([]PromotionAuditLog, 0),
		RoleAffinities:     make(map[string]map[string]float64),
		StatePotentials:    make(map[string]*StatePotential),
	}
	es.load()
	es.loadSeeds()
	return es, nil
}

func (s *ExperienceStore) SetEmbeddingClient(cli IEmbeddingClient) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.embeddingClient = cli
}

func (es *ExperienceStore) load() {
	es.Mu.Lock()
	defer es.Mu.Unlock()

	readJSON(filepath.Join(es.dir, "task_patterns.json"), &es.TaskPatterns)
	readJSON(filepath.Join(es.dir, "role_profiles.json"), &es.RoleProfiles)
	readJSON(filepath.Join(es.dir, "skill_affinity.json"), &es.SkillAffinities)
	readJSON(filepath.Join(es.dir, "routing_matrix.json"), &es.RoutingMatrix)
	readJSON(filepath.Join(es.dir, "generated_skills.json"), &es.GeneratedSkills)
	readJSON(filepath.Join(es.dir, "archived_skills.json"), &es.ArchivedSkills)
	readJSON(filepath.Join(es.dir, "environment_issues.json"), &es.EnvironmentIssues)
	readJSON(filepath.Join(es.dir, "role_affinities.json"), &es.RoleAffinities)
	readJSON(filepath.Join(es.dir, "decision_precedents.json"), &es.DecisionPrecedents)
	// A28 DB collapse: try SQLite backend first for experience graph
	if dbNodes, err := es.backend.LoadExperienceNodes(context.Background()); err == nil && len(dbNodes) > 0 {
		es.Nodes = dbNodes
	} else {
		readJSON(filepath.Join(es.dir, "experience_nodes.json"), &es.Nodes)
	}
	if dbEdges, err := es.backend.LoadExperienceEdges(context.Background()); err == nil && len(dbEdges) > 0 {
		es.Edges = dbEdges
	} else {
		readJSON(filepath.Join(es.dir, "experience_edges.json"), &es.Edges)
	}
	readJSON(filepath.Join(es.dir, "jit_candidates.json"), &es.JITCandidates)
	// A31 DB collapse: try SQLite backend first for promotion audit logs
	if dbLogs, err := es.backend.LoadPromotionAuditLogs(context.Background()); err == nil && len(dbLogs) > 0 {
		es.PromotionAuditLogs = dbLogs
	} else {
		readJSON(filepath.Join(es.dir, "promotion_audit_logs.json"), &es.PromotionAuditLogs)
	}
	readJSON(filepath.Join(es.dir, "state_potentials.json"), &es.StatePotentials)

	// Apply temporal decay on load (ADDED 2026-08-16)
	es.applyTemporalDecayLocked()
}

// monthlyDecayRate (ADDED 2026-08-16): multiplier applied to scores per month of inactivity.
// 0.9 means a skill loses 10% of its score every 30 days it remains unused.
const monthlyDecayRate = 0.9

func (es *ExperienceStore) applyTemporalDecayLocked() {
	now := time.Now()
	// 1. Decay GeneratedSkills
	for id, gs := range es.GeneratedSkills {
		daysSinceUsed := now.Sub(gs.LastUsed).Hours() / 24
		if daysSinceUsed > 30 {
			months := daysSinceUsed / 30
			decay := math.Pow(monthlyDecayRate, months)
			gs.SuccessRate *= decay
			// Also decay usage count to allow new variations to compete
			gs.UsageCount = int(float64(gs.UsageCount) * decay)
			es.GeneratedSkills[id] = gs
		}
	}

	// 2. Decay SkillAffinities
	for key, sa := range es.SkillAffinities {
		if sa.IsSeed {
			continue // Seeds don't decay
		}
		daysSinceUpdated := now.Sub(sa.LastUpdated).Hours() / 24
		if daysSinceUpdated > 30 {
			months := daysSinceUpdated / 30
			decay := math.Pow(monthlyDecayRate, months)
			sa.ConfidenceDelta *= decay
			sa.SampleCount = int(float64(sa.SampleCount) * decay)
			es.SkillAffinities[key] = sa
		}
	}
}

func (es *ExperienceStore) loadSeeds() {
	path := filepath.Join(es.dir, "seed.json")
	var seeds struct {
		Patterns     []TaskPattern          `json:"task_patterns"`
		Affinities   []*SkillAffinity       `json:"skill_affinities"`
		AntiPatterns []AntiPatternPrecedent `json:"anti_patterns"`
	}
	readJSON(path, &seeds)

	for _, p := range seeds.Patterns {
		p.IsSeed = true
		if _, ok := es.TaskPatterns[p.ID]; !ok {
			es.TaskPatterns[p.ID] = p
		}
	}
	for _, a := range seeds.Affinities {
		a.IsSeed = true
		key := fmt.Sprintf("%s:%s", a.BaseCapability, a.AddedSkill)
		if _, ok := es.SkillAffinities[key]; !ok {
			es.SkillAffinities[key] = a
		}
	}
	for _, ap := range seeds.AntiPatterns {
		es.AntiPatternStore.Upsert(context.Background(), ap)
	}

	// Always inject canonical anti-pattern precedents — guaranteed presence
	// even if seed.json is absent or incomplete. Safe: Upsert is idempotent.
	es.seedCanonicalAntiPatterns(context.Background())
}

// seedCanonicalAntiPatterns injects the curated pitfall precedents from
// the 2026-08-27 Anti-Cliff development retrospective into the store.
// Idempotent by ID — repeated calls are harmless.
func (es *ExperienceStore) seedCanonicalAntiPatterns(ctx context.Context) {
	seeds := []AntiPatternPrecedent{
		{
			ID:               "prec_antipattern_large_file_patch",
			Category:         "golang_development",
			AntiPattern:      "Using patch tool on files exceeding 1000 lines",
			CorrectPattern:   "Use read_file with targeted offset/limit + write_file to overwrite the target method block, or split the logic into smaller files",
			TriggerCondition: "file_lines > 1000",
			Symptom:          "Anchor drift: patch cannot find old_string despite correct content — repeated 'not found' errors",
			SourceText:       "Large file patch anchor drift in Go orchestrator",
			Confidence:       0.95,
		},
		{
			ID:               "prec_antipattern_assetmanager_threshold",
			Category:         "golang_development",
			AntiPattern:      "Assume NewAssetManager(storePath, 0) disables the threshold",
			CorrectPattern:   "Explicitly set threshold=1 to force side-load for any non-empty payload; the constructor silently falls back to 50KB default when 0 is passed",
			TriggerCondition: "calling NewAssetManager with threshold=0",
			Symptom:          "40KB payload silently inlined despite expecting ref-based side-load — false test failures",
			SourceText:       "AssetManager threshold=0 fallback trap in Go orchestrator",
			Confidence:       0.9,
		},
		{
			ID:               "prec_antipattern_mock_interface",
			Category:         "golang_development",
			AntiPattern:      "Hand-write mock structs from memory without verifying the interface contract",
			CorrectPattern:   "Before writing a new mock, search for an existing mock template in *_test.go files and copy its method signatures; verify against the ITaskStore / IEmbeddingClient interface",
			TriggerCondition: "implementing a mock for ITaskStore, IEmbeddingClient, or similar interfaces",
			Symptom:          "Compile error: mock does not implement interface (missing Claim/EmbedWithModel method) — 2-3 compiler round-trips to discover",
			SourceText:       "Mock interface mismatch in Go orchestrator tests",
			Confidence:       0.85,
		},
		{
			ID:               "prec_antipattern_static_threshold",
			Category:         "architecture",
			AntiPattern:      "Use a single global static byte threshold for ref-based handoff across all models",
			CorrectPattern:   "Derive threshold adaptively from the active model's MaxContextWindow (e.g. 15% of window × 4 bytes/token); allow explicit override",
			TriggerCondition: "designing a size/threshold decision that must work across models with 8k–2M context windows",
			Symptom:          "Large-window models (Gemini 128k/2M) over-side-load small payloads; small-window models (8k) under-side-load and overflow",
			SourceText:       "Static ref-handoff threshold fails across heterogeneous model context windows",
			Confidence:       0.9,
		},
		{
			ID:               "prec_antipattern_role_protected_context",
			Category:         "architecture",
			AntiPattern:      "Compress system prompt alongside user history to reclaim token budget",
			CorrectPattern:   "Enforce two-zone budget isolation: Protected Zone (SOP/Goal/OutputContract) is zero-compression, Volatile Zone (tool output/logs) is compressible",
			TriggerCondition: "session exceeds hundreds of turns and compression is triggered",
			Symptom:          "Model begins violating rules it was originally instructed to follow (Compaction Cliff, arXiv:2608.22752)",
			SourceText:       "Compaction Cliff: system prompt and user history compressed equally",
			Confidence:       0.95,
		},
	}
	for _, s := range seeds {
		es.AntiPatternStore.Upsert(ctx, s)
	}
}

func (es *ExperienceStore) PersistAll(ctx context.Context) error {
	// Phase 1: Marshal to JSON + snapshot DB data under RLock.
	// (audit: was RLock held during 13+ file writes + DB saves, blocking all writers.)
	es.Mu.RLock()

	taskPatternsData, _ := json.MarshalIndent(es.TaskPatterns, "", "  ")
	roleProfilesData, _ := json.MarshalIndent(es.RoleProfiles, "", "  ")
	skillAffinitiesData, _ := json.MarshalIndent(es.SkillAffinities, "", "  ")
	routingMatrixData, _ := json.MarshalIndent(es.RoutingMatrix, "", "  ")
	generatedSkillsData, _ := json.MarshalIndent(es.GeneratedSkills, "", "  ")
	archivedSkillsData, _ := json.MarshalIndent(es.ArchivedSkills, "", "  ")
	environmentIssuesData, _ := json.MarshalIndent(es.EnvironmentIssues, "", "  ")
	roleAffinitiesData, _ := json.MarshalIndent(es.RoleAffinities, "", "  ")
	decisionPrecedentsData, _ := json.MarshalIndent(es.DecisionPrecedents, "", "  ")

	// Snapshot DB iteration data (shallow copy — values are pointers, safe for read-only DB save)
	nodesCopy := make([]*ExperienceNode, 0, len(es.Nodes))
	for _, n := range es.Nodes {
		nodesCopy = append(nodesCopy, n)
	}
	edgesCopy := append([]ExperienceEdge(nil), es.Edges...)
	experienceNodesData, _ := json.MarshalIndent(es.Nodes, "", "  ")
	experienceEdgesData, _ := json.MarshalIndent(es.Edges, "", "  ")
	jitCandidatesData, _ := json.MarshalIndent(es.JITCandidates, "", "  ")
	logsCopy := append([]PromotionAuditLog(nil), es.PromotionAuditLogs...)
	promotionAuditLogsData, _ := json.MarshalIndent(es.PromotionAuditLogs, "", "  ")
	statePotentialsData, _ := json.MarshalIndent(es.StatePotentials, "", "  ")

	es.Mu.RUnlock()

	// Phase 2: Write to disk + DB outside lock (slow I/O, no lock needed)
	writeBytes(filepath.Join(es.dir, "task_patterns.json"), taskPatternsData)
	writeBytes(filepath.Join(es.dir, "role_profiles.json"), roleProfilesData)
	writeBytes(filepath.Join(es.dir, "skill_affinity.json"), skillAffinitiesData)
	writeBytes(filepath.Join(es.dir, "routing_matrix.json"), routingMatrixData)
	writeBytes(filepath.Join(es.dir, "generated_skills.json"), generatedSkillsData)
	writeBytes(filepath.Join(es.dir, "archived_skills.json"), archivedSkillsData)
	writeBytes(filepath.Join(es.dir, "environment_issues.json"), environmentIssuesData)
	writeBytes(filepath.Join(es.dir, "role_affinities.json"), roleAffinitiesData)
	writeBytes(filepath.Join(es.dir, "decision_precedents.json"), decisionPrecedentsData)
	// A28 DB collapse: save experience graph to SQLite backend + file fallback
	for _, node := range nodesCopy {
		es.backend.SaveExperienceNode(ctx, node)
	}
	for _, edge := range edgesCopy {
		es.backend.SaveExperienceEdge(ctx, edge)
	}
	writeBytes(filepath.Join(es.dir, "experience_nodes.json"), experienceNodesData)
	writeBytes(filepath.Join(es.dir, "experience_edges.json"), experienceEdgesData)
	writeBytes(filepath.Join(es.dir, "jit_candidates.json"), jitCandidatesData)
	// A31 DB collapse: save promotion audit logs to SQLite backend + file fallback
	for _, log := range logsCopy {
		es.backend.SavePromotionAuditLog(ctx, &log)
	}
	writeBytes(filepath.Join(es.dir, "promotion_audit_logs.json"), promotionAuditLogsData)
	writeBytes(filepath.Join(es.dir, "state_potentials.json"), statePotentialsData)
	return nil
}

func (es *ExperienceStore) getConfidenceThreshold() float64 {
	if es.System != nil && es.System.ConfidenceThreshold > 0 {
		return es.System.ConfidenceThreshold
	}
	return confidenceThresholdFallback
}

func (es *ExperienceStore) getMaxDecisionOutcomes() int {
	if es.System != nil && es.System.MaxDecisionOutcomes > 0 {
		return es.System.MaxDecisionOutcomes
	}
	return maxDecisionOutcomesFallback
}

// maxPromotionAuditLogsFallback bounds PromotionAuditLogs when no explicit
// retention setting is configured. See STORAGE_COMPACTION_DESIGN.md §3.2.
const maxPromotionAuditLogsFallback = 1000

func (es *ExperienceStore) getMaxPromotionAuditLogs() int {
	if es.System != nil && es.System.Retention.MaxPromotionAuditLogs > 0 {
		return es.System.Retention.MaxPromotionAuditLogs
	}
	return maxPromotionAuditLogsFallback
}

// RecordTaskCompletion records the outcome of a completed task: per-step
// role/skill affinity updates, a task-pattern upsert for JIT-composition
// suggestions, and decision outcomes for QueryDecisionAdvice.
//
// attributableFailure (ADDED 2026-08-07): only meaningful when success is
// false. True means the failure has a specific, positive signal that it
// reflects on the skill/role choice itself (not on infrastructure the
// skill had no control over) -- see applyTaskLevelPenalty's doc comment
// for the exact reasoning and which failure classes qualify. Callers
// should default to false (no penalty) whenever the cause is ambiguous;
// under-penalizing on uncertain signal is the safer failure mode than
// wrongly punishing a skill for a rate limit or a network blip it had no
// way to avoid.
//
// As of 2026-09-06, GetTelemetrySnapshot has zero call sites in core/
// or tools/. It remains in the interface for a future admin telemetry
// tool or dashboard endpoint.
func (es *ExperienceStore) GetTelemetrySnapshot(detailed bool) map[string]any {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	patterns := make(map[string]TaskPattern, len(es.TaskPatterns))
	for id, p := range es.TaskPatterns {
		sanitized := p
		sanitized.TaskID = "" // Always hide task instance ID
		if !detailed {
			sanitized.SourceText = ""
			sanitized.Embedding = nil
		}
		patterns[id] = sanitized
	}

	profiles := make(map[string]RoleProfile, len(es.RoleProfiles))
	for id, rp := range es.RoleProfiles {
		profiles[id] = *rp
	}

	skills := make(map[string]GeneratedSkill, len(es.GeneratedSkills))
	for id, gs := range es.GeneratedSkills {
		sanitized := *gs
		if !detailed {
			sanitized.SourceText = ""
			sanitized.Embedding = nil
		}
		skills[id] = sanitized
	}

	return map[string]any{
		"timestamp":        time.Now(),
		"task_patterns":    patterns,
		"role_profiles":    profiles,
		"generated_skills": skills,
		"metadata": map[string]any{
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
		},
	}
}

// GetRoleProfilesSnapshot returns a shallow copy of all role profiles.
// As of 2026-09-06, called only by GetTelemetrySnapshot (which is itself
// an orphan). It remains in the interface for the same future admin tool.
func (es *ExperienceStore) GetRoleProfilesSnapshot() map[string]*RoleProfile {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	snapshot := make(map[string]*RoleProfile, len(es.RoleProfiles))
	for id, rp := range es.RoleProfiles {
		profileCopy := *rp
		snapshot[id] = &profileCopy
	}
	return snapshot
}

// GetTaskPatternsSnapshot returns a shallow copy of all task patterns.
// As of 2026-09-06, called only by GetTelemetrySnapshot (which is itself
// an orphan). It remains in the interface for the same future admin tool.
func (es *ExperienceStore) GetTaskPatternsSnapshot() map[string]TaskPattern {
	es.Mu.RLock()
	defer es.Mu.RUnlock()

	snapshot := make(map[string]TaskPattern, len(es.TaskPatterns))
	for id, tp := range es.TaskPatterns {
		snapshot[id] = tp
	}
	return snapshot
}
