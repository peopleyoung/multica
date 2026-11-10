# 单一模型运行时：阶段三验收记录

2026-11-06。代码目标为 Multica API 统一调用部署配置的一个模型，daemon 执行本机工具。开发和验证均由主智能体完成，未调用真实模型、真实外部渠道或其他 AI CLI。

## 已交付

- 默认仅注册 Multica Native，关闭原生运行时配置会明确报错。移除 AI CLI 查找、启动适配器、专属认证和会话目录准备；Git、Shell、MCP 子进程仍是工具能力。
- 服务端拒绝旧运行时注册（409）、领取及启动（410）；批量和 WebSocket 领取只放行兼容原生运行时。禁止 profile 新增／修改和智能体 CLI 参数／其他模型覆盖。旧响应字段与未知枚举 fallback 保留，已安装客户端得到明确错误。
- Web/Desktop 共享逻辑及 Mobile 独立界面统一固定部署模型，保留机器选择、权限、用量、错误与历史。历史读取权限独立于执行资格，旧 provider/profile 的可见用量不会因退役丢失。
- 迁移命令提供 inventory/status/drain/apply/resume，默认 dry-run、显式 operation ID、数据库领取屏障、活动执行与租约检查、事务回执及幂等重试。保留旧历史、目录、权限、MCP 和产物，旧外部会话重新执行需建立原生会话。
- Docker 同时打包 native-migration；环境模板、Compose、自托管文档、五种语言的安装／运行时说明及教程更新。Windows/macOS CI 改为原生执行、工具、MCP 与进程树验证。
- 全量验收修复运行说明丢失：原生 brief 直接传入模型，包含工作区及智能体指令，准备失败时停止执行。运行级回归先失败，再通过 race 检查。

## 自动化验证

| 范围 | 结果 |
| --- | --- |
| Go 全套 | 管理环境内运行 `env GOFLAGS=-p=2 bash scripts/dev-env.sh exec -- bash scripts/test-go.sh --race` 通过；使用本机 Git 2.50.1，包含数据库和原生模块 |
| 前端类型与 lint | `pnpm typecheck`、`pnpm lint` 通过；lint 存在既有警告 |
| Core / Views | 170 文件 2227 测试／474 文件 5978 测试通过；新增历史访问回归另行通过 |
| Desktop / Web | 59 文件 666 测试／35 文件 279 测试通过 |
| Docs / UI Lab | 3 文件 16 测试／8 文件 46 测试通过 |
| Mobile | 独立 typecheck、lint、38 文件 251 测试及 iOS 运行脚本断言通过 |
| 浏览器全链路 | `e2e/native-runtime.spec.ts` 通过（2.2 分钟）；本地假模型和显式源码构建二进制，全部外部 AI CLI 配置为测试缺失路径 |
| 跨平台构建 | `CGO_ENABLED=0` 的 Windows amd64、macOS arm64 CLI 交叉编译通过；不是对应操作系统的运行验收 |
| 迁移恢复 | `MULTICA_RUN_NATIVE_MIGRATION_DRILL=1` 的隔离 schema PostgreSQL 真实备份／恢复测试通过 race 检查 |
| 共享模型配额 | `pkg/llm`、`internal/modelcontrol` 的 race 回归通过，覆盖逐次重试、拒绝后不调用上游及缺失／无效／零用量区分 |
| 配置与辅助脚本 | CI 辅助脚本 41 项测试通过；CI YAML 可解析；Compose 配置检查、37 个文档相对引用及 `git diff --check` 通过 |

首次无并发约束的前端测试出现 worker 启动超时，限制为两个 worker 后通过。Desktop 的 Electron 下载首次网络中断，安装完成后全套通过。系统 Git 2.25.1 不支持夹具使用的 `git init -b`，改用已有 Git 2.50.1 后完成 Go 全套。未更改生产约束以绕过这些环境条件。

浏览器验收的假模型最初未返回用量，多轮请求的保守预留累计后使图片请求触发共享配额。夹具现显式返回测试用量；生产配额规则、图片上下文预留和缺失用量的保守核算保持原有行为。临时诊断已移除。

最终浏览器用例验证固定部署模型、文件工具及磁盘结果、第二轮历史、Agent Builder 草稿确认创建、Mika 技能／项目／任务与评论及 `in_review` 交接。自动化覆盖预览后重启 daemon、历史恢复、上游 503 重试，以及只创建一条 09:00 Asia/Shanghai 触发器；来源图片覆盖评论入口、授权图片字节进入模型、只创建一个子任务、不可变附件克隆，以及原描述变更后快照仍可读取。全部模型请求均断言使用 `native-browser-fixture`。

## 迁移与恢复证据

隔离演练建立旧／原生运行时、智能体、历史执行、排队执行、聊天、项目目录及本地产物。真实 `pg_dump` 归档、drain/apply、原生写入后，在同一个隔离 schema 恢复维护前备份，再逐项对账。结果为 1 个智能体、2 个运行时、1 个排队运行及产物保留，恢复至 `restored_pre_migration`。本次归档 415581 字节，SHA-256 为 `1d4a9c866e299526879f815d729971279620174d501af7dc74b84eb924bafd5d`。该归档包含测试数据，不作为任何实际部署的备份。

运行代码见 `server/internal/nativemigration/restore_drill_test.go`，操作顺序与回退限制见[迁移说明](native-runtime-migration.zh.md)。先在旧部署停止领取并排空，再升级最终原生构建；发生原生写入后禁止无条件降级并重放旧队列。

## 发布前仍需验收

G3 尚不能整体勾选。当前环境没有 Android 模拟器或 iOS 设备，未执行移动端原生导航／断线重连及三端交叉续聊；未运行正式 Electron 安装包或 Windows/macOS 真实进程测试。交叉编译和共享组件测试不能替代这些流程。

真实模型质量、供应商用量／费用、生产机器工具及 MCP 凭据、生产备份和迁移均未验收。发布候选须记录实际 API／daemon 制品版本、固定模型配置、三端设备结果与每个目标工作区的迁移对账，然后按维护窗口执行发布。此次仅完成本地工程变更与可执行演练。
