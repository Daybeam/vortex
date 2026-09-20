package store

import (
	"context"
	"time"

	"github.com/daybeam/vortex/config"
	pkg_interfaces "github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/schemas"
)

// ITaskBackend defines the low-level persistence operations.
type ITaskBackend interface {
	Save(ctx context.Context, taskID, stepID string, data []byte) error
	Load(ctx context.Context, taskID, stepID string) ([]byte, error)
	Delete(ctx context.Context, taskID string) (int, error)
	Claim(ctx context.Context, taskID, stepID string) (bool, error)
}

// TaskRegistry persists task-level lifecycle state (as opposed to ITaskStore,
// which is step-scoped). It is intentionally separate so FileTaskBackend and
// other ITaskBackend implementations do not have to implement it.
// Consumers must treat a nil TaskRegistry as "persistence unavailable".
type TaskRegistry interface {
	SaveTask(ctx context.Context, taskID, status string, graphJSON []byte) error
	UpdateTaskStatus(ctx context.Context, taskID, status string) error
	LoadTask(ctx context.Context, taskID string) (*TaskRow, []byte, error)
	ListTasks(ctx context.Context, limit int) ([]TaskRow, error)
}

// ITaskStore defines the interface for task-scoped persistent storage.
type ITaskStore interface {
	Set(ctx context.Context, taskID, stepID string, result *StepResult) error
	Get(ctx context.Context, taskID, stepID string) (*StepResult, error)
	GetByRef(ctx context.Context, ref string) (*StepResult, error)
	ClearTask(ctx context.Context, taskID string) (int, error)
	// Claim atomically attempts to mark a step as running. Returns true if successful.
	Claim(ctx context.Context, taskID, stepID string) (bool, error)
}

// IEmbeddingClient defines a simple interface for vector embeddings.
type IEmbeddingClient interface {
	EmbedWithModel(ctx context.Context, text string) ([]float32, string, error)
}

// IExperienceBackend defines low-level persistence operations.
type IExperienceBackend interface {
	Save(ctx context.Context, data map[string]any) error
	Load(ctx context.Context) (map[string]any, error)

	// Incremental updates
	SaveTaskPattern(ctx context.Context, p *TaskPattern) error
	SaveRoleProfile(ctx context.Context, rp *RoleProfile) error
	SaveRouteWeight(ctx context.Context, rw *RouteWeight) error
	SaveGeneratedSkill(ctx context.Context, gs *GeneratedSkill) error
	SaveDecisionOutcome(ctx context.Context, do *DecisionOutcome) error
	SaveSkillAffinity(ctx context.Context, sa *SkillAffinity) error
	SaveEnvironmentIssue(ctx context.Context, ei *EnvironmentIssue) error
	SaveRoleAffinity(ctx context.Context, roleID, taskType string, score float64) error
	SaveDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error
	SaveStatePotential(ctx context.Context, sp *StatePotential) error
	SaveCooccurrence(ctx context.Context, c *CooccurrenceEntry) error
	GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*CooccurrenceEntry, error)
	GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*CooccurrenceEntry, error)

	// Experience Graph (A28 DB collapse)
	SaveExperienceNode(ctx context.Context, node *ExperienceNode) error
	SaveExperienceEdge(ctx context.Context, edge ExperienceEdge) error
	LoadExperienceNodes(ctx context.Context) (map[string]*ExperienceNode, error)
	LoadExperienceEdges(ctx context.Context) ([]ExperienceEdge, error)

	// Promotion Audit (A31 DB collapse)
	SavePromotionAuditLog(ctx context.Context, log *PromotionAuditLog) error
	LoadPromotionAuditLogs(ctx context.Context) ([]PromotionAuditLog, error)
}

// IExperienceStore defines the interface for cross-task learned knowledge.
type IExperienceStore interface {
	RecordTaskCompletion(ctx context.Context, taskID string, records []StepRecord, overallConf float64, decisions []map[string]any, success bool, attributableFailure bool) error
	UpdateRouteWeight(ctx context.Context, roleID, modelID, skill, capability string, score float64, success bool) error
	SelectSkill(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string
	AddGeneratedSkill(ctx context.Context, skill *GeneratedSkill) error
	QueryRoleAdvice(ctx context.Context, roleID string) map[string]any
	// Experience Graph Memory (docs/architecture/EXPERIENCE_GRAPH_DESIGN.md)
	RetrieveRelevantExperience(ctx context.Context, query []float32, capability, errorSignal string, tokenBudget int) []*ExperienceNode
	QuerySimilarPatterns(ctx context.Context, capabilities []string, limit int) []TaskPattern
	QueryRelevantSkills(ctx context.Context, taskType string, limit int) []string
	QuerySkillsBySignal(ctx context.Context, signal string) []string
	GetOrchestrationBrief(ctx context.Context, skillIDs []string) map[string]any
	PersistAll(ctx context.Context) error
	PruneSkills() []string
	RootSkillForCapability(capability string) string
	QuerySkillRecommendations(ctx context.Context, capability string, minConfidence float64) []string
	QueryDecisionAdvice(ctx context.Context, decisionType, roleID string) *DecisionOutcome

	// Environment issue tracking
	RecordEnvironmentIssue(ctx context.Context, cmd string, message string) error
	GetEnvironmentAlerts(ctx context.Context) map[string]any

	// Accessor methods for reflection engine
	GetGeneratedSkill(id string) (*GeneratedSkill, bool)
	GetGeneratedSkillsSnapshot() map[string]*GeneratedSkill
	UpdateGeneratedSkillVerified(id string, verified bool)
	IncrementGeneratedSkillUsage(id string)

	// Role Affinity (EvoX)
	QueryRoleAffinity(ctx context.Context, roleID, taskType string) float64

	// PGPO: Query state potential for proactive intervention.
	GetStatePotential(stateHash string) float64

	// FMC: Query relevant anti-patterns from historical pitfalls.
	QueryRelevantAntiPatterns(intent string, limit int) []AntiPatternPrecedent

	// Semantic Precedent Retrieval (Priority 6)
	UpsertDecisionPrecedent(ctx context.Context, node *schemas.DecisionNode) error
	QueryDecisionPrecedents(ctx context.Context, intent string, limit int) []*schemas.DecisionNode

	// JIT & Maintenance
	QueryJITCandidates(ctx context.Context, taskText string, minSamples int, minConfidence float64, limit int) []TaskPattern
	PruneStaleJITRefs(reg *config.Registry) int
	Reindex(ctx context.Context, provider pkg_interfaces.Provider, modelID string) error
	GetRoleProfilesSnapshot() map[string]*RoleProfile
	GetTaskPatternsSnapshot() map[string]TaskPattern
	GetTelemetrySnapshot(detailed bool) map[string]any
	GetCompoundCandidates(ctx context.Context, minCount int, minSuccessRate float64) ([]*CooccurrenceEntry, error)
	GetCooccurrencePartners(ctx context.Context, toolA string, minCount int) ([]*CooccurrenceEntry, error)
	PruneByMetabolicROI() []string

	// WaitAsyncSaves waits for all in-flight async DB save goroutines to finish.
	// audit H10: prevents data loss on shutdown.
	WaitAsyncSaves()
}
type IScheduleBackend interface {
	Save(ctx context.Context, s *schemas.Schedule) error
	LoadAll(ctx context.Context) ([]*schemas.Schedule, error)
	Delete(ctx context.Context, id string) error
}

// IMemoryBankBackend defines indexing/persistence for memory bank items.
type IMemoryBankBackend interface {
	SaveItem(ctx context.Context, category, key, content string, metadata map[string]any) error
	LoadCategory(ctx context.Context, category string) (map[string]string, error)
}

// IAntiPatternBackend defines low-level persistence for anti-pattern precedents.
type IAntiPatternBackend interface {
	Upsert(ctx context.Context, p AntiPatternPrecedent) error
	LoadAll(ctx context.Context) ([]AntiPatternPrecedent, error)
}

// IChatBackend defines persistence for chat sessions and messages (A20 DB collapse).
type IChatBackend interface {
	SaveSession(ctx context.Context, sessionID, rootID, activeLeafID string, createdAt time.Time) error
	SaveMessage(ctx context.Context, msgID, sessionID, parentID, role, content string, createdAt time.Time) error
	LoadSession(ctx context.Context, sessionID string) (rootID, activeLeafID string, messages map[string]*ChatMessageRow, err error)
	ListSessions(ctx context.Context, limit int) ([]ChatSessionRow, error)
}

// ChatMessageRow is the DB representation of a chat message.
type ChatMessageRow struct {
	ID        string
	SessionID string
	ParentID  string
	Role      string
	Content   string
	CreatedAt time.Time
}

// ChatSessionRow is the DB representation of a chat session.
type ChatSessionRow struct {
	ID           string
	RootID       string
	ActiveLeafID string
	CreatedAt    time.Time
}
