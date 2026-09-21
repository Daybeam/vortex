package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/pkg/interfaces"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/schemas"
)

// CapabilityNavigator provides a semantic view of what the orchestrator can do.
// It helps LLMs understand high-level capabilities and workflows instead of just
// technical tool names.
type CapabilityNavigator struct {
	Registry *config.Registry
	Index    *CapabilityIndex
	Graph    *CapabilityGraph
	Pipeline *DiscoveryPipeline
}

func NewCapabilityNavigator(reg *config.Registry) *CapabilityNavigator {
	idx := NewCapabilityIndex(reg)
	graph := NewCapabilityGraph()

	// Initialize Pipeline
	pipeline := NewDiscoveryPipeline([]Retriever{
		&FilterRetriever{Index: idx, Registry: reg},
		&KeywordRetriever{Registry: reg},
		&SemanticRetriever{Graph: graph, Registry: reg},
	}, NewSimpleRanker(0.5))

	return &CapabilityNavigator{
		Registry: reg,
		Index:    idx,
		Graph:    graph,
		Pipeline: pipeline,
	}
}

// Refresh rebuilds the internal index and graph from the latest registry state.
func (n *CapabilityNavigator) Refresh() {
	n.Index.Rebuild(n.Registry)
	n.Graph.Refresh()
}

// ClassifyIntent attempts to extract high-level capability tags from a query.
// It uses an LLM if available, falling back to lexical analysis.
func (n *CapabilityNavigator) ClassifyIntent(ctx context.Context, query string) []string {
	// 1. Lexical Analysis (Fallback/Fast Path)
	tokens := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return r == ' ' || r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':'
	})

	lexicalTags := make(map[string]bool)
	knownTags := n.Index.ListKnown()

	// Manual Vocabulary for Lexical Intent Analysis
	vocab := map[string][]string{
		"compare":  {"knowledge.compare", "synthesis"},
		"paper":    {"research.paper", "document.read"},
		"arxiv":    {"research.paper"},
		"stock":    {"finance.stock"},
		"verify":   {"formal.verify", "logic.check"},
		"report":   {"document.report", "synthesis"},
		"code":     {"code", "implement"},
		"search":   {"code.search", "research"},
		"fragment": {"knowledge.compare", "fragment.diff"},
	}

	for _, token := range tokens {
		if len(token) < 3 {
			continue
		}

		// Basic stemming (plurals)
		cleanToken := strings.TrimSuffix(token, "s")

		// Match against Vocabulary
		if tags, ok := vocab[cleanToken]; ok {
			for _, t := range tags {
				lexicalTags[t] = true
			}
		}

		// Match against known tags
		for _, tag := range knownTags {
			if strings.Contains(tag, cleanToken) || strings.Contains(cleanToken, tag) {
				lexicalTags[tag] = true
			}
		}
	}

	// 2. LLM Analysis (Primary)
	var provider interfaces.Provider
	n.Registry.Mu.RLock()
	for _, role := range n.Registry.Roles {
		pCfg := n.Registry.ResolveProviderConfig(role)
		if p, err := providers.Get(pCfg, n.Registry.ExternalRuntimes); err == nil {
			provider = p
			break
		}
	}
	n.Registry.Mu.RUnlock()

	if provider != nil {
		prompt := fmt.Sprintf("Analyze the user query and extract a list of high-level capability tags (e.g., 'research.paper', 'knowledge.compare').\nKnown capabilities: %s\n\nQuery: %s\n\nRespond ONLY with a comma-separated list of tags.",
			strings.Join(knownTags, ", "), query)

		resp, err := provider.Complete(ctx, providers.CompleteRequest{
			System:  "You are an intent classifier for an autonomous agent orchestrator.",
			User:    prompt,
			Secrets: n.Registry.Secrets,
		})

		if err == nil && resp.Text != "" {
			llmTags := strings.Split(resp.Text, ",")
			for _, t := range llmTags {
				tag := strings.TrimSpace(strings.ToLower(t))
				if tag != "" {
					lexicalTags[tag] = true
				}
			}
		}
	}

	res := make([]string, 0, len(lexicalTags))
	for t := range lexicalTags {
		res = append(res, t)
	}
	sort.Strings(res)
	return res
}

type NavigatorView struct {
	Capabilities map[string][]RoleSummary     `json:"capabilities"`
	Workflows    map[string][]WorkflowSummary `json:"workflows"`
}

type RoleSummary struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Purpose  string   `json:"purpose"`
	BestFor  string   `json:"best_for"`
	Examples []string `json:"examples,omitempty"`
}

type WorkflowSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"` // "SOP" or "RoleGroup"
	Description  string   `json:"description"`
	TypicalTasks []string `json:"typical_tasks,omitempty"`
}

// GetOverview returns a high-level menu of capabilities and workflows.
func (n *CapabilityNavigator) GetOverview() string {
	n.Registry.Mu.RLock()
	defer n.Registry.Mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("# 🧭 Capability Navigator\n\n")
	sb.WriteString("Welcome to the semantic discovery center. Use this to find the right role or workflow for your intent.\n\n")

	// 1. High-Level Capabilities (Grouped Roles)
	sb.WriteString("## 🚀 Capabilities (By Domain)\n")
	domains := make(map[string][]string)
	for _, r := range n.Registry.Roles {
		domain := r.BaseCapability
		if domain == "" {
			domain = "General"
		}
		domains[domain] = append(domains[domain], r.ID)
	}

	domainKeys := make([]string, 0, len(domains))
	for k := range domains {
		domainKeys = append(domainKeys, k)
	}
	sort.Strings(domainKeys)

	for _, d := range domainKeys {
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", d, strings.Join(domains[d], ", ")))
	}
	sb.WriteString("\n")

	// 2. Workflow Entry Points (SOPs & RoleGroups)
	sb.WriteString("## 🔄 Recommended Workflows\n")

	// RoleGroups
	for _, g := range n.Registry.RoleGroups {
		sb.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", g.Name, g.ID, g.Description))
	}

	// SOPs
	for _, s := range n.Registry.SOPs {
		sb.WriteString(fmt.Sprintf("- **%s** (%s) [SOP]: %s\n", s.ID, s.ID, s.Description))
	}
	sb.WriteString("\n")

	// 3. Capability Catalog (Abstract Capabilities & Atomic Verbs)
	sb.WriteString("## 🛠️ Capability Catalog (Verbs)\n")
	sb.WriteString("Use these for fine-grained planning or when defining new roles.\n\n")

	// Use a map to deduplicate by ID
	catalog := make(map[string]string)

	// From Skills
	for _, s := range n.Registry.Skills {
		if s.Capability != "" {
			catalog[s.Capability] = s.Description
		}
	}

	// From MCPs (Declared Provides & Atomic Tools)
	for _, m := range n.Registry.MCPs {
		for _, p := range m.Provides {
			if _, exists := catalog[p]; !exists {
				catalog[p] = "Declared capability from " + m.ID
			}
		}
		for _, t := range m.AvailableTools {
			key := fmt.Sprintf("%s.%s", m.ID, t)
			desc := "Atomic tool"
			for _, dt := range m.FullToolDefinitions {
				if dt.Name == t {
					desc = dt.Description
					break
				}
			}
			catalog[key] = desc
		}
	}

	catKeys := make([]string, 0, len(catalog))
	for k := range catalog {
		catKeys = append(catKeys, k)
	}
	sort.Strings(catKeys)

	for _, k := range catKeys {
		desc := catalog[k]
		if len(desc) > 100 {
			desc = desc[:97] + "..."
		}
		sb.WriteString(fmt.Sprintf("- **%s**: %s\n", k, desc))
	}

	sb.WriteString("\n> To learn more about a specific item, call `orchestrator_discover(intent=\"<id>\")`.\n")

	return sb.String()
}

// ExploreIntent provides detailed metadata for a specific role, group, or SOP.
func (n *CapabilityNavigator) ExploreIntent(intent string) string {
	n.Registry.Mu.RLock()
	defer n.Registry.Mu.RUnlock()

	// Check Roles
	if r, ok := n.Registry.Roles[intent]; ok {
		return n.formatRoleDetail(r)
	}

	// Check Groups
	if g, ok := n.Registry.RoleGroups[intent]; ok {
		return n.formatGroupDetail(g)
	}

	// Check SOPs
	if s, ok := n.Registry.SOPs[intent]; ok {
		return n.formatSOPDetail(s)
	}

	// Check MCPs (Static & Dynamic)
	if m, ok := n.Registry.MCPs[intent]; ok {
		return n.formatMCPDetail(intent, m)
	}
	if m, ok := n.Registry.DynamicMCPs[intent]; ok {
		return n.formatMCPDetail(intent, m)
	}

	return fmt.Sprintf("Intent %q not found. Try `orchestrator_discover()` for an overview.", intent)
}

// Search performs a multi-layered discovery across Roles, Skills, Groups, SOPs, and MCPs.
func (n *CapabilityNavigator) Search(ctx context.Context, query string) string {
	// 1. Detect Intents
	tags := n.ClassifyIntent(ctx, query)

	// 2. Run Pipeline
	candidates := n.Pipeline.Execute(ctx, query, tags)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 🔍 Search Results for %q\n", query))
	if len(tags) > 0 {
		sb.WriteString(fmt.Sprintf("> **Detected Intents**: %s\n\n", strings.Join(tags, ", ")))
	}

	if len(candidates) == 0 {
		sb.WriteString("\nNo specific Roles or Skills matched your query. Try different keywords or call `orchestrator_discover()` for the full menu.")
		return sb.String()
	}

	// Group by Type for nice display
	byType := make(map[CandidateType][]DiscoveryCandidate)
	for _, c := range candidates {
		byType[c.Type] = append(byType[c.Type], c)
	}

	// Helper to render a group
	renderGroup := func(title string, cType CandidateType) {
		group := byType[cType]
		if len(group) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("### %s\n", title))
		for _, c := range group {
			sb.WriteString(fmt.Sprintf("- **%s** (%s): %s (confidence: %.2f, via %s)\n",
				c.Name, c.ID, c.Description, c.Confidence, c.Source))
		}
		sb.WriteString("\n")
	}

	renderGroup("🚀 Roles", CandidateRole)
	renderGroup("🧠 Skills", CandidateSkill)
	renderGroup("📋 SOPs", CandidateSOP)
	renderGroup("🛠️ MCP Servers", CandidateMCP)
	renderGroup("🔄 RoleGroups", CandidateGroup)

	sb.WriteString("\n> Call `orchestrator_discover(intent=\"<id>\")` for details on any match.")
	return sb.String()
}

func (n *CapabilityNavigator) formatRoleDetail(r *config.Role) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Role: %s (%s)\n", r.Name, r.ID))
	sb.WriteString(fmt.Sprintf("> %s\n\n", r.BaseCapability))

	if r.Purpose != "" {
		sb.WriteString("### Purpose\n")
		sb.WriteString(r.Purpose + "\n\n")
	}

	if r.BestFor != "" {
		sb.WriteString("### Best For\n")
		sb.WriteString(r.BestFor + "\n\n")
	}

	if len(r.Examples) > 0 {
		sb.WriteString("### Examples\n")
		for _, e := range r.Examples {
			sb.WriteString("- " + e + "\n")
		}
		sb.WriteString("\n")
	}

	if len(r.Requires) > 0 {
		sb.WriteString("### Required Capabilities\n")
		sb.WriteString(strings.Join(r.Requires, ", ") + "\n\n")
	}

	if len(r.Optional) > 0 {
		sb.WriteString("### Optional Capabilities\n")
		sb.WriteString(strings.Join(r.Optional, ", ") + "\n\n")
	}

	sb.WriteString("### Bound Skills\n")
	sb.WriteString(strings.Join(r.BoundSkills, ", ") + "\n\n")

	return sb.String()
}

func (n *CapabilityNavigator) formatGroupDetail(g *config.RoleGroup) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# RoleGroup: %s (%s)\n", g.Name, g.ID))
	sb.WriteString(fmt.Sprintf("> Policy: %s\n\n", g.Policy))
	sb.WriteString(g.Description + "\n\n")

	if g.Purpose != "" {
		sb.WriteString("### Purpose\n")
		sb.WriteString(g.Purpose + "\n\n")
	}

	if g.BestFor != "" {
		sb.WriteString("### Best For\n")
		sb.WriteString(g.BestFor + "\n\n")
	}

	sb.WriteString("### Members\n")
	for _, m := range g.Members {
		sb.WriteString(fmt.Sprintf("- %s\n", m.RoleID))
	}

	return sb.String()
}

func (n *CapabilityNavigator) formatSOPDetail(s *schemas.SOP) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# SOP: %s\n", s.ID))
	sb.WriteString(s.Description + "\n\n")

	if len(s.TypicalTasks) > 0 {
		sb.WriteString("### Typical Tasks\n")
		for _, t := range s.TypicalTasks {
			sb.WriteString("- " + t + "\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("### Steps\n")
	for _, st := range s.Steps {
		sb.WriteString(fmt.Sprintf("- **%s** (%s): %s\n", st.ID, st.Role, st.Task))
	}

	return sb.String()
}

func (n *CapabilityNavigator) formatMCPDetail(id string, m *config.MCPDef) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# MCP Server: %s\n", id))
	if m.URL != "" {
		sb.WriteString(fmt.Sprintf("> URL: %s\n\n", m.URL))
	}

	if len(m.Provides) > 0 {
		sb.WriteString("### Declared Capabilities\n")
		for _, p := range m.Provides {
			sb.WriteString("- " + p + "\n")
		}
		sb.WriteString("\n")
	}

	if len(m.AvailableTools) > 0 {
		sb.WriteString("### Available Tools\n")
		for _, t := range m.AvailableTools {
			desc := ""
			for _, dt := range m.FullToolDefinitions {
				if dt.Name == t {
					desc = " - " + dt.Description
					break
				}
			}
			sb.WriteString(fmt.Sprintf("- `%s`%s\n", t, desc))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// GenerateDynamicMenu builds a user-friendly, auto-generated menu of active Roles
// and hot-pluggable MCP servers grouped by domain. This replaces the static
// capabilities list to reflect the live, hot-pluggable nature of the system.
func (n *CapabilityNavigator) GenerateDynamicMenu(reg *config.Registry) string {
	reg.Mu.RLock()
	defer reg.Mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("# 🚀 Vortex Dynamic Capability Menu\n\n")
	sb.WriteString("> *Live view of active roles and hot-pluggable MCP modules. When you describe a task, I will automatically route it to the most relevant role and dynamically mount the required MCP modules.*\n\n")

	// Roles
	if len(reg.Roles) > 0 {
		sb.WriteString("## 👥 Active Roles\n")
		for id, r := range reg.Roles {
			if r.Name != "" && r.Purpose != "" {
				sb.WriteString(fmt.Sprintf("- **%s** (`%s`): %s\n", r.Name, id, r.Purpose))
			} else {
				sb.WriteString(fmt.Sprintf("- `%s`\n", id))
			}
		}
		sb.WriteString("\n")
	}

	// Group MCPs by Domain
	mcpByDomain := make(map[string][]*config.MCPDef)
	for _, m := range reg.MCPs {
		domain := m.Domain
		if domain == "" {
			domain = "General"
		}
		mcpByDomain[domain] = append(mcpByDomain[domain], m)
	}
	for _, m := range reg.DynamicMCPs {
		domain := m.Domain
		if domain == "" {
			domain = "General"
		}
		mcpByDomain[domain] = append(mcpByDomain[domain], m)
	}

	if len(mcpByDomain) > 0 {
		sb.WriteString("## 🧩 Hot-Pluggable MCP Modules\n")
		for _, domain := range sortedKeys(mcpByDomain) {
			sb.WriteString(fmt.Sprintf("\n**%s**\n", domain))
			for _, m := range mcpByDomain[domain] {
				sb.WriteString(fmt.Sprintf("- `%s` (Tools: %d)\n", m.ID, len(m.AvailableTools)))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func sortedKeys(m map[string][]*config.MCPDef) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
