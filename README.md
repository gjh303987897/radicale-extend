# radicale-extend

Go 编写的 Radicale 扩展服务器：MCP 日程与待办管理、重复日程提醒、SMTP 邮件和 Telegram 通知。没有前端页面，也不修改 Radicale 上游代码。兼容基线为 **Radicale 3.9.0**。

```text
MCP 客户端 -> Go 服务 -> CalDAV -> Radicale <- 手机/桌面日历
                                  |
                          Python Hook -> SQLite 事件队列
                                             |
                          Go Worker -> 提醒/投递任务 -> SMTP、Telegram
                          周期 DAV 同步校验 ------^
```

## 能力与边界

- 单个 DAV 账号；HTTP MCP 使用独立 Bearer Token，另支持 stdio。
- 14 个 MCP 工具，覆盖日历列表、日程与待办管理、完成待办、提醒覆盖和状态查询。
- RRULE、RDATE、EXDATE、RECURRENCE-ID 单次改期/删除，IANA 时区、夏令时、浮动时间和全天事件。
- VALARM 相对开始/结束、绝对时间、REPEAT/DURATION；原提醒收件人不会被用作消息接收者。
- SQLite 持久化、任务租约、独立渠道重试、变更合并、免打扰和隐私隐藏。
- 提醒顺序：服务端显式覆盖/关闭 > VALARM > 默认提前 15 分钟。无提醒的全天事件当天 09:00 通知，默认时区 Asia/Shanghai。
- 不包含联系人管理、多用户、每日汇总、会议邀请邮件、Telegram 按钮及 OAuth 自动发现；HTTP 客户端需要能自行配置 Authorization 请求头。
- 不支持自定义 VTIMEZONE 时区定义、RDATE PERIOD、RECURRENCE-ID RANGE 等高级提醒语义，原数据保留，调度报告不支持并取消该资源的待发提醒，不进行猜测。Radicale 本身仍可存储这些数据。
- VJOURNAL 由 Radicale 保管，本服务不调度。没有 DTSTART/DUE 的待办可创建、查询和完成，但没有时间提醒。

## Docker Compose 部署

以下 Docker 命令用于部署服务器或 GitHub Runner。**项目的本地开发测试不运行 Docker。**

### 1. 准备配置和凭据

在目标服务器下载本项目部署文件。使用 Python 创建独立环境并生成凭据：

```sh
python3 -m venv .venv
.venv/bin/python -m pip install bcrypt==4.3.0
.venv/bin/python scripts/init_secrets.py --username alice
cp .env.example .env
cp config.example.yaml config.yaml
```

Windows 可使用 `.venv\Scripts\python.exe` 和对应的文件复制命令。

生成器创建随机 DAV 密码、MCP Token、bcrypt 用户文件及空的渠道密钥文件，已有文件时拒绝覆盖。`secrets/` 目录为 0700，单个文件为 0644，使容器 UID 10001 能读取文件挂载；宿主机通过父目录限制访问。不要将目录整体改成公开可读。Windows 下另用 NTFS ACL 限制目录访问。

编辑 `.env`：

```dotenv
APP_IMAGE=ghcr.io/your-owner/your-repo
RADICALE_IMAGE=ghcr.io/your-owner/your-repo-radicale
IMAGE_TAG=0.1.0
APP_CONFIG=./config.yaml
SECRETS_DIR=./secrets
DAV_USERNAME=alice
```

镜像名必须与 GitHub 仓库标识的小写形式一致。`DAV_USERNAME`、生成器的用户名以及日历路径需要一致。默认只向宿主机 `127.0.0.1` 发布 5232 和 8080。

### 2. 配置消息渠道

在 `config.yaml` 中按需启用：

- 邮件：设置 `smtp.enabled: true`、host、port、security、username、from、to；密码写到 `secrets/smtp_password`。默认 STARTTLS，TLS 证书必须有效；使用 `security: tls` 对接隐式 TLS。`none` 只适合可信的无认证内网中继。
- Telegram：向 BotFather 创建 Bot，把 Token 写入 `secrets/telegram_token`；先向 Bot 发送 `/start`，通过 Bot API 的 `getUpdates` 确认自己的 `chat.id`，再配置 `telegram.chat_id` 和 `enabled: true`。不要把 Token 或更新内容粘贴到公共日志。

两个渠道默认关闭，启用后只通知显式配置的接收者。所有密钥文件避免尾部多余空格，尾部换行会被移除。

按日历配置规则时，`calendars` 同时充当服务的集合白名单；留空表示该用户主页中可发现的所有日历：

```yaml
calendars:
  - path: /alice/work/
    channels: [smtp, telegram]
    changes: true
    reminders: true
    hide_details: false
quiet:
  start: '22:00'
  end: '08:00'
```

省略 `channels` 表示全部已启用渠道，空数组表示不发送。免打扰默认关闭。免打扰结束时已经超过提醒宽限期的消息标记过期，变更通知则延后发送。配置修改后重启 Go 服务生效。

### 3. 启动

公开镜像可直接拉取；私有 GHCR 镜像需要先在部署服务器登录 GHCR，并确保账户具有读取包的权限：

```sh
docker compose pull
docker compose up -d --no-build
docker compose ps
docker compose logs -f app radicale
```

启动顺序为数据库初始化任务成功、Radicale 健康、Go 服务启动并完成首次同步。容器均使用 UID/GID `10001:10001`，卷分别保存 Radicale 数据、事件队列和 Go 状态。不要将 SQLite/WAL 放在 NFS 等网络文件系统。

尚未发布镜像时，可以在部署服务器用 `docker compose build` 构建，然后 `docker compose up -d`；这不属于项目的本地测试流程。

首次启动没有日历是正常情况。用 CalDAV 客户端创建日历，或在服务器创建集合：

```sh
curl --user alice -X MKCALENDAR \
  -H 'Content-Type: application/xml' \
  --data '<c:mkcalendar xmlns:c="urn:ietf:params:xml:ns:caldav"/>' \
  http://127.0.0.1:5232/alice/work/
```

密码取自 `secrets/dav_password`。MCP 工具操作已有集合，不创建日历。配置日历白名单后，只能操作其中可发现的集合。

远程客户端应通过 HTTPS 反向代理访问。将代理连接到 Compose 网络，或转发宿主机回环端口；保留 Authorization 和 MCP 协议请求头，并禁止代理缓存 MCP 响应。Radicale Web 插件设为 `none`，不提供管理页面。

### 4. MCP 接入

配置 HTTP 地址 `https://your-server/mcp`，请求头 `Authorization: Bearer <mcp_token 文件内容>`。不要使用 DAV 密码代替 MCP Token。

工具及参数：

| 工具 | 参数 |
| --- | --- |
| `list_calendars` | 无 |
| `search_events` / `list_tasks` | `calendar`、`from`、`to`、可选 `query`、`limit` |
| `get_event` / `get_task` | `resource`、可选 `recurrence_id` |
| `create_event` / `create_task` | `calendar`、`fields` |
| `update_event` / `update_task` | `resource`、`etag`、`fields`、可选 `recurrence_id` |
| `delete_event` / `delete_task` / `complete_task` | `resource`、`etag`、可选 `recurrence_id` |
| `set_reminder` | `resource`、`mode: calendar/off/before`、可选 `before_seconds` |
| `list_reminders` | `resource` |

`fields` 支持 summary、description、location、status、start、end（事件）、due（待办）、all_day、timezone、rrule、rdates、exdates。时间使用带偏移量的 RFC3339；全天使用 YYYY-MM-DD。`timezone` 表示储存时间的 IANA 时区，省略则使用配置时区。省略字段保留原值，空字符串用于清除可选文本/时间或 RRULE，空数组清除 RDATE/EXDATE。

搜索时间段为 `[from,to)`，最长 366 天，默认返回 100 项，最大 1000 项；生成实例有额外总量限制。待办默认包含无日期任务，可传 `include_undated: false` 关闭；已完成项默认隐藏，可传 `include_completed: true` 查看。

例如 `create_event` 的参数：

```json
{
  "calendar": "/alice/work/",
  "fields": {
    "summary": "项目例会",
    "start": "2026-11-02T10:00:00+08:00",
    "end": "2026-11-02T11:00:00+08:00",
    "timezone": "Asia/Shanghai",
    "rrule": "FREQ=WEEKLY;BYDAY=MO"
  }
}
```

更新和删除必须使用最近一次读取返回的完整 `etag`（包括引号）。收到 412 后重新读取，由调用者解决并发冲突。`recurrence_id` 使用原实例标识：改期后仍使用原定时间，不使用改期后的时间；省略会作用于整个系列。单次修改不能包含系列规则字段。

`set_reminder` 是按日历+UID 的服务端覆盖，不改写日历中的 VALARM；`calendar` 清除覆盖效果，恢复 VALARM/默认规则。提醒状态查询返回 `jobs`、是否已同步的 `synchronized` 及可选的 `scheduling_error`；任务时间为 UTC Unix 时间戳，不返回消息正文，最多列出 1000 项并优先展示活动任务。

本地 stdio 客户端配置示例：

```json
{
  "mcpServers": {
    "calendar": {
      "command": "/path/to/radicale-extend",
      "args": ["mcp-stdio", "--config", "/path/to/config.yaml"]
    }
  }
}
```

stdio 模式不会启动后台 Worker，仍需独立运行 `serve` 并使用同一组数据库路径。首版按单个 `serve` 实例部署，不配置多个调度副本。日志只写 stderr，stdout 用于 MCP 协议。

### 5. 健康检查、升级和备份

- `/healthz` 返回进程存活状态；`/readyz` 返回数据库连接、最近同步和积压/失败计数，不含日程正文。
- `docker compose stop` 停止；`docker compose down` 移除容器并保留卷。**不要用 `down -v` 删除业务数据。**
- 升级前备份，然后修改 `.env` 的 IMAGE_TAG，运行 `docker compose pull` 和 `docker compose up -d --no-build`。schema/protocol 不兼容会报错；v1 不自动执行破坏性降级。
- 一致性备份先停止 app、radicale，再备份三个命名卷及配置、凭据。保留整个 SQLite 目录，不能只复制 `.db` 而忽略 WAL。恢复时停止服务，恢复三个卷、配置和凭据，确认 UID/GID 为 10001，再启动。
- 日志中可按 job ID、channel 和 attempt 排查失败。任务最多尝试 8 次，退避上限 1 小时，Telegram 遵守 retry_after；终止失败任务保留供检查，本版没有管理页面或手动重放工具。

### 连接已有 Radicale

在已有 Radicale 的 Python 环境中安装 `hook/` 包，配置 `[hook] type = radicale_extend_hook`，并将 `RADICALE_EXTEND_QUEUE` 指向初始化完成的队列库。

Go 使用 `init-db --config ...` 初始化数据库，之后再启动 Radicale。两个进程需共享同一主机的本地队列目录和读写权限，不能只共享 `.db` 文件。调整 DAV URL、home、账号和密码后运行 Go `serve`。不需要同步挂载 Radicale 的业务存储目录。

注意：上游加载 Hook 失败时会降级到 `none`；配套镜像在启动前额外检查插件和协议，外部部署也应检查启动日志。

## 开发与测试（无本地 Docker）

需要 Go 1.27 和 Python 3.11+；CI 使用 Python 3.13。复制 `config.local.example.yaml` 到 `config.yaml`，通过 `DAV_PASSWORD`、`MCP_TOKEN` 配置凭据。HTTP Token 至少 32 字节。也支持相应的 `*_FILE`，文件设置优先于直接环境变量，环境设置优先于 YAML。

```sh
go build -o bin/radicale-extend ./cmd/radicale-extend
go run ./cmd/radicale-extend check-config --config config.yaml
go run ./cmd/radicale-extend init-db --config config.yaml
go run ./cmd/radicale-extend serve --config config.yaml
```

测试安装和运行：

```sh
python3 -m venv .venv
.venv/bin/python -m pip install ./hook bcrypt==4.3.0 PyYAML==6.0.3
RADICALE_PYTHON="$PWD/.venv/bin/python" go test ./... -count=1
go vet ./...
.venv/bin/python -m unittest discover -s hook/tests -v
.venv/bin/python scripts/check_assets.py
```

未设置 RADICALE_PYTHON 时，原生 Radicale 集成测试会明确跳过；CI 显式设置并运行。源码直接测试 Hook 时设置 `PYTHONPATH=hook`。Windows 可设置 TEMP/TMP 为项目内 `.cache/tmp`，避免受限制的系统临时目录；套接字测试要求系统允许回环连接。

CLI 另提供 `healthcheck --url http://127.0.0.1:8080/readyz`。数据库初始化和配置检查不会启动 HTTP 或 Worker。

## GitHub Actions 与 GHCR

- CI：Go 格式/vet/竞态检查、Python Hook、原生 Radicale 集成、部署静态检查；GitHub Runner 额外构建两个镜像并运行 Compose 冒烟测试，用合成 SMTP/Telegram 服务验证投递和重启去重，不发送真实通知。
- 发布：推送代码到自己的 GitHub 仓库默认分支，在 Actions 选择 **Publish GHCR images → Run workflow**，输入 tag（如 `0.1.0`）。默认不更新 latest。
- 发布前复用完整 CI；通过后用 GITHUB_TOKEN 推送两套 linux/amd64、linux/arm64 镜像，同时生成 `sha-<完整提交SHA>` 标签、OCI 来源、SBOM 和构建来源证明。`latest` 和 `sha-*` 为保留输入标签。
- 勾选 update_latest 只允许从默认分支执行。所有 Action 引用固定到经过查询的提交 SHA。
- 工作流申请 contents:read、packages:write。组织策略需允许工作流发布包；已有同名包需给仓库 Actions 写权限。首次发布后在包设置中自行选择可见性；本工作流不更改可见性，不需要额外 PAT。
- 工作流汇总输出镜像摘要，部署可进一步固定到摘要。此目录没有绑定远程仓库，源码交付本身不会触发 GitHub 构建或上传镜像。

## 可靠性说明

首次同步建立基线，不发送已有日程的新建通知。Hook 只作为及时同步信号，Go 读取 DAV 的当前权威状态，默认每 5 分钟补偿校验并每小时延展提醒窗口；连续变化默认合并 30 秒。事件队列与业务库之间采用幂等处理及成功后确认，不承诺跨数据库事务。

提醒默认预生成未来 30 天内的触发时间，展开数量受限；改期、删除、取消、完成后撤销旧提醒。发送前重新读取 DAV，DAV 不可用则延后重试。重启补发默认最多迟到 10 分钟，超过宽限期不再发送。

手机写入和队列入队并非同一事务。进程崩溃可能漏掉历史中间变化；补偿可以恢复当前提醒状态，无法恢复全部历史通知。远端收件成功但本地尚未记录成功时崩溃也可能导致重复发送，属于至少一次投递的边界。不要将提醒用于依赖恰好一次投递的流程。

## 验证范围

本地检查不调用任何 Docker 命令。容器运行、多架构构建和 GHCR 发布依赖 GitHub 工作流实际执行；提供部署文件不代表这些远端操作已经成功。
