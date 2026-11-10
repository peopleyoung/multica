# 当前源码的独立部署

访问地址：<http://172.16.66.249:23000/login>

直接通过 IP 和端口访问，不依赖 DNS 或跳转域名。访问电脑需要能够连接该内网。
本部署的认证、CSRF、登录标记和工作区 Cookie 统一使用 `_harness_23000` 后缀，
可以在同一个浏览器中与 `13000` 实例分别登录，并同时保持登录状态。
升级后需在 `23000` 重新登录一次，之后正常刷新页面即可。

## 管理

在仓库根目录运行，始终使用此包装脚本，以固定项目名和配置：

```bash
deploy/harness/compose.sh ps
deploy/harness/compose.sh logs --tail 100 backend
deploy/harness/compose.sh up -d --wait --wait-timeout 300
deploy/harness/compose.sh restart
deploy/harness/compose.sh down
```

`down` 保留数据库和附件卷。容器设置了 `unless-stopped` 重启策略。
上述命令只管理 `multica-harness`，现有的 `multica` 项目由其原配置管理。
不要在本部署上直接运行根目录的 `make selfhost` 或 `make selfhost-stop`，
它们默认使用旧实例的项目名 `multica`。

## 登录和邮件

当前没有配置 SMTP、Resend 或 Google OAuth。按本实例的使用要求，已设置
`APP_ENV=development`。使用用户名和密码注册，邮箱可选，不使用验证码。
旧账号请由管理员按原用户 UUID 初始化用户名和密码，参见[账号设置](../../apps/docs/content/docs/auth-setup.zh.mdx)。

本实例原账号已初始化为 `admin`，保留原用户 UUID、工作区成员身份和历史。
初始随机密码保存在仓库内
`.multica/harness-deploy/password-rollout/existing-account-password.txt`，权限为 `0600`。
请从服务器读取该文件，不要将其纳入 Git 或部署日志。

如需发送邮件，在本目录 `.env` 中设置项目支持的 `SMTP_*` 或
`RESEND_API_KEY` / `RESEND_FROM_EMAIL`，然后执行：

```bash
deploy/harness/compose.sh up -d --no-deps backend
```

密钥保存在权限为 `0600` 的 `.env` 中，未纳入 Git。请保留该文件，尤其是
`JWT_SECRET`、数据库密码和 `MULTICA_VCS_SECRET_KEY`。当前为内网 HTTP 入口；
如需公网使用，应先配置自己的域名和 HTTPS，并同步更新 `.env`
中的访问地址及 nginx 的域名配置。

## 隔离与验证

- Compose 项目：`multica-harness`。
- 当前发布：`harness-modeltest-20261110t071835z`。后端和前端镜像均使用
  `modeltest-20261110t071835z` 标签，来自当前 checkout，包含尚未提交的改造。
  本次更新前端和后端，页面与 API 版本一致；Native 守护进程保留现有二进制，独立作业服务保持原进程。
- 数据卷：`multica-harness_pgdata`、`multica-harness_backend_uploads`。
- 网络：`multica-harness_default`。
- 唯一发布端口：内网地址和 loopback 的 `23000`；PostgreSQL、API 和前端容器均未单独发布端口。
- 数据库、JWT 和集成加密密钥独立生成。
- Cookie 名称在后端源码副本和前端镜像构建时统一隔离；更新时须使用本目录
  的构建脚本和 Dockerfile，以保留 `_harness_23000` 命名空间。

```bash
curl --noproxy '*' -fsS http://172.16.66.249:23000/readyz
```

应返回数据库和迁移检查均为 `ok`。构建日志和部署前容器快照位于
仓库内被忽略的 `.multica/harness-deploy/`。

## 备份和更新

数据库备份：

```bash
deploy/harness/compose.sh exec -T postgres pg_dump -U multica_harness -d multica_harness > multica-harness.sql
```

附件位于 `multica-harness_backend_uploads` 卷，需要与数据库及 `.env` 一起备份。
更新源码后，在 `.env` 中设置新的 `MULTICA_IMAGE_TAG` 和 `MULTICA_BUILD_VERSION`，
再执行（构建版本应与 `.env` 中一致）：

```bash
MULTICA_BUILD_VERSION=harness-<version> deploy/harness/build.sh
deploy/harness/compose.sh up -d --wait --wait-timeout 300
```

backend 启动时自动迁移本实例的独立数据库。
本目录的构建脚本也打包 `account-admin` 和 `native-migration` 管理工具。

2026-11-07 更新前的数据库、附件及环境配置备份位于
`.multica/harness-deploy/password-rollout/`，分别为 `database-before.dump`、
`uploads-before.tar.gz` 和 `env-before-20261107T004617Z`，均为 `0600`。
旧版镜像标签 `b115ec80f` 保留。认证数据库的回退约束见
[账号迁移说明](../../docs/engineering/password-accounts.zh.md#回退约束)。

本机 Docker Hub 下载受限，因此部署使用专用构建文件：前端使用本机缓存的
Node 22 Debian 镜像（按 digest 固定），并安装 Debian TLS 更新；后端使用
经 SHA-256 校验的 Go 1.26.9 工具链在本地编译，再打包进项目使用的 Alpine
运行环境。Go 工具链和缓存均位于 `.multica/harness-deploy/`，不会安装到
系统目录。Go 依赖通过 `goproxy.cn` 下载，保留 Go 的校验机制。

前端构建保留项目锁文件，并补充拷贝根目录依赖声明引用的移动端补丁文件；
没有引入移动端应用构建。构建依赖 curl、tar、Git、Docker 和 Linux x86_64。

## Native 与独立作业服务

23000 实例的 Native 守护进程由用户单元 `multica-daemon-harness.service` 管理，
二进制位于 `~/.local/lib/multica-harness/bin/multica`。宿主策略允许的作业服务源地址为
`http://127.0.0.1:23110`，五个智能体通过现有外部服务配置使用 `persistent_job`。
各自的 `JOBS_TOKEN` 通过平台环境变量 API 保存，令牌不进入任务或模型提示。
该接口带权限检查和审计；环境变量沿用现有 JSON 数据库存储，没有增加静态加密。

执行节点为现有训练容器 `172.16.66.79:10711`，工作目录为
`/workspace/Labeling-Training-Platform`。作业服务使用该容器既有的 root 执行身份，
namespace 只隔离 API 可见范围，不隔离文件权限。文件位置：

- 二进制：`/opt/multica-harness/bin/native-jobs`。
- 私有配置：`/etc/multica-harness/native-jobs.json`（0600）。
- 状态、日志及监督进程锁：`/var/lib/multica-harness-jobs/`（0700）。
- 每个 namespace：最多 4 个并发作业、256 条准入记录、每个作业 64 MiB 日志。

容器没有 systemd，使用[容器监督脚本](native-jobs-container-supervisor.py)，安装为
`/opt/multica-harness/bin/native-jobs-supervisor.py`。监督进程脱离 SSH 会话，
文件锁保证重复 `ensure` 不替换已有服务。宿主用户定时器每分钟执行一次 `ensure`，
用于在容器重新启动后恢复监督进程；需要容器保留上述文件。
服务异常退出后监督进程重启服务，但未结算作业标为 `interrupted`，不接管或重跑命令。
核验残留进程后再决定下一步，不能把进程监督理解为训练恢复。

服务在执行节点仅监听 `127.0.0.1:23110`。宿主用户服务建立同端口的私有 SSH 隧道，
没有对外发布作业端口。维护命令：

```bash
systemctl --user status multica-daemon-harness.service
systemctl --user status multica-jobs-tunnel-harness.service
systemctl --user status multica-jobs-ensure-harness.timer
systemctl --user show multica-jobs-ensure-harness.service -p Result -p ExecMainStatus
curl --noproxy '*' -fsS http://127.0.0.1:23110/healthz
```

API 或 Native 守护进程重启不会停止独立作业；停止隧道只会断开访问。
升级或停止作业服务前，先检查各 namespace 的运行作业。它自身的正常关闭会取消托管进程。
回退平台版本时保留服务和私有配置，先核验全部作业，再恢复旧智能体配置和环境变量。
完整接口、凭据规则和生命周期见[独立作业服务说明](../../docs/engineering/native-job-service.zh.md)。

独立作业服务的部署证据、数据库备份、原守护进程、原 `.env`、原智能体配置和环境变量保存在私有目录
`.multica/harness-deploy/rollout-nativereliability-20261109t235709z/`。
其中包含密钥，不要纳入 Git 或公开日志。

当前版本补齐 Native 的任务状态与父任务交接指引：推进工作时设为 `in_progress`，
交付后设为 `in_review`，并在父任务中引用证据、明确 @实际负责的小队或智能体。
`done` 仍由人工验收；`child_done` 仍是阶段关闭或全部子任务关闭后的屏障。
这些指引通过既有工具执行，没有增加自动验收或强制交付校验。
本次部署没有重放历史任务或补发通知。模型兼容性诊断仅使用合成输入，不执行业务工具。
运行时约束见 [Native 执行说明](../../server/internal/daemon/NATIVE_RUNTIME.md)。

当前版本的私有备份、构建日志、切换脚本及验证结果位于
`.multica/harness-deploy/rollout-modeltest-20261110t071835z/`。
当前运行时部署记录由 `.multica/harness-deploy/current-runtime-rollout` 指向；
独立作业服务原部署记录仍由 `current-reliability-rollout` 保留。

当前版本还提供以下能力：

- 智能体页面「设置 → 运行配置」可启用「使用远程执行环境」。现有五个智能体保持
  `local`，没有自动迁移；启用后文件、规则、命令、产物和交互终端共用执行节点工作目录。
  独立作业服务已允许文件写入，实际写入和命令执行仍受智能体与外部服务权限约束。
- 独立作业终态写入系统评论，并按权限及当前运行状态安排后续任务；活跃执行结束前等待。
  不自动设为 `done`，不隐式唤醒队长，不补发历史任务通知。
- 上下文预算利用模型实际用量，压缩时保留完整工具调用组及作业句柄、读取偏移。
- Linux 交互终端支持输入、中断和清理；不支持全屏应用或服务重启后的终端恢复。
  本节点未启用 bubblewrap，执行环境为 `unconfined`，没有新增资源配额。

工作区「设置 → 模型」现在可选择每个模型的 API 协议：`Chat Completions` 或 `Responses`。
`gpt-6.1-sol` 与 `gpt-6-luna` 已配置为 `Responses`，模型地址、密钥、智能体绑定和默认模型不变；
其他模型保留原协议。运行中的任务使用其原配置快照，协议修改从新任务生效。
旧客户端省略协议字段保存时会保留现有选择。

本轮通过两个 GPT 的真实接口验证合成工具调用及模拟结果续接，没有提交训练任务或执行真实业务工具。
上下文摘要使用同一模型协议；Responses 的终态、用量和加密推理回放均有回归测试。
原 `13000` 实例、独立作业服务及既有 namespace、令牌和作业记录保留。
网关本身仍可能返回 502/503，运行时不会因错误切换协议或重新执行工具。


## 部署验证

2026-11-07 密码账号版本验证结果：

- 前后端生产镜像构建成功，前端构建内置的 TypeScript 检查通过。
- 四个容器均为 `healthy`；最新迁移为 `588_user_email_unique_idx`，新增索引有效，
  `/readyz` 的数据库和迁移检查均为 `ok`。
- Chromium 的两项密码账号 E2E 通过：填写与不填写邮箱的注册、即时登录、
  大写用户名登录、错误密码提示及重试。
- 原账号 `admin` 密码登录通过，用户 UUID 和工作区成员身份一致，页面刷新及
  CSRF 会话续期正常；退出后无法读取账号，旧邮箱验证码接口返回 410。
- 登录、刷新和退出保持其他实例的 Cookie 不变；认证 Cookie 使用 `_harness_23000` 后缀。
- 测试账号清理后数据与部署前一致：1 个用户、1 个工作区、1 个成员、5 个智能体、
  5 个议题、22 条任务执行记录、2 个聊天会话。
- nginx 配置检查通过。仅重建本实例前后端，其余 57 个容器启动时间及重启次数未变；
  旧实例登录页、配置接口和后端健康检查均返回 200。
- 备份完整性及 `git diff --check` 通过。构建日志、浏览器结果、容器快照及备份校验值
  保存在 `.multica/harness-deploy/password-rollout/`，部署记录为 `release.json`。

改造代码的完整验证记录见[账号迁移说明](../../docs/engineering/password-accounts.zh.md#验证范围)。
此次部署验收没有调用真实模型或外部 AI CLI。

## 模型运行配置

在工作区打开 **设置 → 模型**。所有者和管理员可以添加多个兼容 OpenAI Chat Completions 或 Responses 的 API 配置，填写显示名称、API 基础地址、模型标识、可选密钥以及该模型实际支持的能力和限制。首个模型自动成为工作区默认模型。密钥使用本实例的 `MULTICA_VCS_SECRET_KEY` 加密，请保留此加密密钥及数据库备份。

模型列表及新增、编辑窗口提供「测试连接」按钮，按当前地址、模型、协议及密钥检查文本响应、工具调用和模拟结果续接，显示耗时与失败阶段。编辑时密钥留空会复用已保存的密钥；勾选移除密钥则测试无密钥连接。测试最长 60 秒，不自动重试，不保存配置或执行真实任务，会消耗少量 API 配额。图片输入、JSON 输出和上下文上限仍需按提供方文档确认。

连接测试与任务共用限额，用量以 `connection_test` 单独记录；迁移 `594_model_connection_test` 扩展其类型约束。回退镜像时保留该兼容迁移及用量记录，不恢复旧数据库覆盖新数据。

创建或编辑智能体时，可选择工作区中的启用模型，或选择跟随工作区默认模型。模型配置独立于执行电脑，同一个运行时上的不同智能体可以调用不同的模型和 API 地址。执行电脑仍负责文件与工具操作，API 服务负责模型调用。

任务首次读取模型时保存加密配置快照；之后对模型配置、默认模型或智能体模型的修改供后续任务使用。禁用或删除被智能体显式引用的模型前，先调整这些智能体；删除默认模型前，先选择另一个默认模型。

模型能力中的流式工具调用是执行前提。图片和 JSON 输出仅在提供方支持时启用。本实例未预填真实提供方地址或密钥，需在页面中添加实际配置后才能执行模型任务。

原有智能体的运行时迁移及新版守护进程切换结果记录在 `.multica/harness-deploy/models-rollout/`。该目录还保存本次更新前的数据库及环境备份、构建日志和浏览器验收结果。

### Native 执行能力

在智能体的「设置 → 运行配置」中选择文件写入、脚本执行和命名外部服务。
只有运行时所有者可授权这些能力，配置对下次运行生效；未配置的智能体默认只读且不能执行命令。

主机环境变量限定授权上限，修改后重启 `multica-daemon-harness.service`：

- `MULTICA_NATIVE_ALLOW_WRITE=true`：允许在前端为智能体开启工作目录文件写入，包含 `make_directory` 和 `apply_patch`。
- `MULTICA_NATIVE_ALLOW_SHELL=true`：允许在前端开启命令执行。命令具有主机用户权限、可访问网络及目录外文件，长命令以受管理作业运行，等待超时只返回作业 ID，不会终止命令；这不是操作系统沙箱。
- `MULTICA_NATIVE_HTTP_ORIGINS=https://cvat.example,http://annotation.internal:8080`：允许的外部服务源地址，以逗号分隔。未列出的源地址不可访问。

例如，CVAT 服务名称填 `cvat`、API 基础地址填 `https://cvat.example/api`、认证方式选 `Token`，
凭据环境变量名填 `CVAT_TOKEN`，再在该智能体的「环境变量」中保存实际 token。
不要把 token 放进运行配置、URL 或智能体指令。
服务默认仅允许 GET，开启修改后可使用 POST、PUT、PATCH、DELETE。
`http_request` 仅接受相对 API 路径，不跟随重定向、不自动重试，请求限时 30 秒，响应上限 64 KiB。
大结果应按服务 API 分页获取。外部写入结果不确定时保留明确的未知结果，智能体可继续核验服务端状态，不能盲目重复写入。取消导致的中断会结束本次执行，后续任务仍可核验原调用。

`native_runtime_info` 可供智能体检查本次实际权限、可用服务及应回复的评论。
压缩失败会保留完整历史，并返回可区分的错误代码；已有空白摘要在下次运行时从原始历史恢复。


Native 命令现在参考 DeepSeek Harness 的 shell/jobs 机制：

- `shell` 支持 `run_in_background` 和 `wait_ms`。默认等待 1 秒，最多等待 30 秒；等待期满后原命令继续运行，不重新执行。
- `job_output` 按短作业 ID 查询状态、退出码及日志，用返回的 `next_offset` 继续读取；同一偏移量可重复读取。每次最多 8 KiB；运行时内存保留最近 256 KiB，落盘最多 8 MiB。用 `job_search` 检索早期错误；达到磁盘配额后明确提示缺失，保留有限尾部。
- `job_list` 列出当前会话作业；`job_kill` 停止本地进程树。每个会话最多同时运行 4 个作业、保留 128 条作业记录。
- 本地作业归当前任务执行所有，任务结束、取消或正常关闭守护进程时停止。需要跨任务持续运行的训练应交给远程持久作业管理器，并保留其远程 ID；终止 SSH 不代表远端训练已停止。
- 作业完成通知写入当前执行的模型历史，减少无效轮询；已经读取终态的作业不重复通知。`read_file` 支持按字节偏移分页读取大文本。
- 独立 `native-jobs` 服务可通过名为 `multica_jobs` 的外部服务配置接入，提供 `persistent_job` 的提交、查询、日志检索和取消能力。它的作业独立于 Native 任务和守护进程生命周期。配置、权限与重启边界见[独立作业服务说明](../../docs/engineering/native-job-service.zh.md)，systemd 模板为 [native-jobs.service](native-jobs.service)。
- 作业凭据保存在工作区外的 `.native-results/<workspace>/<agent>/shell-jobs` 私有目录。会话与工具调用共同确定启动身份，重复接收同一调用只返回原作业；重启后未完成记录标记为 `interrupted`，核对远程结果后再决定后续操作。
- 已开始的 shell 调用可从既有作业凭据恢复工具结果；平台业务写入优先读取服务端回执。仍无法确认的历史调用以 `TOOL_OUTCOME_UNKNOWN` 返回模型，允许继续核验外部状态，同组未开始的调用标记为 `TOOL_NOT_STARTED`；旧调用不会自动重放。

### 旧 shell 操作结果核对

`native_tool_outcome_unknown` 表示此前操作结果尚未确认。新版运行时可以闭合工具组并继续核验，但不会把历史未知结果改写成成功或失败。
对于仍未闭合的旧检查点，也可先核对远端进程、日志和产物，确认实际结果，再由工作区所有者或管理员使用容器内 `native-reconcile`：

```sh
deploy/harness/compose.sh exec -T backend ./native-reconcile \
  --session-id <session-uuid> --operator-id <manager-user-uuid> \
  --call-id <shell-call-id> --expected-version <version> \
  --outcome failed --evidence-file /tmp/native-evidence.txt
```

默认只检查输入。确认后增加 `--apply --backup-file /tmp/native-checkpoint-before.json`。
证据文件应记录核验时间、远端作业及日志位置和实际结果，不得包含密钥。
执行时会备份旧检查点并验证版本、管理员成员身份及会话空闲状态；仅记录该 shell 调用的核验结果，不执行命令，也不重新排队任务。
完成后应将证据和私有备份移入部署审计目录。该工具只处理仍未闭合的旧 shell 调用；已经进入模型历史的未知事实不回写，后续核验结果应通过新的观察记录保存。
