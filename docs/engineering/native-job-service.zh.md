# 独立 Multica 作业服务

`native-jobs` 将命令生命周期从 Native 任务中分离。它适合部署到训练主机：模型提交训练后可以结束当前任务，之后的任务继续查询同一作业。API 服务或 Native 守护进程重启不会停止它的训练进程。

## 生命周期与权限

- 作业服务直接运行命令，权限来自服务运行账号。它不是容器或 GPU 调度器；可选 Linux 隔离策略见下文，默认不启用。为不同工作区、智能体配置不同令牌和 namespace；namespace 控制作业 API 可见范围，不隔离同一系统账号的文件权限。
- 每个 namespace 的并发、累计记录数、单作业磁盘日志均有上限。达到容量返回 429，不排队、不抢占、不删除幂等记录。按 `max_records × log_bytes × namespace 数量` 规划最大日志空间；达到记录上限后需要管理已有数据或配置新的独立数据目录，不能删除未核验作业以绕过保护。
- Native 任务结束、客户端断开不会取消作业。显式取消或关闭作业服务会清理其托管进程树。通过 SSH 启动的远端进程必须另行核验；结束 SSH 客户端不证明远端已经停止。
- 作业服务自身重启后，未结算记录标记为 `interrupted`，保留日志，不使用 PID 接管或杀停旧进程，不自动重跑命令。硬崩溃后须核验残留进程；systemd 的进程组清理可以限制本机残留，但不构成跨节点恢复保证。
- 完成记录、提交摘要和日志保留在私有数据目录。命令内容和环境不写入作业元数据；命令输出仍可能包含私密数据。不要让多个服务共享同一个数据目录；文件锁会拒绝第二个实例。

## 构建与启动

在仓库根目录使用当前 Go 环境构建：

```bash
go -C server build -o /tmp/native-jobs ./cmd/native-jobs
```

将二进制安装到执行节点。创建仅服务账号可读的 JSON 配置，权限为 0600。以下是模板；令牌必须替换为独立生成的随机值，不能使用示例文本。

```json
{
  "listen": "127.0.0.1:23110",
  "work_dir": "/var/lib/multica-jobs/work",
  "data_dir": "/var/lib/multica-jobs/data",
  "keys": [
    {
      "namespace": "workspace-agent",
      "token": "REPLACE_WITH_A_RANDOM_SECRET_OF_AT_LEAST_32_CHARACTERS"
    }
  ],
  "limits": {
    "max_active": 4,
    "max_records": 4096,
    "log_bytes": 67108864
  }
}
```

`work_dir` 必须已经存在，`data_dir` 必须是私有目录（0700；不存在时自动创建）。二者必须为绝对路径。令牌在请求中使用 Bearer 认证；为每个 namespace 配置唯一令牌，轮换时保留 namespace 才能继续访问其已有记录。

```bash
/opt/multica/bin/native-jobs --config /etc/multica/jobs.json
```

[systemd 模板](../../deploy/harness/native-jobs.service) 使用独立服务账号和私有状态目录。安装前调整账号、二进制、工作目录及数据目录。服务默认绑定回环；跨主机接入时使用受控内网入口或具备 TLS 的反向代理。不要在 URL、命令行参数或任务描述中填写令牌。

## Native 接入

1. 将服务的最终源地址加入运行时所有者配置 `MULTICA_NATIVE_HTTP_ORIGINS`。不要在有在途任务时重启守护进程应用此配置。
2. 在智能体的环境变量中配置 `JOBS_TOKEN`。
3. 在该智能体 Native 配置的外部服务列表中加入以下条目，替换为实际最终地址：

```json
{
  "name": "multica_jobs",
  "base_url": "http://127.0.0.1:23110",
  "token_env": "JOBS_TOKEN",
  "auth_scheme": "Bearer",
  "allow_write": true
}
```

允许写入即授权在该执行节点提交和取消命令，独立于本地 Shell 开关。只读服务仍可查询已有作业。服务名 `multica_jobs` 启用 `persistent_job` 工具，配置中的真实令牌只从环境变量私下注入，不进入模型请求。该保留服务拒绝通用 `http_request` 访问，防止绕过运行时生成的提交身份。

| action | 参数 | 行为 |
| --- | --- | --- |
| `submit` | `command` | 提交到服务工作目录；执行身份由运行时会话和当前工具 call ID 生成，模型不能指定 |
| `list` | `after?`、`limit?` | 当前令牌可见的作业；用 `next_cursor` 翻页 |
| `output` | `job_id`、`offset?`、`wait_ms?` | 状态、退出码和日志；等待最多 20 秒，不拥有命令截止时间 |
| `search` | `job_id`、`text`、`offset?` | 搜索磁盘保留日志；按 `next_offset` 继续 |
| `lookup` | 原提交的 `call_id` | 在当前 Native 会话中找回提交回执，不执行命令 |
| `cancel` | `job_id` | 取消并等待本机进程树结算 |

提交成功只证明作业已准入，不证明训练成功。智能体应将 `job_id`、运行状态和日志位置写入任务。不要为查询进度重新提交；结束任务前不必取消独立作业。后续不同 Native 会话通过 `list` 和已记录的 `job_id` 找回工作。

响应丢失后，运行时可以按原身份读取提交回执；模型也可以使用 `lookup` 或 `list`。404 表示未找到持久准入，不应换新身份盲目提交。服务日志达到配额会设置 `log_truncated`，保留已落盘前缀和有限尾部，缺失区间由读取结果明确标注。

## 服务接口

除 `/healthz` 外，所有接口均要求 Bearer 认证；没有工作区或所有者参数，范围来自令牌。

| 方法和路径 | 请求或查询 |
| --- | --- |
| `POST /v1/jobs` | JSON：`key`、`command`；相同 key 和内容返回已有准入，内容不同拒绝 |
| `GET /v1/submissions/{key}` | 只观察已有准入 |
| `GET /v1/jobs` | `after`、`limit`（1–100，默认 20） |
| `GET /v1/jobs/{id}` | `offset`、`wait_ms`（0–30000） |
| `GET /v1/jobs/{id}/search` | `text`、`offset`；每页最多扫描 1 MiB，返回最多 20 个匹配 |
| `POST /v1/jobs/{id}/cancel` | 无请求体要求 |

`output` 的偏移为原始字节位置，读取不会消费日志。保留的磁盘日志先于模型回执同步到磁盘。元数据原子替换并同步目录；Windows 尚不支持独立服务。服务不自动重试提交，不提供训练调度队列。任务结束后的观察和自动跟进由 Native 守护进程与 API 协作完成，规则见下文。


## 统一远程执行环境

参考 deepseek-harness 的远程文件与子进程共用环境设计，智能体 Native 配置增加 `environment`。在页面中配置 `multica_jobs` 服务后，可开启“使用远程执行环境”：

```json
{
  "environment": "multica_jobs",
  "write_files": true,
  "shell": true,
  "services": [{
    "name": "multica_jobs",
    "base_url": "http://127.0.0.1:23110",
    "token_env": "JOBS_TOKEN",
    "auth_scheme": "Bearer",
    "allow_write": true
  }]
}
```

作业服务的 `limits.write_files` 也须为 `true` 才能授权文件写入。新接口要求先升级作业服务，再切换智能体环境；旧服务会被明确拒绝。升级前核验在途作业，关闭服务会取消其托管进程。

启用后，`read_file`、目录、搜索、规则读取、文件修改、`shell`、作业查询、产物读取与 `terminal` 都在服务的 `work_dir` 中执行。路径仍为相对路径，目录穿越和逃逸符号链接会被拒绝。本机 stdio MCP 不再启动；HTTP MCP 仍使用各自明确配置的服务。默认省略 `environment` 时使用本地环境，已有配置不会自动切换。

新增接口均使用原有 Bearer 认证：

| 接口 | 用途 |
| --- | --- |
| `GET /v1/environment` | 工作目录、文件写入权限、隔离与网络策略 |
| `POST /v1/environment/tools` | 受限工具名、参数及运行时生成的 scope/key |
| `POST /v1/environment/artifact` | 受限相对路径的文件字节，最多 1 MiB |

同一 namespace 的普通作业可跨 Native 会话查询；交互终端还绑定创建它的引擎会话。文件目录并未按 namespace 隔离，需要互不信任的租户使用独立账号、目录或容器。

## 作业完成后的自动跟进

`persistent_job` 提交前，守护进程先保存持久观察义务；远程 `shell` 显式后台提交也先登记，前台返回仍在运行、响应丢失或恢复到已有提交回执时补登记。观察进程只读取原提交回执，不执行新命令。

完成、失败、取消或中断后，待原 Native 运行退出，API 将一条系统评论和至多一个后续任务放入同一事务。相同通知重试、响应丢失和守护进程重启都复用原身份，不重复排队。通知使用守护进程控制凭据：短期凭据必须匹配源执行器；长期 PAT/JWT 必须属于该运行时所有者，其他成员或管理员不能代报。另有在途运行或重试时暂缓通知，避免与当前运行竞争。后续任务继承原始用户与责任人，重新核验成员资格、调用权限、当前分配、运行时和任务状态，并刷新关联应用授权。

原任务取消、任务已关闭、智能体或运行时变更、成员退出或调用权限撤销时，只记录事实，不自动启动。完成进程不会自动将业务任务设为 `done`，也不会隐式通知队长；智能体仍须核验产物、交付 `in_review`，并按父任务协作规则显式报告。

私有观察目录为守护进程工作区根目录下 `.native-job-watches`，目录 0700、文件 0600，最多 4096 条。文件含作业服务令牌，不得提交或放进模型上下文。缺失回执、401、撤销源地址及平台不可用都会保留记录并限频告警，需要运维核验；没有盲目重新提交或自动丢弃机制。令牌轮换时尚需安全更新未结算记录；当前没有积压管理页面。聊天运行不自动创建业务任务跟进。

## 上下文与交互终端

参考 deepseek-harness 的压缩策略，触发阈值调整为剩余输入预算的 80%，保留最近完整工具调用组，并从原始工具结果确定性恢复作业 ID、状态和日志偏移。原始历史保留用于审计；摘要中的状态不代表实时状态。

同一运行中，未改变的请求前缀使用模型返回的真实输入 token 用量，新数据仍按字节保守预留。模型不返回用量、历史投影或工具集改变、守护进程重启时，会重新采用保守估算；没有通用 tokenizer，图片附件仍保留独立保守预算。

Linux 上新增 `terminal`，支持 `open/send/read/list/resize/interrupt/close`。变量、当前目录与 REPL 状态可跨调用保留；中断发送到前台进程组，静默不被视作完成。单次输出最多 8 KiB，保留 256 KiB 滚动输出，每会话最多 4 个、每主机最多 64 个终端；闲置十分钟回收。终端在任务结束时关闭，不支持重启恢复或全屏程序协议。

## 可选执行隔离

宿主所有者可在服务 `limits` 中配置：

```json
{
  "max_active": 4,
  "max_records": 4096,
  "log_bytes": 67108864,
  "write_files": true,
  "sandbox": {
    "mode": "workspace_write",
    "network": false,
    "read_paths": [],
    "device_paths": []
  }
}
```

`workspace_write` 仅将工作目录以可写方式挂载；`read_only` 以只读方式挂载并拒绝文件写入授权。两者使用 bubblewrap 命名空间、只读系统运行库和私有临时目录，默认隔离网络；命令与终端共用策略，且不继承服务凭据。GPU 设备、额外可读目录和网络必须由宿主所有者明确授权。策略在服务运行期间不可变。

受限模式在启动时执行真实探测，不支持或内核拒绝时直接启动失败，不降级为无隔离。默认省略配置仍为 `unconfined`。该能力不提供 CPU、内存、GPU 配额或对主动脱离终端会话的进程的通用 cgroup 清理保证；需要相应能力时仍应使用容器或系统服务监督器。本开发主机的内核未通过 bubblewrap 探测，因此已验证拒绝路径，实际隔离执行需在支持的节点进一步验证。
