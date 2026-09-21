package config

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Compactor manages the merging of WAL entries into the main configuration snapshot.
type Compactor struct {
	reg            *Registry
	thresholdCount int
	thresholdSize  int64
	interval       time.Duration
}

func NewCompactor(reg *Registry) *Compactor {
	return &Compactor{
		reg:            reg,
		thresholdCount: 50,
		thresholdSize:  1024 * 1024, // 1MB
		interval:       30 * time.Minute,
	}
}

// Start runs the compaction loop in a background goroutine.
func (c *Compactor) Start(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkAndCompact()
		}
	}
}

func (c *Compactor) checkAndCompact() {
	if c.reg.wal == nil {
		return
	}

	count, size, err := c.reg.wal.Stats()
	if err != nil {
		log.Printf("[Compactor] Failed to get WAL stats: %v", err)
		return
	}

	if count >= c.thresholdCount || size >= c.thresholdSize {
		log.Printf("[Compactor] Triggering compaction: %d commits, %d bytes", count, size)
		if err := c.Compact(); err != nil {
			log.Printf("[Compactor] Compaction failed: %v", err)
		} else {
			log.Printf("[Compactor] Compaction successful")
		}
	}
}

// Compact performs a manual compaction.
func (c *Compactor) Compact() error {
	// 1. Lock registry to ensure we have the latest memory state
	c.reg.Mu.Lock()
	defer c.reg.Mu.Unlock()

	// 2. Archive current snapshot before overwriting
	c.archiveCurrentSnapshot()

	// 3. Full Persist (which creates a new snapshot)
	if err := c.reg.PersistForceSnapshot(); err != nil {
		return err
	}

	// 4. Clear WAL
	return c.reg.wal.Clear()
}

func (c *Compactor) archiveCurrentSnapshot() {
	configDir := filepath.Dir(c.reg.configPath)
	archiveDir := filepath.Join(configDir, "config_archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		log.Printf("[Compactor] Failed to create archive dir: %v", err)
		return
	}

	timestamp := time.Now().Format("20060102_150405.000000")
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("config_%s.json", timestamp))

	// Copy current config.json to archive
	data, err := os.ReadFile(c.reg.configPath)
	if err != nil {
		log.Printf("[Compactor] Failed to read current config for archive: %v", err)
		return
	}

	if err := os.WriteFile(archivePath, data, 0644); err != nil {
		log.Printf("[Compactor] Failed to write archive snapshot: %v", err)
		return
	}

	// Cleanup old archives (keep 10)
	c.pruneArchives(archiveDir)
}

func (c *Compactor) pruneArchives(archiveDir string) {
	files, err := os.ReadDir(archiveDir)
	if err != nil {
		return
	}

	var archives []os.DirEntry
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), "config_") && strings.HasSuffix(f.Name(), ".json") {
			archives = append(archives, f)
		}
	}

	if len(archives) <= 10 {
		return
	}

	// Sort by name (which contains timestamp)
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].Name() < archives[j].Name()
	})

	// Delete oldest
	for i := 0; i < len(archives)-10; i++ {
		_ = os.Remove(filepath.Join(archiveDir, archives[i].Name()))
	}
}
