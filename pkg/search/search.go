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
	return RRFMergeCapped(0, rankings...)
}

// RRFMergeCapped fuses multiple rankings using RRF with a per-source cap.
// maxPerSource <= 0 means no cap (process all items in each ranking).
// maxPerSource > 0 truncates each ranking to its top-K before fusion,
// preventing a noisy arm from flooding the fusion pool.
func RRFMergeCapped(maxPerSource int, rankings ...map[string]float64) map[string]float64 {
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
		// Deterministic sort: primary by score descending, secondary by ID
		// ascending. The secondary key only makes the OUTPUT order stable for
		// display/serialization; tied items receive the same fusion credit
		// below, so their relative order cannot influence the fused score.
		sort.Slice(items, func(i, j int) bool {
			if items[i].Score != items[j].Score {
				return items[i].Score > items[j].Score
			}
			return items[i].ID < items[j].ID
		})

		// Pre-fusion cap: truncate to top maxPerSource items per source.
		// If the cap falls in the middle of a tie group, extend to include
		// the entire group — truncating inside a tie group gives
		// alphabetically-earlier items credit while later ones get nothing,
		// fabricating an ordering the channel never expressed.
		if maxPerSource > 0 && len(items) > maxPerSource {
			cap := maxPerSource
			for cap < len(items) && items[cap].Score == items[cap-1].Score {
				cap++
			}
			items = items[:cap]
		}

		// Tied items share one rank credit (the average of the positions they
		// occupy). RRF is rank-based, so handing position 0,1,2,... to items
		// that all scored identically fabricates an ordering the channel never
		// expressed: the "ranking" degenerates into whatever the tie-break key
		// happens to be, and that fabricated order then carries the same weight
		// as a channel with real discriminating power.
		//
		// This is the textbook RRF behaviour for ties. It matters most when a
		// lexical matcher fires flat over a whole family of identifiers (one
		// query token matching a shared prefix/suffix): the family ends up with
		// one identical contribution, which cancels out when comparing members
		// of that family and lets a channel that actually discriminates decide
		// the order between them.
		for i := 0; i < len(items); {
			j := i
			for j+1 < len(items) && items[j+1].Score == items[i].Score {
				j++
			}
			rrfScore := 1.0 / (k + (float64(i)+float64(j))/2)
			for t := i; t <= j; t++ {
				merged[items[t].ID] += rrfScore
			}
			i = j + 1
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
