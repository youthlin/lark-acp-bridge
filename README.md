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
3. 通过 stdio 启动 `traex acp serve` 或其他 ACP server。
4. 执行 ACP `initialize`、`session/new` 和 `session/prompt`。
5. 将 ACP agent 文本、工具调用状态和最终回复转发到飞书。
6. 向本机 ACP agent 子进程提供 session `cwd` 和 bot workspace 上下文，由 agent 使用自身本地工具维护长期记忆、知识和技能文件；bridge 默认不声明 ACP client-side `fs` / `terminal` 能力。
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
      "app_secret": {
        "source": "file",
        "path": "$HOME/.lark-acp-bridge/secrets/default.appsecret"
      },
      "workspace": "$HOME/.lark-acp-bridge/bots/default",
      "bot_open_id": "ou_bot",
      "owner_open_ids": ["ou_xxx"]
    }
  ]
}
```

推荐用命令行维护 bot 配置和 secret 文件，避免手动在多个文件之间同步：

```bash
printf '%s\n' '<app_secret>' | lark-acp-bridge bots add default cli_xxx --stdin-secret
lark-acp-bridge bots list
lark-acp-bridge bots migrate-secret default
lark-acp-bridge bots remove default
```

也支持省略 `bots` 的简写，例如 `lark-acp-bridge add default cli_xxx --stdin-secret`。`bots add` 会生成两个文件：`~/.lark-acp-bridge/secrets/<id>.appsecret` 保存加密后的 secret，`~/.lark-acp-bridge/secrets/<id>.key` 保存随机生成的解密密钥，两个文件权限均为 `600`；`config.json` 中只写入 `app_secret` 的 file 引用，不写明文 secret。已有明文配置可用 `bots migrate-secret <id>` 迁移，它会保留该 bot 的其他字段，只把 `app_secret` 改写为加密 file 引用。启动时如果发现 `.key` 文件，会解密 `.appsecret`；没有 `.key` 时仍兼容读取旧的明文 file secret。这个设计用于避免模型直接读取 `<id>.appsecret` 时看到明文，但如果同一主体同时拥有 `.appsecret` 和 `.key` 的读取权限，仍可解密。也可以通过 `--workspace`、`--secret-file`、`--bot-open-id` 和 `--owner-open-ids` 指定更多字段。兼容旧配置中直接使用字符串形式的 `app_secret`，但不推荐在长期运行环境中保存明文 secret。

`workspace` 是该 bot 的持久化工作目录，用于存放 L0 记忆、L1 知识和 L2 技能；启动时会自动创建目录。`bot_open_id` 是这个 bot 自己的 open_id，用于群聊中确认 `@Bot` 是否真的提及当前 bot；为空时，bridge 启动时会尝试通过飞书机器人信息接口自动读取，读取失败时需要手动配置，否则群聊默认需要 at 的过滤无法可靠识别提及。`owner_open_ids` 是这个 bot 的 owner 白名单，用于审批 ACP agent 发起的权限卡片和执行 `/restart`。为空时，bridge 启动时会尝试通过飞书应用接口自动读取应用所有者、创建者和协作者中的管理员/开发者作为 owner；如果接口权限不足或未解析到 owner，权限卡片不会允许任何人批准，也不能通过飞书命令重启服务。多个 bot 可以共享同一套 ACP agent 配置，但必须使用不同的 `id`，并且展开后的 `workspace` 绝对路径也必须不同。

自动读取 owner 需要飞书应用具备查询本应用信息和协作者的权限，例如 `application:application:self_manage` 或等价的应用管理只读权限。也可以直接在 `owner_open_ids` 中手动配置允许审批人的 open_id，配置后启动时不会再查询飞书应用协作者。

可选配置 `restart_command` 用于覆盖 `/restart` 的实际重启方式，格式是字符串数组，第一项是命令，后续项是参数，例如：

```json
{
  "restart_command": ["systemctl", "--user", "restart", "lark-acp-bridge"]
}
```

如果当前 bridge 是内置后台 daemon 子进程，未配置时会使用当前可执行文件按内置后台 `restart` 模式重启；通过 `--config` 启动时会把当前配置路径传给新进程。如果使用 `run` 前台模式、systemd 或其他进程管理器运行，必须配置 `restart_command`，避免额外拉起一个后台实例。

可选配置 `message_reaction` 用于控制是否在普通飞书消息 prompt 中提示 ACP agent：如果收到的消息适合用轻量表情表达判断、认可、惊讶、好笑、无语或鼓励，可以给原消息添加 reaction。该配置是全局开关，不按 chat 单独配置，默认 `false`。开启后，prompt 会提供当前消息的 `message_id` 用法、推荐 `emoji_type` 列表，以及 `lark-cli im reactions create` / 飞书 IM MessageReaction Create API 的操作提示。

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

普通文本会自动创建 ACP 会话；群聊默认需要 at bot 才响应，可用 `@Bot /at off` 为当前 chat 改成免 at 且每条消息都响应，也可用 `@Bot /at off auto` 改成免 at 后自动判断是否响应，私聊始终响应且不支持 `/at` 配置。用户只 at 当前 bot 且不带正文时，会按“用户提及你，但本次无消息内容，请按历史消息，引用上下文回复”作为普通 prompt 发送给 ACP agent。`/new [cwd] [title]` 仍可用于手动重开当前会话、指定 cwd 或指定标题。话题群按话题区分会话，普通群和私聊按整个 chat 复用同一会话。`/new` 未指定标题时会按当前聊天历史生成 `session#N`；它只回复会话创建结果和当前 mode/model，不额外发送 prompt。下一条普通文本会携带 workspace 上下文一起作为 `session/prompt` 发给 ACP agent。

初始化说明会要求 ACP agent 一次性询问用户：

- 用户想叫它什么名字。
- 它应采用什么性格、语气和边界。
- 需要长期记住的用户信息、偏好或常用上下文。
- 是否有需要沉淀到知识库的领域知识、项目经验或常用流程。

首次生成 workspace 时，bridge 会额外创建 `Bootstrap.md`，其中写明一次性初始化引导提示词。每条普通 prompt 都会拼接当前 workspace 的引导、长期记忆、知识入口和技能入口；只要 `Bootstrap.md` 还存在，它就会作为普通 workspace 文件被注入给 ACP agent，让 agent 先向用户提问并完成初始化。

用户回答后，由 ACP agent 使用自身可用的本地文件工具写入 L0/L1/L2 相关文件，然后删除 `Bootstrap.md`。`Bootstrap.md` 不存在后，后续 prompt 自然只会注入根目录记忆、`knowledge/` 入口、wiki skill 和记忆策略。若已经手动写好了文件，也可以在本地直接删除 `Bootstrap.md`。

每条普通 prompt 还会注入 workspace 记忆策略：用户要求“记住”、沉淀经验或总结可复用流程时，ACP agent 应先读取相关 workspace 文件，再用可用的本地文件工具合并写回。新增、删除或重命名知识/技能文件后必须同步 `knowledge/index.md`，并在 `knowledge/log.md` 末尾追加 `[YYYY-MM-DD] 操作 文件 摘要`。

自动知识沉淀默认开启。每次 bot 完整回复普通消息后，bridge 会为当前 ACP session 启动一个分钟级定时器；默认 5 分钟后向同一个 ACP session 发送内部 wiki 反思 prompt，要求 agent 读取 `skills/wiki/SKILL.md` 并按规范更新 L0/L1/L2 文件。该反思轮次是 internal/silent，不创建飞书卡片，也不把 agent 输出转发给用户；prompt 中仍要求如果必须输出文本只输出 `NoReply` 作为兜底。等待期间如果同一会话有新普通消息进入，会取消旧定时器并在新消息完成后重新计时；如果用户发送 `/new` 重开会话，bridge 会取出尚未触发的上一轮 wiki 反思并用独立 wiki runtime key 在后台执行，不阻塞新会话创建和后续消息处理。这个后台 wiki 轮次属于上一轮会话收尾，不会被新会话里的后续普通消息取消；它仍纳入 service/runtime 生命周期，可随 `/wiki off`、会话关闭或服务退出取消并关闭对应 ACP client。

bridge 不在本地实现多轮 onboarding 状态机，也不做旧文件名迁移；本地开发阶段只使用 `SOUL.md`、`MEMORY.md`、`AGENTS.md`、`TOOLS.md` 这些大写文件名作为 L0 记忆入口。

会话映射会持久化到每个 bot 的 workspace 下：

```text
$BOT_WORKSPACE/sessions.json
$BOT_WORKSPACE/restart_ack.json
```

会话映射使用 JSON 文件保存 `bot_id + source + main_id + sub_id -> ACP session`。当前聊天入口使用 `source=im`，普通群和私聊的 `main_id` 是 `chat_id`、`sub_id` 为空，表示整个 chat 共用一个 ACP session；话题群的 `main_id` 是 `chat_id`、`sub_id` 是当前话题的 `thread_id`。旧版 IM 记录中的 `chat_id + thread_id` 会兼容读取，写盘时也尽量保持旧 JSON 形态。重启后不会丢失当前会话的 `agent`、`cwd` 和 `acp_session_id`；暂不需要 SQLite。`sessions.json` 还会保留同一主资源里的历史 ACP session，用于 `/session list` 和 `/session resume <index>`，并保存 chat 维度的 `/agent`、`/show`、`/at`、`/wiki` 配置。把 session 放在 workspace 下，可以让每个 bot 的记忆、会话映射和后续缓存一起迁移、备份和清理。服务进程内会为活跃飞书会话维护对应的 ACP agent 子进程；重启后普通消息会按已保存的 `acp_session_id` 尝试 `session/load` 恢复。

`restart_ack.json` 是一次性重启回执文件。用户通过 `/restart` 触发重启时，旧进程先记录原消息位置并发送“准备重启”，新进程启动后读取该文件，向原消息回复“已重启”，发送成功后删除文件。

配置中的路径支持 `~` 和 `$HOME` 展开，例如：

```json
{
  "agent_list": [
    {
      "name": "traex",
      "command": "traex",
      "args": ["acp", "serve", "-c", "permission_mode=auto"],
      "default_cwd": "$HOME"
    }
  ]
}
```

默认配置内置 `traex` agent。`agent_list` 是有序数组；全新 chat 没有保存 `/agent` 配置和历史 session 时，默认使用列表第一个可用 agent。可以在飞书侧用 `/agent <name>` 切换当前聊天默认 agent。启动服务前会校验每个 `agent_list[].command` 是否可执行：普通命令通过 `PATH` 查找，带 `/` 或 `\` 的路径命令会检查文件存在且有执行权限。命令不存在时会打印 stderr 并跳过该 agent；其他配置错误仍会直接退出，避免到用户发送 `/new` 时才发现 ACP server 配置不可用。

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
- `/agent`：查看当前聊天默认使用的 ACP agent 和可用 agent 列表。
- `/agent <name>`：把当前聊天默认 ACP agent 切换为 `agent_list[].name`。切换后 `/new` 会使用新的默认 agent；如果当前已有会话仍属于旧 agent，下一条普通消息也会自动基于当前 `cwd` 创建新 agent 的 ACP session。
- `/session list`：列出当前聊天里的历史 ACP 会话，序号从 1 开始，`*` 表示当前会话。
- `/session resume <index>`：把 `/session list` 中第 `index` 项恢复为当前会话；普通群和私聊恢复到当前 chat，话题群恢复到当前话题。
- `/session title <title>`：设置当前 ACP 会话标题，便于 `/session list` 区分。
- `/wiki on`、`/wiki off`、`/wiki status`、`/wiki interval <duration>`：管理当前会话的自动知识沉淀。`duration` 支持 `5m`、`30s`，纯数字按分钟理解。
- `/queue <prompt>`：把提示词暂存到当前会话的内存队列，不打断正在运行的用户任务；当前任务自然结束后按 FIFO 顺序逐条执行，结果会主动回复到原消息上下文。当前没有运行任务时会立即异步执行队列内容。
- `/cmds`：查看当前 ACP server 上报的 slash commands。
- `/cmds /command [args]`：把 ACP slash command 原样发送到当前 ACP session，通过 `session/prompt` 执行。
- `//command [args]`：`/cmds /command [args]` 的简写，用于避免 bridge 本地命令拦截。
- `/config`：查看当前 ACP server 上报的配置项。
- `/config <id>`：查看指定配置项的类型、当前值和可选值。
- `/config <id> <value>`：通过 ACP `session/set_config_option` 设置指定配置项。当前支持 `select` 和 `boolean` 类型；布尔值可用 `true/false`、`on/off`、`yes/no`、`1/0`。
- `/model`：打开飞书模型选择卡片，通过下拉列表设置当前会话模型。
- `/model <model>`：通过 ACP `session/set_config_option` 设置当前会话模型。
- `/mode`：打开飞书模式选择卡片，通过下拉列表设置当前会话模式。
- `/mode <mode>`：通过 ACP `session/set_config_option` 设置当前会话模式。
- `/usage [day|week|month|year]`：查看当前 bot workspace 内的 token 用量报告，按 agent 和模型维度聚合；不指定周期时默认查看今日。统计数据保存到 workspace 下的 `token_usage.json`，覆盖普通 IM、定时任务和文档评论等 prompt 入口。
- `/show step|plan|thought|tool|status|used on|off`：设置当前聊天流式卡片展示项。默认展示 `step`、`plan`、`tool`、`status` 和 `used`，`thought` 默认关闭，需显式开启；`step` 控制 `💬` 过程消息，`plan` 控制 `📌` 计划消息，`thought` 控制 `🧠` 思考消息，`tool` 控制 `⏳/✅/❌` 工具调用和工具输出，`status` 控制底部状态栏，`used` 控制用量明细。`step`、`plan`、`thought`、`tool` 都关闭时，流式卡片只展示正文，不展示“执行过程”折叠区域。
- `/at status|on|off`：查看或设置当前群聊是否需要 at bot 才响应。群聊默认需要 at，因此默认状态下需使用 `@Bot /at off` 改为免 at；这里的 `@Bot` 必须提及当前 bot 的 open_id，随便 at 其他用户或其他 bot 不会触发。`/at off` 等同于 `/at off every`，每条消息都响应；`/at off auto` 会先让 agent 自动判断是否需要响应，不为未 at 消息添加处理中表情；`/at off auto-reaction` 同样自动判断，但会添加处理中表情；免 at 后可用 `/at on` 或 `@Bot /at on` 恢复为需要 at。私聊不支持该命令，at 或不 at 都会响应。
- `/restart`：仅 bot owner 可用。旧进程会先回复“准备重启”并写入 `$BOT_WORKSPACE/restart_ack.json`，然后执行 `restart_command`；内置后台 daemon 子进程也可在未配置 `restart_command` 时使用内置后台 restart。新进程启动后会向原消息回复“已重启”并删除回执文件。
- `/new [cwd] [title]`：为当前飞书会话手动创建或重开当前聊天默认 agent 的 ACP 会话，执行 `initialize` 和 `session/new`，并持久化会话映射。传入 `cwd` 时必须是可访问目录，支持绝对路径、`~/path`、`./path` 和 `../path`；相对路径优先基于当前会话已有 `cwd` 解析，首次创建则基于当前聊天默认 agent 配置里的 `default_cwd` 解析。不传 `cwd` 时优先沿用当前会话已有的 `cwd`，首次创建则使用当前聊天默认 agent 配置里的 `default_cwd`。`cwd` 后面的文本会作为标题；也可以使用 `/new --title 标题` 或 `/new 标题`。未指定标题时默认使用 `session#N`。回复会短暂等待 `session/update`，并展示当前 mode 和 model；ACP server 未上报时显示未知。如果旧会话有尚未触发的 wiki 反思轮次，`/new` 会将其放到独立 wiki runtime key 后台执行，不等待反思完成；该后台反思不会被新会话后续普通消息取消。如果新 ACP session 创建或持久化失败，原 pending wiki 定时器会恢复。
- 普通文本：发送到当前会话的 ACP session，执行 `session/prompt`；执行过程中会创建一张飞书流式卡片，持续更新 agent 文本和工具调用状态，最终文本如果已经写入卡片则不再重复发送普通文本。当前会话没有 session 时会自动使用当前聊天默认 agent 的 `default_cwd` 创建；如果当前会话 agent 和当前聊天默认 agent 不一致，会自动基于当前 `cwd` 创建新 agent 的 ACP session。会话创建失败或未配置默认 cwd 时再提示用户用 `/new <cwd>` 指定。话题群卡片会进入当前话题；普通群和私聊回复引用原消息但不强制进入话题模式。
- 权限卡片：权限选项来自 ACP agent，正文完整展示选项内容，按钮使用短编号文本避免截断。只有 bot owner 可以点击权限卡片；owner 优先来自 `bots[].owner_open_ids`，未配置时启动阶段会尝试从飞书应用所有者、创建者和管理员/开发者协作者自动解析。未解析到 owner 时，无论群聊还是私聊，权限卡片都不能被任何人批准。请求被取消时，bridge 会把卡片更新为已取消/已失效状态并移除按钮。

同一会话里新消息优先级最高。用户重新发送普通消息时，bridge 会取消当前会话正在执行的上一轮 ACP prompt 或当前会话 wiki 反思任务，再处理新消息。用户发送 `/new` 时会取消正在执行的上一轮用户 prompt；如果只是存在尚未触发的旧会话 wiki 反思定时器，则把该轮反思移到后台独立 wiki runtime key 执行，避免阻塞新会话。这个旧会话后台 wiki 是 `/new` 的收尾目标，不会被新会话里的后续普通消息取消；取消边界仍按 runtime scope 隔离，不会影响其他群聊、私聊或其他话题。

