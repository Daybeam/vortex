//go:build !web && !dev && !desktop

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/daybeam/vortex/config"
	"github.com/daybeam/vortex/core"
	"github.com/daybeam/vortex/pkg/registry"
	"github.com/daybeam/vortex/providers"
	"github.com/daybeam/vortex/store"
	"github.com/daybeam/vortex/tools"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	ci_analyzer "github.com/daybeam/vortex/pkg/codeintel/analyzer"
	ci_overlay "github.com/daybeam/vortex/pkg/codeintel/overlay"
	ci_tools "github.com/daybeam/vortex/pkg/codeintel/tools"
)

func main() {
	mode := flag.String("mode", "hub", "run mode: hub | worker")
	execTool := flag.String("exec", "", "one-off tool execution (e.g., orchestrator_discover)")
	execArgs := flag.String("args", "{}", "JSON arguments for the tool")
	execFile := flag.String("file", "", "JSON file containing arguments for the tool")
	flag.Parse()

	// ── Resolve paths ──────────────────────────────────────────────────────
	exe, _ := os.Executable()
	root := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(root, "config.json")); err != nil {
		root, _ = os.Getwd()
	}

	configPath := filepath.Join(root, "config.json")
	if envConfig := os.Getenv("VORTEX_CONFIG"); envConfig != "" {
		configPath = envConfig
	} else {
		osConfig := filepath.Join(root, "config_"+runtime.GOOS+".json")
		if _, err := os.Stat(osConfig); err == nil {
			configPath = osConfig
		}
	}
	outputBase := filepath.Join(root, "outputs")
	tmpBase := filepath.Join(root, "tmp")
	logDir := filepath.Join(root, "logs")
	expDir := filepath.Join(root, "experience")
	schedulePath := filepath.Join(expDir, "schedules.json")

	// ── Load Environment ──────────────────────────────────────────────────
	loadDotEnv(filepath.Join(root, ".env"))

	// ── Registry ───────────────────────────────────────────────────────────
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	reg, err := config.NewRegistry(configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	reg.StartWatcher(rootCtx)
	reg.StartCompactor(rootCtx)

	// Override outputBase from config if specified (LE-2 fix)
	outputBase = filepath.Join(root, reg.System.OutputDirOrDefault())

	// FIX (2026-07-11): eagerly register any opt-in multi-instance provider
	// failover slots (ProviderConfig.Instances) into providers.GlobalRouter.
	// Must happen before any task is spawned, since GlobalRouter's normal
	// registration path is reactive (only on first use via a role), which
	// would leave a dedicated backup instance unregistered until some role
	// happened to reference it directly -- defeating the point of a backup.
	providers.RegisterConfiguredInstances(reg, reg.ExternalRuntimes, func(msg string) {
		log.Printf("[provider-instances] %s", msg)
	})

	// ── Logger ────────────────────────────────────────────────────────────
	logger, err := core.NewLogger(logDir, &reg.System)
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	defer logger.Close()

	// ── Runtime DB (SQLite) ───────────────────────────────────────────────
	dbPath := getenv("VORTEX_DB_PATH", filepath.Join(root, "orchestrator_runtime.db"))
	db, err := store.InitDB(dbPath)
	if err != nil {
		log.Printf("[warning] failed to init sqlite db: %v. Falling back to File storage.", err)
		db = nil
	}
	if db != nil {
		defer db.Close()
	}

	// ── Store initialization ──────────────────────────────────────────────
	s, err := store.NewStore(root, expDir, schedulePath, &reg.System, db)
	if err != nil {
		log.Fatalf("failed to init store: %v", err)
	}

	// ── Storage Compactor (SQLite retention + VACUUM) ─────────────────────
	if db != nil {
		compactor := store.NewStorageCompactorFromSettings(db, &reg.System)
		if reg.System.Retention.VacuumOnStartup {
			if err := compactor.RunCompaction(rootCtx); err != nil {
				log.Printf("[warning] startup compaction failed: %v", err)
			}
		}
		go compactor.Start(rootCtx)
	}

	// ── Bootstrap Config (File-First) ─────────────────────────────────────
	if s.Config != nil {
		rolesDir := filepath.Join(root, "workspace", "roles")
		_ = os.MkdirAll(rolesDir, 0755)
		syncedRoles, err := store.BootstrapConfigSync(s.Config, rolesDir)
		if err == nil {
			// Update registry with synced roles from DB/Disk
			reg.Mu.Lock()
			for id, role := range syncedRoles {
				reg.Roles[id] = role
			}
			reg.Mu.Unlock()
		} else {
			log.Printf("[warning] bootstrap config sync failed: %v", err)
		}
	}

	if *mode == "worker" {
		log.Fatal("worker mode is not available in this build")
	} else if *execTool != "" {
		argsJSON := *execArgs
		if *execFile != "" {
			data, err := os.ReadFile(*execFile)
			if err != nil {
				log.Fatalf("failed to read args file: %v", err)
			}
			argsJSON = string(data)
		}
		runExec(reg, logger, root, outputBase, tmpBase, logDir, expDir, schedulePath, configPath, *execTool, argsJSON, s)
	} else {
		runHub(reg, logger, root, outputBase, tmpBase, logDir, expDir, schedulePath, configPath, s, rootCtx, rootCancel)
	}
}

func runExec(reg *config.Registry, logger *core.Logger, root, outputBase, tmpBase, logDir, expDir, schedulePath, configPath string, toolName, argsJSON string, s *store.Store) {
	ts := s.Tasks
	es := s.Experience
	ss := s.Schedules
	mbStore := s.MemoryBank
	jitMgr := core.NewJITManager(reg, root)

	loader := core.NewResourceLoader()
	cookbookDir := filepath.Join(root, "store", "cookbooks")
	loader.SetCache(cookbookDir, reg.System.CookbookCacheEnabled)

	sf := core.NewSignalField(0.95, 10*time.Second)
	scheduler := core.NewDirectedEngine(reg, ts, es, jitMgr, logger, sf, outputBase, tmpBase, loader)

	// Wire capability-aware routing: Pareto frontier selection + IRT theta
	// turn budgeting + model registry for context-window fallback.
	var capProfileStore *store.CapabilityProfileStore
	if s.DB != nil {
		capProfileStore = store.NewCapabilityProfileStore(s.DB)
	}
	modelRegistry := registry.NewModelRegistry()
	scheduler.WireCapabilityRouting(capProfileStore, modelRegistry)

	scheduler.JITSessions = core.NewJITSessionManager(reg, filepath.Join(root, "scripts"))
	scheduler.TaskRegistry = s.TaskRegistry
	scheduler.Sessions = core.NewSessionManager(reg.System.Sandbox.AllowedWorkspaces, 0)
	sm := core.NewScheduleManager(scheduler, es, ss, logger)

	ciAnalyzer := ci_analyzer.NewGoAnalyzer("")
	ciOverlay := ci_overlay.NewManager(ciAnalyzer, "")
	ciHandler := ci_tools.NewHandler(ciAnalyzer, ciOverlay)
	navigator := core.NewCapabilityNavigator(reg)
	sopsDir := filepath.Join(config.ConfigDir(configPath), "workspace/sops/")
	sopMgr := core.NewSOPManager(reg, sopsDir)

	app := &tools.App{
		Registry:        reg,
		Scheduler:       scheduler,
		TaskStore:       ts,
		ExpStore:        es,
		Schedule:        sm,
		SStore:          ss,
		JIT:             jitMgr,
		Logger:          logger,
		CodeIntel:       ciHandler,
		Navigator:       navigator,
		SOPManager:      sopMgr,
		ConfigPath:      configPath,
		ResourceLoader:  loader,
		MemoryBankStore: mbStore,
		DB:              s.DB,
	}
	app.InitArchive(outputBase)

	// Internal MCP Server for tool resolution
	mcpServer := server.NewMCPServer("exec", "1.0.0")
	tools.RegisterAll(mcpServer, app)

	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		log.Fatalf("invalid args JSON: %v", err)
	}

	callReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: args,
		},
	}

	// Set ID for the JSON-RPC message
	msgID := 1
	rawReq, _ := json.Marshal(map[string]any{
		"jsonrpc": mcp.JSONRPC_VERSION,
		"method":  string(mcp.MethodToolsCall),
		"id":      msgID,
		"params":  callReq.Params,
	})

	res := mcpServer.HandleMessage(context.Background(), rawReq)

	data, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(data))
}

func runHub(reg *config.Registry, logger *core.Logger, root, outputBase, tmpBase, logDir, expDir, schedulePath, configPath string, s *store.Store, rootCtx context.Context, rootCancel context.CancelFunc) {
	fmt.Fprintln(os.Stderr, "[vortex] Starting in Hub mode...")
	for _, dir := range []string{outputBase, tmpBase, logDir, expDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("failed to create directory %s: %v", dir, err)
		}
	}

	// ── Stores ────────────────────────────────────────────────────────────
	ts := s.Tasks
	es := s.Experience
	ss := s.Schedules
	mbStore := s.MemoryBank

	// ── Telemetry Exporter (Phase 2 Roadmap - 2026-08-22) ──────────────────

	// ── JIT Manager ───────────────────────────────────────────────────────
	jitMgr := core.NewJITManager(reg, root)

	// ── Resource Loader (ADDED 2026-07-14) ────────────────────────────────
	loader := core.NewResourceLoader()
	cookbookDir := filepath.Join(root, "store", "cookbooks")
	loader.SetCache(cookbookDir, reg.System.CookbookCacheEnabled)

	// ── Cookbook Syncer (ADDED 2026-07-14) ────────────────────────────────
	syncer := core.NewCookbookSyncer(reg, loader, logger)
	syncer.Start()
	defer syncer.Stop()

	// ── Signal Field ──────────────────────────────────────────────────────
	sf := core.NewSignalField(0.95, 10*time.Second)
	sf.Start()
	defer sf.Stop()

	// ── Scheduler ─────────────────────────────────────────────────────────
	scheduler := core.NewDirectedEngine(reg, ts, es, jitMgr, logger, sf, outputBase, tmpBase, loader)
	scheduler.JITSessions = core.NewJITSessionManager(reg, filepath.Join(root, "scripts"))
	scheduler.TaskRegistry = s.TaskRegistry
	scheduler.Sessions = core.NewSessionManager(reg.System.Sandbox.AllowedWorkspaces, 0)
	// audit H10: wait for fire-and-forget DB saves to drain before db.Close.
	// Must be deferred BEFORE scheduler.Stop so LIFO order runs it AFTER
	// scheduler.Stop (which cancels lifecycleCtx) but BEFORE db.Close.
	if expStore, ok := es.(*store.ExperienceStore); ok {
		defer expStore.WaitAsyncSaves()
	}
	defer scheduler.Stop() // drain background goroutines on exit

	// ── Global Event Logger (trajectory capture) ─────────────────────────
	gel, gelErr := core.NewGlobalEventLogger(logDir)
	if gelErr != nil {
		log.Printf("[event-log] failed to init GlobalEventLogger: %v (continuing without)", gelErr)
	} else {
		gel.HookEventBus(core.DefaultBus)
		defer gel.Close()
	}

	// ── Offline Replay Scheduler (self-optimization) ──────────────────────
	if s.DB != nil && getenv("VORTEX_REPLAY_ENABLED", "1") != "" {
		replayer := core.NewReplayer(filepath.Join(logDir, "global_trajectory.jsonl"))
		if expStore, ok := s.Experience.(*store.ExperienceStore); ok {
			replaySched := core.NewReplayScheduler(replayer, nil, expStore, s.DB, nil)
			replaySched.SetCrossFamilyVerifier(
				func() string {
					return scheduler.GetCrossFamilyVerifier(reg.DefaultProviderName())
				},
				scheduler.GetSpawner(),
			)
			go replaySched.Start(rootCtx, 6*time.Hour)
			log.Printf("[replay] offline replay scheduler enabled (6h interval, cross-family verifier wired)")
		}
	}

	// ── Schedule Manager ──────────────────────────────────────────────────
	sm := core.NewScheduleManager(scheduler, es, ss, logger)
	sm.Start()
	defer sm.Stop()

	// ── FMC Batch Ticker (ADDED 2026-09-09) ──────────────────────────────
	// Opt-in: no-ops unless registry.System.FMCWeakModel is configured. See
	// core/fmc_ticker.go's doc comment for the full design rationale.
	fmcTicker := core.NewFMCBatchTicker(reg, reg.ExternalRuntimes, es, logger)
	fmcTicker.Start()
	defer fmcTicker.Stop()

	// ── CodeIntel ─────────────────────────────────────────────────────────
	ciAnalyzer := ci_analyzer.NewGoAnalyzer("")
	ciOverlay := ci_overlay.NewManager(ciAnalyzer, "")
	ciHandler := ci_tools.NewHandler(ciAnalyzer, ciOverlay)
	navigator := core.NewCapabilityNavigator(reg)
	sopsDir := filepath.Join(config.ConfigDir(configPath), "workspace/sops/")
	sopMgr := core.NewSOPManager(reg, sopsDir)

	// ── MCP Server ────────────────────────────────────────────────────────
	mcpServer := server.NewMCPServer("vortex_mcp", "1.0.0", server.WithInstructions(tools.ResolveServerInstructions(reg, false)))

	app := &tools.App{
		Registry:        reg,
		Scheduler:       scheduler,
		TaskStore:       ts,
		ExpStore:        es,
		Schedule:        sm,
		SStore:          ss,
		JIT:             jitMgr,
		Logger:          logger,
		CodeIntel:       ciHandler,
		Navigator:       navigator,
		SOPManager:      sopMgr,
		ConfigPath:      configPath,
		ResourceLoader:  loader,
		MemoryBankStore: mbStore,
		DB:              s.DB,
	}
	app.InitArchive(outputBase)
	defer app.StopArchive() // audit M12: stop pruning goroutine on shutdown
	if err := app.SetupSubsystems(mcpServer); err != nil {
		log.Fatalf("failed to init subsystems: %v", err)
	}
	tools.RegisterAll(mcpServer, app)

	// Prune stale JIT experience patterns on startup (audit finding H8 —
	// was only called in main_web.go, not the standard edition).
	if app.ExpStore != nil && app.Registry != nil {
		if pruned := app.ExpStore.PruneStaleJITRefs(app.Registry); pruned > 0 {
			fmt.Fprintf(os.Stderr, "[vortex] Pruned %d stale JIT experience patterns\n", pruned)
		}
	}

	// Close all cached MCP subprocesses and JIT sessions on shutdown to
	// prevent orphaned processes (audit findings C2/C3).
	defer scheduler.GetSpawner().CloseMCPConnections()
	if scheduler.JITSessions != nil {
		defer scheduler.JITSessions.CloseAll()
	}

	// ── Transport ─────────────────────────────────────────────────────────
	transport := getenv("VORTEX_TRANSPORT", "stdio")

	switch transport {
	case "sse":
		serveSSE(mcpServer, rootCancel) // audit L4: pass rootCancel so background goroutines stop on shutdown
	default:
		fmt.Fprintln(os.Stderr, "[vortex] stdio mode (Claude Desktop)")
		// Graceful shutdown on SIGINT/SIGTERM: run ServeStdio in a goroutine,
		// wait for either the server to finish or a signal. On signal, cancel
		// the root context (stops background goroutines) and close stdin to
		// unblock ServeStdio, then return normally so deferred cleanup runs
		// (scheduler.Stop, logger.Close, JIT CloseAll, MCP Close, etc.).
		// Without this, SIGINT kills the process immediately and all deferred
		// cleanup is skipped (audit finding H7).
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)

		serverDone := make(chan error, 1)
		go func() {
			serverDone <- server.ServeStdio(mcpServer)
		}()

		if err := waitForServerOrSignal(serverDone, sigCh, rootCancel, os.Stdin); err != nil {
			// audit L7: don't use log.Fatalf — os.Exit skips deferred cleanup
			// (CloseMCPConnections, JITSessions.CloseAll, scheduler.Stop, etc.).
			// Log and return normally so defers run.
			log.Printf("[vortex] stdio server error: %v", err)
		}
	}
}

// loadDotEnv is defined in dotenv.go (shared with main_web.go, no build tag).
