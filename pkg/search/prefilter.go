package search

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// DirectoryRouter builds a keyword → file index from the workspace's
// directory and file naming conventions. Zero external dependency.
//
// Design: docs/architecture/HYBRID_CODE_SEARCH_DESIGN.md §9.2.1
//
// File and directory names are natural semantic tags:
//
//	"core/scheduler_decision.go" → ["core", "scheduler", "decision"]
//	"tools/tools_task.go"        → ["tools", "task"]
//
// PreFilter tokenizes the query the same way and returns candidate files
// whose path contains matching tokens. Cost: < 1ms (pure memory map lookup).
type DirectoryRouter struct {
	fileIndex     map[string][]string // keyword → []filePath
	workspaceRoot string
}

// NewDirectoryRouter creates a router for the given workspace root.
func NewDirectoryRouter(workspaceRoot string) *DirectoryRouter {
	return &DirectoryRouter{
		fileIndex:     make(map[string][]string),
		workspaceRoot: workspaceRoot,
	}
}

// Build scans the workspace and builds the keyword index from file and
// directory names. Skips .git, vendor, node_modules, and hidden directories.
func (d *DirectoryRouter) Build() error {
	return filepath.WalkDir(d.workspaceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable paths
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(d.workspaceRoot, path)
		for _, tok := range tokenizeIdentifier(rel) {
			d.fileIndex[tok] = append(d.fileIndex[tok], path)
		}
		return nil
	})
}

// PreFilter returns candidate file paths whose name or directory path
// contains tokens matching the query. Returns nil if no matches,
// signaling the caller to degrade to full search.
func (d *DirectoryRouter) PreFilter(query string) []string {
	tokens := tokenizeIdentifier(query)
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var result []string
	for _, tok := range tokens {
		for _, path := range d.fileIndex[tok] {
			if !seen[path] {
				seen[path] = true
				result = append(result, path)
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// shouldSkipDir returns true for directories that should not be indexed.
func shouldSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	return name == "vendor" || name == "node_modules"
}

// tokenizeIdentifier splits a string into lowercase keyword tokens.
// Handles camelCase, snake_case, kebab-case, path separators, and file extensions.
//
// Examples:
//
//	"core/scheduler_decision.go" → ["core", "scheduler", "decision"]
//	"retryStep"                  → ["retry", "step"]
//	"HTTPServer"                 → ["http", "server"]
//	"tools-task"                 → ["tools", "task"]
func tokenizeIdentifier(s string) []string {
	// Strip file extension
	if ext := filepath.Ext(s); ext != "" {
		s = strings.TrimSuffix(s, ext)
	}

	runes := []rune(s)
	var tokens []string
	var buf strings.Builder

	flush := func() {
		if buf.Len() > 0 {
			w := strings.ToLower(buf.String())
			if len([]rune(w)) >= 2 { // skip single-char tokens
				tokens = append(tokens, w)
			}
			buf.Reset()
		}
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '_' || r == '-' || r == '/' || r == '\\' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r):
			if i > 0 {
				prev := runes[i-1]
				if unicode.IsLower(prev) {
					// camelCase boundary: retry|Step
					flush()
				} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					// Acronym → word boundary: HTTP|Server
					flush()
				}
			}
			buf.WriteRune(unicode.ToLower(r))
		default:
			buf.WriteRune(r)
		}
	}
	flush()

	return tokens
}

// --- RegexSymbolFilter ---

// symbolDefRegex matches common definition patterns across languages.
// Handles Go func/method, Python def, JS/TS function, class, interface,
// type, struct, const, enum — all followed by an identifier.
var symbolDefRegex = regexp.MustCompile(
	`(?:func\s+(?:\([^)]*\)\s*)?|def\s+|function\s+|class\s+|interface\s+|type\s+|struct\s+|const\s+|enum\s+)(\w+)`,
)

// sourceExtensions lists file extensions worth scanning for symbol definitions.
var sourceExtensions = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true,
	".jsx": true, ".tsx": true, ".java": true, ".rs": true,
	".c": true, ".cpp": true, ".h": true, ".hpp": true,
	".cs": true, ".rb": true, ".php": true, ".swift": true,
	".kt": true, ".scala": true,
}

// RegexSymbolFilter extracts symbol definitions from source files using
// common regex patterns across languages. Language-agnostic, zero external
// dependency.
//
// Design: docs/architecture/HYBRID_CODE_SEARCH_DESIGN.md §9.2.2b
//
// Unlike SymbolTableFilter (Go AST, precise but Go-only), this uses regex
// to match func/def/function/class/interface/type/struct/const/enum patterns
// across Go/Python/JS/TS/Java/Rust/C/C++ etc. Less precise (may match
// strings/comments) but language-agnostic.
type RegexSymbolFilter struct {
	symbols       map[string][]string // symbolName → []filePath
	workspaceRoot string
}

// NewRegexSymbolFilter creates a filter for the given workspace root.
func NewRegexSymbolFilter(workspaceRoot string) *RegexSymbolFilter {
	return &RegexSymbolFilter{
		symbols:       make(map[string][]string),
		workspaceRoot: workspaceRoot,
	}
}

// Build scans source files and extracts symbol definitions into the index.
func (s *RegexSymbolFilter) Build() error {
	return filepath.WalkDir(s.workspaceRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExtensions[filepath.Ext(path)] {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil // skip unreadable files
		}
		for _, m := range symbolDefRegex.FindAllSubmatch(content, -1) {
			name := string(m[1])
			s.symbols[name] = append(s.symbols[name], path)
		}
		return nil
	})
}

// PreFilter returns candidate file paths that define symbols matching
// the query. Returns nil if no matches, signaling the caller to degrade
// to DirectoryRouter or full search.
func (s *RegexSymbolFilter) PreFilter(query string) []string {
	candidates := extractSymbolCandidates(query)
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var result []string
	for _, sym := range candidates {
		for _, path := range s.symbols[sym] {
			if !seen[path] {
				seen[path] = true
				result = append(result, path)
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// extractSymbolCandidates extracts possible symbol names from a query.
// Handles: exact identifier ("retryStep"), multi-word ("retry step" →
// "retry", "step", "retryStep", "retry_step").
func extractSymbolCandidates(query string) []string {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}

	var candidates []string
	seen := make(map[string]bool)
	add := func(s string) {
		if len(s) >= 2 && !seen[s] {
			seen[s] = true
			candidates = append(candidates, s)
		}
	}

	// Try the whole query if it looks like an identifier
	if isIdentifier(query) {
		add(query)
	}

	// Try each token
	tokens := tokenizeIdentifier(query)
	for _, tok := range tokens {
		add(tok)
	}

	// Try camelCase and snake_case joins of tokens
	if len(tokens) > 1 {
		var cc strings.Builder
		for i, tok := range tokens {
			if i == 0 {
				cc.WriteString(tok)
			} else {
				cc.WriteString(strings.ToUpper(tok[:1]))
				if len(tok) > 1 {
					cc.WriteString(tok[1:])
				}
			}
		}
		add(cc.String())
		add(strings.Join(tokens, "_"))
	}

	return candidates
}

// isIdentifier returns true if s contains only letters, digits, and underscores.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			return false
		}
	}
	return true
}
