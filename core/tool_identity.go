package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daybeam/vortex/schemas"
)

// controlArgPrefix marks engine-level control arguments that steer the model or
// tag the trace but are stripped before the tool payload is sent (`_reason`,
// `_decision_id`). They must never influence a loop-detection hash: otherwise
// the model can mint a fresh key for an otherwise identical call just by
// rephrasing its `_reason`, which silently defeats A1, #11 and every Sieve
// check at once. Guidance is not supposed to change the judgement it is
// attached to.
const controlArgPrefix = "_"

// canonicalToolArgs returns args with control arguments removed. It never
// mutates the input: the reflection gate still needs to read `_reason` after
// the key has been computed. The original map is returned untouched when it
// holds no control arguments, so the common path allocates nothing.
func canonicalToolArgs(args map[string]any) map[string]any {
	hasControl := false
	for k := range args {
		if strings.HasPrefix(k, controlArgPrefix) {
			hasControl = true
			break
		}
	}
	if !hasControl {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if strings.HasPrefix(k, controlArgPrefix) {
			continue
		}
		out[k] = v
	}
	return out
}

// toolCallKey computes a deterministic key for a tool call (tool name + args).
// Used by #11 (eval §8.41) to detect identical repeated calls. Uses FNV-1a
// hash of the JSON-marshaled args for O(1) map lookup.
//
// Control arguments are excluded via canonicalToolArgs — see controlArgPrefix.
func toolCallKey(toolName string, args map[string]any) string {
	args = canonicalToolArgs(args)
	if len(args) == 0 {
		return toolName + ":"
	}
	b, err := json.Marshal(args)
	if err != nil {
		// If args can't be marshaled, fall back to tool name only.
		// This is overly permissive (won't dedup) but safe.
		return toolName + ":unmarshalable"
	}
	// FNV-1a 64-bit hash — fast, deterministic, no collisions in practice
	// for argument maps with < 100 keys.
	h := uint64(14695981039346656037) // FNV offset basis
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211 // FNV prime
	}
	return fmt.Sprintf("%s:%d", toolName, h)
}

// stateChangingTools is a package-level set to avoid per-call map allocation (audit P-H8).
var stateChangingTools = map[string]bool{
	"click":          true,
	"type":           true,
	"navigate":       true,
	"press_key":      true,
	"scroll":         true,
	"set_cookie":     true,
	"execute_script": true,
}

func isStateChangingTool(name string) bool {
	return stateChangingTools[name]
}

// readOnlyToolPrefixes lists the verbs after which a tool call provably cannot
// mutate remote state. Deliberately a whitelist rather than the write-verb
// blacklist used by IsWriteMCPTool: enumerating every write verb is unbounded
// (AppWorld alone uses download_, remind_, deny_, place_, settle_, record_,
// unfollow_, unlike_, move_, copy_, reply_, label_ … 100 mutating endpoints, 45
// of which IsWriteMCPTool misses), whereas the read vocabulary is small and
// stable. Anything not listed here is assumed to *possibly* have side effects,
// which is the safe direction for the zero-effect gate below.
//
// `download_` is intentionally absent: in AppWorld spotify__download_song is a
// POST that flips player state, so treating it as read-only would disable the
// gate on exactly the task (3ab5b8b_1) where the model does perform real work.
var readOnlyToolPrefixes = []string{
	"show_", "search_", "get_", "list_", "read_", "view_",
	"find_", "lookup_", "query_", "count_", "check_", "describe_", "fetch_",
}

// IsReadOnlyToolName reports whether a tool call, judging by its name alone,
// provably cannot change external state. Namespaced names are accepted
// (supervisor__show_profile → show_profile). Core tools are handled by the same
// rule: read_file is read-only, write_file and execute_code are not.
//
// Used by the zero-effect completion gate (spawner_dospawn.go). NOT a
// replacement for IsWriteMCPTool — that one answers "should I ask for a
// _reason", a different question with a different failure mode.
func IsReadOnlyToolName(name string) bool {
	if name == "" {
		return false
	}
	n := stripNamespace(name)
	for _, p := range readOnlyToolPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// writeToolPrefixes lists verb prefixes for MCP tools that modify external
// state (database, API, business domain). Used by #19 (eval §8.42) to gate
// write operations with a _reason requirement.
//
// Classification is by name prefix — any tool starting with these verbs is
// considered a write tool. This is intentionally broad: false positives
// (treating a read tool as write) just require a _reason, which is a mild
// friction. False negatives (treating a write tool as read) are the real
// risk, so the list errs on the side of inclusion.
//
// Added add_, pay_, like_, follow_, mark_, upload_,
// execute_, run_, publish_ — these were missing, causing auditor to inherit
// write tools as read-only.
var writeToolPrefixes = []string{
	"cancel_", "update_", "create_", "delete_", "set_", "write_",
	"send_", "transfer_", "book_", "submit_", "close_", "modify_",
	"edit_", "insert_", "remove_", "drop_", "put_", "post_", "replace_",
	"approve_", "reject_", "assign_", "revoke_", "activate_", "deactivate_",
	// Additional write verbs
	"add_", "pay_", "like_", "follow_", "mark_", "upload_",
	"execute_", "run_", "publish_",
}

// stripNamespace returns the fragment of name after the last namespace
// separator (__, ., /). This handles tool names with arbitrary namespace
// prefixes uniformly:
//
//	"app__send_message"   → "send_message"
//	"mcp1.create_file"    → "create_file"
//	"service/delete_user" → "delete_user"
//	"send_message"        → "send_message"  (no separator)
//
// This replaces the hardcoded "app__" prefix stripping with a general
// approach that doesn't bake any specific namespace convention into the
// routing logic.
func stripNamespace(name string) string {
	bestAfter := -1
	for _, sep := range []string{"__", ".", "/"} {
		i := strings.LastIndex(name, sep)
		if i >= 0 {
			after := i + len(sep)
			if after > bestAfter {
				bestAfter = after
			}
		}
	}
	if bestAfter > 0 && bestAfter < len(name) {
		return name[bestAfter:]
	}
	return name
}

// IsWriteMCPTool returns true if the tool name starts with a write verb prefix.
// Core tools (write_file, read_file, execute_code) are excluded — they are
// filesystem operations tracked by StagedWorkspace, not domain writes.
// Exported for contract testing (Feature #17: Write Authorization).
//
// Handle namespaced tool names by stripping the
// namespace prefix (__, ., /) before checking the verb underneath.
func IsWriteMCPTool(name string) bool {
	if CoreToolNames[name] {
		return false // core tools are fs-only, tracked by StagedWorkspace
	}
	checkName := stripNamespace(name)
	for _, prefix := range writeToolPrefixes {
		if strings.HasPrefix(checkName, prefix) {
			return true
		}
	}
	return false
}

func containsTool(tools []string, target string) bool {
	for _, t := range tools {
		if t == target {
			return true
		}
	}
	return false
}

// flattenContentBlocks joins the Text of each ContentBlock with double newlines,
// producing a plain string for providers that do not support block-based
// prompt caching (Gemini, OpenAI-compatible, Ollama, etc). Anthropic-style
// providers should prefer the SystemBlocks/UserBlocks fields directly; this
// is a compatibility fallback so CompleteRequest.System/User are never left
// empty now that buildSystemPrompt returns []schemas.ContentBlock.
func flattenContentBlocks(blocks []schemas.ContentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// copyToolCallArguments creates a shallow copy of src and optionally stamps a
// _decision_id into the copy (not the original). This isolates the trace-only
// metadata from the arguments that get sent to the MCP server, so _decision_id
// never leaks into an actual tool call payload (fix M2).
func copyToolCallArguments(src map[string]any, decisionID string) map[string]any {
	dst := make(map[string]any, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	if decisionID != "" {
		dst["_decision_id"] = decisionID
	}
	return dst
}
