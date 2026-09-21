# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this is

Vortex is a task orchestration engine for complex multi-step agentic workflows.
It schedules DAG task graphs, routes work across model providers, persists
experience for self-correction, and exposes orchestration tools via MCP (stdio
+ raw SSE) and direct API.

## Build and test

```bash
export GOPROXY=https://goproxy.cn,direct   # faster mirror if the default is slow
go build ./...
go test ./...                             # provider tests are slow / API-key-gated
```

To verify a change, run only the affected package:

```bash
go test ./core/ -run TestForkTask
```

## Layout

main.go            entry point (stdio + SSE transport dispatch)
transport_sse.go   raw SSE transport (/health, /sse, /message, /mcp)
core/              the engine (scheduler, DAG, reflection, event bus, sandbox)
providers/         model providers (OpenAI, Anthropic, Gemini, Ollama, ...)
store/             persistence (SQLite + file backends, experience graph)
config/            registry, loader, and settings types
pkg/interfaces/    the extension seam (implement these to extend Vortex)
schemas/           shared task/DAG types
tools/             orchestration tool registrations (12 tools, orchestrator_* prefix)
skills/            example skills (code-review, test-generator)

## Entry points / interfaces

stdio (default)                     MCP over stdio — Claude Desktop / IDE clients
VORTEX_TRANSPORT=sse ./vortex       raw SSE on :8000 (local compatibility/fallback)
direct API                          embed as Go library (pkg/interfaces/)

## Configuration

Copy `config.example.json` to `config.json` and set the API key via an environment
variable name (never inline). The example uses `api_key_env` (the name of the env
var that holds the key). The full schema lives in the Go config package
(`config/types.go`, `config/config.go`). See `docs/CONFIGURATION.md` for a
field-by-field reference.

### Key sections

| Section | What it does |
|---------|-------------|
| `providers` | Model providers (OpenAI, Anthropic, Gemini, Ollama). Each has `api_key_env`, `model`, `rate_limit`. |
| `skills` | Reusable capability units (e.g. code review, test generation). Can be inline or loaded from `skills_dir`. |
| `roles` | Agent personas with bound skills and providers. Can be inline or loaded from `roles_dir`. |
| `role_groups` | Multi-role ensembles with policies: `sequential`, `parallel`, `voting`, `chain_of_thought`, `sop`. |
| `mcps` | External MCP servers — local stdio (`command`+`args`) or remote SSE (`url`+auth). |
| `system` | Engine limits: concurrency, timeouts, sandbox, context window, output dir. |

### Environment variables

All use `VORTEX_` prefix. Key ones: `VORTEX_TRANSPORT` (stdio|sse), `VORTEX_DB_PATH`,
`VORTEX_HOST`, `VORTEX_PORT`. API keys are referenced by name via `api_key_env` — set
them in `.env` (copy `.env.example`). See `docs/CONFIGURATION.md` for the full list.

## License

Apache 2.0.
