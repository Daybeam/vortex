package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type MemoryBank struct {
	ActiveContext  ActiveContext `json:"active_context"`
	Decisions      []Decision    `json:"decisions"`
	ProductContext string        `json:"product_context"`
	Progress       Progress      `json:"progress"`
	SystemPatterns []string      `json:"system_patterns"`
	UserProfile    UserProfile   `json:"user_profile"`
}

// UserProfile captures user preferences, expertise, and interaction patterns.
// The ChatHarness injects this into the system prompt so the gateway agent can
// disambiguate vague intents and delegate precisely. Maintained externally
// (REST API or config file); the ChatHarness only loads and injects it.
type UserProfile struct {
	Preferences        []string `json:"preferences"`         // e.g. "prefers Python", "uses React"
	Expertise          []string `json:"expertise"`           // e.g. "backend", "data science"
	PastDelegations    []string `json:"past_delegations"`    // summaries of past delegated tasks
	CommunicationStyle string   `json:"communication_style"` // e.g. "concise", "detailed"
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

// Save writes all MemoryBank categories to their respective markdown files.
// ADDED 2026-10-17. See docs/plan/MEMORYBANK_CONTROL_LAYER_DESIGN.md §改动 2.
func (s *MemoryBankStore) Save(mb MemoryBank) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return err
	}

	// activeContext.md
	var ac strings.Builder
	if len(mb.ActiveContext.Goals) > 0 {
		ac.WriteString("## Goals\n")
		for _, g := range mb.ActiveContext.Goals {
			ac.WriteString("- " + g + "\n")
		}
	}
	if len(mb.ActiveContext.RecentChanges) > 0 {
		ac.WriteString("## Recent Changes\n")
		for _, c := range mb.ActiveContext.RecentChanges {
			ac.WriteString("- " + c + "\n")
		}
	}
	if len(mb.ActiveContext.OpenQuestions) > 0 {
		ac.WriteString("## Open Questions\n")
		for _, q := range mb.ActiveContext.OpenQuestions {
			ac.WriteString("- " + q + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, "activeContext.md"), []byte(ac.String()), 0644); err != nil {
		return err
	}

	// decisionLog.md
	var dl strings.Builder
	for _, d := range mb.Decisions {
		dl.WriteString("### " + d.Title + "\n")
		if d.Context != "" {
			dl.WriteString(d.Context + "\n")
		}
		if d.Decision != "" {
			dl.WriteString("**Decision:** " + d.Decision + "\n")
		}
		if d.Rationale != "" {
			dl.WriteString("**Rationale:** " + d.Rationale + "\n")
		}
		dl.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, "decisionLog.md"), []byte(dl.String()), 0644); err != nil {
		return err
	}

	// productContext.md
	if err := os.WriteFile(filepath.Join(s.baseDir, "productContext.md"), []byte(mb.ProductContext), 0644); err != nil {
		return err
	}

	// progress.md
	var pg strings.Builder
	if len(mb.Progress.Done) > 0 {
		pg.WriteString("## Done\n")
		for _, d := range mb.Progress.Done {
			pg.WriteString("- [x] " + d + "\n")
		}
	}
	if len(mb.Progress.Current) > 0 {
		pg.WriteString("## In Progress\n")
		for _, c := range mb.Progress.Current {
			pg.WriteString("- [ ] " + c + "\n")
		}
	}
	if len(mb.Progress.Next) > 0 {
		pg.WriteString("## Next\n")
		for _, n := range mb.Progress.Next {
			pg.WriteString("- [ ] " + n + "\n")
		}
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, "progress.md"), []byte(pg.String()), 0644); err != nil {
		return err
	}

	// systemPatterns.md
	var sp strings.Builder
	for _, p := range mb.SystemPatterns {
		sp.WriteString("- " + p + "\n")
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, "systemPatterns.md"), []byte(sp.String()), 0644); err != nil {
		return err
	}

	// userProfile.md — reuse existing SaveUserProfile logic inline
	var up strings.Builder
	if len(mb.UserProfile.Preferences) > 0 {
		up.WriteString("## Preferences\n")
		for _, p := range mb.UserProfile.Preferences {
			up.WriteString("- " + p + "\n")
		}
	}
	if len(mb.UserProfile.Expertise) > 0 {
		up.WriteString("## Expertise\n")
		for _, e := range mb.UserProfile.Expertise {
			up.WriteString("- " + e + "\n")
		}
	}
	if len(mb.UserProfile.PastDelegations) > 0 {
		up.WriteString("## Past Delegations\n")
		for _, d := range mb.UserProfile.PastDelegations {
			up.WriteString("- " + d + "\n")
		}
	}
	if mb.UserProfile.CommunicationStyle != "" {
		up.WriteString("## Communication Style\n" + mb.UserProfile.CommunicationStyle + "\n")
	}
	if err := os.WriteFile(filepath.Join(s.baseDir, "userProfile.md"), []byte(up.String()), 0644); err != nil {
		return err
	}

	return nil
}

// SaveUserProfile writes the user profile to userProfile.md in the memory-bank directory.
func (s *MemoryBankStore) SaveUserProfile(up UserProfile) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	if len(up.Preferences) > 0 {
		b.WriteString("## Preferences\n")
		for _, p := range up.Preferences {
			b.WriteString("- " + p + "\n")
		}
	}
	if len(up.Expertise) > 0 {
		b.WriteString("## Expertise\n")
		for _, e := range up.Expertise {
			b.WriteString("- " + e + "\n")
		}
	}
	if len(up.PastDelegations) > 0 {
		b.WriteString("## Past Delegations\n")
		for _, d := range up.PastDelegations {
			b.WriteString("- " + d + "\n")
		}
	}
	if up.CommunicationStyle != "" {
		b.WriteString("## Communication Style\n" + up.CommunicationStyle + "\n")
	}

	if err := os.MkdirAll(s.baseDir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.baseDir, "userProfile.md"), []byte(b.String()), 0644)
}

func (s *MemoryBankStore) Load() (*MemoryBank, error) {
	s.mu.RLock()

	mb := &MemoryBank{
		ActiveContext:  ActiveContext{Goals: []string{}, RecentChanges: []string{}, OpenQuestions: []string{}},
		Decisions:      []Decision{},
		Progress:       Progress{Done: []string{}, Current: []string{}, Next: []string{}},
		SystemPatterns: []string{},
		UserProfile:    UserProfile{Preferences: []string{}, Expertise: []string{}, PastDelegations: []string{}},
	}

	// audit C-HIGH-3: read+parse files under RLock (ensures consistency with
	// concurrent Save), but defer DB writes until after RUnlock to avoid
	// blocking all writers for the duration of DB round-trips.
	type pendingSave struct{ category, key, content string }
	var pending []pendingSave

	// 1. activeContext.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "activeContext.md")); err == nil {
		content := string(data)
		mb.ActiveContext = parseActiveContext(content)
		if s.backend != nil {
			pending = append(pending, pendingSave{"active_context", "raw", content})
		}
	}

	// 2. decisionLog.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "decisionLog.md")); err == nil {
		content := string(data)
		mb.Decisions = parseDecisionLog(content)
		if s.backend != nil {
			pending = append(pending, pendingSave{"decisions", "raw", content})
		}
	}

	// 3. productContext.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "productContext.md")); err == nil {
		content := string(data)
		mb.ProductContext = content
		if s.backend != nil {
			pending = append(pending, pendingSave{"product_context", "raw", content})
		}
	}

	// 4. progress.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "progress.md")); err == nil {
		content := string(data)
		mb.Progress = parseProgress(content)
		if s.backend != nil {
			pending = append(pending, pendingSave{"progress", "raw", content})
		}
	}

	// 5. systemPatterns.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "systemPatterns.md")); err == nil {
		content := string(data)
		mb.SystemPatterns = parseList(content)
		if s.backend != nil {
			pending = append(pending, pendingSave{"system_patterns", "raw", content})
		}
	}

	// 6. userProfile.md
	if data, err := os.ReadFile(filepath.Join(s.baseDir, "userProfile.md")); err == nil {
		content := string(data)
		mb.UserProfile = parseUserProfile(content)
		if s.backend != nil {
			pending = append(pending, pendingSave{"user_profile", "raw", content})
		}
	}

	s.mu.RUnlock()

	// DB writes with timeout — no longer holding RLock.
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, p := range pending {
		s.backend.SaveItem(saveCtx, p.category, p.key, p.content, nil)
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

// parseUserProfile parses a markdown file with sections:
// ## Preferences, ## Expertise, ## Past Delegations, ## Communication Style.
func parseUserProfile(content string) UserProfile {
	up := UserProfile{Preferences: []string{}, Expertise: []string{}, PastDelegations: []string{}}
	currentSection := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			currentSection = strings.ToLower(strings.TrimSpace(line[3:]))
			continue
		}
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			item := strings.TrimSpace(line[2:])
			switch currentSection {
			case "preferences":
				up.Preferences = append(up.Preferences, item)
			case "expertise":
				up.Expertise = append(up.Expertise, item)
			case "past delegations", "past_delegations":
				up.PastDelegations = append(up.PastDelegations, item)
			}
			continue
		}
		if currentSection == "communication style" && line != "" {
			up.CommunicationStyle = line
		}
	}
	return up
}
