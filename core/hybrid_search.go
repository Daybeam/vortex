package core

import (
	"github.com/daybeam/vortex/pkg/search"
)

type BM25Corpus = search.BM25Corpus

func NewBM25Corpus(k1, b float64, docs map[string]string) *BM25Corpus {
	return search.NewBM25Corpus(k1, b, docs)
}

func RRFMerge(rankings ...map[string]float64) map[string]float64 {
	return search.RRFMerge(rankings...)
}

func CosineRank(queryVec []float32, candidates map[string][]float32) map[string]float64 {
	return search.CosineRank(queryVec, candidates)
}

func tokenize(text string) map[string]int {
	return search.Tokenize(text)
}
