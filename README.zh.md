# Vortex

一个面向复杂多步骤智能体工作流的任务编排引擎。

> **语言**：[English](README.md) | 中文

## 这是什么

Vortex 调度基于 DAG 的任务图，跨模型提供商路由工作，持久化经验以实现自我纠错，
并对外暴露工具以供外部控制。


**核心能力：**
- **DAG 任务调度** — 支持并行分支、依赖解析与预算感知执行的多步骤任务图
- **多提供商路由** — OpenAI、Anthropic、Gemini、Ollama、DeepSeek，内置故障转移、
  限流，并可通过 `pkg/interfaces/` 扩展自定义提供商
- **经验图** — 持久化执行结果，用于失败模式纠正与路由优化
- **JIT 代码执行** — Python、Node、Bun、Lua REPL 会话，用于动态工具生成
- **角色 + 技能系统** — 将能力绑定到智能体人格，组合多角色协作
- **信号场** — 基于执行信号的反思与自适应预算
- **循环断路器** — 检测步骤重复失败并以增量回溯重试（注入先前输出摘要，避免全量重跑）
- **输出网关** — 基于模型上下文窗口的自适应正文截断、跨重试的粘性结果累积、
  以及工件外部化（大输出写入文件而非内联）
- **插件完整性验证** — 外部拦截器脚本的 SHA256 哈希校验与权限声明
- **特性开关** — 沙箱、出站脱敏、思考模式等运行时开关；支持原子同步与 `.env` 热重载
- **记忆库控制层** — 开关 MemoryBank 提示注入与自动抽取；持久化用户画像与会话上下文
- **非交互模式** — 为 CI/批量评测以保守默认值自动解决阻塞决策；可选关键层级覆盖与决策角色委托
- **MCP 环境缓存** — 缓存 MCP 服务器环境变量，配置重载时失效
- **任务规划器** — 可选的步骤前规划器派生，将调用方的单一步骤替换为多步骤 DAG
  （`enable_planner_first`）；可配置超时与最大步骤数
- **脱敏提供器** — 提供商路径上的双向 PII 令牌化；在提示中掩蔽密钥并在响应中还原（`sanitizer_enabled`）
- **资产门** — 带 SHA-256 哈希校验的不可变资产管道门（`allow_external_assets`）
- **风险分级** — 可配置的基于关键词的任务风险分级（`critical_risk_keywords`、`moderate_risk_keywords`）
- **粘性联合路由** — 先前轮次调用过的工具在重路由后保留，避免任务中途能力丢失
- **确定性工具路由** — 带 always-include 绑定的确定性工具排序，保证 MCP 工具选择稳定
- **预算门** — 当每任务或每角色 token 用量超过配置阈值时自动挂起任务（`budget_gate`）
- **成本追踪** — 每次 LLM 调用的 token 支出持久化到 SQLite，用于支出分析
- **审计日志** — 面向决策记录的防篡改 SHA-256 哈希链
- **挂载审批门** — 逐 MCP/技能挂载策略（`open` / `approval` / `admin_only`），随提交者层级传播；
  审批门控的 MCP 以阻塞决策形式呈现
- **MCP/工具开关** — 操作员控制的严格只减开关：禁用整个 MCP 或单个工具（`disabled`、`disabled_tools`）
- **输入规范化** — 提示加固：HTML 注释提取、CSS 隐藏文本检测、以及提交前的顺序约束
- **规划器 DAG 执行** — 规划器产出的多步骤 DAG 直接执行，带计划覆盖缺口检测与信封解包
- **惰性 MCP 发现** — 发现延迟到第 0 轮，配合角色作用域工具搜索
- **技能清单漂移检查** — 启动时检测已加载技能与 `skills_dir` 的偏差
- **提供商遥测** — 用量核算（token、stop_reason）与提供商路由统计

**接口：**
- **MCP**（stdio + 原始 SSE）— 12 个核心编排工具，供 MCP 兼容客户端
  （Claude Desktop、IDE 等）使用：
  - `orchestrator_submit_task` — 提交复杂多步骤任务图
  - `orchestrator_wait_task` — 同步等待任务完成
  - `orchestrator_get_task_status` — 查询任务生命周期与各步骤状态
  - `orchestrator_discover` — 发现可用能力、角色与技能
  - `orchestrator_context_search` — 检索记忆库与历史轨迹
  - `orchestrator_submit_decision` — 为挂起步骤提供人在环选择
  - `orchestrator_get_logs` — 获取任务的流式执行日志
  - `orchestrator_invoke` — 直接触发子系统动作
  - `orchestrator_admin_deploy` — 健壮的文件部署管道（直传或分块）
  - `orchestrator_cancel_task` — 取消正在运行的任务
  - `orchestrator_fork_task` — 从历史检查点时间旅行并分叉任务
  - `orchestrator_run_command` — 执行带超时追踪的原子 shell 命令

- **直接 API** — 将 Vortex 作为 Go 库嵌入
- **原始 SSE** — 面向远程容器的 HTTP 传输（无鉴权，按设计作为本地回退）

## 构建与测试

```bash
export GOPROXY=https://goproxy.cn,direct   # 默认源慢时可使用镜像
go build -o vortex .
./vortex  # 以 stdio 启动（默认）

# 或使用原始 SSE 传输（例如远程容器场景）：
VORTEX_TRANSPORT=sse ./vortex  # 在 :8000 端口提供服务
```

验证测试是否全部通过：

```bash
go test ./...
```

> 提示：部分提供商测试需要 API Key 或会跳过实时调用（如 `agnes_test.go` 使用
> `t.Skip()`），其余均使用 `httptest` 模拟服务器，可在无网络环境下运行。

## 配置

复制示例配置并设置你的 API Key：

```bash
cp config.example.json config.json
cp .env.example .env
# 编辑 .env 并填入你的 API Key
```

配置使用 `api_key_env`（环境变量名），绝不内联密钥。
`config.example.json` 包含 4 个提供商、技能、角色、MCP 服务器、限流、
上下文窗口及全部系统设置 — 字段级参考见
[docs/CONFIGURATION.md](docs/CONFIGURATION.md)。`.env.example` 列出了所有环境变量。

### 技能

Vortex 在 `skills/` 目录中附带示例技能。可直接复制或改用，也可自行创建并让
`skills_dir` 指向你的目录。技能 schema 见
[docs/CONFIGURATION.md](docs/CONFIGURATION.md#skills)。

## 与 MCP 客户端配合使用

将以下内容加入你的 MCP 客户端配置（例如 Claude Desktop 的
`claude_desktop_config.json`）：

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

SSE 传输则使用 URL：

```json
{
  "mcpServers": {
    "vortex": {
      "url": "http://localhost:8000/sse"
    }
  }
}
```

## 构建版本

| 版本 | 入口 | 构建标签 | 说明 |
|------|------|----------|------|
| 标准版 | `.`（根目录） | （无） | stdio + 原始 SSE 传输 |

## 路线图

| 阶段 | 状态 | 范围 |
|------|------|------|
| 阶段 1 | **当前** | 无头核心：任务图执行、多模型路由、经验存储、JIT、技能、MCP + SSE 接口 |
| 阶段 2 | 规划中 | 桌面/Web UI：图形化任务管理、可视化工作流构建器、实时监控仪表盘 |

## 许可证

Apache 2.0 — 见 [LICENSE](LICENSE)。
