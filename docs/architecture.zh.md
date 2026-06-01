# Lark Bridge MVP 架构

## 范围

这个 daemon 让唯一的 owner 在飞书里控制本机 Codex。当前版本不支持多用户、群聊策略、管理后台或复杂权限模型。

对话切分：

- 普通私聊：`chat:default`，更像聊天，不主动贴 task 表情。
- 显式 `/create` 创建的飞书话题：`thread:<thread_id/root_id>`，对应独立 Codex session。
- `/history` / `/import` 可把本机已有 Codex session 导入到新的或已有的飞书 task 话题。

## 主链路

```text
Feishu SDK WebSocket
  -> EventDispatcher
  -> daemon route/session
  -> Codex app-server
  -> Feishu SDK card/text reply
```

飞书全链路使用官方 Go SDK：

- WebSocket 长连接接收 `im.message.receive_v1`。
- WebSocket callback 接收 `card.action.trigger`。
- SDK API 发送文本、卡片、表情，更新卡片。

不再依赖 `lark-cli`。

## 实体边界

- `config.Config`：只负责启动配置和默认模型。
- `state.State` / `state.Session`：只持久化 session key 到 Codex thread、workdir、模型配置的映射。
- `lark.EventConsumer` / `lark.Client`：封装飞书 SDK 的事件消费、消息发送、表情、卡片 patch。
- `codex.Client`：封装 Codex app-server JSON-RPC、turn drain、pending、steer。
- `codex.SessionInfo`：扫描本机 Codex 历史 session，供 `/history` / `/import` 使用。
- `bridge.Command`：只负责 slash command 解析。
- `bridge` phrase pools：集中管理用户可见短提示，避免每条反馈都长得一样。
- `daemon` runtime：只保存运行中状态，如 status、active client、pending、active turn、steer backlog。

## Session Key

SDK 消息事件里的 `message` 已包含 `thread_id/root_id/parent_id`，主链路不再额外查 message detail。

```text
if known thread_id exists:
  key = "thread:" + thread_id
else if known root_id exists:
  key = "thread:" + root_id
else:
  key = "chat:default"
```

未知话题消息回落到 chat，避免误创建 task session。新 task 必须由 `/create` 显式创建。

## 卡片

Task 使用飞书 V2 interactive card：

- 运行中卡片：任务收到、Codex 启动、session 就绪等阶段原地更新。
- 运行中卡片提供 `Cancel` 按钮。
- Codex 返回 agent message delta 后，daemon 每 1 秒把当前答案 patch 到同一张卡片；没有新内容时不 patch。
- Pending 卡片：展示 Codex 请求确认内容，并提供 `Approve / Deny / Cancel` 按钮。
- 结果卡片：同一张卡片更新为成功或失败结果。

卡片按钮使用 `card.action.trigger` callback，value 携带：

```json
{
  "action": "approve",
  "session_key": "thread:...",
  "turn_id": "...",
  "item_id": "..."
}
```

callback handler 会校验 operator open_id 必须等于配置里的 owner。

## 命令

- `/help`
- `/status`
- `/projects`
- `/create [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast] [任务]`
- `/attach CODEX_THREAD_ID [--project NAME | --cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast]`
- `/history [--limit N]`
- `/import ID_OR_INDEX [--cwd DIR] [--model MODEL] [--effort low|medium|high|xhigh] [--tier fast]`
- `/reset`
- `/approve`
- `/deny`
- `/cancel`

`/approve`、`/deny`、`/cancel` 保留为按钮之外的 fallback。

同一个 session 已有 turn 在运行时，新普通消息会通过 Codex `turn/steer` 插入当前 active turn，用于修正方向或补充约束，不取消上一条任务，也不丢弃已经产生的上下文。

## 日志

配置 `log_level`：

- `info`：daemon online、路由异常、任务状态变化、pending、失败。
- `debug`：耗时、lag、SDK API 成功明细、卡片 patch 成功明细。
- `error`：只输出失败。

## 配置

```json
{
  "owner_open_id": "ou_xxx",
  "default_work_dir": ".",
  "state_path": "state.json",
  "lark_app_id": "cli_xxx",
  "lark_app_secret": "use-env-or-local-secret",
  "codex_cli": "codex",
  "work_dirs": {
    "Argus": "/home/yyi/Projects/Argus"
  },
  "default_task_model": "gpt-5.5",
  "default_task_reasoning_effort": "xhigh",
  "default_task_service_tier": "fast",
  "default_chat_model": "gpt-5.5",
  "default_chat_reasoning_effort": "medium",
  "default_chat_service_tier": "fast",
  "chat_initial_prompt": "",
  "log_level": "info"
}
```

`lark_app_id` / `lark_app_secret` 也可通过 `LARK_APP_ID` / `LARK_APP_SECRET` 提供。
`chat_initial_prompt` 只会注入私聊 chat session 新建 Codex thread 的第一条消息，默认空；也可通过 `LARK_BRIDGE_CHAT_INITIAL_PROMPT` 覆盖。
