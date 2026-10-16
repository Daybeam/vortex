package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
)

func registerAdminSubsystem(app *App) {
	s := &Subsystem{
		Name:        "admin",
		Description: "Logs, stats, and task management.",
		Actions:     make(map[string]Action),
	}

	s.Actions["list_tasks"] = Action{
		Name:        "list_tasks",
		Description: "List task graphs.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return app.Scheduler.ListGraphs(), nil
		},
	}

	s.Actions["clear_task"] = Action{
		Name:        "clear_task",
		Description: "Clear a completed task.",
		Parameters:  map[string]any{"task_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "task_id")
			return map[string]any{"ok": true}, app.Scheduler.Clear(id)
		},
	}

	s.Actions["get_stats"] = Action{
		Name:        "get_stats",
		Description: "Get usage statistics.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			return core.GetGlobalStats().GetSnapshot(), nil
		},
	}

	s.Actions["get_manifest"] = Action{
		Name:        "get_manifest",
		Description: "Get full task manifest.",
		Parameters:  map[string]any{"task_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			id := strArg(args, "task_id")
			manifest, ok := app.Scheduler.GetManifest(id)
			if !ok {
				return nil, fmt.Errorf("not found")
			}
			return manifest, nil
		},
	}

	s.Actions["get_step_result"] = Action{
		Name:        "get_step_result",
		Description: "Get result of a specific task step.",
		Parameters:  map[string]any{"task_id": "string", "step_id": "string"},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			sid := strArg(args, "step_id")
			res := app.Scheduler.GetStepResult(tid, sid)
			if res == nil {
				return nil, fmt.Errorf("not found")
			}
			return res, nil
		},
	}

	s.Actions["patch_workspace"] = Action{
		Name:        "patch_workspace",
		Description: "Inject or modify structured data in a task's GlobalWorkspace (EvoX). Allows external correction of data flow.",
		Parameters: map[string]any{
			"task_id": "string (required)",
			"data":    "object (required) - Key-value pairs to merge into workspace",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			data, ok := args["data"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("'data' must be an object")
			}
			if tid == "" {
				return nil, fmt.Errorf("'task_id' is required")
			}
			return map[string]any{"ok": true}, app.Scheduler.PatchWorkspace(tid, data)
		},
	}

	s.Actions["get_coordination_report"] = Action{
		Name:        "get_coordination_report",
		Description: "Calculate coordination metrics (arXiv:2608.16801) for a task graph: network density, centrality, and S/M token ratio.",
		Parameters: map[string]any{
			"task_id": "string (required)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			tid := strArg(args, "task_id")
			manifest, ok := app.Scheduler.GetManifest(tid)
			if !ok {
				return nil, fmt.Errorf("task %s not found", tid)
			}

			// In a real implementation, we'd deserialize to TaskGraph to access CoordinationEdges.
			// Since GetManifest returns map[string]any, we look for coordination_edges key.
			edgesRaw, ok := manifest["coordination_edges"].([]any)
			if !ok || len(edgesRaw) == 0 {
				return map[string]any{"status": "no_data", "message": "No coordination edges recorded for this task."}, nil
			}

			totalPayload := int64(0)
			sharedPayload := int64(0)
			messagePayload := int64(0)
			nodeMap := make(map[string]bool)

			for _, e := range edgesRaw {
				// audit CRIT-4 (2026-10-08): use comma-ok checks instead of unchecked
				// type assertions — panics on malformed coordination-edge data.
				edge, ok := e.(map[string]any)
				if !ok {
					continue
				}
				payloadF, ok := edge["payload"].(float64)
				if !ok {
					continue
				}
				payload := int64(payloadF)
				totalPayload += payload
				if edge["type"] == "shared_vfs" {
					sharedPayload += payload
				} else {
					messagePayload += payload
				}
				src, ok := edge["source"].(string)
				if !ok {
					continue
				}
				tgt, ok := edge["target"].(string)
				if !ok {
					continue
				}
				nodeMap[src] = true
				nodeMap[tgt] = true
			}

			numNodes := len(nodeMap)
			numEdges := len(edgesRaw)
			density := 0.0
			if numNodes > 1 {
				density = float64(numEdges) / float64(numNodes*(numNodes-1))
			}

			return map[string]any{
				"task_id":          tid,
				"network_density":  fmt.Sprintf("%.4f", density),
				"node_count":       numNodes,
				"edge_count":       numEdges,
				"total_data_load":  totalPayload,
				"sm_ratio":         fmt.Sprintf("%.2f", float64(sharedPayload)/float64(messagePayload+1)), // Avoid div by zero
				"shared_data_pct":  fmt.Sprintf("%.1f%%", float64(sharedPayload)/float64(totalPayload+1)*100),
				"message_data_pct": fmt.Sprintf("%.1f%%", float64(messagePayload)/float64(totalPayload+1)*100),
			}, nil
		},
	}

	s.Actions["import_skills"] = Action{
		Name:        "import_skills",
		Description: "Import external agentskills.io-compliant SKILL.md bundles into the Orchestrator registry. Scans a directory recursively for SKILL.md files and registers them as native Skills (and SOPs if multi-step).",
		Parameters: map[string]any{
			"skill_directory": "string (required) - Absolute path to root directory containing SKILL.md files",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			skillDir := strArg(args, "skill_directory")
			if skillDir == "" {
				return nil, fmt.Errorf("skill_directory is required")
			}
			adapter := core.NewExternalSkillAdapter(app.Registry)
			count, err := adapter.ImportSkillDirectory(skillDir)
			if err != nil {
				return nil, fmt.Errorf("import failed: %w", err)
			}
			return map[string]any{
				"imported_count": count,
				"directory":      skillDir,
			}, nil
		},
	}

	s.Actions["sync_skills"] = Action{
		Name:        "sync_skills",
		Description: "Sync SKILL.md files from a GitHub repository. Downloads to a temp directory and imports using the same parser as import_skills (proper frontmatter + capability derivation).",
		Parameters: map[string]any{
			"owner": "string (required) - GitHub repo owner (e.g. mattpocock)",
			"repo":  "string (required) - GitHub repo name (e.g. skills)",
			"path":  "string (optional) - Path within repo to sync from (default: root)",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			owner := strArg(args, "owner")
			repo := strArg(args, "repo")
			repoPath := strArg(args, "path")
			if owner == "" || repo == "" {
				return nil, fmt.Errorf("owner and repo are required")
			}

			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()

			tempDir, err := os.MkdirTemp("", "sync_skills_*")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp dir: %w", err)
			}
			defer os.RemoveAll(tempDir)

			// Recursively download .md files, preserving directory structure
			// so ImportSkillDirectory's filepath.Walk can find SKILL.md files.
			var download func(repoPath, localDir string, depth int) (int, error)
			download = func(rp, ld string, depth int) (int, error) {
				if depth > 5 {
					return 0, nil
				}
				files, err := app.ResourceLoader.FetchRepoContents(ctx, owner, repo, rp)
				if err != nil {
					return 0, err
				}
				count := 0
				for _, f := range files {
					if f.Type == "dir" {
						subDir := filepath.Join(ld, f.Name)
						if err := os.MkdirAll(subDir, 0755); err != nil {
							continue
						}
						n, _ := download(f.Path, subDir, depth+1)
						count += n
					} else if f.Type == "file" && strings.HasSuffix(f.Name, ".md") {
						content, err := app.ResourceLoader.Fetch(ctx, f.DownloadURL)
						if err != nil {
							continue
						}
						if err := os.WriteFile(filepath.Join(ld, f.Name), []byte(content), 0644); err != nil {
							continue
						}
						count++
					}
				}
				return count, nil
			}

			downloaded, err := download(repoPath, tempDir, 0)
			if err != nil {
				return nil, fmt.Errorf("failed to download from %s/%s: %w", owner, repo, err)
			}
			if downloaded == 0 {
				return map[string]any{"imported_count": 0, "message": "no .md files found"}, nil
			}

			adapter := core.NewExternalSkillAdapter(app.Registry)
			imported, err := adapter.ImportSkillDirectory(tempDir)
			if err != nil {
				return nil, fmt.Errorf("import failed: %w", err)
			}

			return map[string]any{
				"imported_count":   imported,
				"downloaded_files": downloaded,
				"source":           fmt.Sprintf("%s/%s/%s", owner, repo, repoPath),
			}, nil
		},
	}

	s.Actions["promote_skill"] = Action{
		Name:        "promote_skill",
		Description: "Promote a pending external skill to the active registry, making it discoverable and injectable. Use after testing an imported skill to confirm it is trustworthy.",
		Parameters: map[string]any{
			"skill_id": "string (required) - ID of the pending skill to promote",
		},
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			skillID := strArg(args, "skill_id")
			if skillID == "" {
				return nil, fmt.Errorf("skill_id is required")
			}

			app.Registry.Mu.Lock()
			skill, ok := app.Registry.PendingSkills[skillID]
			if !ok {
				app.Registry.Mu.Unlock()
				return nil, fmt.Errorf("pending skill %q not found", skillID)
			}
			if app.Registry.Skills == nil {
				app.Registry.Skills = make(map[string]*config.Skill)
			}
			app.Registry.Skills[skillID] = skill
			delete(app.Registry.PendingSkills, skillID)
			app.Registry.Mu.Unlock()

			// Rebuild capability index so the promoted skill becomes discoverable
			if app.Navigator != nil {
				app.Navigator.Refresh()
			}

			return map[string]any{
				"promoted_skill": skillID,
				"name":           skill.Name,
				"capability":     skill.Capability,
				"message":        "skill is now discoverable and injectable",
			}, nil
		},
	}

	s.Actions["debug_dump"] = Action{
		Name:        "debug_dump",
		Description: "Get a goroutine stack dump for debugging hangs.",
		Handler: func(ctx context.Context, app *App, args map[string]any) (any, error) {
			buf := make([]byte, 1024*1024)
			n := runtime.Stack(buf, true)
			return map[string]string{"stack": string(buf[:n])}, nil
		},
	}

	registry.Register(s)
}
