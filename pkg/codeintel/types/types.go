package types

// SymbolKind represents the kind of a code symbol
type SymbolKind string

const (
	KindFunction  SymbolKind = "function"
	KindMethod    SymbolKind = "method"
	KindInterface SymbolKind = "interface"
	KindStruct    SymbolKind = "struct"
	KindType      SymbolKind = "type"
	KindVar       SymbolKind = "var"
	KindConst     SymbolKind = "const"
	KindField     SymbolKind = "field"
)

// Position is a location in a source file
type Position struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Symbol represents a named code entity
type Symbol struct {
	Name       string     `json:"name"`
	Kind       SymbolKind `json:"kind"`
	Package    string     `json:"package"`
	File       string     `json:"file"`
	Line       int        `json:"line"`
	Signature  string     `json:"signature"`
	DocComment string     `json:"doc_comment,omitempty"`
	Exported   bool       `json:"exported"`
	Receiver   string     `json:"receiver,omitempty"`
}

// CallEdge represents a call relationship
type CallEdge struct {
	CallerFile string `json:"caller_file"`
	CallerName string `json:"caller_name"`
	CalleeName string `json:"callee_name"`
	CallLine   int    `json:"call_line"`
}

// ImplementsEdge represents an interface implementation relationship
type ImplementsEdge struct {
	TypeName      string `json:"type_name"`
	TypeFile      string `json:"type_file"`
	InterfaceName string `json:"interface_name"`
	InterfaceFile string `json:"interface_file"`
}

// Reference is a usage location of a symbol
type Reference struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Diagnostic is a type error or warning
type Diagnostic struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Message string `json:"message"`
	Kind    string `json:"kind"` // "error" | "warning"
}

// ImpactResult holds the blast radius of changing a symbol
type ImpactResult struct {
	TargetSymbol    string   `json:"target_symbol"`
	DirectCallers   []Symbol `json:"direct_callers"`
	IndirectCallers []Symbol `json:"indirect_callers"`
	Implementors    []Symbol `json:"implementors,omitempty"`
	AffectedFiles   []string `json:"affected_files"`
	RiskLevel       string   `json:"risk_level"` // low/medium/high/critical
}

// SessionStatus tracks the state of an edit session
type SessionStatus string

const (
	SessionOpen       SessionStatus = "open"
	SessionValidated  SessionStatus = "validated"
	SessionCommitting SessionStatus = "committing" // crash-recovery sentinel
	SessionCommitted  SessionStatus = "committed"
	SessionRolledBack SessionStatus = "rolled_back"
	SessionConflict   SessionStatus = "conflict"
)

// FileEntry tracks per-file state within a session snapshot
type FileEntry struct {
	// SnapshotPath is the path of the saved original inside the snapshot dir
	SnapshotPath string `json:"snapshot_path"`
	// TargetPath is the real on-disk destination
	TargetPath string `json:"target_path"`
	// TmpPath is the .tmp_ci file written before rename
	TmpPath string `json:"tmp_path"`
	// Renamed is true after os.Rename succeeded for this file
	Renamed bool `json:"renamed"`
	// OriginalHash is the SHA-256 of the original content (for conflict detection)
	OriginalHash string `json:"original_hash"`
}

// Manifest is written to disk before commit begins (crash-recovery journal)
type Manifest struct {
	SessionID string        `json:"session_id"`
	Phase     SessionStatus `json:"phase"`
	Files     []FileEntry   `json:"files"`
}

// EditSession tracks a group of atomic edits (in-memory state)
type EditSession struct {
	ID       string            `json:"id"`
	Status   SessionStatus     `json:"status"`
	Edits    map[string]string `json:"edits"`    // file -> new content
	Original map[string]string `json:"original"` // file -> original content
	Errors   []Diagnostic      `json:"errors,omitempty"`
	// SnapshotDir is the directory holding the manifest + original file copies
	SnapshotDir string `json:"snapshot_dir,omitempty"`
}

// SymbolView is a 360-degree view of a symbol
type SymbolView struct {
	Symbol        Symbol      `json:"symbol"`
	Callers       []Symbol    `json:"callers"`
	Callees       []Symbol    `json:"callees"`
	Implements    []string    `json:"implements,omitempty"`
	ImplementedBy []string    `json:"implemented_by,omitempty"`
	References    []Reference `json:"references"`
}

// ConflictError is returned when a concurrent modification is detected
type ConflictError struct {
	File         string `json:"file"`
	ExpectedHash string `json:"expected_hash"`
	ActualHash   string `json:"actual_hash"`
}

func (e *ConflictError) Error() string {
	return "conflict on " + e.File + ": file was modified externally since session began"
}
