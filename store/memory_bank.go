package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type MemoryBank struct {
	ActiveContext  ActiveContext `json:"active_context"`
	Decisions      []Decision    `json:"decisions"`
	ProductContext string        `json:"product_context"`
	Progress       Progress      `json:"progress"`
	SystemPatterns []string      `json:"system_patterns"`
}

type ActiveContext struct {
	Goals         []string `json:"goals"`
	RecentChanges []string `json:"recent_changes"`
	OpenQuestions []string `json:"open_questions"`
}

type Decision struct {
	Title     string `json:"title"`
	Context   string `json:"context"`
	Decision  string `json:"decision"`
	Rationale string `json:"rationale"`
}

type Progress struct {
	Done    []string `json:"done"`
	Current []string `json:"current"`
	Next    []string `json:"next"`
}

type MemoryBankStore struct {
	mu      sync.RWMutex
	baseDir string
	backend IMemoryBankBackend
}

func NewMemoryBankStore(baseDir string, backend IMemoryBankBackend) *MemoryBankStore {
	return &MemoryBankStore{
		baseDir: filepath.Join(baseDir, "memory-bank"),
		backend: backend,
	}
}

func (s *MemoryBankStore) Load() (*MemoryBank, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	mb := &MemoryBank{
		ActiveContext:  ActiveContext{Goals: []string{}, RecentChanges: []string{}, OpenQuestions: []string{}},
		Decisions:      []Decision{},
		Progress:       Progress{Done: []string{}, Current: []string{}, Next: []string{}},
		SystemPatterns: []string{},
	}

	// 1. activeContext.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "activeContext.md")); err == nil {
		content := string(data)
		mb.ActiveContext = parseActiveContext(content)
		if s.backend != nil {
			s.backend.SaveItem(context.Background(), "active_context", "raw", content, nil)
		}
	}

	// 2. decisionLog.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "decisionLog.md")); err == nil {
		content := string(data)
		mb.Decisions = parseDecisionLog(content)
		if s.backend != nil {
			s.backend.SaveItem(context.Background(), "decisions", "raw", content, nil)
		}
	}

	// 3. productContext.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "productContext.md")); err == nil {
		content := string(data)
		mb.ProductContext = content
		if s.backend != nil {
			s.backend.SaveItem(context.Background(), "product_context", "raw", content, nil)
		}
	}

	// 4. progress.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "progress.md")); err == nil {
		content := string(data)
		mb.Progress = parseProgress(content)
		if s.backend != nil {
			s.backend.SaveItem(context.Background(), "progress", "raw", content, nil)
		}
	}

	// 5. systemPatterns.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "systemPatterns.md")); err == nil {
		content := string(data)
		mb.SystemPatterns = parseList(content)
		if s.backend != nil {
			s.backend.SaveItem(context.Background(), "system_patterns", "raw", content, nil)
		}
	}

	return mb, nil
}

func parseActiveContext(content string) ActiveContext {
	ac := ActiveContext{Goals: []string{}, RecentChanges: []string{}, OpenQuestions: []string{}}
	lines := strings.Split(content, "\n")
	currentSection := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ") {
			currentSection = strings.ToLower(line)
			continue
		}
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			item := strings.TrimSpace(line[2:])
			if strings.Contains(currentSection, "goal") {
				ac.Goals = append(ac.Goals, item)
			} else if strings.Contains(currentSection, "change") || strings.Contains(currentSection, "recent") {
				ac.RecentChanges = append(ac.RecentChanges, item)
			} else if strings.Contains(currentSection, "question") {
				ac.OpenQuestions = append(ac.OpenQuestions, item)
			}
		}
	}
	return ac
}

func parseDecisionLog(content string) []Decision {
	var decisions []Decision
	parts := strings.Split(content, "### ")
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		lines := strings.Split(part, "\n")
		d := Decision{Title: strings.TrimSpace(lines[0])}
		for _, line := range lines[1:] {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "**Rationale:**") {
				d.Rationale = strings.TrimSpace(strings.TrimPrefix(line, "**Rationale:**"))
			} else if strings.HasPrefix(line, "**Decision:**") {
				d.Decision = strings.TrimSpace(strings.TrimPrefix(line, "**Decision:**"))
			}
		}
		if d.Title != "" {
			decisions = append(decisions, d)
		}
	}
	return decisions
}

func parseProgress(content string) Progress {
	p := Progress{Done: []string{}, Current: []string{}, Next: []string{}}
	lines := strings.Split(content, "\n")
	section := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			section = strings.ToLower(line)
			continue
		}
		if strings.HasPrefix(line, "- [x]") || strings.HasPrefix(line, "- [ ]") {
			item := strings.TrimSpace(line[5:])
			if strings.Contains(section, "done") || strings.Contains(section, "completed") {
				p.Done = append(p.Done, item)
			} else if strings.Contains(section, "current") || strings.Contains(section, "in progress") {
				p.Current = append(p.Current, item)
			} else if strings.Contains(section, "next") {
				p.Next = append(p.Next, item)
			} else {
				p.Current = append(p.Current, item)
			}
		}
	}
	return p
}

func parseList(content string) []string {
	var list []string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			list = append(list, strings.TrimSpace(line[2:]))
		}
	}
	return list
}
