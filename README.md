# wecom-push

基于企业微信[智能机器人长连接协议（WebSocket）](https://developer.work.weixin.qq.com/document/path/101463)的 Go CLI 推送工具，支持向企业微信**个人（单聊）**或**群聊**主动发送通知，并内置实时 **Debug 模式**用于快速获取用户 `userid` 与群 `chatid`。

## 核心特性

- **单 Bot 多 Channel 模型**：所有 Channel 共享同一组智能机器人凭证（`BotID` + `Secret`），每个 Channel 维护独立的接收人列表（可自由混编用户与群聊）。
- **混合 Daemon + 直连架构（单二进制）**：
  - **后台 Daemon 模式（推荐）**：`wecom-push daemon start` 在后台维护唯一的企业微信 WebSocket 长连接（30 秒心跳保活、断线指数退避重连、被踢退避防限流），本地通过 Unix Domain Socket（Windows 下为本地回环 TCP）提供 HTTP + SSE IPC 接口。并发调用无冲突、发送极快、避免触发 `aibot_subscribe` 频率限制。
  - **自动直连降级**：未启动 Daemon（如 CI/CD 一次性任务、临时容器）时，`send` 和 `debug` 会自动降级为「建连 → 发送/监听 → 断联」，也可通过 `--direct` / `--daemon` 显式指定。
- **无痛 ID 发现（Debug 模式）**：运行 `wecom-push debug` 后，在企业微信私聊机器人或在群里 `@机器人`，终端即刻展示对应的 `userid` / `chatid` 以及可直接复制的 `wecom-push add` 命令。当 Daemon 运行时，`debug` 自动复用 Daemon 的 SSE 事件流，不会踢掉线上长连接。
- **丰富的消息类型**：支持 Markdown、管道输入（stdin）、标题快捷拼接、模板卡片（`--card`）、以及基于长连接分片上传的图片（`--image`）与文件附件（`--attachment`）。
- **多平台自动发布**：内置 GitHub Actions CI/Release 与 GoReleaser v2 配置，自动发布 Linux/macOS/Windows（`amd64`/`arm64`）二进制、Homebrew Tap、以及 `.deb` / `.rpm` / `.apk` 包（为后续 APT 源留好完整空间）。

## 架构概览

```text
                    +-------------------------------+
wecom-push send --->|                               |
                    |  wecom-push daemon (optional) | === WSS (单长连接 + 30s ping) ===> wss://openws.work.weixin.qq.com
wecom-push debug -->|  Unix Socket (HTTP + SSE)     |
                    +-------------------------------+
                         (未启动 daemon 时自动直连)
```

## 安装

### Homebrew（macOS / Linux）

```sh
brew install Cinea4678/tap/wecom-push
```

### Debian / Ubuntu（`.deb`）

从 [Releases](https://github.com/Cinea4678/wecom-push/releases) 下载对应架构的 `.deb` 包：

```sh
sudo apt install ./wecom-push_<version>_linux_amd64.deb
```

### 从源码编译安装

```sh
make install
# 默认安装到 ~/.local/bin/wecom-push，可通过 PREFIX=/usr/local/bin 覆盖
```

## 快速开始

### 1. 准备企业微信智能机器人凭证

在企业微信管理后台或客户端创建「智能机器人」→ 开启 **API 模式** → 选择 **使用长连接**，获取 **BotID** 和 **Secret**。

### 2. 初始化共享 Bot 身份

```sh
# 交互式配置（Secret 隐藏输入，不进入 Shell 历史）
wecom-push init

# 或通过参数配置（适合自动化脚本）
wecom-push init --bot-id "<BOT_ID>" --secret "<SECRET>"
```

配置默认保存在 `~/.config/wecom-push/config.yaml`（权限 `0600`）。也支持环境变量覆盖：

| 环境变量 | 说明 |
| --- | --- |
| `WECOM_PUSH_BOT_ID` / `WECOM_BOT_ID` | Bot ID |
| `WECOM_PUSH_BOT_SECRET` / `WECOM_BOT_SECRET` | 长连接专用 Secret |
| `WECOM_PUSH_WS_URL` / `WECOM_WS_URL` | 自定义 WebSocket 地址（默认 `wss://openws.work.weixin.qq.com`） |
| `WECOM_PUSH_CONFIG` | 自定义配置文件路径 |
| `WECOM_PUSH_SOCKET` | 自定义 Daemon Socket 路径或 `tcp://127.0.0.1:PORT` |

### 3. 启动 Debug 模式获取群 ID / 用户 ID

> 企业微信协议要求：用户或群需先与机器人产生过互动，机器人才能向该会话主动推送消息。

```sh
wecom-push debug
```

然后在企业微信中：
- **获取个人 ID**：给机器人发送一条私聊消息；
- **获取群 ID**：把机器人拉入群聊并在群里 `@机器人` 发一条消息。

终端会直接打印发现的 `User ID`、`Group ID` 和一键添加命令。如需在聊天窗口里也让机器人回复 ID，可加 `--reply`；如需查看原始 JSON 帧，可加 `--raw` 或 `--json`。

### 4. 创建与管理 Channel

```sh
# 创建 channel "ops"，包含一个群和两个用户（支持 =备注名），并设为默认频道
wecom-push add ops \
  --group wrXXXXXXXX=生产告警群 \
  --user zhangsan=张三 \
  --user lisi \
  --desc "生产环境告警通知" \
  --default

# 向已有 channel 追加接收人
wecom-push add ops --append --user wangwu=王五

# 查看所有 channel
wecom-push list

# 查看单个 channel 详情
wecom-push channel show ops

# 从 channel 移除某个接收人
wecom-push remove ops --user lisi

# 删除整个 channel
wecom-push remove ops
```

### 5. 发送消息

```sh
# 向指定 channel 发送 Markdown 消息
wecom-push send ops "**发布完成**：api-server v1.4.2 已上线"

# 从标准输入读取并附带标题
tail -n 30 /var/log/app.log | wecom-push send ops --title "错误日志摘录"

# 发送到默认 channel
wecom-push send "备份任务执行成功"

# 临时指定接收人（不依赖 channel）
wecom-push send --user zhangsan "请查看最新报表"

# 发送图片或文件附件
wecom-push send ops --image ./metrics.png
wecom-push send ops --attachment ./report.pdf

# 发送模板卡片
wecom-push send ops --card @./card.json

# 强制走单次直连（跳过 daemon）或强制要求走 daemon
wecom-push send --direct ops "CI 构建通过"
wecom-push send --daemon ops "高频告警消息"
```

### 6. 后台 Daemon 管理

```sh
# 启动 / 查看状态 / 重载配置 / 查看日志 / 停止
wecom-push daemon start
wecom-push daemon status
wecom-push daemon reload
wecom-push daemon logs -n 50
wecom-push daemon stop

# 前台运行（适合 systemd / launchd / Docker）
wecom-push daemon run

# 生成系统自启配置模板
wecom-push daemon service launchd > ~/Library/LaunchAgents/cc.cinea.wecom-push.plist
wecom-push daemon service systemd > ~/.config/systemd/user/wecom-push.service
```

## CI 与发布配置（GitHub Actions + GoReleaser）

本仓库包含两个 GitHub Actions Workflow：

- `.github/workflows/ci.yml`：在 `main` push 和 PR 时运行 `go vet`、`go test -race`、多平台交叉编译检查以及 `goreleaser` 快照校验。
- `.github/workflows/release.yml`：推送形如 `v0.1.0` 的 Git Tag 时自动触发 GoReleaser v2 发布：
  1. 编译 `linux` / `darwin` / `windows` × `amd64` / `arm64` 二进制并打包上传至 GitHub Releases；
  2. 生成并推送 Homebrew Cask 到 `Cinea4678/homebrew-tap`（`Casks/wecom-push.rb`）；
  3. 同步构建 `.deb` / `.rpm` / `.apk` 包附带到 Release（后续接入 APT 源详见 [`packaging/apt/README.md`](packaging/apt/README.md)）。

### 首次发布前准备

1. 在 GitHub 创建公开仓库 `Cinea4678/homebrew-tap`（初始化一个空的 `README.md` 即可，确保默认分支为 `main`）。
2. 生成一个具有 `Cinea4678/homebrew-tap` 仓库 `Contents: Read and write` 权限的 Fine-grained PAT，添加到本仓库的 Actions Secret，命名为 **`HOMEBREW_TAP_GITHUB_TOKEN`**。
3. 打 tag 并推送即可自动发布：
   ```sh
   git tag -a v0.1.0 -m "Release v0.1.0"
   git push origin v0.1.0
   ```

## 本地开发与测试

```sh
make vet
make test
make build
```
