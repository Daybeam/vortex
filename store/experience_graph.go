package store

import "time"

// ExperienceNode represents a single decision or step-execution event.
// Analogous to KOPE's "Case" — the source of truth for measured outcomes.
type ExperienceNode struct {
	NodeID      string    `json:"node_id"`
	TaskID      string    `json:"task_id"`
	StepID      string    `json:"step_id"`
	RoleID      string    `json:"role_id"`
	Capability  string    `json:"capability"`
	Strategy    string    `json:"strategy"`               // What was attempted
	Action      string    `json:"action"`                 // Code/command/tool used
	Outcome     string    `json:"outcome"`                // "success" | "failure" | "blocked"
	ErrorSignal string    `json:"error_signal,omitempty"` // Error message / diagnostic
	ModelID     string    `json:"model_id,omitempty"`     // FMC: Model that produced this outcome (ADDED 2026-09-08)
	FailureMode string    `json:"failure_mode,omitempty"` // FMC: Structured label (ADDED 2026-09-06)
	Critique    string    `json:"critique,omitempty"`     // FMC: Natural-language critique (ADDED 2026-09-06)
	Confidence  float64   `json:"confidence,omitempty"`
	Timestamp   time.Time `json:"timestamp"`

	// Semantic embedding for warm-context retrieval
	SourceText     string    `json:"source_text,omitempty"`
	Embedding      []float32 `json:"embedding,omitempty"`
	EmbeddingModel string    `json:"embedding_model,omitempty"`
}

// ExperienceEdge represents a directed relationship between two nodes.
type ExperienceEdge struct {
	SourceID string `json:"source_id"` // Parent node
	TargetID string `json:"target_id"` // Child node
	Relation string `json:"relation"`  // "evolved_from" | "retried_after_failure" | "alternative_branch" | "next_step"
}

// ExperienceGraph is the in-memory directed graph of all experience nodes.
// Persisted to SQLite (nodes table + edges table) via ExperienceStore.
type ExperienceGraph struct {
	Nodes map[string]*ExperienceNode `json:"nodes"`
	Edges []ExperienceEdge           `json:"edges"`
}
