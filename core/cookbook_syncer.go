package core

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daybeam/vortex/config"
)

// CookbookSyncer handles periodic background synchronization of cookbook repositories.
type CookbookSyncer struct {
	registry       *config.Registry
	resourceLoader *ResourceLoader
	logger         *Logger
	stopChan       chan struct{}
	stopOnce       sync.Once
	ctx            context.Context
	cancel         context.CancelFunc
}

func NewCookbookSyncer(reg *config.Registry, loader *ResourceLoader, logger *Logger) *CookbookSyncer {
	ctx, cancel := context.WithCancel(context.Background())
	return &CookbookSyncer{
		registry:       reg,
		resourceLoader: loader,
		logger:         logger,
		stopChan:       make(chan struct{}),
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (s *CookbookSyncer) Start() {
	go s.loop()
}

func (s *CookbookSyncer) Stop() {
	s.stopOnce.Do(func() {
		s.cancel()
		close(s.stopChan)
	})
}

func (s *CookbookSyncer) loop() {
	interval := time.Duration(s.registry.System.CookbookSyncIntervalHours) * time.Hour
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

func (s *CookbookSyncer) SyncAll() {
	s.registry.Mu.RLock()
	sources := make(map[string]string)
	if s.registry.RoleCookbookSources != nil {
		for k, v := range s.registry.RoleCookbookSources {
			sources[k] = v
		}
	}
	if s.registry.RoleCookbookSource != "" {
		sources["legacy"] = s.registry.RoleCookbookSource
	}
	s.registry.Mu.RUnlock()

	if len(sources) == 0 {
		return
	}

	s.logger.Log("CookbookSyncStarted", "", "", map[string]any{
		"repo_count": len(sources),
	})

	ctx := s.ctx
	for family, source := range sources {
		if ctx.Err() != nil {
			break // shutdown in progress
		}
		s.syncRepo(ctx, family, source)
	}

	s.logger.Log("CookbookSyncCompleted", "", "", nil)
}

func (s *CookbookSyncer) syncRepo(ctx context.Context, family, source string) {
	owner, repo, path, isRepo := parseGitHubRepoSource(source)
	if !isRepo {
		return
	}

	s.logger.Log("CookbookSyncRepoStarted", "", "", map[string]any{
		"family": family,
		"repo":   fmt.Sprintf("%s/%s", owner, repo),
	})

	// For background sync, we just pre-fetch the tree to warm up the cache
	// and ensure it's reachable. A more advanced version would also
	// download all file contents, but for now we rely on FetchCookbook
	// saving contents to disk when they are actually retrieved.
	_, err := s.resourceLoader.fetchRepoTreeRecursive(ctx, owner, repo, path)
	if err != nil {
		s.logger.Log("CookbookSyncRepoFailed", "", "", map[string]any{
			"family": family,
			"repo":   fmt.Sprintf("%s/%s", owner, repo),
			"error":  err.Error(),
		})
	} else {
		s.logger.Log("CookbookSyncRepoCompleted", "", "", map[string]any{
			"family": family,
			"repo":   fmt.Sprintf("%s/%s", owner, repo),
		})
	}
}
