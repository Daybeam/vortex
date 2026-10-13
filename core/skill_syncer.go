package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
)

// SkillSyncer is the skill analog of CookbookSyncer: periodically syncs skills
// from trusted public GitHub repos (e.g. mattpocock/skills) into the registry.
//
// Lifecycle: Start() launches a background goroutine that calls SyncAll() on a
// ticker (interval from System.SkillSyncIntervalHours, default 24h). Stop()
// cancels the context and closes the stop channel. Wire in main.go alongside
// CookbookSyncer.
//
// The sync_skills action in tools/subsystems.go provides on-demand sync
// (user-triggered), while this struct provides periodic background sync
// (ticker-driven, like CookbookSyncer).
type SkillSyncer struct {
	registry       *config.Registry
	resourceLoader *ResourceLoader
	logger         *Logger
	stopChan       chan struct{}
	stopOnce       sync.Once
	ctx            context.Context
	cancel         context.CancelFunc
}

func NewSkillSyncer(registry *config.Registry, loader *ResourceLoader, logger *Logger) *SkillSyncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &SkillSyncer{
		registry:       registry,
		resourceLoader: loader,
		logger:         logger,
		stopChan:       make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Start launches the periodic sync goroutine. Idempotent via stopOnce.
func (s *SkillSyncer) Start() {
	go s.loop()
}

// Stop cancels the sync context and closes the stop channel. Idempotent.
func (s *SkillSyncer) Stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		close(s.stopChan)
	})
}

// loop is the periodic sync goroutine, mirroring CookbookSyncer.loop().
func (s *SkillSyncer) loop() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "[skill-syncer] goroutine panic: %v\n%s", r, debug.Stack())
		}
	}()

	interval := time.Duration(s.registry.System.SkillSyncIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial sync
	s.SyncAll()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.SyncAll()
		}
	}
}

// SyncAll iterates over SkillSyncSources and syncs each repo.
// SkillSyncSources is a map[string]string of "category" → "owner/repo/path".
func (s *SkillSyncer) SyncAll() {
	s.registry.Mu.RLock()
	sources := make(map[string]string)
	if s.registry.SkillSyncSources != nil {
		for k, v := range s.registry.SkillSyncSources {
			sources[k] = v
		}
	}
	s.registry.Mu.RUnlock()

	if len(sources) == 0 {
		return
	}

	s.logger.Log("EventSkillSyncAllStarted", "", "", map[string]any{
		"source_count": len(sources),
	})

	ctx := s.ctx
	for category, source := range sources {
		if ctx.Err() != nil {
			break // shutdown in progress
		}
		owner, repo, path, isRepo := parseGitHubRepoSource(source)
		if !isRepo {
			s.logger.Log("EventSkillSyncWarning", "", category, map[string]any{
				"error":  "invalid source format",
				"source": source,
			})
			continue
		}
		s.SyncRepo(ctx, owner, repo, path)
	}

	s.logger.Log("EventSkillSyncAllCompleted", "", "", nil)
}

// SyncRepo downloads skills from a GitHub repo and saves them as local JSON files.
func (s *SkillSyncer) SyncRepo(ctx context.Context, owner, repo, path string) (int, error) {
	s.logger.Log("EventSkillSyncStarted", "RemoteSync", "", map[string]any{
		"owner": owner,
		"repo":  repo,
		"path":  path,
	})

	files, err := s.resourceLoader.FetchRepoContents(ctx, owner, repo, path)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch repo contents: %w", err)
	}

	syncedCount := 0
	skillsDir := filepath.Join("data", "skills")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return 0, fmt.Errorf("failed to create skills dir: %w", err)
	}

	for _, file := range files {
		if file.Type != "file" {
			continue
		}
		if !strings.HasSuffix(file.Name, ".md") && !strings.HasSuffix(file.Name, ".json") {
			continue
		}

		content, err := s.resourceLoader.Fetch(ctx, file.DownloadURL)
		if err != nil {
			s.logger.Log("EventSkillSyncWarning", "RemoteSync", file.Name, map[string]any{
				"error": fmt.Sprintf("failed to fetch skill: %v", err),
			})
			continue
		}

		skill, err := s.parseSkill(file.Name, content)
		if err != nil {
			s.logger.Log("EventSkillSyncWarning", "RemoteSync", file.Name, map[string]any{
				"error": fmt.Sprintf("failed to parse skill: %v", err),
			})
			continue
		}

		// Save to disk as JSON for the Registry to load on startup
		skillData, err := json.MarshalIndent(skill, "", "  ")
		if err != nil {
			continue
		}
		
		skillFileName := strings.TrimSuffix(file.Name, filepath.Ext(file.Name)) + ".json"
		if err := os.WriteFile(filepath.Join(skillsDir, skillFileName), skillData, 0644); err != nil {
			continue
		}

		// Also update in-memory registry
		s.registry.Mu.Lock()
		s.registry.Skills[skill.ID] = skill
		s.registry.Mu.Unlock()

		syncedCount++
	}

	s.logger.Log("EventSkillSyncCompleted", "RemoteSync", "", map[string]any{
		"synced_count": syncedCount,
	})

	return syncedCount, nil
}

func (s *SkillSyncer) parseSkill(filename, content string) (*config.Skill, error) {
	// If it's already JSON, just unmarshal it
	if strings.HasSuffix(filename, ".json") {
		var skill config.Skill
		if err := json.Unmarshal([]byte(content), &skill); err == nil {
			return &skill, nil
		}
	}

	// If it's Markdown (like google/skills), we do a basic heuristic parse
	// In a real implementation, we would use a proper parser for SKILL.md
	// For now, we'll wrap the MD content into a default skill structure
	
	// Extract ID from filename or content
	id := strings.TrimSuffix(filename, filepath.Ext(filename))
	id = strings.ReplaceAll(id, " ", "_")
	id = strings.ToLower(id)

	// Heuristic: Look for a "Name:" or "# " for the skill name
	name := id
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			name = strings.TrimPrefix(line, "# ")
			break
		}
	}

	return &config.Skill{
		ID:          id,
		Name:        name,
		Capability:  "general", // Default, would be extracted in a real parser
		Description: "Imported from remote repository",
		Implementations: map[string]config.SkillImplementation{
			"default": {
				SystemPrompt:  content,
				PromptVersion: "remote-1.0",
			},
		},
	}, nil
}
