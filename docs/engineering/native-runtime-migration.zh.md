# 原生运行时迁移预演

当前交付 P3-01 的只读盘点、P3-02 的原生协议协商与配置写入校验，以及 P3-03 的可重跑排空和迁移写入。命令只迁移明确的运行时绑定与排队运行，不转换旧会话正文或外部会话 ID。

从仓库根目录，使用当前 checkout 的管理环境运行：

```bash
bash scripts/dev-env.sh exec -- go -C server run ./cmd/native-migration \
  --workspace-id=工作区UUID --dry-run > /tmp/native-migration-report.json
```

维护窗口中先用同一个 `operation-id` 排空，再应用迁移；写入动作必须显式关闭 dry-run：

```bash
OPERATION_ID=维护操作UUID
bash scripts/dev-env.sh exec -- go -C server run ./cmd/native-migration \
  --workspace-id=工作区UUID --operation-id="$OPERATION_ID" --action=drain --dry-run=false
bash scripts/dev-env.sh exec -- go -C server run ./cmd/native-migration \
  --workspace-id=工作区UUID --operation-id="$OPERATION_ID" --action=apply \
  --clear-cli-config --dry-run=false
```

`drain` 会持久化工作区屏障；领取 SQL 在屏障生效时返回空结果，因此旧 HTTP/WS daemon 不能继续领取，新排队写入可以等待迁移。已派发任务允许完成，`apply` 会同时检查运行状态、原生会话租约和模型调用租约。迁移失败会回滚本次事务；再次执行同一 `operation-id` 可安全重试。`--clear-cli-config` 只在已审查 CLI 配置后使用，清除遗留 CLI 参数和模型覆盖，保留 `custom_env`、MCP 和权限字段。

也可构建 `server/cmd/native-migration` 独立二进制，在已设置 `DATABASE_URL` 的运维环境运行。必须显式指定工作区 UUID；默认 `--dry-run=true`；写入需要指定 `action`、工作区和操作 UUID，并显式传入 `--dry-run=false`。`--timeout` 默认一分钟。连接失败只返回固定错误，不输出数据库连接串。

盘点使用同一个 PostgreSQL `REPEATABLE READ READ ONLY` 事务；读取期间有其他写入时，报告仍反映 `snapshot_at` 时刻。报告带 `schema_version: 1`、工作区 UUID 和只读标记。所有关联投影限定工作区，结果按 ID 稳定排序，空集合输出 `[]`。

| 报告字段 | 内容 |
| --- | --- |
| `runtimes` | provider、机器 daemon ID、owner、visibility、自定义 profile、状态与最近心跳；匹配的原生运行时候选和活动/排队/延迟运行数 |
| `agents` | 当前运行时绑定、owner、调用权限模式、user/system 类型、归档状态；是否存在 CLI 配置、模型覆盖、内联 MCP 和允许的 Connected Apps 数量 |
| `profiles` | 自定义 profile ID、协议、可见范围、创建者及启用状态；命令和参数需要人工翻译为原生配置 |
| `sessions` | 原生检查点及旧聊天/运行的历史引用、会话范围、机器、目录和租约状态；不输出外部会话标识或模型历史 |
| `runs` / `pending` | queued、deferred、dispatched、running、waiting_local_directory 的清单与汇总；保存的运行时和有效绑定分别报告 |
| `integrations` | 工作区 MCP、智能体 MCP 绑定、插件、成员 Connected Apps、渠道安装的 ID、归属及状态 |
| `directories` | 项目本地目录资源的 ID、项目、daemon ID 与路径；只投影指定字段 |

报告不读取原始 MCP/插件/渠道配置，不输出环境变量值、CLI 命令/参数、认证信息、模型名覆盖值、会话正文或写回执内容。

`assessment` 的解释：

| `disposition` | 处理方式 |
| --- | --- |
| `can_migrate` | 存在唯一、在线、同机器、同 owner、同 visibility 的原生候选；这是数据库层面的迁移候选，需要在维护窗口重新检查 |
| `already_native` | 已绑定原生运行时；无需改 provider |
| `requires_reconfiguration` | 缺少身份/目标、目标离线或不唯一、自定义 profile、CLI 参数或模型覆盖、私有机器 owner 不匹配等；逐条查看 `reasons` |
| `requires_drain` | 活动/已派发运行或原生租约仍存在；不能直接重绑正在执行的运行 |
| `cannot_resume` | 旧 CLI 会话只能保留历史展示；继续执行需要新的原生会话，不能将旧会话 ID 当作原生检查点 |
| `review_queue` | 迁移前重验排队/延迟运行的绑定、发起者权限和 Connected Apps overlay；不得直接重放旧快照 |
| `verify_on_machine` / `verify_on_target` | 在目标机器验证目录、原生会话和工具能力；集成继续遵守已有授权 |

数据库快照不能证明目录存在、权限可用、daemon 版本兼容、模型已配置或外部账号有效。机器本地 CLI/profile 文件也不在数据库中，报告的 `limitations` 明确列出这些后续检查。命令不探测本机 AI CLI、不访问模型或第三方服务，也不将旧 CLI 会话转换为原生历史。

本地验证使用独立 fixture，覆盖机器/owner/visibility 匹配、离线与重复目标、活动运行阻断、配置重配、外部与原生会话分类、集成配置排除、跨工作区隔离、报告前后数据对账，以及读取期间并发修改仍保持单一快照。具体实现和测试位于 `server/internal/nativemigration/`、`server/cmd/native-migration/`。

## 协议和升级顺序（P3-02）

原生运行时的最低受支持协议为 **1**，能力名为 `native-runtime-v1`。最低 daemon 指实现并发送该能力的 Multica 构建；版本字符串不能替代协议能力。当前变更尚未发布，不预先指定一个不存在的 CLI 发布版本；发布候选需要记录实际包含此协议的 API/daemon 制品版本。

API 注册响应新增以下字段，保留原有 `runtimes`、`repos`、`repos_version` 和 `settings`：

```json
{
  "server_capabilities": ["rpc-v1", "native-runtime-v1"],
  "native_runtime_protocol": {"version": 1, "minimum_version": 1}
}
```

`version` 为服务端支持的最高协议版本，`minimum_version` 为最低版本。daemon 要求自身协议位于此范围内，并同时确认服务端声明 `native-runtime-v1`。注册时只要请求或响应含原生运行时，就需要完成协商；旧 API 返回 `unknown` 或遗漏运行时也会失败，不进入执行循环。

服务端在原生注册、单条领取、批量领取和启动入口校验协议。领取与启动同时检查当前请求的 `X-Client-Capabilities` 及注册时持久化的能力，防止旧二进制沿用新二进制的注册记录。WebSocket `tasks.claim` 使用连接握手的能力，复用相同领取逻辑。

| 场景 | 行为与恢复 |
| --- | --- |
| 旧 daemon 注册原生运行时，或不兼容的原生领取/启动 | HTTP 426，`code: native_runtime_protocol_required`、`required_capability: native-runtime-v1`、`minimum_protocol_version: 1`；升级 daemon 后重新注册并重连 WebSocket |
| 同一批请求包含旧运行时和原生运行时 | 跳过旧运行时，只领取协议兼容的原生任务；不兼容的原生集合返回 426 |
| 旧 AI CLI 运行时注册／单条领取或启动／profile 写入 | 注册返回 409；单条领取和启动返回 410 `external_runtime_retired`；profile 新增和修改返回 410，历史读取和清理继续按原权限处理 |
| 原生任务使用旧 `{}` 启动请求 | HTTP 400；必须发送领取返回的 `runtime_id` 和 `dispatched_at`，过期领取仍按原有代次约束返回冲突 |
| 原生注册附带 CLI profile | HTTP 400；原生运行时不能绑定外部 CLI 启动配置 |
| 原生智能体写入非空 `runtime_config`、`custom_args`、`thinking_level` 或 `service_tier` | HTTP 400，`code: native_cli_config_not_supported`；使用 `{}`、`[]`、`""`、`""` 显式清除后重试 |
| 原生智能体写入其他模型，或重绑时继承旧模型覆盖 | HTTP 400；显式清空 `model` 或使用部署模型。错误不回显提交值 |

注册中的原生协议/profile 校验在任何运行时写入之前完成。协议不兼容的领取不会改动任务状态、领取次数、派发时间或创建任务令牌；启动拒绝也不会将任务标记为运行中。原生单条领取和启动还校验 daemon token 的机器身份。这里的协议声明用于兼容性协商，认证和工作区/机器/执行授权仍独立执行。

原生智能体的 `custom_env` 仍用于工具环境，MCP 配置仍属于支持的工具接入，继续遵守既有授权。普通智能体新增/修改接口校验最终配置，重绑时不能静默携带 CLI 参数。阶段三最终构建仅注册和执行内置运行时，禁用外部 AI CLI 配置写入，并保留历史读取和删除。历史用量访问与执行资格分别校验，退役不会抹去原有可见历史。

按以下顺序准备切换：

1. 备份并运行 P3-01 只读盘点，记录机器、owner、visibility、目录、活动运行和队列；在维护窗口重跑，不把早期快照作为写入授权。
2. 在旧部署上停止新领取并排空活动执行；保留备份与盘点报告。再升级 API，使所有 API 副本都支持原生协议、会话及模型调用。验证注册响应，部署唯一模型配置；混合 API 版本期间不启用原生任务调度。
3. 升级目标机器的 Multica daemon，重新注册并建立新的 WebSocket 连接。确认注册返回原生运行时和支持的协议范围、元数据记录能力；先通过本地假模型验证任务领取、启动和续接。
4. 执行 P3-03 的维护阻断、排空、配置清理、绑定及队列协调。必须使用持久化迁移屏障，不能仅靠 `status=offline` 或协议升级视作迁移已完成。旧执行器的新领取会在数据库领取边界被拒绝。
5. 完成三端产品收敛、CLI 实现移除和 P3-08 对账/恢复演练后，再准备发布候选与生产切换。生产迁移和发布仍是后续明确操作。

最终构建阻止旧运行时注册、领取及启动；已有部署必须先在升级前的维护窗口排空旧任务。协议升级也不迁移历史：外部会话仍不能原地续接，原生会话不交给旧 API/旧 daemon 恢复。进程崩溃后的恢复需要同时遵守原生会话租约和模型调用配额租约；重启验收通过模拟两种租约到期验证，不代表重启后可立即绕过尚未到期的调用保留。需要回退部署时，先停止新领取并排空活动原生运行，再按经过演练的备份恢复方案处理；不自动转交外部 AI CLI。

协议测试位于 `server/pkg/protocol/native_test.go`、`server/internal/daemon/native_protocol_test.go` 和 `server/internal/handler/native_protocol_test.go`，覆盖缺失/未来协议、旧 API、过期注册、HTTP/WS 领取、混合批次、无副作用拒绝、机器身份和配置清除。渠道和自动化集成夹具显式代表已注册的当前 daemon。
