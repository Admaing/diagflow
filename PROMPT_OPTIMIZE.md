# DiagFlow 生产级优化提示词

> 可直接整体粘贴给 Claude Code / Cursor 等使用。
> 目标：把 Go 版 DiagFlow 从「能跑的 demo」提升到真正的生产级别。

---

## 任务

你是一名资深 Go 后端工程师 + AI Agent 架构师 + SRE。请对当前仓库 `/Users/user/Prj/diagflow` 做一次**生产就绪（production-readiness）评估与改造**：先通读代码摸清现状，再按优先级逐项落地，每项改动都要有测试护航。不要凭 README 假设——以代码为准。

## 一、项目真实背景

- DiagFlow 是面向大数据平台（Flink/HDFS/YARN/Kafka/Spark）的 **AI 故障根因诊断 Agent**，已用 **Go 重写**（原 Python 实现已删除）。
- **LLM 链路**：Anthropic SDK 形态 → modelverse 代理（api.modelverse.cn）→ DeepSeek 后端。所有 LLM 调用都是 `Anthropic().messages.create(...)` 形态。
- **核心架构**（`internal/diagagent/diag_agent.go` 的 `DiagAgent`）五阶段流水线：
  1. KB 语义检索（0 LLM）
  2. YAML 策略确定性执行（`internal/strategy/strategy.go`，同优先级并行）
  3. SDK 原生 ReAct + DeepWiki 已知 bug 验证（`internal/tools/v3tools/v3tools.go`）
  4. 结论合成 + 独立校验（`internal/validator/validator.go`，4 层幻觉控制）
  5. 高置信度自动入库（`internal/rag/knowledgebase/`）
- **5 个工具**：query_yarn、ssh_exec（只读）、call_umr_agent、deepwiki_query、fingerprint_match。
- **RAG**：混合检索（向量 + BM25 + RRF 融合），向量库抽象在 `internal/rag/vectorstore/`。
- **基础设施层**（`internal/infra/infra.go`）：umrAgent HMAC-SHA1 签名调用（IPv6:65431）、NodeInfoClient 走 uhadoop-manage HTTP API。**约束：禁止直连 MySQL 取节点信息，禁止 SSH 写操作。**
- **可观测性**：`internal/observability/`（event tracker、Markdown 报告），`sql/schema.sql` 定义持久化表。
- **部署**：`deploy/`（Dockerfile、deploy.sh、kubernetes/）。
- **测试**：Go 原生测试，7 个测试文件 / 18 个源文件——覆盖率明显不足。
- 文档：`LINK_CHAIN.md`（链路）、`OBSERVABILITY.md`（可观测性设计）。

## 二、生产就绪改造清单（按优先级执行）

### P0 — 正确性与健壮性

1. **并发与 goroutine 安全**：审计所有共享状态（EvidencePool、KB、event tracker）的并发访问；用 `go test -race` 全量跑通并修复所有告警。
2. **超时与取消**：每一处网络调用（LLM、umrAgent、uhadoop-manage、DeepWiki、SSH）必须有 context 超时；整个 diagnose 流程支持从 HTTP/CLI 入口透传 context 取消。
3. **LLM 调用韧性**：重试（指数退避 + 抖动）、限流、错误分类（可重试 vs 不可重试）、token 用量统计。
4. **错误处理规范**：统一错误包装（`fmt.Errorf("%w")`）、哨兵错误；杜绝吞错与 panic 逃逸（recover 边界在 agent 主循环）。
5. **输入校验**：所有外部入口（HTTP handler、CLI 参数、YAML 策略文件）的输入校验与大小限制。

### P1 — 安全

6. **Secrets 管理**：API key、HMAC 密钥不得出现在代码/日志/错误信息中；支持从环境变量或 secret 文件注入；审计 `deploy/` 与 `deploy.example/` 中的默认凭证。
7. **SSH 只读保证**：在代码层面（命令白名单/黑名单 + 测试）强制 ssh_exec 只读约束，而不是靠约定。
8. **注入防护**：所有 SQL 用参数化查询；所有 shell 调用禁止字符串拼接。
9. **HTTP 服务加固**（如有 HTTP 入口）：限流、认证、CORS、请求体大小限制、优雅关闭（graceful shutdown + drain）。

### P2 — 可观测性与运维

10. **结构化日志**：统一用 `slog`（或已选定的库），JSON 输出，带 trace_id/request_id 贯穿五阶段；分环境日志级别。
11. **Metrics**：Prometheus 指标——每阶段耗时、LLM token 消耗、工具调用成功率、KB 命中率、诊断置信度分布。
12. **Tracing**：OpenTelemetry 接入（可选，先留接口）。
13. **健康检查**：/healthz、/readyz，区分进程存活与依赖可用。
14. **配置管理**：`internal/config/config.go` 支持 env/文件双来源、启动时校验并 fail-fast、配置项文档化。

### P3 — 测试与 CI

15. **测试覆盖**：目标核心包（diagagent、strategy、validator、rag、infra）行覆盖 ≥ 70%；为每个工具写 mock/httptest 集成测试；补一条端到端冒烟测试（可用 `internal/simulated/` 模拟集群）。
16. **CI 流水线**：GitHub Actions——lint（golangci-lint）、vet、`go test -race`、构建镜像、（可选）覆盖率门禁。检查 `.github/workflows/` 现状并补齐。
17. **混沌/降级测试**：依赖不可达（LLM 超时、KB 不可用、umrAgent 掉线）时优雅降级并输出部分结果，不得整链路失败。

### P4 — 部署与交付

18. **Dockerfile 优化**：多阶段构建、distroless/alpine 基础镜像、非 root 用户、镜像体积审计。
19. **K8s 清单校对**：resource limits、liveness/readiness probe、PDB、HPA 按需；secrets 用 Secret 对象而非明文。
20. **版本化**：`-ldflags "-X main.version=..."` 注入版本号，/healthz 暴露；CHANGELOG 与语义化版本。

### P5 — 文档校准

21. 校准 README（含陈旧的 Python 徽章）、LINK_CHAIN.md、OBSERVABILITY.md 与代码一致；补运行手册（常见告警 → 处置步骤）。

## 三、工作方式要求

- **先出评估报告再动手**：通读代码后输出一份 production-readiness 评估（现状、差距、风险分级），列出你要改的每一项及理由，等我确认后按优先级逐项实施。
- 每完成一项任务：跑 `go build ./... && go test ./... -race && go vet ./...`，全绿才算完成。
- 小步提交，每个逻辑单元一个 commit，commit message 用中文、遵循仓库现有风格。
- 不引入大框架；优先用标准库，确需引入新依赖时先说明理由。
- 不做超出清单的"顺手重构"；发现清单外的重要问题先汇报。
