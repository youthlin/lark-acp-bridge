# AGENTS

## 项目

`lark-acp-bridge` 是一个 Go 服务，用于把飞书 / Lark Bot 对话桥接到兼容 ACP 的 agent server。

## 约定

- 第一版实现保持小而清晰，优先使用 Go 标准库。
- 不要在桥接层真正实现前声明 ACP client capability。
- 将 `traex acp serve` 视为 stdio 子进程，不要当成 HTTP server。
- 保持飞书适配、桥接 session 管理、ACP runtime 的包边界清晰。
- 话题群优先采用一条飞书话题对应一个 ACP session；普通群和私聊按整个 chat 复用同一个 ACP session，只有用户显式发送 `/new` 才重开。`/new [cwd] [title]` 可指定新会话标题，普通文本自动创建会话时用首条消息截断生成标题；`/session title <title>` 可修改当前标题；`/session list` 列出当前聊天的历史 ACP session，`/session resume <index>` 把列表中的历史项恢复为当前会话。
- `/new` 触发重开时，如果旧会话存在尚未执行的 wiki 反思定时器，应把该反思用独立 wiki runtime key 放到后台执行，不能同步等待 wiki 完成后才创建或处理新会话；这个后台 wiki 轮次属于上一轮会话收尾，不应被新会话里的后续普通消息取消，但仍要纳入 service/runtime 生命周期，可由 `/wiki off`、会话关闭或服务退出取消并关闭对应 ACP client。
- 配置文件默认持久化到 `~/.lark-acp-bridge/config.json`；飞书机器人使用 `bots` 数组配置，每个 bot 必须有独立 `id`，展开后的 `workspace` 绝对路径也必须唯一，用于存放该 bot 的 `SOUL.md`、`MEMORY.md` 等持久化内容。
- 服务启动前必须校验 `agent_list[].command` 可执行；普通命令走 `PATH` 查找，路径命令检查文件存在且有执行权限，避免 ACP server 命令拼错后到 `/new` 才失败。
- 多个 agent 通过配置里的有序 `agent_list` 声明，并通过 chat 维度的 `/agent <name>` 切换；选择结果持久化在 bot workspace 的 `sessions.json`，后续 `/new` 和无会话普通文本自动创建会话时使用当前聊天默认 agent。
- session、缓存等 bot 相关持久化数据优先放在对应 bot 的 `workspace` 下；示例配置不要写入个人机器的真实绝对路径。
- 普通文本在当前会话没有 ACP session 时应自动创建；`/new [cwd]` 只作为手动重开或指定 cwd 的入口。workspace 首次设置通过新会话的第一条 `session/prompt` 交给 ACP agent 完成；bridge 层只负责创建模板文件、注入 ready workspace 内容和提供受限 workspace 文件读写能力，不实现多轮 onboarding 状态机。
- 配置中的用户目录路径使用 `~` 或 `$HOME` 表达，运行时由配置加载层展开。
- `go run ./cmd/lark-acp-bridge` 默认按后台 restart 运行；前台运行需显式使用 `go run ./cmd/lark-acp-bridge run`，适合 systemd。后台 pid/log 文件放在 config.json 同级目录。
- 本仓库代码注释、文档、日志、commit message 均使用中文。

## 命令

```bash
go test ./...
go run ./cmd/lark-acp-bridge --help
go run ./cmd/lark-acp-bridge --version
```

