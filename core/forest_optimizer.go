package core

import (
	"sort"

	"github.com/daybeam/vortex/pkg/search"
)

// --- Union-Find (Standard Disjoint Set Union) ---
type UnionFind struct {
	parent []int
	rank   []int
}

func NewUnionFind(n int) *UnionFind {
	uf := &UnionFind{
		parent: make([]int, n),
		rank:   make([]int, n),
	}
	for i := 0; i < n; i++ {
		uf.parent[i] = i
	}
	return uf
}

func (uf *UnionFind) Find(x int) int {
	if uf.parent[x] != x {
		uf.parent[x] = uf.Find(uf.parent[x]) // Path compression
	}
	return uf.parent[x]
}

func (uf *UnionFind) Union(x, y int) bool {
	px, py := uf.Find(x), uf.Find(y)
	if px == py {
		return false
	}
	if uf.rank[px] < uf.rank[py] {
		px, py = py, px
	}
	uf.parent[py] = px
	if uf.rank[px] == uf.rank[py] {
		uf.rank[px]++
	}
	return true
}

// --- Semantic Edge Construction (mutual-kNN) ---
type WeightedEdge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Weight   float64 `json:"weight"`
	EdgeType string  `json:"type"` // "schema" | "semantic"
}

const (
	maxSeedCount = 48 // Cap seed count to keep mutual-kNN complexity O(L^2) manageable
)

// BuildSemanticEdges computes mutual-kNN semantic edges from embeddings.
// Only returns edges where BOTH nodes consider each other in their top-k.
func BuildSemanticEdges(embeddings map[string][]float32, k int) []WeightedEdge {
	ids := make([]string, 0, len(embeddings))
	for id := range embeddings {
		ids = append(ids, id)
	}
	if len(ids) > maxSeedCount {
		ids = ids[:maxSeedCount]
	}

	// For each node, find top-k nearest
	topK := make(map[string]map[string]bool) // nodeID -> (neighborID -> true)
	for _, id := range ids {
		vec := embeddings[id]
		type scored struct {
			id  string
			sim float64
		}
		neighbors := make([]scored, 0, len(ids)-1)
		for _, other := range ids {
			if other == id {
				continue
			}
			sim := search.CosineSimilarity(vec, embeddings[other])
			if sim > 0 {
				neighbors = append(neighbors, scored{id: other, sim: sim})
			}
		}
		sort.Slice(neighbors, func(i, j int) bool { return neighbors[i].sim > neighbors[j].sim })

		for i := 0; i < k && i < len(neighbors); i++ {
			if topK[id] == nil {
				topK[id] = make(map[string]bool)
			}
			topK[id][neighbors[i].id] = true
		}
	}

	// Mutual edges
	var edges []WeightedEdge
	seen := make(map[string]bool)
	for _, id := range ids {
		if topK[id] == nil {
			continue
		}
		for neighbor := range topK[id] {
			if topK[neighbor] == nil || !topK[neighbor][id] {
				continue // not mutual
			}
			// Dedup edge direction
			id1, id2 := id, neighbor
			if id1 > id2 {
				id1, id2 = id2, id1
			}
			edgeKey := id1 + "->" + id2
			if seen[edgeKey] {
				continue
			}
			seen[edgeKey] = true
			sim := search.CosineSimilarity(embeddings[id], embeddings[neighbor])
			edges = append(edges, WeightedEdge{
				From:     id,
				To:       neighbor,
				Weight:   sim,
				EdgeType: "semantic",
			})
		}
	}
	return edges
}

// --- Kruskal Maximum Weight Forest ---

// MaxWeightForest selects up to K nodes forming an acyclic forest maximizing total edge weight.
// Schema edges always included (they are the DAG structure). Semantic edges are pruned
// for redundancy (cosine > threshold) and the remaining are optimized via Kruskal.
func MaxWeightForest(
	seedNodes []string,
	schemaEdges []WeightedEdge,
	semanticEdges []WeightedEdge,
	redundancyThreshold float64,
	k int,
) (SelectedNodes []string, ForestEdges []WeightedEdge) {
	if len(seedNodes) > maxSeedCount {
		seedNodes = seedNodes[:maxSeedCount]
	}

	// 1. Node ID -> integer index
	nodeIndex := make(map[string]int)
	for i, id := range seedNodes {
		nodeIndex[id] = i
	}

	// 2. Prune redundant semantic edges (cosine > threshold = too similar)
	filteredSemantic := make([]WeightedEdge, 0, len(semanticEdges))
	for _, e := range semanticEdges {
		if e.Weight > redundancyThreshold {
			continue // too similar - redundant
		}
		if _, ok := nodeIndex[e.From]; ok {
			if _, ok2 := nodeIndex[e.To]; ok2 {
				filteredSemantic = append(filteredSemantic, e)
			}
		}
	}

	// 3. Combine all edges
	allEdges := make([]WeightedEdge, 0, len(schemaEdges)+len(filteredSemantic))
	allEdges = append(allEdges, schemaEdges...)
	allEdges = append(allEdges, filteredSemantic...)

	// 4. Sort edges by weight descending (Kruskal)
	sort.Slice(allEdges, func(i, j int) bool { return allEdges[i].Weight > allEdges[j].Weight })

	// 5. Kruskal selection
	uf := NewUnionFind(len(seedNodes))
	selected := make(map[string]bool)
	var forest []WeightedEdge

	for _, e := range allEdges {
		fi, ok1 := nodeIndex[e.From]
		ti, ok2 := nodeIndex[e.To]
		if !ok1 || !ok2 {
			continue
		}
		if uf.Union(fi, ti) {
			forest = append(forest, e)
			selected[e.From] = true
			selected[e.To] = true
			// We don't stop early here because we want to include as many non-redundant edges as possible
			// up to the k nodes budget.
		}
	}

	// 6. Collect selected node IDs (preserve original order)
	var nodes []string
	for _, id := range seedNodes {
		if selected[id] {
			nodes = append(nodes, id)
			if len(nodes) >= k {
				break
			}
		}
	}

	// If no edges selected any nodes, but we have seed nodes, just take the first k
	if len(nodes) == 0 && len(seedNodes) > 0 {
		for i := 0; i < k && i < len(seedNodes); i++ {
			nodes = append(nodes, seedNodes[i])
		}
	}

	return nodes, forest
}
