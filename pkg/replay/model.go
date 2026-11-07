// Package replay reconstructs historical task runs from on-disk telemetry so
// they can be replayed and inspected without a live scheduler.
package replay

// Run status values.
const (
	StatusCompleted   = "completed"
	StatusPartial     = "partial"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted"
	StatusArtifacts   = "artifacts"
	StatusRunning     = "running"
)

// Replay event kinds emitted by the loader. The frontend maps these onto its
// own animation vocabulary.
const (
	KindSubmit  = "submit"
	KindRole    = "role"
	KindStart   = "start"
	KindThought = "thought"
	KindTool    = "tool"
	KindRetry   = "retry"
	KindGate    = "gate"
	KindDone    = "done"
	KindFail    = "fail"
	KindFinal   = "final"
)

// Step status values.
const (
	StepCompleted = "completed"
	StepFailed    = "failed"
	StepRunning   = "running"
)

// RunSummary is the flat row used by run pickers.
type RunSummary struct {
	TaskID     string   `json:"taskId"`
	Title      string   `json:"title"`
	Task       string   `json:"task,omitempty"`
	Status     string   `json:"status"`
	Replayable bool     `json:"replayable"`
	Settled    bool     `json:"settled"`
	Dates      []string `json:"dates"`
	Segments   int      `json:"segments"`
	StartedAt  string   `json:"startedAt,omitempty"`
	EndedAt    string   `json:"endedAt,omitempty"`
	DurationMs int64    `json:"durationMs"`
	// Counts are suffixed so RunDetail's embedded summary does not collide
	// with its Steps/Events/Artifacts slices of the same names.
	Steps     int    `json:"stepCount"`
	Completed int    `json:"completedSteps"`
	Failed    int    `json:"failedSteps"`
	Tools     int    `json:"toolCalls"`
	Retries   int    `json:"retries"`
	Events    int    `json:"eventCount"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Artifacts int    `json:"artifactCount,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Step is one unit of work inside a run. Times are milliseconds relative to
// the start of the replayed segment.
type Step struct {
	ID         string   `json:"id"`
	Index      int      `json:"index"`
	Role       string   `json:"role,omitempty"`
	Task       string   `json:"task,omitempty"`
	Status     string   `json:"status"`
	StartMs    int64    `json:"startMs"`
	EndMs      int64    `json:"endMs"`
	DurationMs int64    `json:"durationMs"`
	Tools      int      `json:"tools"`
	Retries    int      `json:"retries"`
	DependsOn  []string `json:"dependsOn,omitempty"`
	Thought    string   `json:"thought,omitempty"`
	Error      string   `json:"error,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	// ResultRef names the stored step output inside the task's artifact dir,
	// matched from the filename rather than telemetry (see attributeArtifacts).
	ResultRef string `json:"resultRef,omitempty"`
}

// Event is a normalized timeline entry. StepID is empty for run-level events.
type Event struct {
	AtMs   int64  `json:"atMs"`
	Kind   string `json:"kind"`
	StepID string `json:"stepId,omitempty"`
	Text   string `json:"text,omitempty"`
	Tool   string `json:"tool,omitempty"`
}

// Graph mirrors an explicit task-graph definition when one exists on disk.
type Graph struct {
	Goal  string      `json:"goal,omitempty"`
	Steps []GraphStep `json:"steps"`
}

// GraphStep is a node of an explicit task graph.
type GraphStep struct {
	ID        string   `json:"id"`
	Role      string   `json:"roleId,omitempty"`
	Task      string   `json:"task,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// RunDetail is the full replay payload for a single run.
type RunDetail struct {
	RunSummary
	Steps     []Step   `json:"steps"`
	Events    []Event  `json:"events"`
	Graph     *Graph   `json:"graph,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	LogFiles  []string `json:"logFiles,omitempty"`
}

// ArtifactOnly describes a task directory that has stored step results but no
// telemetry, so it can be listed without pretending it is replayable.
type ArtifactOnly struct {
	TaskID    string `json:"taskId"`
	Artifacts int    `json:"artifacts"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// Index is the run-picker payload.
type Index struct {
	Runs         []RunSummary   `json:"runs"`
	ArtifactOnly []ArtifactOnly `json:"artifactOnly"`
}
