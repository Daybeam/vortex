package core

import (
	"sync"
)

// CapabilityGraph defines semantic relationships between capability tags.
// It helps the discovery process expand a single intent into a logical
// set of related capabilities and tools.
type CapabilityGraph struct {
	Mu    sync.RWMutex
	Edges map[string][]string // Tag -> []RelatedTags
}

func NewCapabilityGraph() *CapabilityGraph {
	g := &CapabilityGraph{
		Edges: make(map[string][]string),
	}
	g.Refresh()
	return g
}

// Refresh resets the graph to default expansion rules.
func (g *CapabilityGraph) Refresh() {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	// Clear existing edges
	g.Edges = make(map[string][]string)

	// Hardcoded relationships based on common domains
	g.Edges["research.paper"] = []string{"research", "rag.retrieve", "document.read", "code.search"}
	g.Edges["knowledge.compare"] = []string{"synthesis", "fragment.diff"}
	g.Edges["finance.stock"] = []string{"research.market", "time_series", "finance"}
	g.Edges["formal.verify"] = []string{"logic.check", "math.reason", "verify"}
	g.Edges["document.report"] = []string{"synthesis", "format.docx", "document"}
}

// GetRelated returns tags semantically linked to the input tag.
func (g *CapabilityGraph) GetRelated(tag string) []string {
	g.Mu.RLock()
	defer g.Mu.RUnlock()
	return g.Edges[tag]
}

// AddRelation defines a new edge in the graph.
func (g *CapabilityGraph) AddRelation(from, to string) {
	g.Mu.Lock()
	defer g.Mu.Unlock()

	for _, existing := range g.Edges[from] {
		if existing == to {
			return
		}
	}
	g.Edges[from] = append(g.Edges[from], to)
}
