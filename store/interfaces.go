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

// StaleTaskSweeper is an optional capability of a TaskRegistry: close task rows
// left 'running' by a process that no longer exists. Optional for the same reason
// batchLoader is — file mode has no task ledger at all, and a mock registry
// should not have to implement a sweep nobody will ask it for. Consumers detect
// it by type assertion and skip when it is absent.
type StaleTaskSweeper interface {
	// SweepStaleRunning fails rows still 'running' whose last write predates
	// cutoff and returns the task IDs this call actually closed. The UPDATE
	// carries the same predicate as the SELECT, so a row a live hub touched in
	// between is left alone, and two hubs sweeping at once cannot both report
	// the same ID.
	SweepStaleRunning(ctx context.Context, cutoff time.Time) ([]string, error)
}

// BaselineStore pins and retrieves SOP/Role versions at task creation time.
// Nil-safe: consumers must nil-check before use (mirrors TaskRegistry pattern).
// See docs/BASELINE_VERSION_PINNING_DESIGN.md
type BaselineStore interface {
	PinBaseline(ctx context.Context, taskID, sopID, sopVer, roleID, roleVer string) error
	GetBaseline(ctx context.Context, taskID string) (*TaskBaseline, error)
	LoadPinnedSOP(ctx context.Context, sopID, sopVer string) (*schemas.SOP, error)
	LoadPinnedRole(ctx context.Context, roleID, roleVer string) (*config.Role, error)
}

// TaskBaseline records which SOP/Role version a task was created against.
type TaskBaseline struct {
	TaskID      string
	SOPID       string
	SOPVersion  string
	RoleID      string
	RoleVersion string
}

// ITaskStore defines the interface for task-scoped persistent storage.
type ITaskStore interface {
	Set(ctx context.Context, taskID, stepID string, result *StepResult) error
	Get(ctx context.Context, taskID, stepID string) (*StepResult, error)
	// GetBatch fetches multiple step results in a single call (audit P-H1-H4).
	// Replaces N+1 query patterns at 4 call sites. Returns a map keyed by stepID;
	// missing steps are simply absent from the map (not an error).
	GetBatch(ctx context.Context, taskID string, stepIDs []string) (map[string]*StepResult, error)
	GetByRef(ctx context.Context, ref string) (*StepResult, error)
	ClearTask(ctx context.Context, taskID string) (int, error)
	// Claim atomically attempts to mark a step as running. Returns true if successful.
	Claim(ctx context.Context, taskID, stepID string) (bool, error)
}

// batchLoader is an optional interface that ITaskBackend implementations can
// satisfy to provide true batch loading (single SQL query instead of N).
// TaskStore.GetBatch uses type assertion to detect this capability.
type batchLoader interface {
	LoadBatch(ctx context.Context, taskID string, stepIDs []string) (map[string][]byte, error)
}

// IEmbeddingClient defines a simple interface for vector embeddings.
type IEmbeddingClient interface {
	EmbedWithModel(ctx context.Context, text string) ([]float32, string, error)
}

// IRerankerClient scores candidate documents for semantic relevance to a query.
// Scores are runtime-only — never persisted to ExperienceNode or TaskPattern.
// Callers must convert scores to relative ranks before cross-backend fusion
// (whitepaper §1.3: absolute scores/confidence are backend-specific).
type IRerankerClient interface {
	Rerank(ctx context.Context, query string, documents []string) (scores []float64, err error)
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

	// SaveExperienceBatch saves all nodes, edges, and logs in a single transaction
	// (audit PERF-1: was N+1 individual saves causing 5000-10000 sequential DB writes).
	SaveExperienceBatch(ctx context.Context, nodes []*ExperienceNode, edges []ExperienceEdge, logs []PromotionAuditLog) error
}

// IExperienceStore defines the interface for cross-task learned knowledge.
type IExperienceStore interface {
	RecordTaskCompletion(ctx context.Context, taskID string, records []StepRecord, overallConf float64, decisions []map[string]any, success bool, attributableFailure bool) error
	UpdateRouteWeight(ctx context.Context, roleID, modelID, skill, capability string, score float64, success bool) error
	SelectSkill(ctx context.Context, availableSkills []string, capability, roleID, modelID string, epsilon float64) string
	AddGeneratedSkill(ctx context.Context, skill *GeneratedSkill) error
	QueryRoleAdvice(ctx context.Context, roleID string) map[string]any
	// Experience Graph Memory (docs/architecture/EXPERIENCE_GRAPH_DESIGN.md)
	RetrieveRelevantExperience(ctx context.Context, query []float32, capability, errorSignal, domain string, tokenBudget int) []*ExperienceNode
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
	// RecordSkillExposure / TakeSkillExposures hand dynamically retrieved
	// skills from the spawn path to RecordTaskCompletion's usage credit.
	RecordSkillExposure(taskID, stepID string, ids []string)
	TakeSkillExposures(taskID, stepID string) []string
	// SetSkillLivenessProbe lets the runtime drop generated skills whose
	// backing tool has expired (JIT tools have a TTL; skill records do not).
	// Nil disables filtering.
	SetSkillLivenessProbe(probe func(skillID string) bool)

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
	// SaveItemWithEmbedding persists an item with its pre-computed embedding vector.
	// DB-audit P1 (VECTOR_EMPTY): SaveItem never saved the embedding column, so
	// SearchItems (WHERE embedding IS NOT NULL) always returned 0 rows.
	SaveItemWithEmbedding(ctx context.Context, category, key, content string, metadata map[string]any, embedding []float32, embeddingModel string) error
	LoadCategory(ctx context.Context, category string) (map[string]string, error)
}

// IAntiPatternBackend defines low-level persistence for anti-pattern precedents.
type IAntiPatternBackend interface {
	Upsert(ctx context.Context, p AntiPatternPrecedent) error
	LoadAll(ctx context.Context) ([]AntiPatternPrecedent, error)
}

// PendingReplyRow is the DB representation of a chat turn awaiting a reply.
// See docs/plan/CHAT_REPLY_FAULT_RECOVERY_PLAN.md.
type PendingReplyRow struct {
	SessionID string
	MessageID string
	Content   string
	StartedAt time.Time
}

// IChatBackend defines persistence for chat sessions and messages (A20 DB collapse).
type IChatBackend interface {
	SaveSession(ctx context.Context, sessionID, rootID, activeLeafID string, createdAt time.Time) error
	SaveMessage(ctx context.Context, msg *ChatMessageRow) error
	LoadSession(ctx context.Context, sessionID string) (rootID, activeLeafID string, messages map[string]*ChatMessageRow, err error)
	ListSessions(ctx context.Context, limit, offset int) ([]ChatSessionRow, error)

	// SavePendingReply records that a user message is awaiting an assistant
	// reply. Upsert on session_id: a new turn replaces any stale pending mark.
	SavePendingReply(ctx context.Context, row PendingReplyRow) error
	// ClearPendingReply removes the pending mark for a session (turn completed).
	ClearPendingReply(ctx context.Context, sessionID string) error
	// ListPendingReplies returns all sessions with an outstanding pending mark.
	// Used by the startup scan to detect turns interrupted by a crash.
	ListPendingReplies(ctx context.Context) ([]PendingReplyRow, error)
}

// ChatMessageRow is the DB representation of a chat message.
type ChatMessageRow struct {
	ID        string
	SessionID string
	ParentID  string
	// ClientID is the caller-supplied message id (web optimistic id,
	// Telegram message id). Persisted so later turns that reference it
	// (parent_id / reply_to) can be resolved to the stored ID.
	ClientID   string
	ReplyTo    string // quoted message ID (Mode 2)
	ForkedFrom string // branch point this message forked from (Mode 1)
	Role       string
	Content    string
	CreatedAt  time.Time
}

// ChatSessionRow is the DB representation of a chat session.
type ChatSessionRow struct {
	ID           string    `json:"id"`
	RootID       string    `json:"root_id,omitempty"`
	ActiveLeafID string    `json:"active_leaf_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// OutboxItem is one notification waiting to reach a user through whichever
// transport can currently deliver it.
type OutboxItem struct {
	ID        int64
	SessionID string
	Kind      string
	DedupeKey string
	Text      string

	// MetaJSON is opaque to the store: the producer and the deliverer agree on
	// its contents, so the queue itself never needs a schema per notification.
	MetaJSON string

	CreatedAt time.Time

	// Attempts counts claims, not delivery failures reported by the caller.
	Attempts int
}

// IChatOutbox is the durable queue between "something happened to a session"
// and "a transport pushed it to the user".
//
// It exists because those two sides are not always in the same process: the
// task finishes in whichever hub runs it, but only the hub holding the bot
// token can speak to Telegram, and neither of them may still be alive when the
// notification comes due. Producers enqueue and forget; a deliverer claims,
// sends, and confirms.
//
// Claims are leased and atomic. claim_holder doubles as the fence: once a lease
// lapses and another hub takes the row over, the previous owner's confirm call
// matches no rows and its delivery goes unrecorded rather than being credited
// to a send that never happened.
//
// Scope, stated because it decides what this queue can be used for: the SQLite
// backend is safe for several hubs sharing one file on one host, which is the
// WAL + busy_timeout configuration the runtime DB already runs. It is not safe
// for several nodes sharing one file over RWX/NFS, which is why the hub HA plan
// rejects that shape outright (docs/plan/HUB_HA_PRIMARY_STANDBY_DESIGN.md D2).
// On k3s a hub therefore either owns the channel as the single writer of that
// host's file, or this interface gets a Postgres backend -- the seam is here so
// nothing above the store changes when that backend lands.
type IChatOutbox interface {
	// Enqueue stores a notification, reporting false when dedupe_key already
	// exists so a producer retrying after a lost response cannot double-send.
	Enqueue(ctx context.Context, item OutboxItem) (bool, error)

	// ClaimPending reserves up to limit claimable rows for holder for the lease
	// duration and returns them.
	ClaimPending(ctx context.Context, holder string, limit int, lease time.Duration) ([]OutboxItem, error)

	// ClaimPendingKinds is ClaimPending restricted to the listed row kinds.
	// Independent pumps must not consume one another's delivery work.
	ClaimPendingKinds(ctx context.Context, holder string, limit int, lease time.Duration, kinds []string) ([]OutboxItem, error)

	// MarkDelivered closes a row still held by holder, reporting false when the
	// claim was already lost.
	MarkDelivered(ctx context.Context, id int64, holder string) (bool, error)

	// MarkFailed releases a row still held by holder for a later pass, or drops
	// it once it has run out of attempts. reason is recorded for the operator.
	MarkFailed(ctx context.Context, id int64, holder, reason string) error

	// Purge deletes rows that reached a terminal state before the bound and
	// reports how many went.
	Purge(ctx context.Context, before time.Time) (int, error)

	// RowExists reports whether a row with the given dedupe_key exists in
	// the outbox, regardless of status. Used by the startup scan to
	// determine whether a chat reply was enqueued before a crash.
	RowExists(ctx context.Context, dedupeKey string) (bool, error)
}
