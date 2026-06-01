# 一键部署方案

目标：让其他人在一台 Linux 服务器上从仓库目录执行 `./scripts/deploy.sh` 后得到一个可常驻的 `lark-bridge` user service。

## 命令入口

```bash
./scripts/deploy.sh --check
./scripts/deploy.sh
```

可选参数：

- `--check`：只做预检，不安装、不登录、不写配置、不写 systemd service。
- `--use-local-config`：目标配置不存在时，显式允许复制仓库根目录的 `config.json`。
- `--no-probe`：部署时跳过主动飞书权限探测。

## 支持范围

- Linux + `systemd --user`
- 已安装并可登录的 shell 用户
- Codex CLI 需要已配置好账号和代理
- 飞书应用创建、权限审批、事件订阅发布仍需要用户按页面提示操作

暂不支持 macOS launchd、Windows service、多用户、多租户。

## 部署步骤

脚本按幂等顺序执行：

1. 初始化 PATH
   - `$HOME/.local/bin`
   - `$HOME/.local/npm_packages/bin`
   - `$HOME/.local/go/bin`
   - `/usr/local/go/bin`
   - 如果存在 nvm，脚本会加载 `$NVM_DIR/nvm.sh` 或 `$HOME/.nvm/nvm.sh`
   - 部署时会写入 `$HOME/.config/lark-bridge/shell.env`，并让 `.profile` / 当前 shell rc source 它

2. 创建目录
   - config: `$XDG_CONFIG_HOME/lark-bridge` 或 `$HOME/.config/lark-bridge`
   - state: `$XDG_STATE_HOME/lark-bridge` 或 `$HOME/.local/state/lark-bridge`
   - binary: `$HOME/.local/bin`

3. 检查依赖
   - `node` / `npm` / `npx`：要求 Node.js >= 18；缺失或版本过低时通过 nvm 用户态安装 Node.js LTS
   - `lark-cli`：缺失时用 `npm install --prefix $HOME/.local/npm_packages` 安装 `@larksuite/cli`
   - Lark skills：缺失时执行 `npx --yes skills add larksuite/cli -g -y`
   - `codex`：缺失时用 `npm install --prefix $HOME/.local/npm_packages` 安装 `@openai/codex`
   - Codex auth：没有 `OPENAI_API_KEY` 且没有 `$CODEX_HOME/auth.json` 时运行 `codex login`
   - `go`：缺失时安装 Go 到 `$HOME/.local/go`

4. 飞书应用初始化
   - `lark-cli doctor --offline` 通过则跳过
   - 否则运行 `lark-cli config init --new`
   - 用户按 CLI 提示完成扫码、创建应用、审批授权

5. 配置文件
   - 默认路径：`$HOME/.config/lark-bridge/config.json`
   - 默认不会复制仓库根目录的 `config.json`
   - 只有传 `--use-local-config` 时，才复制仓库根目录的 `config.json` 并设为 `0600`
   - `owner_open_id` 默认通过 `lark-cli contact +get-user --as user` 自动获取
   - 可用环境变量 `OWNER_OPEN_ID` 显式覆盖
   - 否则交互输入 `lark_app_id`、`lark_app_secret`

6. 编译安装
   - `go build -o $HOME/.local/bin/lark-bridge ./cmd/lark-bridge`
   - 执行 `lark-bridge --config ... --check-config`
   - 执行 `lark-bridge --config ... --probe-lark`
   - probe 会发送测试文本、添加表情、发送测试卡片、patch 测试卡片
   - 如果不想发送测试消息，传 `--no-probe`

7. 环境文件
   - 写入 `$HOME/.config/lark-bridge/env`
   - 固化 PATH，包括当前使用的 Node.js bin 目录
   - 继承当前 shell 中的 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY`、`CODEX_HOME`
   - 不写入持久 npm prefix，避免和 nvm 冲突

8. systemd user service
   - 要求 `systemctl --user` 可用
   - 要求当前用户已启用 linger；未启用时先运行 `loginctl enable-linger $USER`
   - 写入 `$HOME/.config/systemd/user/lark-bridge.service`
   - 写入 `$HOME/.config/systemd/user/lark-bridge-health.service`
   - 写入 `$HOME/.config/systemd/user/lark-bridge-health.timer`
   - 执行 `systemctl --user daemon-reload`
   - 执行 `systemctl --user enable --now lark-bridge.service`
   - 执行 `systemctl --user enable --now lark-bridge-health.timer`

9. 最后输出检查命令
   - `systemctl --user status lark-bridge`
   - `journalctl --user -u lark-bridge -f`
   - `lark-cli auth scopes --format pretty`
   - `systemctl --user list-timers lark-bridge-health.timer`

10. 每天 10 点健康问候
    - timer 调用 `lark-bridge --health-once`
    - one-shot 会创建临时 Codex session
    - Codex 返回一句健康检查文本后，bot 发私聊给 owner

## 飞书后台检查项

脚本无法可靠替用户点击后台发布，因此把下面项目作为人工 gate：

- Bot 已启用
- 能给 owner 发私聊
- 事件订阅包含 `im.message.receive_v1`
- 卡片回调 `card.action.trigger` 已启用
- 消息发送、回复、更新、表情相关权限已审批
- 权限变更后已发布应用版本

如果运行日志里出现 Feishu permission error，daemon 会尽量在错误里输出 `missing_scopes`、`console_url`、`request_id`。按 `console_url` 开通缺失权限，然后重启：

```bash
systemctl --user restart lark-bridge
```

## 后续缺口

- macOS/非 systemd Linux 需要单独 installer。
