package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// ErrRepetitiveOutput is returned when the stream chunk detector identifies
// a repetitive output pattern (consecutive identical chunks or short-period
// cycle). Callers should inject a steer message and retry rather than abort.
var ErrRepetitiveOutput = errors.New("repetitive output detected")

// StreamChunkDetector detects intra-call repetitive output patterns in
// streaming LLM responses. It is designed to be per-call: create a new
// instance for each StreamComplete invocation.
//
// Detection modes (inspired by @argszero/cordis-plugin-thinking-loop-guard):
//  1. Consecutive identical chunks: >= maxConsecutive identical chunk hashes
//     in a row.
//  2. Short-period cycle: the tail of accumulated text has a period <=
//     maxPeriod bytes that spans >= minSpan bytes.
//
// Both modes use exact matching (not entropy). Cordis field data: normal
// output has periodicity=0, loop output has non-zero periodicity, false
// positive=0.
type StreamChunkDetector struct {
	// consecutive detection
	lastHash    string
	consecCount int
	maxConsec   int

	// short-period detection
	buf       strings.Builder
	maxPeriod int // maximum period length in bytes
	minSpan   int // minimum total span in bytes to trigger
	bufCap    int // hard cap on buffer size; trimmed when exceeded
}

// NewStreamChunkDetector creates a detector with default thresholds:
// - 60 consecutive identical chunks
// - period <= 64 bytes, span >= 256 bytes
// - buffer capped at 4096 bytes (trimmed to 2048)
func NewStreamChunkDetector() *StreamChunkDetector {
	return &StreamChunkDetector{
		maxConsec: 60,
		maxPeriod: 64,
		minSpan:   256,
		bufCap:    4096,
	}
}

// Check examines a single streaming chunk. It returns ErrRepetitiveOutput
// if a repetitive pattern is detected, nil otherwise.
func (d *StreamChunkDetector) Check(chunk string) error {
	// Mode 1: consecutive identical chunks (hash-based).
	h := hashChunk(chunk)
	if h == d.lastHash {
		d.consecCount++
	} else {
		d.lastHash = h
		d.consecCount = 1
	}
	if d.consecCount >= d.maxConsec {
		return ErrRepetitiveOutput
	}

	// Mode 2: short-period cycle in accumulated text.
	d.buf.WriteString(chunk)
	d.trimBuf()
	if d.detectShortPeriod() {
		return ErrRepetitiveOutput
	}

	return nil
}

// trimBuf keeps the buffer from growing unbounded. When it exceeds bufCap,
// only the last bufCap/2 bytes are retained.
func (d *StreamChunkDetector) trimBuf() {
	if d.buf.Len() <= d.bufCap {
		return
	}
	keep := d.bufCap / 2
	s := d.buf.String()
	d.buf.Reset()
	d.buf.WriteString(s[len(s)-keep:])
}

// detectShortPeriod checks whether the tail of the accumulated text is
// periodic with some period <= maxPeriod, spanning >= minSpan bytes.
//
// For each candidate period p, we verify that text[i] == text[i+p] for all
// i in the last minSpan bytes. If the entire window is p-periodic, the
// pattern has been repeating for at least minSpan/p cycles.
func (d *StreamChunkDetector) detectShortPeriod() bool {
	s := d.buf.String()
	n := len(s)
	if n < d.minSpan {
		return false
	}

	start := n - d.minSpan
	for period := 1; period <= d.maxPeriod; period++ {
		ok := true
		for i := start; i+period < n; i++ {
			if s[i] != s[i+period] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Text returns the accumulated text so far (for debugging / observability).
func (d *StreamChunkDetector) Text() string {
	return d.buf.String()
}

func hashChunk(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
