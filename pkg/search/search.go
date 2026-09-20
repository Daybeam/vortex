package search

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// --- BM25 Core Implementation ---

// BM25Corpus is a reusable BM25 corpus, read-only after construction.
type BM25Corpus struct {
	k1        float64                   // BM25 parameter
	b         float64                   // BM25 parameter
	avgDL     float64                   // Average document length
	docLen    map[string]int            // docID -> doc length (token count)
	docFreq   map[string]map[string]int // term -> (docID -> count)
	totalDocs int
}

// NewBM25Corpus builds a corpus from a map of docID to text.
func NewBM25Corpus(k1, b float64, docs map[string]string) *BM25Corpus {
	c := &BM25Corpus{
		k1:      k1,
		b:       b,
		docLen:  make(map[string]int),
		docFreq: make(map[string]map[string]int),
	}
	totalLen := 0
	for id, text := range docs {
		tokens := Tokenize(text)
		dl := 0
		for _, cnt := range tokens {
			dl += cnt
		}
		c.docLen[id] = dl
		totalLen += dl
		for term, cnt := range tokens {
			if c.docFreq[term] == nil {
				c.docFreq[term] = make(map[string]int)
			}
			c.docFreq[term][id] = cnt
		}
	}
	c.totalDocs = len(docs)
	if c.totalDocs > 0 {
		c.avgDL = float64(totalLen) / float64(c.totalDocs)
	}
	return c
}

// Score computes the BM25 scores for a query against all documents.
func (c *BM25Corpus) Score(query string) map[string]float64 {
	qTokens := Tokenize(query)
	scores := make(map[string]float64)

	for id := range c.docLen {
		dl := float64(c.docLen[id])
		s := 0.0
		for term, qf := range qTokens {
			df := len(c.docFreq[term])
			if df == 0 {
				continue
			}
			// IDF with smoothing
			idf := math.Log(1 + (float64(c.totalDocs)-float64(df)+0.5)/(float64(df)+0.5))
			tf := float64(c.docFreq[term][id])
			if tf == 0 {
				continue
			}
			termScore := idf * (tf * (c.k1 + 1)) / (tf + c.k1*(1-c.b+c.b*dl/c.avgDL))
			s += termScore * float64(qf)
		}
		if s > 0 {
			scores[id] = s
		}
	}
	return scores
}

// --- RRF Fusion ---

type Scored struct {
	ID    string
	Score float64
}

// RRFMerge fuses multiple rankings (docID -> score) using Reciprocal Rank Fusion.
func RRFMerge(rankings ...map[string]float64) map[string]float64 {
	merged := make(map[string]float64)
	k := 60.0 // Standard RRF constant
	for _, ranking := range rankings {
		if len(ranking) == 0 {
			continue
		}
		items := make([]Scored, 0, len(ranking))
		for id, score := range ranking {
			items = append(items, Scored{ID: id, Score: score})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Score > items[j].Score })

		for rank, item := range items {
			rrfScore := 1.0 / (k + float64(rank))
			merged[item.ID] += rrfScore
		}
	}
	return merged
}

// --- Common Tokenizer ---

var tokenizeStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "and": true, "or": true, "are": true, "is": true,
	"was": true, "were": true, "be": true, "this": true, "that": true,
	"with": true, "as": true, "at": true, "by": true, "it": true, "if": true,
	"please": true, "me": true, "my": true, "your": true, "you": true,
}

func Tokenize(text string) map[string]int {
	text = strings.ToLower(text)
	freq := make(map[string]int)
	var b strings.Builder
	flush := func() {
		w := b.String()
		b.Reset()
		if len([]rune(w)) >= 2 && !tokenizeStopwords[w] {
			freq[w]++
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return freq
}

// --- Embedding Similarity ---

// CosineRank computes cosine similarity scores for a query vector against candidates.
func CosineRank(queryVec []float32, candidates map[string][]float32) map[string]float64 {
	scores := make(map[string]float64)
	for id, vec := range candidates {
		sim := CosineSimilarity(queryVec, vec)
		if sim > 0 {
			scores[id] = sim
		}
	}
	return scores
}

func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0.0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0.0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
