# 原生运行时第二阶段验收矩阵

本矩阵限定于本地假模型、假渠道和管理开发数据库。真实模型质量、真实 OAuth、真实渠道账号，以及 Electron/移动设备验收不属于这些自动化证据；设备与迁移演练在第三阶段继续完成。生产迁移和发布没有执行。

每种行为优先由所属模块验证，正式 router 集成验证各模块已接入原生执行链。无需将渠道自身的全部协议故障矩阵复制到每个模型用例。

| 验收项 | 规范测试位置 | 验证内容 |
| --- | --- | --- |
| 业务读取、回复与状态 | `server/internal/handler/native_business_test.go`；`server/internal/daemon/native_business_test.go` | 实际 task-token 中间件、运行身份、工作区、revision、评论归因和正常通知/触发；固定工具不运行 Multica CLI |
| 创建及配置写操作 | `server/internal/handler/native_management_test.go`；`native_write_test.go`；daemon 对应工具测试 | 项目/资源、智能体/技能、小队/成员、自动化/触发器；沿用业务服务和权限；持久化回执绑定参数、会话和运行，不重放未知写入 |
| 规则与技能 | `server/internal/daemon/native_prompt_test.go`；`native_skills_test.go`；`execenv/native_context_test.go` | 根规则与目录规则、按需技能、禁用和越界拒绝，不扫描 AI CLI 配置或委派 |
| MCP 传输 | `server/internal/daemon/mcpclient/host_test.go` | stdio、Streamable HTTP 的 JSON/SSE 响应、旧 SSE；初始化、分页、schema、类型化结果、取消、关闭及不重试未知写入 |
| Remote MCP 与插件 | `server/internal/daemon/native_mcp_test.go`；`remote_mcp_broker_test.go`；`pkg/remotemcp/*test.go` | 批准工具与 schema digest、凭据 broker、插件归因、网络限制；仅精确本机 broker 端点有本机权限 |
| Connected Apps | `server/cmd/server/native_connected_apps_integration_test.go` | 八个场景：领取快照开/关、正式入队、正式启动接线、开关关闭、已撤销连接、白名单排除、调用权限拒绝；仅 owner 账号与 allowlist 的交集进入 MCP overlay |
| 压缩与历史 | `server/cmd/server/native_compaction_integration_test.go`；`server/internal/handler/native_context_test.go`；engine 与 agentstate 测试 | 同一模型摘要后继续工具循环；26 秒摘要及超过 45 秒租期的慢摘要持续续租并继续工具执行；工具组完整、保留用户约束、摘要失败原子性、完整审计历史保留 |
| 重复读取与阻塞收敛 | `server/cmd/server/native_progress_integration_test.go`；`server/internal/daemon/engine/progress_test.go`；`native_progress_test.go` | 相同只读参数和结果连续出现时提示收敛，忽略反馈则提前停止；保留原约束并允许指定线程汇报阻塞；新证据、写操作和人类追加输入重置停滞判断；完整历史使摘要与恢复不会重置检测 |
| 追加输入 | `server/internal/daemon/engine/supplement_test.go`；`server/internal/handler/native_supplement_test.go` | 安全步骤装入输入；检查点与 delivered 在同一事务；领取后中断、取消和提交失败不丢失输入 |
| 附件与产物 | `server/internal/handler/native_attachment_test.go`；`native_artifact_test.go`；daemon 对应测试；渠道 router 集成 | 当前运行授权、来源不可变克隆、UTF-8 与图片能力限制、真实本地存储和渠道交付；不将下载能力 URL 写入模型历史 |
| 来源克隆回收 | `server/internal/service/source_context_claim_test.go`；`server/internal/handler/source_context_integration_test.go` | 固定候选集合，在不同 PostgreSQL 执行计划下单次领取只锁定一条清理记录；目标删除后全部克隆可回收 |
| 自动化及小队 | `server/cmd/server/native_automation_integration_test.go`；`native_squad_integration_test.go`；既有 scheduler/service 测试 | schedule/webhook × run_only/create_issue；正式入队、claim、原生执行与终态；十三种既有小队触发/去重/取消场景，未新增委派策略 |
| API/网络中断与恢复 | `server/cmd/server/native_restart_integration_test.go`；`server/internal/daemon/native_model_test.go`；engine、journal 与写回执测试 | 断开 TCP/模型流并重建 router/backend，模拟原生会话与模型调用租约到期后，保存的工具结果不重放、半截回复不提交；未知写入明确停止并要求对账 |
| daemon、租约及授权撤销 | `e2e/native-runtime.spec.ts`；`server/internal/handler/native_session_test.go`；`native_inference_test.go` | 实际 daemon 重启后续聊；租约到期和新 token fencing；credential/runtime/claim/lease 撤销取消打开的上游流 |
| 删除期间的流 | `server/internal/handler/native_inference_test.go`；`native_retention_test.go`；工作区删除 manifest 测试 | issue、chat、builder 经删除入口取消上游，late 推理被拒绝，历史与回执清理；工作区与 acquire 写入有事务围栏 |
| 渠道接线 | `server/cmd/server/native_{telegram,slack,feishu,wecom}_integration_test.go`；`dingtalk_reactions_integration_test.go` | 正式 ingress → claim/start → native → complete → 原线程/会话，入口及终态去重；Telegram/钉钉/企业微信产物交付，Slack/飞书按现有能力仅文本 |
| 渠道故障恢复 | `server/internal/integrations/{telegram,lark,wecom,dingtalk,slack}/*test.go` | Telegram 429/backoff、失败分块不重复发送与 shutdown；飞书重启后从 delivery row 清理 typing；企业微信 close retry/失败终态/ACK；钉钉断线重连及 dispatcher 排空；Slack 失败/取消清理 |
| 隔离与监控 | `server/cmd/server/native_isolation_integration_test.go`；`server/internal/modelcontrol/*test.go`；`server/internal/metrics/model_call_test.go` | 同 daemon 跨工作区的文件/会话/MCP 凭据隔离；跨 API 副本配额、调用账本、脱敏；仪表盘浏览器证据见主计划 |

明确的能力限制：MCP 仅装载工具，不提供 sampling、resources 或 prompts；二进制文档不解析，文本输入上限 64 KiB、原生产物上限 1 MiB。Shell 受显式权限和进程生命周期管理，工作目录约束不构成操作系统沙箱。检查点有大小预算，历史并非无限。业务提交与写回执提交之间仍可能出现未知结果，不能承诺 exactly-once。

阶段二通过表示上述受支持范围的本地结构、授权及故障行为通过；不表示所有第三方服务或模型质量均已实测。阶段三的真实部署切换仍须排空、对账和备份恢复演练。
