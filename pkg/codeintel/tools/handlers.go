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
			Description: "【第一步：索引】扫描并索引 Go 项目。在开始任何分析或编辑任务前，必须运行此工具。它会构建语义地图。如果你发现符号找不到了，请重新运行此工具以刷新缓存。",
			InputSchema: schema([]string{"root_dir"}, "root_dir", str("项目根目录的绝对路径")),
		},
		{
			Name:        "find_symbol",
			Description: "【定位定义】查找符号（函数/类型/接口/变量）的定义。在修改代码前，必须使用此工具确认定义的精确位置、签名和文档。",
			InputSchema: schema([]string{"name"}, "name", str("符号名称，例如 ProcessOrder")),
		},
		{
			Name:        "find_references",
			Description: "【查找引用】在整个代码库中查找符号的所有使用位置。在重命名、删除或修改函数签名之前，**严禁**跳过此步骤。",
			InputSchema: schema([]string{"name"},
				"name", str("符号名称"),
				"file", str("可选：缩小范围到特定定义文件"),
			),
		},
		{
			Name:        "impact_analysis",
			Description: "【高危：爆炸半径分析】计算修改一个符号的影响范围。它会返回直接和间接调用者。在修改任何导出（Exported）符号前，**必须**先运行此分析。如果风险等级为 high 或 critical，请谨慎操作并告知用户。",
			InputSchema: schema([]string{"symbol_name"},
				"symbol_name", str("要分析的符号"),
				"file", str("可选：缩小范围到特定文件"),
			),
		},
		{
			Name:        "symbol_view",
			Description: "【全景视图】一次性获取符号的定义、调用者、被调用者、引用和接口实现。这是探索陌生代码逻辑时的**最佳首选工具**。",
			InputSchema: schema([]string{"name"},
				"name", str("符号名称"),
				"file", str("可选：如果名称有歧义则提供"),
			),
		},
		{
			Name:        "get_diagnostics",
			Description: "【体检】运行类型检查并返回所有错误。在大规模编辑后或提交前，请运行此工具确保项目没有被你改坏。它比编译更快。",
			InputSchema: schema(nil),
		},
		{
			Name:        "begin_session",
			Description: "【编辑 SOP 第1步】启动原子编辑会话。所有写入都将在内存中暂存。返回 session_id。**不要直接写磁盘，请始终使用会话流程。**",
			InputSchema: schema(nil),
		},
		{
			Name:        "session_write",
			Description: "【编辑 SOP 第2步】将文件修改暂存到会话中。必须提供文件的**完整新内容**。注意：此操作不会改变磁盘文件，直到你 commit。",
			InputSchema: schema([]string{"session_id", "file", "content"},
				"session_id", str("来自 begin_session 的 ID"),
				"file", str("文件的绝对路径"),
				"content", str("新的完整文件内容"),
			),
		},
		{
			Name:        "session_validate",
			Description: "【编辑 SOP 第3步：强制步骤】在内存中验证你暂存的所有修改。它会检查你引入的修改是否导致了类型错误。**严禁在未通过验证（error_count 为 0）的情况下提交。**",
			InputSchema: schema([]string{"session_id"}, "session_id", str("会话 ID")),
		},
		{
			Name:        "session_commit",
			Description: "【编辑 SOP 第4步】将暂存的所有修改原子化地写入磁盘。只有在 session_validate 成功后才能调用。如果返回 CONFLICT 错误，说明在你编辑期间文件被外部修改了，你必须 rollback 并重试。",
			InputSchema: schema([]string{"session_id"}, "session_id", str("会话 ID")),
		},
		{
			Name:        "session_rollback",
			Description: "【丢弃修改】放弃当前会话中所有的暂存修改并清理环境。如果你发现改乱了或验证失败无法修复，请果断使用此工具。",
			InputSchema: schema([]string{"session_id"}, "session_id", str("会话 ID")),
		},
		{
			Name:        "search_code",
			Description: "【文本搜索】跨包进行全文搜索。当你不知道符号名，只知道部分关键词时使用。支持区分大小写。",
			InputSchema: schema([]string{"query"},
				"query", str("搜索关键词"),
				"case_sensitive", boolProp("是否区分大小写（默认 false）"),
			),
		},
		{
			Name:        "list_project_structure",
			Description: "【项目概览】返回项目的目录树结构。用于在开始任务前了解代码布局。会自动忽略 vendor, node_modules 等无关目录。",
			InputSchema: schema(nil,
				"depth", intProp("搜索深度（默认 3）"),
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
