package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ListProjectStructure returns a tree-like overview of the project directory.
func (h *Handler) listProjectStructure(depth int) ToolResult {
	if h.rootDir == "" {
		return fail("not indexed or root_dir not set")
	}

	if depth <= 0 {
		depth = 3 // Default depth
	}

	tree, err := buildTree(h.rootDir, h.rootDir, depth)
	if err != nil {
		return failf("failed to build tree: %v", err)
	}

	return okResult(map[string]any{
		"root": rootName(h.rootDir),
		"tree": tree,
	})
}

func rootName(path string) string {
	return filepath.Base(path)
}

func buildTree(root, current string, depth int) (string, error) {
	if depth < 0 {
		return "", nil
	}

	entries, err := os.ReadDir(current)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, entry := range entries {
		name := entry.Name()
		// Basic ignore list
		if strings.HasPrefix(name, ".") && name != ".gitignore" && name != ".env.example" {
			continue
		}
		if name == "node_modules" || name == "vendor" || name == "tmp" || name == "bin" {
			continue
		}

		indent := strings.Repeat("  ", 3-depth)
		if entry.IsDir() {
			sb.WriteString(fmt.Sprintf("%s%s/\n", indent, name))
			subTree, _ := buildTree(root, filepath.Join(current, name), depth-1)
			sb.WriteString(subTree)
		} else {
			sb.WriteString(fmt.Sprintf("%s%s\n", indent, name))
		}
	}
	return sb.String(), nil
}
