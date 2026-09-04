# AGENTS

## 项目

`lark-acp-bridge` 是一个 Go 服务，用于把飞书 / Lark Bot 对话桥接到兼容 ACP 的 agent server。

它接收飞书 IM 事件（长连接），按飞书会话映射到 ACP session，将用户消息通过 stdio 子进程转发给 `traex acp serve` 等 ACP server，再把智能体文本、工具调用状态、计划和权限请求映射回飞书消息或交互卡片。

- Go 版本：`go 1.24.1`（CI 使用 `1.24.10`）。
- 核心依赖：`github.com/larksuite/oapi-sdk-go/v3`；桥接与 ACP 协议优先使用标准库，避免引入不必要依赖。
- 入口：`cmd/lark-acp-bridge`；内部代码位于 `internal/`，不对外暴露稳定 API。

## 包结构

- `cmd/lark-acp-bridge`：CLI 入口、默认后台 daemon（unix）、`bots`/`service` 子命令、版本号、前台运行、实例锁与 pid/log 管理。
- `internal/config`：配置加载、路径展开、bot/agent 校验、加密 file secret、`restart_command` 与默认配置创建。
- `internal/feishu`：飞书长连接适配、事件解析与去重、消息/卡片渲染、权限/会话/模式/模型卡片、群聊信息缓存、云文档评论事件和会议机器人 API。
- `internal/bridge`：核心桥接层，负责 session 映射与生命周期、ACP runtime、prompt 队列与流式更新、斜杠命令、定时/循环/wiki/会议任务、token 用量、workspace 模板与持久化。
- `internal/acp`：基于 stdio JSON-RPC 的 ACP client，包含进程管理、initialize、session/new/prompt/resume/close、agent 能力与配置项、权限请求处理。
- `internal/arg`：零拷贝的 JSON 参数封装工具。
- `internal/logging`：带 context 的 `slog` handler、日志级别与 debug 开关。
- `internal/update`：GitHub Release 自更新：查询最新版本（API + 重定向回退）、跨平台资产名匹配、sha256 校验、tar.gz 解压、原子替换二进制。

保持飞书适配、桥接 session 管理、ACP runtime 的包边界清晰；`internal/feishu` 不直接依赖 ACP 细节，`internal/acp` 不感知飞书。

## ACP 约定

- ACP server 一律作为 stdio 子进程启动（如 `traex acp serve ...`），不要当成 HTTP server。
- `initialize` 时声明 `protocolVersion: 1`，并保守声明 client capabilities；当前不声明 ACP client-side `fs` / `terminal` 能力，让本地 agent 在 session cwd 内使用自身工具。只有 bridge 真正拥有远程、虚拟或权限受控的 workspace surface 时才新增对应能力。
- 按 ACP server 上报的能力决定是否使用 `session/resume`、`session/load`、`session/close`、`session/delete`、`available_commands_update`、配置项、模式/模型和 auth 等能力。
- 不要在桥接层真正实现前声明或承诺任何 ACP client capability。

## 会话与消息

- 话题群：一条飞书话题（`chat_id` + `thread_id`）对应一个 ACP session。
- 普通群和私聊：整个 chat（`chat_id`，`sub_id` 为空）复用同一个 ACP session，只有显式发送 `/new` 才重开。
- 普通文本在当前会话没有 ACP session 时自动创建；`/new [cwd] [title]` 仅作为手动重开或指定 cwd/标题的入口。
- 自动创建会话时用首条消息截断生成标题；`/new` 未指定标题时使用 `session#N`。`/session title <title>` 修改当前标题，`/session list` 列出当前聊天历史 session，`/session resume <index>` 恢复历史项。
- 多个 agent 通过配置里的有序 `agent_list` 声明，通过 chat 维度的 `/agent <name>` 切换；选择持久化在 bot workspace 的 `.local/sessions.json`，后续 `/new` 和自动创建使用当前聊天默认 agent。若当前会话 agent 与聊天默认 agent 不一致，普通文本会基于当前 cwd 自动创建新 agent 的 session。
- 同一会话中新普通消息优先级最高：新消息会取消上一轮正在执行的用户 prompt，再处理新消息；自动 wiki 使用独立伴生会话，不受原会话新消息影响。
- 群聊默认需要 at 当前 bot 才响应；`/at off`、`/at off auto`、`/at off auto-reaction` 可改为免 at，`/at on` 恢复。私聊始终响应，不支持 `/at`。

斜杠命令仅限 bot owner 执行（sender open_id 命中 `owner_open_ids` 或启动时解析到的应用所有者/管理员）。完整命令以 `internal/bridge/service_commands.go` 的 `slashRoutedCommandTable` 为准，当前包含：`/help`、`/new`、`/agent`、`/session`、`/wiki`、`/meeting`、`/drive_comment`、`/loop`、`/queue`、`/rules`、`/schedule`、`/card`、`/cmds`、`//command`、`/compact`、`/config`、`/model`、`/mode`、`/usage`、`/show`、`/at`、`/debug`、`/update`、`/restart`、`/status`。`/rules` 管理当前 chat 的补充规则，并在 workspace 规则注入时一并传给 ACP agent。`/meeting` 管理新会议邀请和会议 trace；关闭时不打断已加入会议。`/update` 只替换二进制，不自动重启，完成后需再用 `/restart`。

## Workspace 与知识沉淀

- 每个 bot 的 `workspace` 是持久化目录，用于存放 `SOUL.md`、`MEMORY.md`、`AGENTS.md`、`TOOLS.md`、`knowledge/`、`skills/` 等适合 git 管理的 L0/L1/L2 内容；本地运行态统一放到 `.local/`，包括 `sessions.json`、`scheduled_tasks.json`、`restart_ack.json`、`token_usage.json`、`processed_messages.json`、`meetings.json` 和图片 `cache/`。服务启动和 workspace 初始化时会确保 `.gitignore` 包含 `.local/`，并把旧根目录运行态一次性移动进 `.local/`，但如果目标已存在则不覆盖旧副本。
- 首次对话且 workspace 未 ready 时，bridge 创建基础模板文件和 `Bootstrap.md`；第一条 `session/prompt` 把 ready workspace 内容注入给 ACP agent，由 agent 完成一次性初始化询问并写入文件后删除 `Bootstrap.md`。bridge 不实现多轮 onboarding 状态机，也不做旧记忆文件名迁移。
- bridge 只负责创建模板、注入 workspace 内容和提供受限 workspace 文件读写能力；长期记忆、知识和技能文件由 ACP agent 用自身本地工具维护。新增/删除/重命名知识或技能文件需同步 `knowledge/index.md` 并追加 `knowledge/log.md`。
- 自动知识沉淀（wiki）默认开启。普通消息的完整终止记录落入 `.local/traces/<acp_session_id>.jsonl` 后，workspace 级 coordinator 默认等待 5 分钟，再由独立 `wiki-companion` ACP session 按冻结的 `seq` 区间增量读取 trace 并维护知识。原用户 session 不接收 wiki prompt；新消息与 `/new` 不取消、不搬运 companion job。成功或 `NoReply` 后才推进 `.local/wiki/state.json` 的 cursor，失败保持原 cursor 并有限重试；同一 workspace 的自动 wiki 和 `/wiki lint` 串行写入。
- trace 的 `seq` 在单文件内严格递增，`turn_result`/`error` 是完整轮次边界；压缩必须保留 `user`、final assistant 和终止记录，且未消费 trace 不得压缩或按 retention 删除。companion 自身 trace 使用 `source=wiki`，禁止再次作为知识沉淀来源。bot 级 `wiki_trace` 默认关闭；开启后把 companion 执行过程旁路到指定群的流式卡片。
- bot 级 `meeting` 默认关闭。启用后机器人静音入会，按飞书会议事件批量维护结构化纪要和明确 TODO，并持续更新唯一接收人的私聊卡片；会议状态写入 `.local/meetings.json`，重启后补拉事件恢复。结束后等待 15 秒并最终补拉，完成后保留 10 分钟迟到事件窗口；随后压缩事件明细，历史最多保留 90 天且不超过 100 条。会议 ACP prompt 必须拒绝工具权限；`meeting.trace_enabled` 独立控制 trace。

## 配置

- 默认配置文件：`~/.lark-acp-bridge/config.json`；后台 pid/log 和按配置区分的实例锁文件放在其同级目录。
- `bots` 为数组，每个 bot 必须有唯一 `id`，展开后的 `workspace` 绝对路径也必须唯一。
- `app_secret` 使用 file 引用，secret 文件必须是加密格式（`lark-acp-bridge-secret:v1:` 前缀）并配套 `.key`，两者权限 `600`；启动时只支持加密 file secret。优先用 `bots register` / `bots add` 维护，不要在配置或日志中出现明文 secret。
- `agent_list[].command` 启动前必须校验可执行：普通命令走 `PATH`（`exec.LookPath`），含路径分隔符的命令检查文件存在且有执行权限；`default_cwd` 若配置必须是可访问目录。命令不存在的 agent 会被跳过并告警，全部不可用则启动失败。
- 路径中的 `~` / `$HOME` 由配置加载层展开；示例配置不要写入个人机器真实绝对路径。
- 可选 `restart_command` 覆盖 `/restart` 行为；前台/systemd 等受管模式必须配置，避免误拉后台实例。
- Unix 和 Windows 上，所有真正运行 Bridge 的入口都必须在启动 `bridge.Service` 前获取按配置文件路径区分的进程级独占锁；同一配置只允许运行一个实例，不同配置可通过 `run` 或各自的进程管理器并行。Unix 使用 `flock`，锁文件保留且内容仅用于记录 PID，不能依赖文件是否存在判断运行状态，也不要在释放锁时删除文件。其他平台保持前台运行能力，不承诺跨进程单实例。内置 daemon 的 pid/log 继续使用配置目录下的旧版固定名称以兼容升级，不支持同目录多配置并行管理。

## 运行与构建

```bash
# 测试（CI 同样执行）
go test ./...

# 默认后台 restart（unix）；pid/log 在配置文件同级
go run ./cmd/lark-acp-bridge
go run ./cmd/lark-acp-bridge start
go run ./cmd/lark-acp-bridge stop
go run ./cmd/lark-acp-bridge restart

# 前台运行，适合 systemd / launchd
go run ./cmd/lark-acp-bridge run

# 版本与帮助
go run ./cmd/lark-acp-bridge --version
go run ./cmd/lark-acp-bridge --help

# bot 管理（支持省略 bots 的简写）
go run ./cmd/lark-acp-bridge bots list
go run ./cmd/lark-acp-bridge bots register default
go run ./cmd/lark-acp-bridge bots add default cli_xxx --stdin-secret
go run ./cmd/lark-acp-bridge bots create-lark-cli-profile default
go run ./cmd/lark-acp-bridge bots remove default

# 安装/卸载系统服务（systemd 或 launchd）
go run ./cmd/lark-acp-bridge service install
go run ./cmd/lark-acp-bridge service uninstall

# 自更新（--check 只检查不替换）
go run ./cmd/lark-acp-bridge update --check
go run ./cmd/lark-acp-bridge update
```

发布构建使用 `CGO_ENABLED=0`、`-trimpath -ldflags "-s -w -X main.version=<version>"`，产物为 `lark-acp-bridge`（Windows 加 `.exe`）。
发布产物命名固定为 `lark-acp-bridge_<version>_<goos>_<goarch>.tar.gz`（Windows 包内二进制为 `.exe`），配套同名 `.sha256`；`internal/update` 依赖该命名约定，改动 release.yml 产物名需同步更新。Gitee 自带镜像同步代码与 tag 到 GitHub；`.github/workflows/gitee.yml` 在 GitHub Release 产出后把附件回传到同名 Gitee Release（需 GitHub Secrets `GITEE_TOKEN`）。`internal/update` 的版本发现和下载都以 GitHub 为主、Gitee 为回退，新增下载源时保持这个回退链。

## 风格与约定

- 本仓库代码注释、文档、日志、commit message 均使用中文。
- 日志使用 `log/slog`，通过 `internal/logging` 的 context handler 输出结构化 JSON；需要上下文时用 `slog.*Context`。
- `context.Context` 只用于取消、超时、trace/logging 等请求生命周期信息；不要用 `context.WithValue` 隐式传递业务数据、回调函数或展示配置。需要跨层传递时优先定义显式参数、options 结构体或小接口。
- 错误信息面向用户/运维，保持中文、具体、可操作；不要打印 secret。
- 改动保持小而聚焦，遵循既有包边界和命名；不要为了局部便利跨层耦合。
- 优先补充或更新相邻的 `_test.go`；bug 修复尽量加回归测试。不要提交 `config.json`、`*.appsecret`、`*.key`、`.local/` 等本地运行产物。
