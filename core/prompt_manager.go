package core

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
)

// PromptManager manages external prompt assets and their rendering.
type PromptManager struct {
	promptDir string
	loader    *ResourceLoader
	mu        sync.RWMutex
	templates map[string]*template.Template
}

const maxTemplateCacheEntries = 500 // cap on template cache; clearing is safe (re-parses on next use)

// NewPromptManager creates a new PromptManager.
func NewPromptManager(promptDir string, loader *ResourceLoader) *PromptManager {
	if loader == nil {
		loader = NewResourceLoader()
	}
	return &PromptManager{
		promptDir: promptDir,
		loader:    loader,
		templates: make(map[string]*template.Template),
	}
}

// GetPrompt retrieves a prompt from the warehouse or a remote URL.
func (pm *PromptManager) GetPrompt(ref string) (string, error) {
	target := ref

	// If it's not a URL and not an absolute path, and we have a promptDir,
	// join it with promptDir.
	if !strings.HasPrefix(ref, "http://") && !strings.HasPrefix(ref, "https://") && !filepath.IsAbs(ref) {
		if pm.promptDir != "" {
			target = filepath.Join(pm.promptDir, ref)
		}
	}

	return pm.loader.Fetch(context.Background(), target)
}

// GetAndRender fetches a prompt (local or remote) and renders it with context.
func (pm *PromptManager) GetAndRender(ref string, context any) (string, error) {
	raw, err := pm.GetPrompt(ref)
	if err != nil {
		return "", err
	}
	return pm.Render(ref, raw, context)
}

// Render renders a prompt template with the provided context.
func (pm *PromptManager) Render(ref string, rawPrompt string, context any) (string, error) {
	pm.mu.RLock()
	tmpl, ok := pm.templates[ref]
	pm.mu.RUnlock()

	if !ok {
		var err error
		tmpl, err = template.New(ref).Parse(rawPrompt)
		if err != nil {
			return "", fmt.Errorf("failed to parse template %s: %w", ref, err)
		}
		pm.mu.Lock()
		pm.templates[ref] = tmpl
		// Cap the template cache to prevent unbounded growth.
		if len(pm.templates) > maxTemplateCacheEntries {
			pm.templates = make(map[string]*template.Template)
			pm.templates[ref] = tmpl
		}
		pm.mu.Unlock()
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, context); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", ref, err)
	}

	return buf.String(), nil
}

// ClearCache clears the template cache and resource loader cache,
// forcing re-parsing/re-fetching on next use.
func (pm *PromptManager) ClearCache() {
	pm.mu.Lock()
	pm.templates = make(map[string]*template.Template)
	pm.mu.Unlock()

	pm.loader.Mu.Lock()
	pm.loader.cache = make(map[string]string)
	pm.loader.Mu.Unlock()
}
