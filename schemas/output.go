package schemas

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// SemanticEnvelope adds structured metadata to tool results for the LLM.
type SemanticEnvelope struct {
	Status    string   `json:"status"`    // ok, error, partial, stdin_required
	Source    string   `json:"source"`    // e.g. "mcp:filesystem", "shell"
	Type      string   `json:"type"`      // e.g. "json", "text", "binary"
	Timestamp int64    `json:"ts"`        // Unix timestamp
	Reason    string   `json:"reason"`    // The _reason provided during call
	Citations []string `json:"citations"` // Optional: where to find key info
	Content   string   `json:"content"`   // The (possibly summarized) result body
	RawRef    string   `json:"raw_ref"`   // Pointer to full raw result if archived
}

// ToMarkdown generates a structured Markdown representation of the envelope.
func (e *SemanticEnvelope) ToMarkdown(toolName string) string {
	var sb strings.Builder
	meta, _ := json.Marshal(e)
	fmt.Fprintf(&sb, "<!-- meta: %s -->\n\n", string(meta))
	fmt.Fprintf(&sb, "### [TOOL RESULT] %s\n\n", toolName)
	fmt.Fprintf(&sb, "**调用原因**\n%s\n\n", e.Reason)

	statusIcon := "✅"
	statusText := strings.ToUpper(e.Status)
	if e.Status == "error" || e.Status == "failed" {
		statusIcon = "❌"
	} else if e.Status == "stdin_required" {
		statusIcon = "⏸"
		statusText = "等待输入"
	}
	fmt.Fprintf(&sb, "**执行状态**\n%s %s | 来源：%s\n\n", statusIcon, statusText, e.Source)

	if len(e.Citations) > 0 {
		fmt.Fprintf(&sb, "**可引用位置**\n- %s\n\n", strings.Join(e.Citations, "\n- "))
	}

	fmt.Fprintf(&sb, "**内容**\n%s\n", e.Content)
	if e.RawRef != "" {
		fmt.Fprintf(&sb, "\n*完整数据见 %s*\n", e.RawRef)
	}

	return sb.String()
}

// SubagentStatus represents the execution status of a subagent step.
type SubagentStatus string

const (
	StatusOK                 SubagentStatus = "ok"
	StatusPartial            SubagentStatus = "partial"
	StatusFailed             SubagentStatus = "failed"
	StatusCapabilityRequired SubagentStatus = "capability_required"
	StatusDelegationRequired SubagentStatus = "delegation_required"
)

// Attachment represents a binary file output by a subagent (e.g. screenshot).
type Attachment struct {
	MimeType string `json:"mime_type"`
	Path     string `json:"path"`
	Label    string `json:"label"`
}

// SubagentOutput is the universal envelope every subagent must return.
type SubagentOutput struct {
	Status               SubagentStatus `json:"status"`
	Confidence           float64        `json:"confidence"`
	Result               map[string]any `json:"result"`
	Attachments          []Attachment   `json:"attachments"`
	MissingContext       []string       `json:"missing_context"`
	Assumptions          []string       `json:"assumptions"`
	Warnings             []string       `json:"warnings"`
	RequiredCapabilities []string       `json:"required_capabilities"`
	Capability           string         `json:"capability"`
	SkillIDs             []string       `json:"skill_ids"`
	TokenUsed            int            `json:"token_used"`

	Usage *Usage `json:"usage,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ─── Capability-specific result schemas ───────────────────────────────────

type ParseResult struct {
	Format           string           `json:"format"`
	IsValid          bool             `json:"is_valid"`
	Extracted        map[string]any   `json:"extracted"`
	ValidationErrors []map[string]any `json:"validation_errors"`
	RawRef           string           `json:"raw_ref,omitempty"`
}

type AnalyzeResult struct {
	Findings       []map[string]any `json:"findings"`
	Score          *float64         `json:"score,omitempty"`
	Recommendation string           `json:"recommendation"`
	EvidenceRefs   []string         `json:"evidence_refs"`
}

type ValidateResult struct {
	Passed         bool             `json:"passed"`
	RulesChecked   []map[string]any `json:"rules_checked"`
	BlockingErrors []string         `json:"blocking_errors"`
	AdvisoryErrors []string         `json:"advisory_errors"`
}

type GenerateResult struct {
	Content    *string  `json:"content,omitempty"`
	OutputRef  *string  `json:"output_ref,omitempty"`
	OutputType string   `json:"output_type"` // inline|file|object_storage|mcp_resource
	Format     string   `json:"format"`
	Summary    string   `json:"summary"`
	SizeBytes  int      `json:"size_bytes"`
	Sections   []string `json:"sections"`
}

type TransformResult struct {
	OutputFormat  string           `json:"output_format"`
	OutputRef     string           `json:"output_ref"`
	RecordCount   *int             `json:"record_count,omitempty"`
	FailedRecords []map[string]any `json:"failed_records"`
}

// ─── Output constraint injected into subagent system prompt ───────────────

var resultSchemaHints = map[string]string{
	"parse": `"format":"pacs.008|json|csv", "is_valid":true|false, ` +
		`"extracted":{}, "validation_errors":[{field,rule,value}], "raw_ref":"path or null"`,
	"analyze": `"findings":[{id,severity,description,evidence}], "score":0.0-1.0 or null, ` +
		`"recommendation":"...", "evidence_refs":["path",...]`,
	"validate": `"passed":true|false, "rules_checked":[{rule_id,passed,detail}], ` +
		`"blocking_errors":["..."], "advisory_errors":["..."]`,
	"generate": `"content":"short inline or null", "output_ref":"file path or null", ` +
		`"output_type":"inline|file|object_storage|mcp_resource", ` +
		`"format":"markdown|xml|json|pdf", "summary":"<=100 token summary", ` +
		`"size_bytes":0, "sections":["..."]`,
	"transform": `"output_format":"...", "output_ref":"required", ` +
		`"record_count":0 or null, "failed_records":[{index,reason}]`,
}

// BuildOutputConstraint returns the system prompt fragment that enforces
// structured JSON output from the subagent.
func BuildOutputConstraint(capability string) string {
	hint := resultSchemaHints[capability]
	if hint == "" {
		hint = `"...capability specific fields..."`
	}
	return fmt.Sprintf(`
================== OUTPUT CONTRACT ==================
Before producing your final JSON report, if the task requires information or actions that available tools can provide, you MUST invoke those tools first. Do not describe a plan to call a tool — call it.

A plan is not an action, and a partial action is not a finished task. Before you report status "ok", re-check the original task against everything you have actually verified via tool results so far in THIS conversation (not just your first turn) — do not report "ok" just because you called a tool earlier if later, unverified steps of the same task remain. If you are unsure whether every part of the task is genuinely done, report "partial" with confidence below 0.6 and say in "assumptions" what you did not verify, rather than reporting "ok" and hoping it is close enough.

Do not invent tool results: every fact you report as coming from a tool must actually come from a tool_result you received in this conversation.

You MUST respond with a single valid JSON object.
You may use <thought>...</thought> blocks BEFORE the JSON object for scratchpad reasoning.
The JSON object must match this structure exactly:

{
  "status": "ok" | "partial" | "failed" | "capability_required",
  "confidence": <float 0.0-1.0>,
  "result": { %s },
  "attachments": [{ "mime_type": "image/png", "path": "...", "label": "..." }],
  "missing_context": ["<string>", ...],
  "assumptions": ["<string>", ...],
  "warnings": ["<string>", ...],
  "required_capabilities": ["<string>", ...],
  "capability": "%s",
  "skill_ids": ["<string>", ...],
  "token_used": <int>
}

RULES:
- confidence < 0.6  → status MUST be "partial"
- Distinguish TWO different kinds of "missing" — they are not the same thing:
  (a) Information the user genuinely never supplied (e.g. no link/file/spec given at all) →
      status="partial", list what's missing in missing_context.
  (b) Information WAS supplied (e.g. a URL, a file path, a spec name) but none of your
      currently bound tools can act on it (e.g. you have a URL but no tool that fetches
      URLs or extracts PDF text) → status="capability_required". Do NOT ask the user to
      paste/attach the content instead — that is asking them to work around a gap that is
      yours, not theirs. Fill required_capabilities with what you need in plain language
      describing the missing ACTION, not a specific tool name (you don't know what tools
      exist beyond what's bound to you), e.g. ["fetch content from a URL", "extract text from a PDF"].
- Never include raw file content in result; store it and use a ref path
- skill_ids must list only the skills you actually applied
=====================================================`, hint, capability)
}

// BuildOutputConstraintWithTemplate is like BuildOutputConstraint but accepts
// an optional external override template (see
// config.PromptOverrides.OutputContract). When template is empty, this
// behaves identically to BuildOutputConstraint(capability). When non-empty,
// the template's literal {{RESULT_HINT}} and {{CAPABILITY}} tokens are
// substituted via strings.Replace -- NOT fmt.Sprintf -- so a '%' character
// in an externally-edited prompt file is never misinterpreted as a format
// verb. schemas intentionally does not import config (config already
// imports schemas; importing back would be a cycle), so the caller
// (core/spawner.go, which imports both) is responsible for passing in
// registry.PromptOverrides.OutputContract. ADDED (2026-07-28).
func BuildOutputConstraintWithTemplate(capability, template string) string {
	if template == "" {
		return BuildOutputConstraint(capability)
	}
	hint := resultSchemaHints[capability]
	if hint == "" {
		hint = `"...capability specific fields..."`
	}
	out := strings.ReplaceAll(template, "{{RESULT_HINT}}", hint)
	out = strings.ReplaceAll(out, "{{CAPABILITY}}", capability)
	return out
}

// ─── Parser ───────────────────────────────────────────────────────────────

var jsonFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// isHollow checks if the parsed output is semantically empty, even if syntactically valid.
func isHollow(out SubagentOutput) bool {
	if out.Confidence == 0 && out.Status == "" {
		return true
	}
	if len(out.Result) == 0 && len(out.Attachments) == 0 {
		return true
	}
	return false
}

// ParseOutput attempts to parse raw LLM text into a SubagentOutput.
// Never returns an error — falls back to a partial result instead.
func ParseOutput(raw, capability string) SubagentOutput {
	trimmed := strings.TrimSpace(raw)

	// 1. Direct JSON
	var out SubagentOutput
	if err := json.Unmarshal([]byte(trimmed), &out); err == nil && !isHollow(out) {
		return out
	}

	// 2. JSON inside markdown fence
	matches := jsonFenceRe.FindAllStringSubmatch(trimmed, -1)
	for _, m := range matches {
		if len(m) > 1 {
			if err := json.Unmarshal([]byte(m[1]), &out); err == nil && !isHollow(out) {
				return out
			}
		}
	}

	// 3. Aggressive Extraction: find the first '{' and last '}'
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start != -1 && end != -1 && end > start {
		jsonPart := trimmed[start : end+1]
		if err := json.Unmarshal([]byte(jsonPart), &out); err == nil && !isHollow(out) {
			return out
		}
	}

	// 4. Final Fallback: The output is fundamentally not JSON
	snippet := trimmed
	if len(snippet) > 2000 {
		snippet = snippet[:2000]
	}

	reason := "output not parseable: could not extract valid JSON"
	if start != -1 && end != -1 {
		reason = "output parseable but semantically hollow: missing confidence/result"
	}

	return SubagentOutput{
		Status:         StatusPartial,
		Confidence:     0.3,
		Result:         map[string]any{"raw": snippet},
		Assumptions:    []string{reason},
		Warnings:       []string{"subagent did not follow output schema"},
		Capability:     capability,
		MissingContext: []string{},
		SkillIDs:       []string{},
	}
}

// IsLowConfidence returns true when the output should trigger a partial path.
func (o *SubagentOutput) IsLowConfidence() bool {
	return o.Confidence < 0.6
}

// ToolConstraintFragment builds the system prompt fragment for tool restrictions.
func ToolConstraintFragment(allowedTools []string) string {
	if len(allowedTools) == 0 {
		return ""
	}
	quoted := make([]string, len(allowedTools))
	for i, t := range allowedTools {
		quoted[i] = "`" + t + "`"
	}
	return fmt.Sprintf(
		"\n## Tool Usage Restriction\nYou may ONLY use the following tools: %s.\n"+
			"Do not call any other tool even if it appears available.\n\n"+
			"**IMPORTANT**: For every tool call, you MUST include a `_reason` parameter explaining why you are calling this tool and what you expect to achieve.",
		strings.Join(quoted, ", "),
	)
}
