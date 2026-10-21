package schemas

// ReadDep records that a step consumed an upstream step's file. Moved out of
// task.go to keep that file within its size budget.
type ReadDep struct {
	StepID       string `json:"step_id"`                 // upstream step that produced the file
	FilePath     string `json:"file_path"`               // path of the file that was read
	Evidence     string `json:"evidence"`                // trace entry that triggered the detection (e.g. "tool:read_file arg:path")
	SourceSHA256 string `json:"source_sha256,omitempty"` // content hash at time of read (StagedWorkspace only)
	SnapshotID   string `json:"snapshot_id,omitempty"`   // workspace snapshot ID (StagedWorkspace only)
}
