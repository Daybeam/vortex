# Configuration Reference

This document describes every field in `config.json`. Start from `config.example.json` and override as needed.

> [!NOTE]
> For high-level usage and installation, see the [README.md](../README.md). For development guidance, see [AGENTS.md](../AGENTS.md).

## Top-level fields

| Field | Type | Description |
|---|---|---|
| `default_provider` | string | Key into `providers` used for task execution when no explicit provider is specified. |
| `default_embedding_provider` | string | Key into `providers` used for embedding operations. Optional. |
| `providers` | map | Provider configurations keyed by name (e.g. `"openai"`, `"local"`). |
| `skills` | array | Skill definitions. Typically loaded from `skills_dir` instead. |
| `mcps` | array | External MCP server definitions. |
| `roles` | array | Role definitions. Typically loaded from `roles_dir` instead. |
| `role_groups` | array | Named groups of roles for batch assignment. |
| `sops` | array | Standard Operating Procedure definitions. |
| `external_runtimes` | object | Paths to Python/Node runtimes for JIT code execution. |
| `system` | object | System-wide settings (see below). |
| `enable_dynamic_role_gen` | bool | Generate roles dynamically from a cookbook source. Default `false`. |
| `enable_ephemeral_role_gen` | bool | Allow creating temporary roles at runtime. Default `true`. |
| `require_plan_review` | bool | Force a plan-review step before task execution. Default `false`. |
| `swarm_mode_enabled` | bool | Enable multi-agent swarm execution. Default `false`. |
| `skills_dir` | string | Directory to load skills from. |
| `roles_dir` | string | Directory to load roles from. |

## ProviderConfig

| Field | Type | Description |
|---|---|---|
| `provider` | string | Provider type: `"openai"`, `"anthropic"`, `"gemini"`, `"ollama"`, `"deepseek"`. |
| `model` | string | Model identifier (e.g. `"gpt-4o"`, `"claude-sonnet-4-20250514"`). |
| `embedding_model` | string | Model for embedding operations. Optional. |
| `api_key_env` | string | Environment variable name holding the API key. **Never inline `api_key`.** |
| `base_url` | string | Custom API endpoint (e.g. Ollama `http://localhost:11434/v1`). |
| `pool_id` | string | Pool identifier for provider routing and health tracking. |
| `max_context_window` | int | Maximum context window in tokens. |
| `reserve_tokens` | int | Tokens reserved for response output. `0` = use tier default. |
| `temperature` | float | Sampling temperature. Optional. |
| `rate_limit` | object | Rate limiting config (see below). |
| `capabilities` | array | Capability tags: `"vision"`, `"t2i"`, `"coding"`, `"tools"`. |
| `fallback_chain` | array | Ordered list of fallback providers for failover. |

### RateLimit

| Field | Type | Description |
|---|---|---|
| `requests_per_minute` | int | Maximum requests per minute. |
| `retry_wait_seconds` | int[] | Exponential backoff schedule in seconds (e.g. `[1, 2, 5, 10]`). |

## Skills

Skills are reusable capability units that can be bound to roles. Define them inline in `skills` or load from a directory via `skills_dir`.

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique skill identifier (e.g. `"code-review"`). |
| `name` | string | Human-readable name. |
| `capability` | string | Capability tag this skill provides. |
| `domain` | string | Optional domain grouping (e.g. `"software"`). |
| `description` | string | What the skill does. |
| `input_schema` | object | JSON Schema for skill input. |
| `output_schema` | object | JSON Schema for skill output. |
| `token_estimate` | int | Estimated token cost. |
| `requires` | string[] | Other skills that must be present. |
| `incompatible` | string[] | Skills that cannot be used alongside this one. |
| `implementations` | map | Per-family implementations (see below). |
| `execution_mode` | string | `"embedded"` (default) or `"directory"`. |
| `skill_dir` | string | Directory path for directory-based skills. |

### SkillImplementation

| Field | Type | Description |
|---|---|---|
| `system_prompt` | string | System prompt for this implementation. |
| `prompt_version` | string | Version tag for prompt tracking. |

**Example:**

```json
{
  "id": "code-review",
  "name": "Code Review",
  "capability": "code_review",
  "description": "Reviews code changes for quality, bugs, and style",
  "input_schema": {"type": "object", "properties": {"diff": {"type": "string"}}},
  "output_schema": {"type": "object", "properties": {"issues": {"type": "array"}}},
  "token_estimate": 2000,
  "implementations": {
    "default": {
      "system_prompt": "You are a code reviewer. Analyze the diff and report issues.",
      "prompt_version": "1.0"
    }
  }
}
```

## Roles

Roles define agent personas with bound skills, providers, and instructions. Define inline in `roles` or load from `roles_dir`.

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique role identifier. |
| `name` | string | Human-readable name. |
| `base_capability` | string | Primary capability tag. |
| `bound_skills` | string[] | Skill IDs this role can use. |
| `bound_mcp_bindings` | MCPBinding[] | MCP servers this role can access. |
| `provider` | string | Provider key from `providers` map. |
| `model` | string | Model override (optional; uses provider default if unset). |
| `fallbacks` | RoleFallback[] | Fallback provider/model pairs. |
| `instruction` | string | Base system instruction for this role. |
| `rules` | string | Additional rules/constraints. |
| `extends` | string | Parent role ID to inherit from. |
| `purpose` | string | What this role is for. |
| `best_for` | string | Recommended use cases. |
| `generatable` | bool | Allow dynamic generation. Default `false`. |
| `allow_dynamic_skills` | bool | Allow adding skills at runtime. |
| `allow_dynamic_mcps` | bool | Allow adding MCPs at runtime. |

**Example:**

```json
{
  "id": "architect",
  "name": "Architect",
  "base_capability": "system_design",
  "bound_skills": ["code-review"],
  "provider": "openai",
  "model": "gpt-4o",
  "instruction": "Design systems with scalability and maintainability in mind.",
  "best_for": "architecture decisions, system design, tech stack selection"
}
```

## Role Groups

Role groups combine multiple roles with an execution policy.

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique group identifier. |
| `name` | string | Human-readable name. |
| `description` | string | What the group does. |
| `members` | GroupMember[] | Roles in the group. |
| `policy` | string | `sequential`, `parallel`, `voting`, `chain_of_thought`, or `sop`. |
| `sop_ref` | string | SOP ID for `sop` policy. |
| `aggregator_role_id` | string | Role that aggregates results. |
| `max_parallelism` | int | Max concurrent members (for `parallel`/`voting`). |

### GroupMember

| Field | Type | Description |
|---|---|---|
| `role_id` | string | Role ID (alias: `role`). |
| `task_template` | string | Optional task template for this member. |
| `provider` | string | Provider override. |
| `weight` | float | Voting weight. |
| `optional` | bool | Skip if role unavailable. |

## MCP Servers

External MCP servers provide additional tools. Define in `mcps`.

| Field | Type | Description |
|---|---|---|
| `id` | string | Unique MCP server identifier. |
| `url` | string | SSE URL for remote servers. |
| `command` | string | Command to launch a local stdio server. |
| `args` | string[] | Command arguments. |
| `dir` | string | Working directory. |
| `env` | map | Environment variables for the process. |
| `trusted` | bool | Whether to trust this server's tools without sandboxing. |
| `sandboxed` | bool | Run in sandbox. |
| `provides` | string[] | Capability tags this server provides. |
| `available_tools` | string[] | Explicit tool allowlist. |
| `auth_header_name` | string | Auth header name (e.g. `"Authorization"`). |
| `auth_header_prefix` | string | Auth header prefix (e.g. `"Bearer"`). |
| `api_key_env` | string | Env var holding the API key. |
| `platform_overrides` | map | Per-OS command/args/dir/env overrides. |

**Example (local stdio):**

```json
{
  "id": "filesystem",
  "command": "npx",
  "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"],
  "trusted": true,
  "provides": ["read_file", "write_file", "list_directory"]
}
```

**Example (remote SSE with auth):**

```json
{
  "id": "remote-api",
  "url": "https://api.example.com/mcp",
  "trusted": false,
  "auth_header_name": "Authorization",
  "auth_header_prefix": "Bearer",
  "api_key_env": "REMOTE_API_KEY",
  "provides": ["query", "search"]
}
```

## SystemSettings

| Field | Type | Default | Description |
|---|---|---|---|
| `confidence_threshold` | float | 0.7 | Minimum confidence for auto-accepting a decision. |
| `max_decision_outcomes` | int | 2000 | Maximum decision outcomes retained. |
| `max_context_keep` | int | 100 | Maximum context items kept per task. |
| `max_tool_turns` | int | 25 | Maximum tool invocations per task step. |
| `max_spawn_depth` | int | 3 | Maximum nested task spawn depth. |
| `default_jit_ttl` | int | 3600 | Default JIT tool TTL in seconds. |
| `sandboxed_memory_mb` | int | 256 | Memory limit for sandboxed code execution. |
| `sandboxed_cpu_secs` | int | 30 | CPU time limit for sandboxed code execution. |
| `max_log_size_mb` | int | 100 | Maximum log file size before rotation. |
| `max_concurrent_steps` | int | 10 | Maximum parallel steps within a task graph. |
| `output_dir` | string | `"outputs"` | Directory for task output files. |
| `delegation_mode` | bool | false | Enable delegation mode (reduces cost, limits escalation). |
| `staging_enabled` | bool | false | Enable staging area for draft outputs. |

## Configuration Recipes

### 1. The "Privacy First" (Local-Only)
Run everything locally using Ollama. No data leaves your machine.

```json
{
  "default_provider": "local",
  "providers": {
    "local": {
      "provider": "ollama",
      "model": "llama3.1",
      "base_url": "http://localhost:11434/v1",
      "pool_id": "local",
      "max_context_window": 32768
    }
  }
}
```

### 2. The "Reliable Worker" (Cloud Failover)
Use Anthropic as primary, but failover to OpenAI if you hit rate limits.

```json
{
  "default_provider": "anthropic",
  "providers": {
    "anthropic": {
      "provider": "anthropic",
      "model": "claude-3-5-sonnet-latest",
      "api_key_env": "ANTHROPIC_API_KEY",
      "fallback_chain": [
        {
          "provider": "openai",
          "model": "gpt-4o",
          "api_key_env": "OPENAI_API_KEY"
        }
      ]
    }
  }
}
```

### 3. The "Researcher" (MCP Integration)
Bind a specialized role to specific external tools.

```json
{
  "roles": [
    {
      "id": "researcher",
      "name": "Web Researcher",
      "provider": "openai",
      "bound_mcp_bindings": [
        { "mcp_id": "google-search" },
        { "mcp_id": "filesystem" }
      ],
      "instruction": "Search the web and save findings to local markdown files."
    }
  ]
}
```

## Environment variables

All environment variables use the `VORTEX_` prefix.

| Variable | Description |
|---|---|
| `VORTEX_CONFIG` | Path to config file (default: `config.json`). |
| `VORTEX_DB_PATH` | Path to SQLite database (default: `vortex.db`). |
| `VORTEX_TRANSPORT` | Transport mode: `stdio` (default) or `sse`. |
| `VORTEX_HOST` | SSE bind host (default: `0.0.0.0`). |
| `VORTEX_PORT` | SSE bind port (default: `8000`). |
| `VORTEX_BASE_URL` | SSE base URL for generated links. |
| `VORTEX_DEBUG` | Enable debug logging (`1` or `true`). |
| `VORTEX_DELEGATION_MODE` | Override delegation mode at startup. |
| `VORTEX_WAL_ENABLED` | Enable write-ahead log (`true`/`false`). |
| `VORTEX_REPLAY_ENABLED` | Enable event replay on startup. |
| `VORTEX_ALLOW_DEPLOY_WRITE` | Allow deploy tool to write (security gate). |
| `VORTEX_PUBLIC_KEY` | Public key for signature verification. |

API keys are referenced by name via `api_key_env` — set them in `.env`:

```bash
cp .env.example .env
# Edit .env and fill in your API keys
```

## .env file

The `.env` file sets environment variables for Vortex. Copy `.env.example` to `.env` and fill in your values. The `.env` file is gitignored — never commit real keys.

### Sections

| Section | Variables | Description |
|---|---|---|
| **Transport** | `VORTEX_TRANSPORT`, `VORTEX_HOST`, `VORTEX_PORT`, `VORTEX_BASE_URL` | How Vortex exposes its MCP server. |
| **Paths** | `VORTEX_CONFIG`, `VORTEX_DB_PATH` | Config file and SQLite database locations. |
| **AI Provider Keys** | `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, ... | API keys for model providers. Set only the ones you use. Ollama needs no key (set `base_url` in `config.json`). |
| **Feature Flags** | `VORTEX_DEBUG`, `VORTEX_DELEGATION_MODE`, `VORTEX_WAL_ENABLED`, `VORTEX_REPLAY_ENABLED` | Runtime behavior toggles. |
| **Security** | `VORTEX_ALLOW_DEPLOY_WRITE`, `VORTEX_PUBLIC_KEY` | Security gates for deploy tool and signature verification. |
| **Advanced** | `VORTEX_CONFIG_NO_OCC`, `VORTEX_CONFIG_SPLIT_THRESHOLD`, `VORTEX_CONFIG_NO_AUTOSPLIT`, `VORTEX_SPAN_ID`, `VORTEX_TRACE_ID` | Internal tuning and distributed tracing. Usually unset. |
