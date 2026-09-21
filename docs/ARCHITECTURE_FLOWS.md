# Architecture Flows — Open Core (stdio + raw SSE)

This document maps the entry chains and operation pathways in the public
`vortex` repository.

> [!NOTE]
> For configuration details, see [CONFIGURATION.md](CONFIGURATION.md). For agent-specific guidance, see [AGENTS.md](../AGENTS.md).

## 0. Bootstrapping & Initialization

```mermaid
graph TD
    A[Start] --> B[Resolve root & config.json paths]
    B --> C[loadDotEnv: load .env]
    C --> D[config.NewRegistry: load config.json]
    D --> E[Init Logger, SQLite DB, and Store]
    E --> F[BootstrapConfigSync: Sync DB roles to workspace/roles]
    F --> G[Start Watchers: Registry, Compactor, SignalField]
    G --> H{Transport?}
    H -->|stdio| I[server.ServeStdio]
    H -->|sse| J[serveSSE: ListenAndServe]
```

Initialization is "environment-aware": it searches for `config.json` relative to the executable or working directory, and respects `VORTEX_CONFIG` and `VORTEX_DB_PATH` overrides. The registry starts a background watcher to hot-reload config changes.

```mermaid
graph TD
    A[main.go: main] --> B[parse flags + load config]
    B --> C[NewDirectedEngine]
    C --> D[server.NewMCPServer]
    D --> E["register 12 tools"]
    E --> F[transport stdio: Serve]
    F --> G{MCP request}
    G -->|orchestrator_submit_task| H[engine.SubmitWithSessionIR]
    G -->|orchestrator_invoke| K["registry.Invoke: subsystem dispatch"]
    G -->|other tools| L["get_status / run_command / wait / discover / decision / logs / cancel / fork / deploy"]
    H --> M[goBackground: run/swarmWatchdog]
    M --> N["executeStep: spawner.Spawn"]
    N --> O[persist graph + broadcast]
```

Tools registered (12): `orchestrator_submit_task`, `orchestrator_get_task_status`,
`orchestrator_wait_task`, `orchestrator_run_command`, `orchestrator_discover`,
`orchestrator_submit_decision`, `orchestrator_get_logs`, `orchestrator_invoke`,
`orchestrator_cancel_task`, `orchestrator_fork_task`, `orchestrator_admin_deploy`,
`orchestrator_context_search` (conditional). The Promoter may hot-promote
frequently-used subsystem actions to top-level tools at runtime.

## 2. Raw SSE Transport (Fallback)

```mermaid
graph TD
    A[main.go: case sse] --> B[serveSSE]
    B --> C[server.NewMCPServer]
    C --> D[register tools]
    D --> E[mcp.NewSSEServer]
    E --> F["/sse + /message"]
    E --> G["/mcp + Streamable HTTP"]
    E --> H["/health"]
    F --> I{MCP request}
    G --> I
    I -->|same 12 tools as stdio| J[engine.SubmitWithSessionIR / registry.Invoke]
```

No auth, no tiering, no `/api/*` routes. A single full MCP server is
exposed over SSE + Streamable HTTP. This is the open fallback for
environments where stdio is impractical (e.g. remote containers).

## 3. Task Execution Flow

```mermaid
graph TD
    A[SubmitWithSessionIR] --> B{smart route?}
    B -->|yes| C[fast path: single step]
    B -->|no| D[standard path: DAG]
    C --> E[goBackground: execute]
    D --> F[goBackground: run]
    F --> G[executeStep per step]
    G --> H{step type}
    H -->|local tool| I[exec.Run]
    H -->|remote MCP| J[ensureMCPClient]
    H -->|JIT| K[jit_session]
    I --> L[SummarizeOutput]
    J --> M[JSON-RPC over stdio/SSE]
    K --> N[Python REPL server]
    L --> O[persist + broadcast]
    M --> O
    N --> O
```

## 4. Shutdown Flow

```mermaid
graph TD
    A["signal: SIGINT/SIGTERM"] --> B[rootCancel]
    B --> C["deferred cleanup (LIFO)"]
    C --> D[scheduler.Stop]
    D --> E["lifecycleCancel: cancel task contexts"]
    D --> F["bgWg.Wait: drain goBackground goroutines"]
    C --> G[CloseMCPConnections]
    C --> H["JITSessions.CloseAll"]
    C --> I[db.Close]
    C --> J[logger.Close]
```

Task goroutines are tracked via `goBackground` and derive their context
from `lifecycleCtx`, so `Stop()` cleanly drains all in-flight work
before the deferred cleanup closes MCP connections, JIT sessions,
database, and logger.

---

## 5. Task Submission Sequence

```mermaid
sequenceDiagram
    participant C as Client
    participant M as MCP Server
    participant E as Engine
    participant S as Spawner
    participant P as Model Provider

    C->>M: orchestrator_submit_task(steps)
    M->>E: SubmitWithSessionIR(inputs)
    E->>E: smart route? (single-step, no deps)
    alt fast path
        E->>S: Spawn (direct, no DAG)
        S-->>E: SpawnResult
    else standard path
        E->>E: goBackground (async)
        M-->>C: task_id (immediate return)
        E->>E: run() loop
        loop until graph terminal
            E->>E: ReadySteps() → sort by signal
            par parallel (up to MaxConcurrentSteps)
                E->>E: executeStep
                E->>S: Spawn
                loop turn 0..maxTurns
                    S->>P: ChatCompletion (with tools)
                    P-->>S: tool_use / final answer
                    alt tool_use
                        S->>S: execute tool (local/MCP/JIT)
                        S->>P: tool_result
                    else final answer
                        S-->>E: SpawnResult
                    end
                end
                E->>E: runInterceptors
                E->>E: persistGraph + Broadcast
            end
        end
    end
    C->>M: orchestrator_get_task_status(task_id)
    M-->>C: status + results
```

The async boundary is at `goBackground`: `SubmitWithSessionIR` returns
`task_id` immediately while the DAG executes in a background goroutine.
Clients poll `orchestrator_get_task_status` for results.

## 6. Invoke Sequence (synchronous)

```mermaid
sequenceDiagram
    participant C as Client
    participant M as MCP Server
    participant R as SubsystemRegistry
    participant Sub as Subsystem

    C->>M: orchestrator_invoke(subsystem, action, args)
    M->>R: Invoke(ctx, app, "config", "reload", {})
    R->>Sub: dispatch action
    Sub-->>R: result
    R-->>M: result
    M-->>C: JSON response (synchronous)
```

## 7. Experience & FMC (Failure Mode Classification)

```mermaid
graph TD
    A[Task Completion/Failure] --> B[ExperienceStore: Record]
    B --> C{is failure?}
    C -->|no| D[update success metrics]
    C -->|yes| E[FailureClassifier: Run]
    E --> F{FMC Weak Model?}
    F -->|yes| G[LLM Classification: Categorize + Fix Suggestion]
    F -->|no| H[Heuristic Classification: RegEx/Code based]
    G --> I[Persist Trajectory + FMC Tag]
    H --> I
    I --> J[Experience Graph: Build Links]
    J --> K[Next Task: Query Experience]
    K --> L[Inject Hints into Prompt]
```

FMC (Failure Mode Classification) allows the engine to "learn" from mistakes. When a task fails, it is passed through a classifier (ideally a cheap "weak model" like Gemini Flash) to determine the root cause (e.g., `TIMEOUT`, `API_ERROR`, `LOGIC_ERROR`) and store a potential correction. This correction is then surfaced as a "Hint" to the sub-agent if a similar task is submitted in the future.

Unlike `submit_task`, `invoke` is synchronous — the MCP call blocks
until the subsystem action completes. Subsystems: `config`, `jit`,
`admin`, `schedule`, `exp`, `intel`, `proxy`. The Promoter may
hot-promote frequently-invoked actions to top-level tools.
