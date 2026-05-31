# Lark Bridge MVP 架构

## 范围

这个 daemon 让唯一的 owner 在飞书里控制本机的 Codex app-server。

MVP 不支持多用户路由、群聊策略、租户隔离或复杂管理后台。对话切分只有两类：

- 飞书私聊且不在话题中：落到一个默认 Codex thread。
- 飞书私聊中的话题/thread：每个飞书话题对应一个独立 Codex thread。

## 目标

- 收到 owner 的飞书消息。
- 路由到正确的 Codex thread。
- 启动或复用 Codex turn。
- 等待 Codex 最终回答。
- 把结果回复到飞书。
- 用表情反馈 received/running/done/failed/waiting 等状态。
- 转发第一层 Codex 用户交互请求。

## 非目标

- 群聊支持。
- 多用户支持。
- 飞书消息流式更新。
- 图片、文件等富媒体输入。
- 完整交互卡片 UI。
- 长期审计和报表。

## 进程模型

Go daemon 管三条长连接/长任务边界：

1. Lark event consumer
   - 启动 `lark-cli event consume im.message.receive_v1 --as bot`。
   - 等待 stderr 的 ready marker。
   - 按 NDJSON 读取 stdout 里的消息事件。

2. Codex RPC client
   - 启动或连接 `codex app-server`。
   - 通过 `codex app-server proxy` 或 Unix socket 使用 JSON-RPC。
   - 调用 `initialize`、`thread/start`、`turn/start`、`turn/interrupt`，并回复 Codex server requests。

3. Orchestrator
   - 基于 `event_id` 去重。
   - 先解析命令。
   - 把飞书 session key 映射到 Codex thread ID。
   - 对同一个 session 串行化 turn。
   - 发飞书回复和表情。

## 模块划分

计划目录：

```text
cmd/lark-bridge/
  main.go

internal/config/
  config.go

internal/lark/
  events.go
  messages.go
  reactions.go
  types.go

internal/codex/
  rpc.go
  threads.go
  turns.go
  requests.go
  types.go

internal/session/
  keys.go
  store.go
  memory.go
  sqlite.go

internal/bridge/
  orchestrator.go
  commands.go
  jobs.go

internal/testkit/
  fake_lark.go
  fake_codex.go
```

## 数据模型

MVP 可以用 SQLite 持久化，测试使用内存实现。

```text
sessions
  id
  lark_session_key
  codex_thread_id
  created_at
  updated_at

seen_events
  event_id
  created_at

turns
  id
  session_id
  lark_message_id
  codex_turn_id
  status
  created_at
  completed_at

pending_requests
  id
  session_id
  codex_request_id
  request_type
  payload_json
  status
  created_at
  resolved_at
```

## Session Key 规则

收到事件后，daemon 先用 `message_id` 查询消息详情。

```text
if message.thread_id exists:
  key = "thread:" + message.thread_id
else if message.root_id exists:
  key = "thread:" + message.root_id
else:
  key = "default"
```

这样普通私聊都进默认上下文，飞书话题隔离到独立 Codex 上下文。

## 命令处理

命令在发送给 Codex 前解析。

MVP 命令：

- `/help`：显示可用命令。
- `/status`：显示 daemon、Lark consumer、Codex 连接、active turn、pending request 状态。
- `/create`：为当前飞书 session key 创建新的 Codex thread。
- `/stop`：中断当前 session 的 active Codex turn。
- `/approve <id>`：批准一个 pending Codex request。
- `/deny <id>`：拒绝一个 pending Codex request。
- `/cancel <id>`：拒绝并中断当前 turn。

未知 slash command 回复简短帮助，不转发给 Codex。

## 表情反馈

在用户原始消息上贴表情作为状态提示：

- received：消息已接收并入队。
- running：Codex turn 已启动。
- done：最终回复已发送。
- failed：turn 或回复失败。
- waiting：Codex 等待用户输入或审批。

具体 `emoji_type` 做成配置项，因为不同租户/客户端对表情名的表现可能不同。

## Codex 交互请求

daemon 需要把 Codex server request 转成同 session 的飞书提示。

MVP 支持：

- `item/commandExecution/requestApproval`
- `item/fileChange/requestApproval`
- `item/permissions/requestApproval`
- `item/tool/requestUserInput`
- `mcpServer/elicitation/request`

第一版用文本命令 `/approve`、`/deny`、`/cancel` 处理；等 daemon 有公网 HTTP callback 后，再接 `card.action.trigger` 做卡片按钮。

## 测试策略

默认测试不能访问真实飞书或真实 Codex。

单测使用：

- fake Lark event source。
- fake Lark sender/reaction client。
- fake Codex RPC client。
- in-memory session store。

主链路测试覆盖：

- 收到一条消息事件。
- 解析 session key。
- 创建或复用 Codex thread。
- 发起一次 `turn/start`。
- 收到 fake final answer。
- 发送一次飞书回复。
- 标记 event 已处理。

可选集成测试：

- 只有设置 `LARK_BRIDGE_E2E=1` 时才运行。
- 使用专用测试私聊/消息。
- 记录测试过程中 bot 发出的 message ID。
- cleanup 阶段尝试撤回 bot 自己发出的测试消息。
- 普通 `go test ./...` 永远不跑真实链路。

## 实现顺序

1. 定义接口和 fake。
2. 实现命令解析。
3. 实现 session key 解析和 store。
4. 实现最小 Codex JSON-RPC client：`initialize`、`thread/start`、`turn/start`。
5. 实现 Lark event consumer、reply、reaction wrapper。
6. 实现 orchestrator 主链路和单测。
7. 加可选 E2E 测试 harness。
8. 加 systemd/dev-run 文档。
