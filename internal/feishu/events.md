# Feishu Message Events

This note records observed `im.message.receive_v1` shapes relevant to
`Message` parsing. IDs are intentionally redacted; examples use placeholders
instead of real `message_id`, `chat_id`, `thread_id`, `root_id`, `parent_id`,
user IDs, tenant keys, and app IDs.

## Private Chat Text Message

普通私聊消息没有 `thread_id`、`root_id`、`parent_id`。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<p2p_chat_id>",
      "chat_type": "p2p",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `p2p` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | empty |
| `RootID` | empty |
| `ParentID` | empty |

## Private Chat Direct Reply

私聊中直接回复一条消息时，event 没有 `thread_id`，但会带
`root_id` 和 `parent_id`。

第一层直接回复时，`root_id` 和 `parent_id` 通常相同，都指向被回复
消息；多层回复时，`root_id` 保持指向回复链起点，`parent_id` 指向
当前消息直接回复的上一条消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<p2p_chat_id>",
      "chat_type": "p2p",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<reply_chain_root_message_id>",
      "parent_id": "<direct_parent_message_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `p2p` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | empty |
| `RootID` | `<reply_chain_root_message_id>` |
| `ParentID` | `<direct_parent_message_id>` |

## Private Chat Topic Reply

私聊中对一条消息创建话题后，在该话题里发送的消息仍然是
`chat_type: "p2p"`，但 event 会带 `thread_id`。

观察到的首条话题回复中，`root_id` 和 `parent_id` 都指向创建话题所
基于的原始消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<p2p_chat_id>",
      "chat_type": "p2p",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<topic_source_message_id>",
      "parent_id": "<topic_source_message_id>",
      "thread_id": "<thread_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `p2p` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<thread_id>` |
| `RootID` | `<topic_source_message_id>` |
| `ParentID` | `<topic_source_message_id>` |

## Private Chat Topic Follow-Up

同一个私聊话题里继续发言时，`thread_id` 保持同一个话题 ID。已观察
到的后续普通发言中，`root_id` 和 `parent_id` 仍都指向创建话题所
基于的原始消息，而不是上一条话题回复消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<p2p_chat_id>",
      "chat_type": "p2p",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<topic_source_message_id>",
      "parent_id": "<topic_source_message_id>",
      "thread_id": "<same_thread_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `p2p` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<same_thread_id>` |
| `RootID` | `<topic_source_message_id>` |
| `ParentID` | `<topic_source_message_id>` |

## Group Chat Direct Reply

普通群里直接回复一条消息时，event 没有 `thread_id`，但会带
`root_id` 和 `parent_id`。

观察到的一层直接回复中，`root_id` 和 `parent_id` 都指向被回复的
普通群消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<group_chat_id>",
      "chat_type": "group",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<reply_chain_root_message_id>",
      "parent_id": "<direct_parent_message_id>",
      "update_time": "<message_update_time_ms>",
      "user_agent": "<user_agent>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `group` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | empty |
| `RootID` | `<reply_chain_root_message_id>` |
| `ParentID` | `<direct_parent_message_id>` |

## Topic Group New Topic Message

话题群里新发一条话题消息时，已观察到 event 的 `chat_type` 仍然是
`"group"`，不是 `"topic_group"`。这类新话题根消息会带 `thread_id`，
但没有 `root_id` 和 `parent_id`。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<topic_group_chat_id>",
      "chat_type": "group",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "thread_id": "<thread_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `group` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<thread_id>` |
| `RootID` | empty |
| `ParentID` | empty |

## Group Chat Topic Reply

普通群里对一条普通消息创建话题后，在该话题里发送的消息仍然是
`chat_type: "group"`，但 event 会带 `thread_id`。

观察到的首条话题回复中，`root_id` 和 `parent_id` 都指向创建话题所
基于的原始普通群消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<group_chat_id>",
      "chat_type": "group",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<topic_source_message_id>",
      "parent_id": "<topic_source_message_id>",
      "thread_id": "<thread_id>",
      "update_time": "<message_update_time_ms>",
      "user_agent": "<user_agent>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `group` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<thread_id>` |
| `RootID` | `<topic_source_message_id>` |
| `ParentID` | `<topic_source_message_id>` |

## Group Chat Topic Follow-Up

普通群或话题群里同一个话题继续发言时，`thread_id` 保持同一个话题
ID。已观察到的普通群后续发言中，`root_id` 和 `parent_id` 仍都指向
创建话题所基于的原始普通群消息，而不是上一条话题回复消息。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<group_chat_id>",
      "chat_type": "group",
      "content": "{\"text\":\"<text>\"}",
      "create_time": "<message_create_time_ms>",
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<topic_source_message_id>",
      "parent_id": "<topic_source_message_id>",
      "thread_id": "<same_thread_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `group` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<same_thread_id>` |
| `RootID` | `<topic_source_message_id>` |
| `ParentID` | `<topic_source_message_id>` |

## Group Chat Topic Follow-Up With Mention

普通群话题回复中包含 @ 时，文本里的 mention key 会出现在 `content`
里，完整 mention 信息出现在 `mentions` 数组里。`ParseMessage` 会用
`mentions` 将文本里的 key 替换为可读名称和 open ID。

```json
{
  "schema": "2.0",
  "header": {
    "event_id": "<event_id>",
    "create_time": "<event_create_time_ms>",
    "event_type": "im.message.receive_v1",
    "tenant_key": "<tenant_key>",
    "app_id": "<app_id>"
  },
  "event": {
    "message": {
      "chat_id": "<group_chat_id>",
      "chat_type": "group",
      "content": "{\"text\":\"@_user_1 <text>\"}",
      "create_time": "<message_create_time_ms>",
      "mentions": [
        {
          "id": {
            "open_id": "<mentioned_open_id>",
            "union_id": "<mentioned_union_id>",
            "user_id": null
          },
          "key": "@_user_1",
          "mentioned_type": "user",
          "name": "陈xx",
          "tenant_key": "<tenant_key>"
        }
      ],
      "message_id": "<current_message_id>",
      "message_type": "text",
      "root_id": "<topic_source_message_id>",
      "parent_id": "<topic_source_message_id>",
      "thread_id": "<same_thread_id>",
      "update_time": "<message_update_time_ms>"
    },
    "sender": {
      "sender_id": {
        "open_id": "<sender_open_id>",
        "union_id": "<sender_union_id>",
        "user_id": null
      },
      "sender_type": "user",
      "tenant_key": "<tenant_key>"
    }
  }
}
```

Parsed `Message` fields:

| Field | Value |
| --- | --- |
| `ChatType` | `group` |
| `MessageID` | `<current_message_id>` |
| `ThreadID` | `<same_thread_id>` |
| `RootID` | `<topic_source_message_id>` |
| `ParentID` | `<topic_source_message_id>` |
| `Mentions[0].Key` | `@_user_1` |
| `Mentions[0].ID` | `<mentioned_open_id>` |
| `Mentions[0].Name` | `陈xx` |

## Field Notes

- `thread_id` is not limited to `chat_type: "topic_group"`; private chat topic
  replies, ordinary group topic replies, and topic group new-topic messages can
  also carry it.
- For plain private chat messages, `thread_id`, `root_id`, and `parent_id` may
  all be absent.
- For private direct replies, `thread_id` can be absent while `root_id` and
  `parent_id` are present.
- For non-private events with `thread_id`, prefer `thread_id` as the stable
  topic identifier. `root_id` and `parent_id` describe reply/source
  relationships and are not equivalent to the topic ID.
- For observed ordinary group topic follow-ups, `parent_id` continues to point
  to the topic source message instead of the previous topic reply.
