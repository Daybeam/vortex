// Package tools implements all MCP tool handlers.
package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/daybeam/vortex/pkg/codeintel/analyzer"
	"github.com/daybeam/vortex/pkg/codeintel/overlay"
	t "github.com/daybeam/vortex/pkg/codeintel/types"
)

// Handler holds references to the analyzer and overlay manager.
type Handler struct {
	analyzer *analyzer.GoAnalyzer
	overlay  *overlay.Manager
	rootDir  string
}

// NewHandler creates a new tool handler.
func NewHandler(a *analyzer.GoAnalyzer, o *overlay.Manager) *Handler {
	return &Handler{analyzer: a, overlay: o}
}

// ToolDef describes an MCP tool.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is the standard MCP tool response.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock is a single content item in a tool response.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func okResult(v interface{}) ToolResult {
	data, _ := json.MarshalIndent(v, "", "  ")
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: string(data)}}}
}

func fail(msg string) ToolResult {
	return ToolResult{IsError: true, Content: []ContentBlock{{Type: "text", Text: msg}}}
}

func failf(format string, args ...interface{}) ToolResult {
	return fail(fmt.Sprintf(format, args...))
}

// AllTools returns the MCP tool definitions.
func AllTools() []ToolDef {
	prop := func(typ, desc string) string {
		return fmt.Sprintf(`{"type":%q,"description":%q}`, typ, desc)
	}
	schema := func(required []string, props ...string) json.RawMessage {
		var pairs []string
		for i := 0; i+1 < len(props); i += 2 {
			pairs = append(pairs, fmt.Sprintf("%q:%s", props[i], props[i+1]))
		}
		reqJSON := "[]"
		if len(required) > 0 {
			quoted := make([]string, len(required))
			for i, r := range required {
				quoted[i] = fmt.Sprintf("%q", r)
			}
			reqJSON = "[" + strings.Join(quoted, ",") + "]"
		}
		return json.RawMessage(fmt.Sprintf(
			`{"type":"object","properties":{%s},"required":%s,"additionalProperties":false}`,
			strings.Join(pairs, ","), reqJSON,
		))
	}
	str := func(desc string) string { return prop("string", desc) }
	boolProp := func(desc string) string { return prop("boolean", desc) }

	return []ToolDef{
		{
			Name:        "index_project",
			Description: "[Step 1: Index] Scan and index a Go project. You MUST run this tool before starting any analysis or editing task. It builds a semantic map. If you find symbols are missing, re-run this tool to refresh the cache.",
			InputSchema: schema([]string{"root_dir"}, "root_dir", str("Absolute path of the project root directory")),
		},
		{
			Name:        "find_symbol",
			Description: "[Locate Definition] Find the definition of a symbol (function/type/interface/variable). Before modifying code, you MUST use this tool to confirm the exact location, signature, and docs of the definition.",
			InputSchema: schema([]string{"name"}, "name", str("Symbol name, e.g. ProcessOrder")),
		},
		{
			Name:        "find_references",
			Description: "[Find References] Find all usages of a symbol across the entire codebase. Before renaming, deleting, or modifying a function signature, **NEVER** skip this step.",
			InputSchema: schema([]string{"name"},
				"name", str("Symbol name"),
				"file", str("Optional: narrow scope to a specific definition file"),
			),
		},
		{
			Name:        "impact_analysis",
			Description: "[High-Risk: Blast Radius Analysis] Compute the impact scope of modifying a symbol. Returns direct and indirect callers. Before modifying any exported symbol, you **MUST** run this analysis first. If the risk level is high or critical, proceed cautiously and inform the user.",
			InputSchema: schema([]string{"symbol_name"},
				"symbol_name", str("Symbol to analyze"),
				"file", str("Optional: narrow scope to a specific file"),
			),
		},
		{
			Name:        "symbol_view",
			Description: "[Panoramic View] Get the symbol's definition, callers, callees, references, and interface implementations all at once. This is the **best first-choice tool** when exploring unfamiliar code logic.",
			InputSchema: schema([]string{"name"},
				"name", str("Symbol name"),
				"file", str("Optional: provide if the name is ambiguous"),
			),
		},
		{
			Name:        "get_diagnostics",
			Description: "[Health Check] Run type checking and return all errors. After large-scale edits or before committing, run this tool to ensure you have not broken the project. It is faster than compiling.",
			InputSchema: schema(nil),
		},
		{
			Name:        "begin_session",
			Description: "[Edit SOP Step 1] Start an atomic editing session. All writes will be staged in memory. Returns session_id. **Do NOT write to disk directly; always use the session flow.**",
			InputSchema: schema(nil),
		},
		{
			Name:        "session_write",
			Description: "[Edit SOP Step 2] Stage a file modification into the session. You MUST provide the **complete new content** of the file. Note: this does not change the on-disk file until you commit.",
			InputSchema: schema([]string{"session_id", "file", "content"},
				"session_id", str("ID from begin_session"),
				"file", str("Absolute path of the file"),
				"content", str("New complete file content"),
			),
		},
		{
			Name:        "session_validate",
			Description: "[Edit SOP Step 3: Mandatory] Validate all staged modifications in memory. Checks whether your changes introduced type errors. **NEVER commit without passing validation (error_count is 0).**",
			InputSchema: schema([]string{"session_id"}, "session_id", str("Session ID")),
		},
		{
			Name:        "session_commit",
			Description: "[Edit SOP Step 4] Atomically write all staged modifications to disk. May only be called after session_validate succeeds. If it returns a CONFLICT error, the file was modified externally during your edit; you must rollback and retry.",
			InputSchema: schema([]string{"session_id"}, "session_id", str("Session ID")),
		},
		{
			Name:        "session_rollback",
			Description: "[Discard Changes] Abandon all staged modifications in the current session and clean up. If you find you have made a mess or validation fails and cannot be fixed, use this tool without hesitation.",
			InputSchema: schema([]string{"session_id"}, "session_id", str("Session ID")),
		},
		{
			Name:        "search_code",
			Description: "[Text Search] Full-text search across packages. Use when you do not know the symbol name but only partial keywords. Supports case-sensitive matching.",
			InputSchema: schema([]string{"query"},
				"query", str("Search keywords"),
				"case_sensitive", boolProp("Whether to match case-sensitively (default false)"),
			),
		},
		{
			Name:        "list_project_structure",
			Description: "[Project Overview] Return the project's directory tree structure. Use to understand code layout before starting a task. Automatically ignores irrelevant directories such as vendor, node_modules.",
			InputSchema: schema(nil,
				"depth", intProp("Search depth (default 3)"),
			),
		},
	}
}

func intProp(desc string) string {
	return fmt.Sprintf(`{"type":"integer","description":%q}`, desc)
}

// Dispatch routes a tool call to the appropriate handler.
func (h *Handler) Dispatch(name string, args map[string]interface{}) ToolResult {
	str := func(k string) string {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	boolean := func(k string) bool {
		if v, ok := args[k]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
		return false
	}

	switch name {
	case "index_project":
		return h.indexProject(str("root_dir"))
	case "find_symbol":
		return h.findSymbol(str("name"))
	case "find_references":
		return h.findReferences(str("name"), str("file"))
	case "get_callers":
		return h.getCallers(str("function_name"))
	case "get_callees":
		return h.getCallees(str("function_name"), str("file"))
	case "get_implementors":
		return h.getImplementors(str("interface_name"))
	case "symbol_view":
		return h.symbolView(str("name"), str("file"))
	case "impact_analysis":
		return h.impactAnalysis(str("symbol_name"), str("file"))
	case "list_symbols":
		return h.listSymbols(str("filter"))
	case "get_diagnostics":
		return h.getDiagnostics()
	case "read_file":
		return h.readFile(str("file"))
	case "search_code":
		return h.searchCode(str("query"), boolean("case_sensitive"))
	case "begin_session":
		return h.beginSession()
	case "session_write":
		return h.sessionWrite(str("session_id"), str("file"), str("content"))
	case "session_validate":
		return h.sessionValidate(str("session_id"))
	case "session_commit":
		return h.sessionCommit(str("session_id"))
	case "session_rollback":
		return h.sessionRollback(str("session_id"))
	case "session_status":
		return h.sessionStatus(str("session_id"))
	case "list_project_structure":
		depth := 3
		if v, ok := args["depth"]; ok {
			if f, ok := v.(float64); ok {
				depth = int(f)
			}
		}
		return h.listProjectStructure(depth)
	default:
		return failf("unknown tool: %s", name)
	}
}

func (h *Handler) indexProject(rootDir string) ToolResult {
	if rootDir == "" {
		return fail("root_dir is required")
	}
	a := analyzer.NewGoAnalyzer(rootDir)
	if err := a.Index(); err != nil {
		return failf("indexing failed: %v", err)
	}
	h.analyzer = a
	h.overlay = overlay.NewManager(a, "")
	h.rootDir = rootDir
	return okResult(map[string]string{"status": "indexed", "root": rootDir})
}

func (h *Handler) findSymbol(name string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	syms := h.analyzer.FindSymbol(name)
	return okResult(map[string]interface{}{"found": len(syms), "symbols": nilSafe(syms)})
}

func (h *Handler) findReferences(name, file string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	refs := h.analyzer.FindReferences(name, file)
	return okResult(map[string]interface{}{"symbol": name, "count": len(refs), "references": nilSafe(refs)})
}

func (h *Handler) getCallers(funcName string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	callers := h.analyzer.GetCallers(funcName)
	return okResult(map[string]interface{}{"function": funcName, "caller_count": len(callers), "callers": nilSafe(callers)})
}

func (h *Handler) getCallees(funcName, file string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	callees := h.analyzer.GetCallees(funcName, file)
	return okResult(map[string]interface{}{"function": funcName, "callee_count": len(callees), "callees": nilSafe(callees)})
}

func (h *Handler) getImplementors(ifaceName string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	impls := h.analyzer.GetImplementors(ifaceName)
	return okResult(map[string]interface{}{"interface": ifaceName, "count": len(impls), "implementors": nilSafe(impls)})
}

func (h *Handler) symbolView(name, file string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	view, err := h.analyzer.SymbolView(name, file)
	if err != nil {
		return failf("%v", err)
	}
	return okResult(view)
}

func (h *Handler) impactAnalysis(symbolName, file string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	result, err := h.analyzer.ImpactAnalysis(symbolName, file)
	if err != nil {
		return failf("%v", err)
	}
	return okResult(result)
}

func (h *Handler) listSymbols(filter string) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	syms := h.analyzer.ListSymbols(filter)
	return okResult(map[string]interface{}{"count": len(syms), "symbols": nilSafe(syms)})
}

func (h *Handler) getDiagnostics() ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	diags := h.analyzer.GetDiagnostics()
	return okResult(map[string]interface{}{"error_count": len(diags), "diagnostics": nilSafe(diags)})
}

func (h *Handler) readFile(file string) ToolResult {
	data, err := readFileBytesImpl(file)
	if err != nil {
		return failf("read failed: %v", err)
	}
	content := string(data)
	return okResult(map[string]interface{}{
		"file":    file,
		"lines":   strings.Count(content, "\n") + 1,
		"content": content,
	})
}

func (h *Handler) searchCode(query string, caseSensitive bool) ToolResult {
	if h.analyzer == nil {
		return fail("not indexed")
	}
	matches := h.analyzer.SearchCode(query, caseSensitive)
	return okResult(map[string]interface{}{"query": query, "count": len(matches), "matches": nilSafe(matches)})
}

func (h *Handler) beginSession() ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	id := h.overlay.Begin()
	return okResult(map[string]string{"session_id": id, "status": "open"})
}

func (h *Handler) sessionWrite(sessionID, file, content string) ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	if err := h.overlay.WriteFile(sessionID, file, content); err != nil {
		return failf("stage failed: %v", err)
	}
	return okResult(map[string]string{"session_id": sessionID, "file": file, "status": "staged"})
}

func (h *Handler) sessionValidate(sessionID string) ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	diags, err := h.overlay.Validate(sessionID)
	if err != nil {
		return failf("validation error: %v", err)
	}
	return okResult(map[string]interface{}{"session_id": sessionID, "error_count": len(diags), "diagnostics": nilSafe(diags)})
}

func (h *Handler) sessionCommit(sessionID string) ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	if err := h.overlay.Commit(sessionID); err != nil {
		if ce, ok := err.(*t.ConflictError); ok {
			return ToolResult{
				IsError: true,
				Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
					"CONFLICT: %s was modified externally since session began.\n"+
						"expected_hash=%s actual_hash=%s\n"+
						"Call session_rollback and re-read the file before retrying.",
					ce.File, ce.ExpectedHash, ce.ActualHash,
				)}},
			}
		}
		return failf("commit failed: %v", err)
	}
	return okResult(map[string]string{"session_id": sessionID, "status": "committed"})
}

func (h *Handler) sessionRollback(sessionID string) ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	if err := h.overlay.Rollback(sessionID); err != nil {
		return failf("rollback failed: %v", err)
	}
	return okResult(map[string]string{"session_id": sessionID, "status": "rolled_back"})
}

func (h *Handler) sessionStatus(sessionID string) ToolResult {
	if h.overlay == nil {
		return fail("not indexed")
	}
	sess, err := h.overlay.GetSession(sessionID)
	if err != nil {
		return failf("%v", err)
	}
	return okResult(sess)
}

func readFileBytesImpl(file string) ([]byte, error) {
	return os.ReadFile(file)
}

func nilSafe[S ~[]E, E any](s S) S {
	if s == nil {
		return S{}
	}
	return s
}
