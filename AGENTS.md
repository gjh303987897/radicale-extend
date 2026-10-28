# 项目约定

## 项目定位

- 本项目是为 Radicale 提供扩展能力的服务器应用。
- 计划提供 MCP 服务器、SMTP 邮件通知和 Telegram 消息通知功能。
- 本项目使用 Go 语言编写。
- 不需要提供前端页面，不引入前端框架、前端工程或页面构建流程。

## 架构与职责

- 采用“Radicale + Go 扩展服务 + 最小 Hook 适配插件”的架构。
- Radicale 负责日历、待办和通讯录的权威存储，以及 CalDAV/CardDAV 同步、认证和访问权限。
- Go 扩展服务负责 MCP 接口、日历业务处理、通知规则、事件消费、提醒调度和消息投递。
- Hook 适配插件负责捕获 Radicale 变更并快速持久化事件，不承担业务解析、调度或网络消息发送。
- MCP、后台 Worker 和调度器保持清晰的模块边界；部署时可以提供独立运行入口，小规模部署也可以由同一 Go 进程托管。
- 通过配置文件、环境变量和必要的命令行参数管理服务，不开发配置管理页面。

主要数据流：

```text
AI 客户端 -> Go MCP 服务 -> 日历/联系人服务层 -> CalDAV/CardDAV -> Radicale
手机/桌面日历客户端 <-> CalDAV/CardDAV <-> Radicale
Radicale -> Hook 适配插件 -> 持久化事件队列 -> Go Worker
Go Worker -> 通知规则/提醒任务 -> SMTP、Telegram
Go 周期同步任务 -> CalDAV/CardDAV -> 校验并修复本地提醒状态
```

## Radicale 集成与 Hook

### 集成方式

- 优先通过 CalDAV/CardDAV 和 Radicale 已有扩展机制集成，不以 fork 或修改上游核心代码为前提。
- Go 服务不直接改写 Radicale 的底层存储文件，日历和联系人读写经过 DAV 接口。
- Radicale 的 Python Hook 仅作为独立的最小适配插件；核心业务逻辑和服务器进程使用 Go 实现。
- 插件安装在运行 Radicale 的 Python 环境，通过配置启用，无须修改上游源码：

```ini
[hook]
type = radicale_extend_hook
dryrun = False
```

- 上述模块名是本项目规划的插件模块名，配置示例不表示插件已经实现。
- 当前兼容基线固定为 Radicale 3.9.0，升级时重新核对接口、事件字段和触发范围，不直接依赖浮动的 master 行为。

### 两种 Hook 的区别

- `[storage] hook` 是执行外部命令的存储 Hook，可用于备份、记录路径变化或触发重新同步。
- `[hook] type` 是 Python 通知插件入口，可接收结构化变更对象，是本项目通知接入的优先方案。
- Python 插件提供继承 `radicale.hook.BaseHook` 的 `Hook` 类，实现 `enabled` 属性和 `notify(notification_item)` 方法。
- 前期核查的接口包含 `upsert`、`delete`、`cpatch` 类型，以及 `point`、`content_type`、`new_content`、`old_content`、`uid`、`actor` 等字段；具体语义以固定版本为准。
- `point` 是集合路径，不能当作完整资源地址；`uid` 可能为空，需要从 iCalendar/vCard 内容解析；`actor` 是操作发起者，不能直接当作集合所有者或通知接收者。
- `VCALENDAR` 内容需进一步区分 `VEVENT`、`VTODO` 等组件；保留原始变更载荷供 Go Worker 处理。

### 执行时机与限制

- 前期核查的普通 PUT/DELETE 路径会在写入或删除数据后，同步调用通知 Hook，调用时仍持有存储写锁。
- Hook 内只执行有时间限制的本地持久化操作，不连接 SMTP、调用 Telegram、不执行耗时的重复日程展开。
- 不在 Hook 内同步回调 Radicale 的 DAV 接口，避免等待自身持有的锁。
- 插件应遵守 `dryrun` 配置；演练模式不入队、不发送真实消息。
- 不假设 Hook 失败可以回滚日历变更。必须明确入队失败策略，并提供错误日志和补偿机制，不能静默丢弃事件。
- 前期方案倾向入队失败时保留日历同步可用性，通过日志和周期同步修复提醒状态；实现时记录这一取舍。
- 不假设所有变更均有通知：此前核查的 MOVE 路径没有调用 Python 通知 Hook，直接修改底层文件也不经过该接口。需要验证整集合导入、覆盖、删除和移动等路径。

## MCP 服务

- MCP 服务器使用 Go 实现，通过日历/联系人服务层访问 Radicale。
- 使用官方 Go MCP SDK，本地接入支持 stdio，远程接入支持 Streamable HTTP；依赖版本固定在 go.mod/go.sum，升级时验证协议兼容性。
- 优先提供日历列表、日程搜索、详情读取、新增、修改、删除工具；后续扩展待办、提醒配置和联系人操作。
- 可参考工具名称：`list_calendars`、`search_events`、`get_event`、`create_event`、`update_event`、`delete_event`、`list_tasks`、`create_task`、`complete_task`、`set_reminder`、`list_reminders`、`search_contacts`。
- 工具接收结构化参数，包括日历标识、标题、起止时间、时区等，由服务层生成和修改 iCalendar/vCard 数据。
- 修改资源时保留未涉及的原始属性和组件，避免丢失客户端写入的信息。
- 更新和删除使用 ETag 条件请求，处理并发冲突，避免覆盖其他客户端的修改。
- 多用户场景必须将 MCP 调用者映射到对应的 DAV 身份和权限，不通过共享管理员凭据绕过用户授权。
- 单用户 HTTP MCP 使用独立 Bearer Token，启动服务时至少 32 字节；默认仅向宿主机回环地址发布端口，公网由 HTTPS 反向代理接入。本版不实现 OAuth 自动发现。

## 通知规则与消息渠道

- 区分变更通知、定时提醒和汇总通知三类能力。
- 变更通知处理新建、重要字段修改、删除、待办完成等操作；定时提醒由后台调度器独立触发，不依赖客户端在线或新的 Hook 调用。
- 每日议程等汇总通知属于后续扩展能力。
- SMTP 和 Telegram 共用通知规则与消息模型，渠道实现分别负责格式化、发送、错误分类和结果记录。
- 通知接收者来自显式配置或经过验证的用户绑定关系，不自动将所有日程参与者作为接收者。
- 规则规划支持按用户、日历、操作类型选择渠道，配置重要字段、短时间变更合并、免打扰时间、显示时区和详情隐藏。
- Radicale 已有邮件 Hook 的能力可作参考或按需复用，但它不替代本项目的定时提醒和统一投递机制；避免同一通知被两套机制重复发送。
- SMTP 第一阶段发送普通通知邮件；会议邀请、改期、取消所需的 iCalendar 调度语义单独规划。
- Telegram 通知通过 Bot API 的 `sendMessage` 发送，用户需先启动 Bot 并验证绑定 `chat_id`。当前另支持显式开启 `telegram.commands_enabled`，通过长轮询提供 `/today`、`/next` 和 `/help` 私人聊天查询；仅响应绑定的数字私人 `chat_id`，群组和其他用户不得访问日程。
- `/today` 按配置时区的自然日查询，包含全天、跨天及重复实例；`/next` 包含进行中日程并查询未来 366 天，按开始时间返回最早 5 个实例。两者通过 DAV 查询权威数据，遵守集合白名单和详情隐藏规则，失败或不支持的数据必须明确报告。
- Bot 接收器只在 `serve` 运行，默认关闭，不增开端口；每个 Token 只运行一个接收器，不自动删除现有 webhook。更新游标与回复队列事务持久化、重启恢复、最多重试 8 次并遵守 `retry_after`，不记录原始私人消息或 Token；处理至少一次的重复边界应如实说明。
- Bot 日程列表提供有效期 24 小时的持久化事件 ID，绑定当前 Bot／接收者及重复实例；`/event` 查看渠道规则，`/title` 修改标题，`/remind` 配置 smtp、telegram 或 all 的提前量、关闭或恢复继承。默认只修改选中的单次实例，`/title_series` 和 `/remind_series` 才修改整个系列；系列标题修改包含单次例外标题，保留未涉及的其他属性和组件。
- 标题编辑使用列表的 ETag 条件写入，拒绝并发覆盖。提醒优先级细化为“单次渠道覆盖 > 系列渠道覆盖 > MCP 事件级覆盖 > VALARM > 默认”，规则与重建的提醒任务同一事务提交。`default` 仅删除当前范围覆盖；全局渠道开关、日历渠道白名单及 reminders 开关不可被 Bot 绕过。未启用的 SMTP 可保存规则但必须说明不会发送，不通过 Bot 配置凭据或收件人。
- Telegram 提供 `/reminders ID` 只读查询事件当前提醒规则，复用已绑定私人聊天、持久化事件 ID 和单次实例范围的授权限制。展示两个渠道的生效规则、来源、默认提前量或全天时刻、渠道状态，以及按配置时区计算的触发时间（最早 3 次及总数）；计算复用日历提醒逻辑，无法可靠解析时明确报告。不修改事件、规则或日程提醒任务，规则触发时间与实际发送时间应明确区分。
- Telegram 的稍后提醒、完成待办等按钮交互属于后续扩展；处理限流时遵守 `retry_after`。

## 提醒与日历语义

- 结合 `upsert` 的新旧内容区分新增和修改；删除时使用旧内容撤销相关提醒。
- 日程改期后撤销旧提醒并生成新提醒；删除日程或完成待办后停止相应提醒。
- 规划支持 iCalendar `VALARM`，并允许为没有提醒的日程设置默认提前量，明确两者优先级，避免重复创建提醒。
- 正确处理时区、夏令时、无时区时间和全天事件，并明确全天事件的默认提醒时刻。
- 重复日程需处理重复规则、附加日期、排除日期和单次例外，按实际发生的实例生成提醒。
- 单次改期必须关联原实例，不能将整个重复系列视为一个提醒任务。
- 提醒发送前核对当前日程状态和版本，防止发送已取消或已经失效的提醒。
- 尚未支持的日历语义必须明确说明，不静默生成错误提醒。

## 持久化与可靠性

- 小规模部署优先考虑本地 SQLite，初期不默认引入 Redis、RabbitMQ 等额外基础设施；更大规模再按实际需求评估。
- Radicale 是业务数据的权威来源，本地数据库保存事件队列、通知规则、同步游标、提醒任务和投递记录。
- 当前使用两个独立 SQLite 数据库：Hook 事件队列库和 Go 业务状态库；schema/protocol 版本均由 Go 初始化和检查，Python 不创建或迁移数据库。
- Hook 与 Go Worker 共享队列时，明确载荷格式、版本、数据库迁移和并发约定；SQLite 文件位于本地磁盘，采用短事务，按需启用 WAL。
- 事件处理、提醒任务更新和消息投递任务创建尽量使用同一数据库事务；真实网络发送在事务外执行。
- 队列消费要支持任务领取、失败重试和崩溃恢复，避免多个 Worker 同时处理同一任务。
- SMTP、Telegram 分别记录投递状态，一个渠道的失败不能导致另一渠道的成功消息被无条件重发。
- 使用稳定的业务标识和幂等策略去重，区分集合、日程 UID、重复实例、提醒规则和渠道；不能只用随机入队 ID 去重。
- 日历存储和本地队列不在同一事务中，存在变更已保存但未入队的窗口，不宣称端到端恰好一次投递。
- 周期性通过 DAV 同步校验当前状态，修复漏记、服务中断和 Hook 覆盖不足造成的提醒偏差；该机制不能恢复所有已丢失的历史变更通知。
- 网络超时可能发生在远端已接收消息之后，需明确重复投递的实际边界。
- 日志记录任务标识、处理状态和错误，默认不输出凭据或完整私人日程内容。

## 分阶段范围

- 用户已确定首版范围：单用户、日程与待办 MCP、重复日程提醒、SMTP/Telegram 双渠道、Hook 队列、持久化、失败重试和同步补偿。
- 提醒优先级为服务端显式覆盖/关闭、日历 VALARM、默认提前 15 分钟；没有提醒的全天事件默认当天 09:00 通知，默认时区 Asia/Shanghai。
- 通知只发送给显式配置的个人邮箱和 Telegram chat_id，不自动通知参与者。
- 多用户、联系人管理、每日汇总、会议邀请邮件和 Telegram 按钮交互属于后续扩展；私人聊天的日程查询、分渠道提醒规则和标题编辑命令已实现。
- 核心运行入口为 `serve`、`mcp-stdio`、`check-config`、`init-db`、`healthcheck`，具体使用与已知兼容限制见 README.md。
- 原生集成测试以 Radicale 3.9.0 为基线，通过 `RADICALE_PYTHON` 指定解释器；不能用跳过集成测试代替完整验收。

## 部署要求

- 必须提供 Docker Compose 部署方式，包含 `compose.yaml`、必要的 Dockerfile、配置示例和部署文档。
- Compose 配置应覆盖 Radicale、Hook 插件和 Go 扩展服务的集成，明确服务网络、端口、配置挂载和数据持久化方式。
- Hook 与 Go 服务共享 SQLite 队列时，挂载同一持久化目录，保证数据库及 WAL 等配套文件可访问，并明确读写权限。
- 提供不含真实凭据的 `.env.example` 或等价配置示例，说明必填项和密钥配置方式。
- 部署文档说明启动、停止、查看日志、升级和备份恢复步骤，以及健康检查和重启策略。
- 提供 Docker Compose 部署文件不代表已经完成容器运行验证，交付时应如实说明验证范围。
- GitHub 托管 Runner 可以构建镜像和运行 Compose 冒烟测试；此例外不允许在本机调用 Docker。

## GitHub Actions 与镜像发布

- GHCR 发布仅手动触发 `workflow_dispatch`，触发后自动检查、构建并推送；不因日常提交或 PR 自动推送镜像。
- 发布 Go 服务和带 Hook 的 Radicale 两个镜像，名称从小写 GitHub 仓库标识生成，Radicale 镜像追加 `-radicale`。
- 目标平台为 linux/amd64、linux/arm64，同时提供用户输入标签和提交 SHA 标签。
- `latest` 更新默认关闭，显式启用时仅允许从默认分支发布；`latest` 和 `sha-*` 为保留输入标签。
- 使用最小权限的 GITHUB_TOKEN，不要求额外 PAT，Action 引用固定到经过核实的提交 SHA。
- 不创建远程仓库、不自行推送源代码或实际发布镜像，不修改 GHCR 包可见性；当前交付是源码、部署文件和工作流。

## 验证要求

- 不使用本地 Docker 进行测试或验证，不启动本地容器、不构建或拉取测试镜像，也不通过本地 Docker Compose 启动测试环境。
- 不调用本地 Docker CLI 或 Docker 守护进程进行验证，包括 `docker compose config`；部署配置通过文件审查和不依赖 Docker 的静态检查验证。
- 本地测试使用 Go 工具链、临时目录、测试数据库和进程内替身服务，集成测试不得隐式依赖 Docker 或 Testcontainers。
- Go 代码提交前使用 `gofmt` 格式化。
- 根据实际改动运行相关测试；项目具备 Go 模块后，使用 `go test ./...` 执行测试。
- 集成验证覆盖日程新增、修改、删除、客户端并发冲突、队列故障、进程重启和渠道重试；声明支持重复日程后，补充时区和单次例外等场景。
- SMTP 和 Telegram 测试使用替身服务或模拟响应，避免测试发送真实通知。
- 不将 SMTP 密码、Telegram Bot Token 或其他凭据提交到代码仓库。

## 参考资料

- Radicale 文档：https://radicale.org/v3.html
- Radicale Hook 接口：https://github.com/Kozea/Radicale/blob/master/radicale/hook/__init__.py
- Radicale 插件加载：https://github.com/Kozea/Radicale/blob/master/radicale/utils.py
- Radicale PUT/DELETE/MOVE 处理：https://github.com/Kozea/Radicale/tree/master/radicale/app
- MCP 官方文档：https://modelcontextprotocol.io/
- Telegram Bot API：https://core.telegram.org/bots/api
- 上述 master 链接用于定位接口，实际兼容依据应记录为固定版本或提交。
