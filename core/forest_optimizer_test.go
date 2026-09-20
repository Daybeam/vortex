package core

import (
	"testing"
)

func TestUnionFind(t *testing.T) {
	uf := NewUnionFind(5)
	if !uf.Union(0, 1) {
		t.Error("expected successful union of 0 and 1")
	}
	if !uf.Union(1, 2) {
		t.Error("expected successful union of 1 and 2")
	}
	if uf.Union(0, 2) {
		t.Error("expected redundant union of 0 and 2 to fail")
	}
	if uf.Find(0) != uf.Find(2) {
		t.Error("0 and 2 should be in same set")
	}
	if uf.Find(0) == uf.Find(3) {
		t.Error("0 and 3 should be in different sets")
	}
}

func TestBuildSemanticEdges(t *testing.T) {
	embeddings := map[string][]float32{
		"a": {1.0, 0.0, 0.0},
		"b": {0.9, 0.1, 0.0}, // close to a
		"c": {0.0, 1.0, 0.0}, // far from a and b
	}

	edges := BuildSemanticEdges(embeddings, 2)

	// a and b should have a mutual edge
	foundAB := false
	for _, e := range edges {
		if (e.From == "a" && e.To == "b") || (e.From == "b" && e.To == "a") {
			foundAB = true
			if e.Weight < 0.8 {
				t.Errorf("expected high weight for a-b, got %f", e.Weight)
			}
		}
	}
	if !foundAB {
		t.Error("expected mutual semantic edge between a and b")
	}
}

func TestMaxWeightForest(t *testing.T) {
	seedNodes := []string{"a", "b", "c", "d"}
	schemaEdges := []WeightedEdge{
		{From: "a", To: "b", Weight: 1.0, EdgeType: "schema"},
	}
	semanticEdges := []WeightedEdge{
		{From: "b", To: "c", Weight: 0.8, EdgeType: "semantic"},
		{From: "c", To: "d", Weight: 0.9, EdgeType: "semantic"},
		{From: "a", To: "d", Weight: 0.1, EdgeType: "semantic"},
	}

	nodes, forest := MaxWeightForest(seedNodes, schemaEdges, semanticEdges, 0.85, 3)

	if len(nodes) > 3 {
		t.Errorf("expected at most 3 nodes, got %d", len(nodes))
	}

	foundAB := false
	foundBC := false
	foundCD := false
	for _, e := range forest {
		if e.From == "a" && e.To == "b" {
			foundAB = true
		}
		if e.From == "b" && e.To == "c" {
			foundBC = true
		}
		if e.From == "c" && e.To == "d" {
			foundCD = true
		}
	}

	if !foundAB {
		t.Error("schema edge a-b should be in forest")
	}
	if !foundBC {
		t.Error("semantic edge b-c should be in forest")
	}
	if foundCD {
		t.Error("redundant edge c-d should NOT be in forest")
	}
}

func TestMaxWeightForest_KExceedsCandidates(t *testing.T) {
	seedNodes := []string{"a", "b"}
	schemaEdges := []WeightedEdge{
		{From: "a", To: "b", Weight: 1.0, EdgeType: "schema"},
	}
	nodes, _ := MaxWeightForest(seedNodes, schemaEdges, nil, 0.85, 10)

	if len(nodes) != 2 {
		t.Errorf("expected 2 nodes when K exceeds candidates, got %d", len(nodes))
	}
}

func TestMaxWeightForest_AllRedundant(t *testing.T) {
	seedNodes := []string{"a", "b", "c"}
	schemaEdges := []WeightedEdge{}
	semanticEdges := []WeightedEdge{
		// 全部超过阈值，都是冗余
		{From: "a", To: "b", Weight: 0.99, EdgeType: "semantic"},
		{From: "b", To: "c", Weight: 0.98, EdgeType: "semantic"},
	}
	nodes, forest := MaxWeightForest(seedNodes, schemaEdges, semanticEdges, 0.85, 3)

	if len(forest) != 0 {
		t.Errorf("expected 0 forest edges when all redundant, got %d", len(forest))
	}
	// 退化为按种子顺序取
	if len(nodes) != 3 {
		t.Errorf("expected 3 nodes in fallback mode, got %d", len(nodes))
	}
}

func TestMaxWeightForest_Empty(t *testing.T) {
	nodes, forest := MaxWeightForest([]string{}, nil, nil, 0.85, 5)

	if len(nodes) != 0 {
		t.Errorf("expected 0 nodes for empty seeds, got %d", len(nodes))
	}
	if len(forest) != 0 {
		t.Errorf("expected 0 edges for empty seeds, got %d", len(forest))
	}
}
