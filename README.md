# Lark ACP Bridge

Lark ACP Bridge 用于把飞书 / Lark Bot 对话桥接到兼容 ACP 的编码智能体。

它接收飞书 IM 事件，把飞书会话映射到 ACP session，将用户消息转发给 `traex acp serve` 这类 ACP server，再把智能体输出回传到飞书。

## 目标

- Bot 后端作为 ACP client，而不是让飞书直接连接 ACP server。
- 第一版优先支持 `traex acp serve`，同时保留接入其他 ACP server 的配置空间。
- 话题群默认一条飞书话题对应一个 ACP session；普通群和私聊默认整个 chat 对应一个 ACP session，除非用户发送 `/new` 重开。
- 将智能体消息、计划、工具调用和权限请求逐步映射回飞书消息或交互卡片。
- 能力声明保持保守；当前仅在 bot 配置了 workspace 时声明受限的文本文件读写能力，用于维护 workspace 记忆、知识和技能文件，终端能力暂不声明。

## 初版架构

```text
飞书 / Lark IM
  -> Feishu Adapter
  -> Session Manager
  -> ACP Client Runtime
  -> ACP Server Registry
  -> traex acp serve / other ACP servers
```

## MVP 范围

1. 接收飞书文本消息。
2. 按飞书会话创建或恢复桥接 session。
3. 通过 stdio 启动 `traex acp serve`。
4. 执行 ACP `initialize`、`session/new` 和 `session/prompt`。
5. 将 ACP agent 文本、工具调用状态和最终回复转发到飞书。
6. 向本机 TraeX 子进程提供 session `cwd` 和 bot workspace 上下文，由 TraeX 使用自身本地工具维护长期记忆、知识和技能文件；bridge 默认不声明 ACP client-side `fs` / `terminal` 能力。
7. 后续再补更完整的工具调用展示和远程/虚拟 workspace 场景。

## 配置

```bash
mkdir -p ~/.lark-acp-bridge
cp config.example.json ~/.lark-acp-bridge/config.json
$EDITOR ~/.lark-acp-bridge/config.json
```

默认配置文件路径为：

```text
~/.lark-acp-bridge/config.json
```

启动时会读取该文件；如果文件不存在，会自动创建一份默认配置并提示填写 `bots[].app_id`、`bots[].app_secret` 和 `bots[].workspace`。可以访问飞书开放平台入口创建飞书智能体：

```text
https://open.larkoffice.com/page/launcher
```

`bots` 是数组，一个配置项对应一个飞书智能体：

```json
{
  "bots": [
    {
      "id": "default",
      "app_id": "cli_xxx",
      "app_secret": "secret",
      "workspace": "$HOME/.lark-acp-bridge/bots/default",
      "bot_open_id": "ou_bot",
      "owner_open_ids": ["ou_xxx"]
    }
  ]
}
```

`workspace` 是该 bot 的持久化工作目录，用于存放 L0 记忆、L1 知识和 L2 技能；启动时会自动创建目录。`bot_open_id` 是这个 bot 自己的 open_id，用于群聊中确认 `@Bot` 是否真的提及当前 bot；为空时，bridge 启动时会尝试通过飞书机器人信息接口自动读取，读取失败时需要手动配置，否则群聊默认需要 at 的过滤无法可靠识别提及。`owner_open_ids` 是这个 bot 的 owner 白名单，用于审批 ACP agent 发起的权限卡片。为空时，bridge 启动时会尝试通过飞书应用接口自动读取应用所有者、创建者和协作者中的管理员/开发者作为 owner；如果接口权限不足或未解析到 owner，权限卡片不会允许任何人批准。多个 bot 可以共享同一套 ACP agent 配置，但必须使用不同的 `id`，并且展开后的 `workspace` 绝对路径也必须不同。

自动读取 owner 需要飞书应用具备查询本应用信息和协作者的权限，例如 `application:application:self_manage` 或等价的应用管理只读权限。也可以直接在 `owner_open_ids` 中手动配置允许审批人的 open_id，配置后启动时不会再查询飞书应用协作者。

第一次和 bot 对话时，如果 workspace 尚未标记为 ready，服务会创建基础知识文件：

```text
$BOT_WORKSPACE/SOUL.md
$BOT_WORKSPACE/MEMORY.md
$BOT_WORKSPACE/AGENTS.md
$BOT_WORKSPACE/TOOLS.md
$BOT_WORKSPACE/knowledge/AGENTS.md
$BOT_WORKSPACE/knowledge/core.md
$BOT_WORKSPACE/knowledge/index.md
$BOT_WORKSPACE/knowledge/log.md
$BOT_WORKSPACE/knowledge/lint.md
$BOT_WORKSPACE/skills/AGENTS.md
$BOT_WORKSPACE/skills/core.md
$BOT_WORKSPACE/skills/wiki/SKILL.md
```

三层含义：

- L0 根目录记忆：`SOUL.md`、`MEMORY.md`、`AGENTS.md`、`TOOLS.md`，记录 bot 身份、用户偏好、工作规则和工具环境。
- L1 `knowledge/`：记录领域知识、项目经验、问题解决方案；`core.md` 是知识入口，`index.md` 是全量索引，`log.md` 是追加式变更日志。
- L2 `skills/`：记录稳定、可复用的多步骤流程；每个技能使用 `<skill-name>/SKILL.md`。

普通文本会自动创建 ACP 会话；群聊默认需要 at bot 才响应，可用 `@Bot /at off` 为当前 chat 改成免 at，私聊始终响应且不支持 `/at` 配置。`/new [cwd] [title]` 仍可用于手动重开当前会话、指定 cwd 或指定标题。话题群按话题区分会话，普通群和私聊按整个 chat 复用同一会话。`/new` 未指定标题时会按当前聊天历史生成 `session#N`；它只回复会话创建结果和当前 mode/model，不额外发送 prompt。下一条普通文本会携带 workspace 上下文一起作为 `session/prompt` 发给 ACP agent。

初始化说明会要求 ACP agent 一次性询问用户：

- 用户想叫它什么名字。
- 它应采用什么性格、语气和边界。
- 需要长期记住的用户信息、偏好或常用上下文。
- 是否有需要沉淀到知识库的领域知识、项目经验或常用流程。

首次生成 workspace 时，bridge 会额外创建 `Bootstrap.md`，其中写明一次性初始化引导提示词。每条普通 prompt 都会拼接当前 workspace 的引导、长期记忆、知识入口和技能入口；只要 `Bootstrap.md` 还存在，它就会作为普通 workspace 文件被注入给 ACP agent，让 agent 先向用户提问并完成初始化。

用户回答后，由 ACP agent 使用 TraeX 可用的本地文件工具写入 L0/L1/L2 相关文件，然后删除 `Bootstrap.md`。`Bootstrap.md` 不存在后，后续 prompt 自然只会注入根目录记忆、`knowledge/` 入口、wiki skill 和记忆策略。若已经手动写好了文件，也可以在本地直接删除 `Bootstrap.md`。

每条普通 prompt 还会注入 workspace 记忆策略：用户要求“记住”、沉淀经验或总结可复用流程时，ACP agent 应先读取相关 workspace 文件，再用可用的本地文件工具合并写回。新增、删除或重命名知识/技能文件后必须同步 `knowledge/index.md`，并在 `knowledge/log.md` 末尾追加 `[YYYY-MM-DD] 操作 文件 摘要`。

自动知识沉淀默认开启。每次 bot 完整回复普通消息后，bridge 会为当前 ACP session 启动一个分钟级定时器；默认 5 分钟后向同一个 ACP session 发送内部 wiki 反思 prompt，要求 agent 读取 `skills/wiki/SKILL.md` 并按规范更新 L0/L1/L2 文件。该反思轮次是 internal/silent，不创建飞书卡片，也不把 agent 输出转发给用户；prompt 中仍要求如果必须输出文本只输出 `NoReply` 作为兜底。等待期间如果同一会话有新消息进入，会取消旧定时器并在新消息完成后重新计时。

bridge 不在本地实现多轮 onboarding 状态机，也不做旧文件名迁移；本地开发阶段只使用 `SOUL.md`、`MEMORY.md`、`AGENTS.md`、`TOOLS.md` 这些大写文件名作为 L0 记忆入口。

会话映射会持久化到每个 bot 的 workspace 下：

```text
$BOT_WORKSPACE/sessions.json
```

第一版使用 JSON 文件保存 `bot_id + chat_id + thread_id -> ACP session` 映射；话题群会话使用当前话题的 `thread_id`，普通群和私聊会话的 `thread_id` 为空，表示整个 chat 共用一个 ACP session。重启后不会丢失当前会话的 `agent`、`cwd` 和 `acp_session_id`；暂不需要 SQLite。`sessions.json` 还会保留同一聊天里的历史 ACP session，用于 `/session list` 和 `/session resume <index>`。把 session 放在 workspace 下，可以让每个 bot 的记忆、会话映射和后续缓存一起迁移、备份和清理。服务进程内会为活跃飞书会话维护对应的 `traex acp serve` 子进程；重启后普通消息会按已保存的 `acp_session_id` 尝试 `session/load` 恢复。

配置中的路径支持 `~` 和 `$HOME` 展开，例如：

```json
{
  "agents": {
    "traex": {
      "default_cwd": "$HOME"
    }
  }
}
```

启动服务前会校验每个 `agents.<name>.command` 是否可执行：普通命令通过 `PATH` 查找，带 `/` 或 `\` 的路径命令会检查文件存在且有执行权限。命令配置错误时会直接退出，避免到用户发送 `/new` 时才发现 ACP server 启动失败。

## 开发

```bash
go test ./...
go run ./cmd/lark-acp-bridge --version
go run ./cmd/lark-acp-bridge run
```

也可以临时指定配置文件：

```bash
go run ./cmd/lark-acp-bridge --config ./config.example.json
```

## 运行

直接运行不带子命令时，默认按后台 `restart` 方式运行：如果已有后台进程，会先停止再启动新的后台进程。

```bash
go run ./cmd/lark-acp-bridge
```

后台运行文件和 `config.json` 放在同一目录：

```text
~/.lark-acp-bridge/lark-acp-bridge.pid
~/.lark-acp-bridge/lark-acp-bridge.log
```

可用子命令：

```bash
go run ./cmd/lark-acp-bridge run      # 前台运行，适合 systemd 等进程管理器
go run ./cmd/lark-acp-bridge start    # 后台启动
go run ./cmd/lark-acp-bridge stop     # 停止后台进程
go run ./cmd/lark-acp-bridge restart  # 重启后台进程
```

Feishu Adapter 使用官方 Go SDK：

```text
github.com/larksuite/oapi-sdk-go/v3
```

如果任一 `bots[]` 缺少 `app_id`、`app_secret` 或 `workspace`，服务会退出并提示配置路径和飞书智能体创建入口；填好凭证后再重新启动。

当前飞书侧已支持：

- `/help`：查看命令。
- `/status`：查看服务和当前会话映射。
- `/session list`：列出当前聊天里的历史 ACP 会话，序号从 1 开始，`*` 表示当前会话。
- `/session resume <index>`：把 `/session list` 中第 `index` 项恢复为当前会话；普通群和私聊恢复到当前 chat，话题群恢复到当前话题。
- `/session title <title>`：设置当前 ACP 会话标题，便于 `/session list` 区分。
- `/wiki on`、`/wiki off`、`/wiki status`、`/wiki interval <duration>`：管理当前会话的自动知识沉淀。`duration` 支持 `5m`、`30s`，纯数字按分钟理解。
- `/cmds`：查看当前 ACP server 上报的 slash commands。
- `/cmds /command [args]`：把 ACP slash command 原样发送到当前 ACP session，通过 `session/prompt` 执行。
- `//command [args]`：`/cmds /command [args]` 的简写，用于避免 bridge 本地命令拦截。
- `/model`：打开飞书模型选择卡片，通过下拉列表设置当前会话模型。
- `/model <model>`：通过 ACP `session/set_config_option` 设置当前会话模型。
- `/mode`：打开飞书模式选择卡片，通过下拉列表设置当前会话模式。
- `/mode <mode>`：通过 ACP `session/set_config_option` 设置当前会话模式。
- `/at status|on|off`：查看或设置当前群聊是否需要 at bot 才响应。群聊默认需要 at，因此默认状态下需使用 `@Bot /at off` 改为免 at；这里的 `@Bot` 必须提及当前 bot 的 open_id，随便 at 其他用户或其他 bot 不会触发。免 at 后可用 `/at on` 或 `@Bot /at on` 恢复为需要 at。私聊不支持该命令，at 或不 at 都会响应。
- `/new [cwd] [title]`：为当前飞书会话手动创建或重开 `traex acp serve` 会话，执行 `initialize` 和 `session/new`，并持久化会话映射。传入 `cwd` 时必须是可访问的绝对目录，也可以使用 `~/path`；不传时优先沿用当前会话已有的 `cwd`，首次创建则使用配置里的 `default_cwd`。`cwd` 后面的文本会作为标题；也可以使用 `/new --title 标题` 或 `/new 标题`。未指定标题时默认使用 `session#N`。回复会短暂等待 `session/update`，并展示当前 mode 和 model；ACP server 未上报时显示未知。
- 普通文本：发送到当前会话的 ACP session，执行 `session/prompt`；执行过程中会创建一张飞书流式卡片，持续更新 agent 文本和工具调用状态，最终文本如果已经写入卡片则不再重复发送普通文本。当前会话没有 session 时会自动使用默认 `default_cwd` 创建，会话创建失败或未配置默认 cwd 时再提示用户用 `/new <cwd>` 指定。话题群卡片会进入当前话题；普通群和私聊回复引用原消息但不强制进入话题模式。
- 权限卡片：权限选项来自 ACP agent，正文完整展示选项内容，按钮使用短编号文本避免截断。只有 bot owner 可以点击权限卡片；owner 优先来自 `bots[].owner_open_ids`，未配置时启动阶段会尝试从飞书应用所有者、创建者和管理员/开发者协作者自动解析。未解析到 owner 时，无论群聊还是私聊，权限卡片都不能被任何人批准。请求被取消时，bridge 会把卡片更新为已取消/已失效状态并移除按钮。

同一会话里新消息优先级最高。用户重新发送要求、查看状态或重新 `/new` 时，bridge 会取消当前会话正在执行的上一轮 ACP prompt 或 wiki 反思任务，再处理新消息。取消是按 ACP session scope 隔离的，不会影响其他群聊、私聊或其他话题。

