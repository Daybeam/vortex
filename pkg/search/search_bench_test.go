package search

import (
	"fmt"
	"testing"
)

func BenchmarkBM25_Score_SmallCorpus(b *testing.B) {
	docs := map[string]string{
		"d1": "read file from disk",
		"d2": "write file to disk",
		"d3": "execute command in shell",
		"d4": "search files by name",
		"d5": "list directory contents",
	}
	corpus := NewBM25Corpus(1.2, 0.75, docs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		corpus.Score("read file")
	}
}

func BenchmarkBM25_Score_MediumCorpus(b *testing.B) {
	docs := make(map[string]string, 50)
	for i := 0; i < 50; i++ {
		docs[fmt.Sprintf("d%d", i)] = fmt.Sprintf("tool %d performs action %d on resource %d", i, i%10, i%5)
	}
	corpus := NewBM25Corpus(1.2, 0.75, docs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		corpus.Score("tool action resource")
	}
}

func BenchmarkBM25_Score_LargeCorpus(b *testing.B) {
	docs := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		docs[fmt.Sprintf("d%d", i)] = fmt.Sprintf("MCP server %d provides tool %d for capability %d with description %d", i, i%20, i%5, i)
	}
	corpus := NewBM25Corpus(1.2, 0.75, docs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		corpus.Score("MCP server tool capability")
	}
}

func BenchmarkBM25_NewCorpus(b *testing.B) {
	docs := make(map[string]string, 50)
	for i := 0; i < 50; i++ {
		docs[fmt.Sprintf("d%d", i)] = fmt.Sprintf("tool %d performs action %d on resource %d", i, i%10, i%5)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NewBM25Corpus(1.2, 0.75, docs)
	}
}
