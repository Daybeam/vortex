# Vortex

A task orchestration engine for complex, multi-step agentic workflows.

## What this is

Vortex schedules DAG-based task graphs, routes work across model providers,
persists experience for self-correction, and exposes tools for external control.

**Core capabilities:**
- **DAG task scheduling** — multi-step task graphs with parallel branches, dependency resolution, and budget-aware execution
- **Multi-provider routing** — OpenAI, Anthropic, Gemini, Ollama, DeepSeek (with built-in failover, rate limiting, and support for custom provider extensions via `pkg/interfaces/`)
- **Experience graph** — persists outcomes for failure-mode correction and route optimization
- **JIT code execution** — Python, Node, Bun, and Lua REPL sessions for dynamic tool generation
- **Role + skill system** — bind capabilities to agent personas, compose multi-role ensembles
- **Signal field** — reflection and adaptive budgeting based on execution signals

**Interfaces:**
- **MCP** (stdio + raw SSE) — 12 core orchestration tools for MCP-compatible clients (Claude Desktop, IDEs, etc.):
  - `orchestrator_submit_task` — Submit a complex multi-step task graph
  - `orchestrator_wait_task` — Synchronously wait for task completion
  - `orchestrator_get_task_status` — Query task lifecycle and step statuses
  - `orchestrator_discover` — Discover available capabilities, roles, and skills
  - `orchestrator_context_search` — Search memory banks and past trajectories
  - `orchestrator_submit_decision` — Provide human-in-the-loop choices for pending steps
  - `orchestrator_get_logs` — Retrieve streaming execution logs for a task
  - `orchestrator_invoke` — Directly trigger a subsystem action
  - `orchestrator_admin_deploy` — Robust file deployment pipeline (direct or chunked)
  - `orchestrator_cancel_task` — Cancel a running task execution
  - `orchestrator_fork_task` — Time-travel and fork a task from historical checkpoints
  - `orchestrator_run_command` — Execute an atomic shell command with timeout tracking
- **Direct API** — embed Vortex as a Go library
- **Raw SSE** — HTTP transport for remote containers (no auth, local fallback by design)

## Quick Start

```bash
go build -o vortex .
./vortex  # starts on stdio (default)

# Or use raw SSE transport (e.g. for remote containers):
VORTEX_TRANSPORT=sse ./vortex  # serves on :8000
```

## Configuration

Copy the example config and set your API keys:

```bash
cp config.example.json config.json
cp .env.example .env
# Edit .env and fill in your API keys
```

The config uses `api_key_env` (environment variable name), never inline keys.
`config.example.json` includes 4 providers, skills, roles, MCP servers, rate limiting,
context windows, and all system settings — see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)
for a field-by-field reference. `.env.example` lists all environment variables.

### Skills

Vortex ships with example skills in `skills/`. Copy or adapt them, or create your own
and point `skills_dir` at your directory. See [docs/CONFIGURATION.md](docs/CONFIGURATION.md#skills)
for the skill schema.

## Usage with MCP Clients

Add to your MCP client config (e.g. Claude Desktop `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "vortex": {
      "command": "/path/to/vortex",
      "env": { "OPENAI_API_KEY": "sk-..." }
    }
  }
}
```

For SSE transport, use a URL instead:

```json
{
  "mcpServers": {
    "vortex": {
      "url": "http://localhost:8000/sse"
    }
  }
}
```

## Build Editions

| Edition | Entry Point | Build Tag | Description |
|---------|-------------|-----------|-------------|
| Standard | `.` (Root) | (none) | stdio + raw SSE transport |

## Roadmap

| Phase | Status | Scope |
|-------|--------|-------|
| Phase 1 | **Current** | Headless core: task graph execution, multi-model routing, experience store, JIT, skills, MCP + SSE interfaces |
| Phase 2 | Planned | Desktop/Web UI: graphical task management, visual workflow builder, real-time monitoring dashboard |

## License

Apache 2.0 — see [LICENSE](LICENSE).
