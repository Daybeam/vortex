package core

import (
	"sync"
	"time"
)

// readCacheTTL bounds how long a cached read result stays valid. It is a
// safety net against stale data when external state mutates out-of-band
// (i.e. not through this engine's write path). Writes through this engine
// invalidate immediately (DomainToolReadCache.Invalidate), so the TTL only
// matters for mutations we cannot observe. It is intentionally generous:
// within-task repetition happens in seconds, while simulations/tasks are
// separated by far more than this, so the TTL also prevents any cross-task
// leakage in shared-engine deployments.
const readCacheTTL = 5 * time.Minute

// readCacheMaxEntries is a simple global cap. When exceeded the whole cache is
// dropped — a coarse but cheap bound that prevents unbounded growth across
// many distinct tools/tasks/servers.
const readCacheMaxEntries = 20000

// DomainToolReadCache is a universal, process-wide read-through cache for
// domain MCP tool results. It breaks repetition loops (execution
// amplification) by replaying the previously computed result for an identical
// read call instead of re-executing it.
//
// Design (generic — no eval- or domain-specific coupling):
//   - Keyed by (mcpID, toolName, normalized args). Any read-only domain tool
//     (get_/list_/search_/query_/read_/fetch_/check_/is_/has_*) is eligible;
//     pollers (progress/health/wait) and state-changing tools are excluded by
//     the caller via isReadTool/isPollerTool from sieve.go.
//   - A state-changing call on an mcpID invalidates every cached read for that
//     server (Invalidate), keeping the cache coherent with external writes.
//   - A TTL bounds staleness for out-of-band mutations.
//
// This replaces the earlier chat_harness-only cache, which never fired for the
// real workload because domain tools execute in the Spawner, not in
// chat_harness (see docs/25-cache-replay-runtime-miss.md §2).
type DomainToolReadCache struct {
	mu      sync.Mutex
	entries map[string]map[string]*readCacheEntry // mcpID -> (key -> entry)
	total   int
}

type readCacheEntry struct {
	md string
	ts time.Time
}

// NewDomainToolReadCache constructs an empty cache.
func NewDomainToolReadCache() *DomainToolReadCache {
	return &DomainToolReadCache{
		entries: make(map[string]map[string]*readCacheEntry),
	}
}

func readCacheKey(mcpID, tool string, args map[string]any) string {
	return mcpID + "|" + toolCallKey(tool, args)
}

// Get returns a cached result for an identical read call, if present and
// within TTL. Expired entries are evicted lazily.
func (c *DomainToolReadCache) Get(mcpID, tool string, args map[string]any) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bucket, ok := c.entries[mcpID]
	if !ok {
		return "", false
	}
	k := readCacheKey(mcpID, tool, args)
	e, ok := bucket[k]
	if !ok {
		return "", false
	}
	if time.Since(e.ts) > readCacheTTL {
		delete(bucket, k)
		c.total--
		return "", false
	}
	return e.md, true
}

// Put stores a successful read result. Caller must ensure the tool is a read.
func (c *DomainToolReadCache) Put(mcpID, tool string, args map[string]any, md string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total >= readCacheMaxEntries {
		// Coarse eviction: drop everything. Cheap and correct.
		c.entries = make(map[string]map[string]*readCacheEntry)
		c.total = 0
	}
	k := readCacheKey(mcpID, tool, args)
	bucket, ok := c.entries[mcpID]
	if !ok {
		bucket = make(map[string]*readCacheEntry)
		c.entries[mcpID] = bucket
	}
	if _, existed := bucket[k]; !existed {
		c.total++
	}
	bucket[k] = &readCacheEntry{md: md, ts: time.Now()}
}

// Invalidate drops every cached read for an mcpID. Called whenever a
// state-changing (write) tool runs on that server.
func (c *DomainToolReadCache) Invalidate(mcpID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bucket, ok := c.entries[mcpID]; ok {
		c.total -= len(bucket)
		delete(c.entries, mcpID)
	}
}

// Clear drops the entire cache (e.g. on a full environment reset).
func (c *DomainToolReadCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]map[string]*readCacheEntry)
	c.total = 0
}
