# OpenDesign 插件说明

本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork。插件 ID 与 provider 为 `open-design`；当前源码版本为 `0.4.16`（紧凑凭据卡片、官方窗口额度与时间直接可见、准确的串行查询进度），生产已本地构建热加载 `0.4.16`，GitHub 最新 Release 仍为 `open-design-v0.4.12`（及时复查额度恢复、策略／退避并发保护、精确美元换算与逐账户汇总；请求日志与账户控制台；额度耗尽自动暂停默认开启），`0.4.6` tag 不得使用。本 fork 不是 OpenDesign 官方发布，也不是 ClinePassBridge 的官方下游。`0.3.0` 已通过本地验收，并由提交 `2601888` 的成功 CI 产物升级 `nuoapi-net`；`0.4.0` 在其上增加重试调度／错误规则，并由提交 `e66497e` 的成功 CI 产物于 2026-11-04 完成生产升级；`0.4.1` 修复 Windows 上的请求日志持久化与跨平台测试问题，`0.4.2` 把其余持久化写入也纳入同一重试发布路径，`0.4.3` 让额度查询在打开「凭据 · 额度」时按账户顺序自动执行，`0.4.4` 识别 OpenDesign 的 `insufficient_balance` 额度耗尽报文并新增对应默认规则，`0.4.5` 新增每个模型独立的思考强度映射，并修复其快照一致性与控制台草稿问题；`0.4.7` 新增额度耗尽自动暂停。详见[验证记录](verification.md)。

## 范围

- 实际 Provider 位于 `internal/opendesign`。
- `internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。
- 使用 CPA 公开的 host HTTP、translation 和 public RPC；不修改、不打包、不发布 CPA 宿主。
- 额度查询按 OpenDesign 官方 control API 实现，可选配置独立 control Key；不使用 Cline 的额度、价格或周期规则。详见[额度说明](quota.md)。
- 模型级覆盖查询同样使用官方 `funding-check` 的 `modelId`，与共享额度缓存分开。
- 请求日志只保存元数据 allowlist；打开日志标签页读取一次日志与统计，之后只在刷新或切换筛选时请求；模型测试、覆盖检查、配置与 runtime 由人工在控制台触发。
- 管理页的账号选择用于模型发现，映射为插件共享；实际请求的账号选择与冷却遵循 CPA 公开接口。

## 协议

OpenDesign 生产接口：

| 项目 | 值 |
| --- | --- |
| Base URL | `https://amr-link.open-design.ai/v1` |
| 认证 | `Authorization: Bearer <key>` |
| 模型列表 | `GET /models` |
| 对话 | `POST /chat/completions` |

插件固定上述官方 origin，拒绝自定义初始请求地址；后续重定向由 CPA 宿主处理。CPA 8.0.48 本地跨域 307 测试确认跳转会被跟随、`Authorization` 会被剥离，不能据此声称插件禁止所有重定向。请求使用 OpenAI 兼容的 Chat Completions 结构。

协议行为参考：

- [AstroQore/opendesign2api](https://github.com/AstroQore/opendesign2api)：独立网关 prior art。仅参考公开协议行为，未复制源码。
- [nexu-io/open-design 的生产配置](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela.ts#L819-L842)：用于核对官方生产地址。

## 插件接口与数据

| 项目 | 值 |
| --- | --- |
| 管理接口 | `/v0/management/open-design` |
| 控制台 | `/v0/resource/plugins/open-design/console` |
| 默认数据目录 | `plugins/open-design-data` |
| 数据目录配置 | `data_dir` |

- 推理 API key 与可选 control Key 保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置不保存 Key，管理接口不回显。
- 管理接口按 CPA 精确路由契约提供：`GET /credentials` 列出账户；`POST /credentials` 创建或更新，body 为 `{id?, label?, api_key?, disabled?, control_key?, workspace_id?, credits_per_usd?, clear_billing?}`；`DELETE /credentials?id=<id>` 按查询参数删除，不使用 `/credentials/{id}`。编辑时 Key 留空保留原值，`clear_billing:true` 显式清除额度授权和工作区。
- 模型列表由管理页调用 OpenDesign `GET /models` 获取，支持搜索与可见项批量勾选。新增客户端别名默认以 `open-design-` 开头，已保存和手动编辑的别名保留。账户刷新不覆盖未保存的映射草稿。
- 模型映射保存在插件数据目录；获取的上游模型列表不持久化。请求日志保存在同一数据目录（`request-logs.snapshot.json` 与 `request-logs.journal.jsonl`），只含元数据。容器部署时应持久化 CPA 插件目录、CPA 的 `auth-dir` 和该数据目录。
- 管理页沿用 CPA 管理认证。手动输入的管理密钥按 CPA 管理页的现有方式处理，不写入 OpenDesign key 存储。

管理路由（均在 `/v0/management/open-design` 下）：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /status` | 插件状态、模型映射与账户的额度暂停字段（含暂停同步失败提示；不含任何 key 或指纹） |
| `GET` / `POST` / `DELETE /credentials` | 账户列表、创建或更新、删除（可选 `credits_per_usd` 每账户换算率） |
| `GET /upstream-models` | 读取 OpenDesign `GET /models` 候选 |
| `GET` / `POST /models` | 读取或保存模型映射 |
| `POST /models/reasoning` | 保存单个模型的思考强度映射（完整替换） |
| `GET /quota-pause` | 当前额度暂停列表（只读；顺带重试失败的本地写入，不查询官方额度） |
| `POST /quota-pause/resume` | 手动解除某个账户的额度暂停（只清插件标记） |
| `POST /quota-pause/recheck` | 对全部已知耗尽模型串行发起官方预检；全部通过才提前解除暂停 |
| `GET /quota?credential_id=<id>` | 共享钱包／套餐窗口缓存；`refresh=1` 手动查询 |
| `GET /model-coverage?credential_id=<id>&model_id=<id>` | 模型级官方覆盖缓存；`refresh=1` 手动查询 |
| `GET /request-logs` | 分页读取元数据日志；支持 `limit`／`offset`／`status`／`model`／`q`／`credential_id`／`attempted`／`since`／`until` |
| `GET /request-stats` | 汇总统计（含本地拒绝数、缓存命中率、平均输出速率、平均首字时间）；可选 `since`／`until` |
| `DELETE /request-logs` | 持久化清空日志 |
| `GET` / `PUT /error-rules` | 读取或保存错误规则策略并逐账号同步 |
| `POST /error-rules/sync` | 只重新推送当前策略 |
| `POST /error-rules/reset` | 恢复内置默认策略并推送 |
| `GET` / `POST /config` | 日志开关、保留天数与条数上限；额度暂停开关与单次上限 |
| `GET /scheduling` | 只读公开账号运行状态；回调不带模型冷却列表，不能还原模型范围 |
| `GET /runtime` | 插件可见运行时状态 |
| `POST /model-test` | 显式单次非流式模型测试 |

`/logs`、`/logs/stats`、`/logs/clear` 是日志路由的别名。

额度暂停在控制台的分工（限制说明，不是承诺）：

- 暂停详情（`quota_pause`）同时出现在 `GET /status`、`GET /credentials` 以及创建／更新凭据的响应里，字段一致且不含 key；`quotaPauseErrors` 只以固定的脱敏文案出现在 `error` 中，用于控制台的「同步待重试」徽标。
- 记录的时间只是复查提示：到点不会直接启用。自动暂停默认开启，后台会串行发起官方复查；控制台只按本地时钟更新倒计时，不自动读取后台恢复结果。`0.4.12` 可点「刷新状态」读取账户最新状态，不会触发上游额度查询。
- 所有已知耗尽模型都要有新鲜的可用结果且属于同一 Workspace 成员；失败、无授权或模型集合不完整均保留暂停。必须由管理员确认 controlKey 与推理 key 属于同一账号，插件无法自动验证。
- 暂停期间请在插件控制台启用／停用账户。插件把通过控制台作出的人工选择与自己的暂停投影分开保存，恢复时保留控制台的人工停用；CPA 原生入口写的是同一个 `disabled` 字段，无法可靠区分来源，两边同时改动可能互相覆盖。
- 内存与文件的合并按「保守」方向处理：live 有暂停而文件没有时保持暂停并重写文件；live 已解除并带修订号而文件仍是旧暂停 body 时按 live 修复且不回退；无修订号的旧 body 不带永久标记；解析、解除和文件恢复都会核对推理 key、control key 与 Workspace 身份，不属于当前凭据的标记不会被接受或复活。单进程写入是受支持的模型，没有跨进程 CAS。

## 控制台布局

`0.4.16` 的账号卡片默认折叠操作与长说明，但额度不折叠：左侧显示名称、ID、暂停／调度受限状态和时间，右侧直接显示官方窗口使用率、剩余 credits、可选美元换算和重置时间。窗口种类不套用 Cline 的周期；未知不补为 0。钱包与套餐的新鲜度或时间不同时分别标注；部分失败不挡住成功部分。点击卡片或用键盘 Enter／空格展开操作，重绘和翻页保留展开状态。

查询进度区分「待查询」「正在查询」「查询失败」及「上次数据」；顶部整批刷新可随时停止后续查询，已发出的请求仍会完成。单账户刷新与批量读取共用前端串行槽，不叠加查询。普通状态刷新不触发额度查询。复查时间、旧报错的重置／重试提示和 CPA 重试时间分别表达，不承诺到点恢复；公开账号标志不带模型范围，因此卡片只显示「调度受限」，不把聚合标志当作全账号不可用。

控制台固定为 4 个标签页（`0.5.0` 起原概览并入配置页，默认打开请求日志）。打开「凭据 · 额度」会读取一次调度快照，并按账户顺序串行查询额度；离开即停止后续额度查询。请求日志首次打开读取一次日志与统计，其他可选功能仍由按钮触发：

| 标签页 | 内容 |
| --- | --- |
| 请求日志 | 元数据日志（首字／耗时、用量、缓存占比、输出速率）、统计卡片与统计时段、筛选分页、清空 |
| 凭据管理 | 账户增删改（对话框）、可选额度授权、打开标签页时按账户顺序自动查询共享额度（也可手动刷新）、额度暂停徽标与手动解除、编辑重试调度／错误规则 |
| 模型映射 | 映射保存与行内测试、上游模型、按模型配置思考强度映射、模型级覆盖、显式模型测试 |
| 配置概览 | 日志开关与保留设置、额度暂停开关与单次上限、运行概览；宿主代理／路由／冷却只读；runtime 手动读取 |

## 重试调度／错误规则

`GET`／`PUT /error-rules` 读取或保存一组策略，`POST /error-rules/sync` 只重新推送，`POST /error-rules/reset` 恢复内置默认。策略保存在插件数据目录的 `error-rules.json`，写入为原子替换；文件缺失时使用默认策略，损坏时回退默认并在响应里上报 `load_error`。

- 规则形如「HTTP 状态码 + 包含／正则条件 → 动作」。状态码为硬匹配；条件支持子串与正则，正则无效或超出上限会在保存时拒绝。
- 动作有 `stop`、`stop-and-cooldown`、`continue`、`continue-and-cooldown` 四种，含义由 CPA 宿主的 request-scoped 机制执行。
- 匹配对象是**插件生成的错误 Message**，不是上游响应体。插件给上游错误、协议错误、传输错误、插件不可用和响应过大分别加上稳定的 `[upstream:<kind>]` 前缀；marker 只由封闭白名单拼出，上游文本无法伪造规则。
- 同步是覆盖式逐账号推送：`host.auth.save` 只重写每个凭据的 `request_scoped_errors` 一个键，保留宿主或运维写入的未知字段。每个账号独立报告结果，失败的账号列在响应中，可再次同步恢复。
- 空规则数组表示该账号不写插件自有规则，由 CPA 配置决定行为。
- 插件**不做并发限制、不做请求排队**：是否换号与是否冷却全部由宿主按规则决定。

## 额度耗尽自动暂停（默认开启）

识别上游额度耗尽并暂时阻断账户；控制台显示「额度耗尽 · 自动暂停 / 约 X 后复查」。下述首次复查调度和历史提示文案为 `0.4.12` 行为；该版本已于 2026-11-08 发布并热加载部署。

- 暂停期间请在本插件控制台启用／停用账户。插件把控制台的人工选择与自己的暂停投影分开保存，恢复时保留控制台的人工停用（隔离测试覆盖暂停中停用、暂停中启用和显式解除三种顺序）。CPA 原生界面的停用写的是同一个 `disabled` 字段，无法可靠归因；这是当前明确声明的限制。

- 暂停状态保存在凭据 auth 文件里（`quota_block` 与 `quota_revision` 两个插件自有字段），参照 ClinePass 的 QuotaBlock 做法；写入时先读取、比对并重读，再原子替换文件，只改这两个键和 `disabled`，尽量保留未知字段。插件进程内写入串行化，但跨进程没有 CAS：宿主或运维在最后一次读取与替换之间并发写入仍可能被覆盖。
- 插件通过 `AuthData.Disabled` 向宿主投影有效禁用状态；`quota_block.manual_disabled` 单独记录控制台操作员的选择。恢复只清除插件自己的暂停标记，不会把控制台停用（`manual_disabled=true`）的账户重新启用。CPA 原生界面的停用不经过该记录，仍可能被恢复路径覆盖。内存中的解除修订标记不跨进程重启；单进程写入是受支持的使用方式。
- 触发信号很窄：`error.code=insufficient_balance` 且状态为 `402`，或者同一机器码出现在 `2xx` 响应体/SSE 事件中且 `error.details.coding_plan_exhausted=true`。裸 `402`、文本里的 `payment required`、普通 `429` 与普通 `insufficient_quota` 都不会触发账户级暂停。
- 持久化截止时间只取结构化的 reset 字段（`error.details.reset_at`／`resets_at`／`resetAt`／`resetsAt`，RFC3339，允许 `resets at ` 前缀）或 `Retry-After`；缺省 30 分钟，钳制在 1 分钟～7 天之间，并受配置的 `pause.max_seconds` 上限约束。没有新 reset 提示的迟到重复错误只更新观察时间；带新提示的会重算，但不会超过 `BlockedAt + pause.max_seconds`。首次自动复查取这个截止时间与 `BlockedAt + 30 分钟` 中较早的时间，较早的上游提示或配置上限仍有效；旧暂停也适用，不修改凭据文件。`resume_at`／`remaining_seconds` 显示该首次复查时间，到点只进入串行队列，实际执行可能延后。控制台另显示历史提示 `estimate_at`（钳制在 7 天内），但它可能来自 `Retry-After`，不是实时恢复时间或恢复证明。
- 到期本身不解除暂停；启用自动暂停时，后台按账号和模型串行请求官方预检。只接受最新耗尽观察之后发起、生成时间新鲜的结果；所有模型均为 `available`、Workspace 成员一致、无同 key 推理在途且凭据／版本未变时才解除。缓存、失败、未授权和不完整模型集合均不能证明恢复。`0.4.12` 在提交恢复状态时会短暂拒绝同一推理 key 的新准入（包括其他凭据和 StorageJSON-only），避免其在在途检查之后进入；不阻塞其他 key，也不向上游发送被拒请求。
- 复查读取一律新发官方请求：不复用 60 秒的模型级缓存/节流窗口，也不加入控制台已发起的那次查询。加入旧请求会拿到「复查开始前就已发出」的响应，永远不能作为恢复证明。若同模型已有查询在途，复查先等它结束再自己发一次；等待受本次复查预算约束。
- 复查失败按 5 分钟起、每次翻倍、最长 30 分钟的退避重试，避免在数天的周额度窗口里反复打官方接口；手动复查不受退避限制。轮次在释放复查锁前保存结果与退避，已排队的自动轮次取得锁后会再次核对最新退避；跳过或取消不会清掉已有退避。退避记录保存在内存中，插件重启后重新开始调度；后台逐账号逐模型串行，账户多或官方请求慢时复查可能排队。
- 策略变更与恢复落盘按同一提交锁串行（管理保存与宿主重注册均适用）：关闭策略先完成时，旧自动轮次放弃；恢复已进入提交阶段时，该次恢复先完成，关闭请求随后返回。重复提交相同配置不作废正在检查的轮次，显式手动复查仍可在关闭自动策略时执行。
- `POST /quota-pause/recheck` 使用同样的复查与身份栅栏，可在到期前手动发起；`POST /quota-pause/resume` 由操作员确认后直接清除，不验证余额。
- 持久化失败时本地拦截立即生效（先发布内存状态再写盘），响应与列表里会带同步错误标记，后台任务持续重试。

**宿主边界**：上游 `402/403` 的冷却由 CPA 控制，现有宿主为 30 分钟。本插件不主动清除宿主冷却，也不承诺暂停／恢复对该冷却无副作用。CPA 原生人工停用无法与插件投影区分，是已知限制。

## 请求日志、统计与配置

日志条目只允许以下元数据：请求 ID、开始／结束时间、耗时、客户端模型、上游模型、凭据 ID、是否流式、状态、HTTP 状态、错误类型、输入／输出／总 token、`upstream_attempted`、思考强度及其映射，以及可选的 `first_token_ms`、`cached_tokens`、`reasoning_tokens`（`0.5.0` 新增，旧日志保持未知）。**不保存**请求/响应正文、headers、query 字符串、API key、control key 或 Workspace ID。

- 默认开启，保留 7 天、最多 500 条；可配置 1–90 天、1–500 条。超范围值在配置校验时拒绝。
- 额度暂停配置为 `pause.enabled`（未明确配置时**默认开启**）与 `pause.max_seconds`（60–604800，默认 86400）。`POST /config` 只接受插件自有的 `logs` 与 `pause` 两段，未知字段直接 400。
- 持久化为异步：25ms 合并写入，写失败后 2 秒重试；`plugin.quiesce`／`plugin.shutdown` 先取消延迟任务再同步 flush。
- 清空是持久化事务：先写入 clear tombstone（同时记录保留策略），再更新内存；崩溃恢复不会让旧日志复活。
- usage 可以是 `null`：响应没有对应 usage 时保持未知，不写成 0。
- 统计包含总数、成功／失败、流式、成功率、token 汇总、带／不带 usage、上游已尝试与本地拒绝，并按模型聚合。`attempted=yes|no` 可筛选已尝试上游与本地拒绝。
- `q` 是大小写不敏感的自由文本筛选，只匹配客户端模型、上游模型、凭据 ID、请求 ID 与错误类型；超过 128 字节或含控制字符返回 400。控制台以 `q` 承载搜索框，并把它与结果／账户筛选组合成同一请求。

`POST /config` 接受插件自有的 `logs` 与开发中的 `pause` 设置；上游地址、代理、路由与宿主冷却不能被插件修改。

## 官方模型覆盖

`GET /model-coverage?credential_id=<id>&model_id=<上游模型 ID>&refresh=1` 使用同一份 control Key 与 Workspace ID，向官方 `funding-check` 传入 `modelId`，只返回 `modelCovered`、`funding`、`fundingCheck`、`fetched_at`、`next_refresh_at` 与 `error`。

- 覆盖结果与 `quota` 的共享钱包／套餐窗口缓存**完全分离**，不伪造额度快照，也不把共享 credits 当成模型余额。
- 每份凭据与模型组合 60 秒内最多尝试一次，并发刷新合并；单次 30 秒总预算，超时通过宿主 operation/cancel 释放。
- 官方返回的 `workspaceId`、`modelId` 必须匹配，否则按 `malformed` 返回，不宽松接受。

## 显式模型测试

`POST /model-test` 由插件构造极小的非流式 Chat Completions 请求：

- 控制台只有点击「发送测试」并确认后才调用；一次操作只发一次请求，**不重试**。
- 最多 3 个并发测试，超出返回 429。
- 响应只含状态、上游 HTTP、耗时、token 计数与错误类型，**不回传 completion**，也不回显 Key。
- 可选传 `reasoning_effort`（不含配置哨兵 `unset`），它会先套用该模型已保存的思考强度映射，再发往上游；响应回显实际发出的强度与命中的转换。省略该字段时请求体与旧版本完全一致。
- 测试会消耗真实推理用量；账户 Key、映射或启停状态在测试期间变化时，结果不写入 runtime 历史。

## runtime 与宿主能力

`GET /runtime` 只公开插件能验证的宿主能力：插件加载状态、在飞请求、待处理操作、映射模型数，以及 `host.auth.list` 能提供的账户状态。宿主未提供该能力时，响应标记 `host.available=false` 并提示“仅插件侧状态”；插件不推断或伪造 CPA 的调度、冷却、代理池行为。

## 模型映射

1. 在控制台添加 OpenDesign API key。
2. 点击获取模型，读取生产地址的 `GET /models`。
3. 勾选模型，保存客户端模型名与上游模型 ID 的映射。
4. 客户端向 CPA 的 `/v1/chat/completions` 发送请求，`model` 使用已保存的客户端模型名。

未配置的客户端模型名不会自动透传为上游模型 ID。

## 思考强度映射

每个模型可以单独配置一份「请求传入 → 发往上游」的思考强度映射，默认关闭：

- 只处理**顶层** `reasoning_effort`。请求里的 `thinking`／`reasoning` 等嵌套字段原样转发，不被改写。
- 只改写表里列出的强度。没有规则的强度（包括空值没有规则时）原样透传，插件不会猜一个默认值。
- 目标可选 `unset`：命中时删除 `reasoning_effort` 字段，让上游使用自己的默认强度。与 `thinking`／`reasoning` 一样，它不会去删除嵌套字段。
- 请求本身带了 `thinking`／`reasoning` 且没有传 `reasoning_effort` 时，`unset → unset` 这类规则不生效，避免与客户端的显式意图冲突。
- 上游 ID 含 `deepseek` 的模型默认预填 DeepSeek 强度表（`minimal/low → low`、`medium/high/xhigh → high`、`max/ultra → max`），仍然需要手动启用。
- 来源集合为 `unset`/`none`/`minimal`/`low`/`medium`/`high`/`xhigh`/`max`/`ultra`，目标集合相同但不含 `ultra`；超出集合的值会被接口拒绝，不会写入报文。
- 插件收到的请求值不是字符串（例如数字）时不做映射；不同 CPA 宿主可能在插件执行前对请求转换，不能保证客户端原始值未经宿主处理就到达插件或上游。

保存是「完整替换」：`POST /models/reasoning` 带 `model`、`upstream_id` 与整个 `reasoning_mapping`。模型不存在返回 404，上游 ID 与当前映射不一致返回 409（要求先刷新状态再配置），规则不合法返回 400。

模型列表保存支持整表乐观锁：`GET /status`、`GET /models` 与 `POST /models` 响应都会返回 `models_revision`，它是完整模型列表（含每个模型的思考强度策略）的确定性摘要。`POST /models` 可携带 `expected_models_revision`；与当前摘要不一致时整单返回 409，且不写盘、不发布，适用于删除、新增、改名与改上游等所有列表变更。省略该字段时保持旧的兼容语义。

整表 token 与原始身份：只改 alias（`upstream_id` 不变）时，行可带 `original_id` 与 `original_upstream_id` 指向快照中的原模型，策略默认保留；只改 alias 且不提供原始身份会被当作新行，不会猜测旧策略。改 `upstream_id` 时旧策略默认清除；若要把原策略显式带到新上游，必须同时提供原始身份并设置 `carry_reasoning_mapping: true`。只回显旧策略但不设 opt-in 会返回 409，而不是静默迁移或静默丢弃。原始身份必须精确对应整表 token 指向的快照（id 与 upstream 同时匹配），不能跨快照冒用，也不允许冲突别名复用他人原始身份。

行 token 与「无法识别的拷贝意图」：在带 `expected_models_revision` 的整表保存中，如果某行仍带非空 `reasoning_revision`，但它提交的 alias 已不在当前列表里（改名或直接换行），且该行还显式提交了 `reasoning_mapping`，则整单返回 409（不写盘、不发布），而不是把这个策略静默绑定到新 alias／新上游。此时后端无法判断那份 mapping 是「真实的新配置」还是「把旧策略照抄过来」：内容可能与旧行恰好相同，也可能与列表里任意一行的策略相同，因此不能按内容判定，也不能据此推断是拷贝还是本意。可选的解除方式只有两种：补上 `original_id`／`original_upstream_id`（改上游时再加 `carry_reasoning_mapping: true`）走显式身份，或去掉该行的 `reasoning_revision` 表示这就是一行新模型。

兼容性边界（以下行为保持不变）：不带行 token 的新行始终是普通新增，其显式 `reasoning_mapping` 视为新策略意图并原样保存，即使内容与现有某行的策略完全相同也不拒绝（无法区分真实新配置）；缺省 `reasoning_mapping`、缺省行 token、缺省 `expected_models_revision` 的旧客户端请求语义不变；显式原始身份 + `carry_reasoning_mapping` 仍按上文允许。换句话说，本限制只在「行 token 指向的目标 alias 已不存在，且该行仍显式提交 mapping」这一种组合下触发。

`GET /models` 行内的 `reasoning_revision` 仍可用于同别名（未改名）时的单模型保护。没有 `expected_models_revision` 的旧调用者保持原有严格行为：非空行 token 必须仍匹配当前同别名模型，已删除或已改名的别名不能被旧 token 复活；显式提交的 `reasoning_mapping` 仍按原来的 last-writer-wins 语义保存。

映射写入 settings，对以下路径生效：正式请求（`executor.execute`／`executor.execute_stream`）、控制台的显式模型测试。模型覆盖探测走的是官方计费接口的 GET，没有请求体，因此不涉及思考强度。

请求日志会记录「实际发出的强度」与「命中的转换」（如 `medium → high`）两个只读字段，值都取自同一个闭集；超出集合的值在写入和读取时都会被丢弃。

## 构建与安装

需要 Go 1.26、CGO 和 C 编译器。

```bash
# 运行管理页及保留的上游 Node 测试
make test-js

# 运行 Node + Go 测试
make test

# 生成 dist/open-design.so（Linux amd64，C shared ABI v1）
make build

# 构建并运行原生 ABI、本地假上游验证
make smoke

# 或使用 Dockerfile.build
make docker
```

安装到建议位置：

```bash
install -Dm755 dist/open-design.so plugins/linux/amd64/open-design.so
```

使用前在 CPA 配置中启用插件系统，并启用本插件：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    open-design:
      enabled: true
      data_dir: plugins/open-design-data
```

只设置 `plugins.enabled` 不会加载本插件，`plugins.configs.open-design.enabled` 必须为 `true`。

安装后的操作顺序：

1. 打开控制台 `/v0/resource/plugins/open-design/console`，用 CPA 管理认证登录。
2. 在「凭据 · 额度」添加 OpenDesign 推理 API key；如需共享额度，再填同一账号的 control Key 与 Workspace ID。打开该标签页会按账户顺序自动查询一次额度。
3. 在「模型映射 · 覆盖」拉取上游模型、勾选并保存映射。
4.（可选）在同一页对单个模型「检查覆盖」，或确认后「发送测试」。
5.（可选）打开「请求日志」会读取一次日志与统计，之后用搜索／结果／账户筛选、翻页或「刷新」重查；在「代理池 · 配置」调整日志保留策略或读取 runtime。

## 当前验证边界

- `0.4.11` 暂停倒计时与额度换算：暂停记录新增 `estimate_at`（上游报错里的原始重置时间，钳制在 7 天内，不随 `pause.max_seconds` 缩短），`GET /status` 与凭据响应的 `quota_pause` 会带上它；控制台倒计时显示「N 后复查 · 预计 M 后恢复」，复查节奏与到点后的「等待复查」状态不变。账户新增可选 `credits_per_usd`（每账户独立，可留空清除），控制台在剩余额度旁显示 `≈ $N`，用 BigInt 精确换算，未填写时只显示 credits。新增 Go 用例覆盖 estimate_at 的保留／钳制／缺失与换算率的逐账户校验与清除；Node 用例覆盖倒计时文案、BigInt 换算与汇总／行内显示；Go／Node 全量通过。
- `0.4.10` 调整控制台账户列表：暂停账户在活跃账户之后按 `/status` 的 `quota_pause.resume_at` 升序排列（无可用恢复时间者排最后并保持原有相对顺序），活跃账户保持操作者顺序；列表上方新增「当前可用总额度」，只汇总启用且未暂停账户的窗口剩余 credits（按 `durationSeconds` 分组）与钱包美元余额，并显示已查询／待查询账户数。新增 Node 用例覆盖排序（含并列与未知时间）、总额排除暂停／停用账户、按窗口求和、钱包汇总与待查询提示；Go／Node 全量通过。
- `0.4.9` 改版控制台「请求日志」页：新增自由文本搜索（后端 `q`，只匹配安全元数据字段）、结果／账户筛选、每页条数、四张统计卡片、可展开日志行与分页；失败行红色标记，详情展示请求 ID、起止时间、HTTP 状态与 token 明细。打开标签页只读取一次日志与统计。新增 Go 用例覆盖 `q` 的大小写不敏感匹配、无匹配、控制字符与超长拒绝；Node 用例覆盖搜索参数、结果筛选映射、自动读取一次、展开状态与格式化。Go／Node 全量通过。
- `0.4.8` 改版控制台账户列表：暂停账户按 `/status` 的 `quota_paused` 自动排到末尾并用红色卡片显示，不需要先查询额度；列表每页 10 个账户，支持翻页、按名称或 ID 搜索、点击暂停徽标跳转；额度详情默认折叠，行内摘要显示钱包余额与各窗口使用率/重置时间。新增 Node 用例覆盖排序、跳过暂停账户的自动查询、分页、搜索、详情展开状态和摘要；Go／Node 全量通过。
- `0.4.7` 新增额度耗尽自动暂停（默认开启）及思考强度映射；暂停信号、人工停用保护、解除修订和旧文件回放由 Go／Node、独立复核及两套隔离宿主覆盖。CPA 原生界面的重复停用仍无法可靠归因是已知限制。思考映射按模型配置、默认关闭；未配置映射时请求体与 `0.4.4` 完全一致。2026-11-06 生产上线后已观察到真实结构化额度耗尽事件，自动复查恢复仍待生产到期窗口验证。

- `0.4.4` 把 OpenDesign 的额度耗尽报文（`402` + `insufficient_balance` 机器码）纳入分类白名单并新增默认规则：命中即换号冷却；不带机器码的裸 402 仍不分类。`0.4.3` 让「凭据 · 额度」标签页在打开时按账户顺序串行查询一次额度（不并发，离开即停止，60 秒内不重复请求）；`0.4.2` 把请求日志、快照、settings.json 与错误规则文件的持久化统一到同一条带有限重试的发布路径；`0.4.1` 修复 Windows 上请求日志持久化在重命名目标仍被占用时失败的问题（先关闭源句柄 + 有限重试），并让依赖 POSIX 权限语义的两处测试在 Windows 上使用等价注入。`0.4.4` 之前的行为与 `0.4.0` 相同；`0.4.4` 是 `0.4.0` 之后唯一改变分类结果的改动。
- `0.4.0` 在 `0.3.0` 之上新增重试调度／错误规则；Go／race／vet、Node 测试、离线 ABI 冒烟与原版 CPA 隔离流程通过，并用双账号真实宿主验证了换号与冷却；详细证据见[验证记录](verification.md)。
- `0.3.0` 全仓 Go／race／vet、Node 139 项、Edge 540 项、Debian 12 构建及原版 CPA 隔离流程通过；详细产物哈希与 CI／上线状态见[验证记录](verification.md)。
- 2026-11-02 已用有效推理 Key 发现 13 个真实上游模型。官方直连和生产 CPA 插件各完成一次最小非流式请求，均返回 `OK`；生产路径使用 `open-design-minimax-m3.1-flash`，usage 219／3／222。这是 `0.2.0` 的历史生产事实，不属于 `0.3.0` 验收。
- 新增额度使用官方 control API。现有推理 Key 查询控制面返回 401 后，另用两份独立 control 授权直接查询钱包和对应工作区 funding-check，四次 GET 均为 200。2026-11-02 上线 `0.2.0` 后，od1 授权也已通过生产插件查询成功；账户归属以完整推理 Key 在服务器内精确匹配确认。消费趋势预测尚未实测。
- 本地测试使用原版 CPA 8.0.48 与 loopback CONNECT/TLS 假上游，不修改系统信任库或 hosts；只允许推理、控制面与重定向测试主机。
- 具体 Go／Node、Edge、真实宿主回归结果见[验证记录](verification.md)；[验证脚本说明](../scripts/README.md)区分 ctypes、原版宿主与假上游的边界。

## 发布 tag

- 发布 tag 使用 `open-design-vX.Y.Z` 形式，不创建或覆盖上游 `v*` tag；不使用或复用不安全的 `open-design-v0.4.6`。
- 工作流触发 `open-design-v*`，去掉前缀后与 `internal/opendesign` 的 `Version` 和 registry 版本比对，三者必须同步（当前源码 `0.4.16`，尚未发布）。
- CPA 插件商店解析 GitHub latest release 时要求 tag 以数字开头，因此从自有源安装时需显式指定已发布且适合当前环境的 tag/版本。
- `open-design-v0.4.11` 已发布并标记 Latest，五平台构建全绿（[release run 42341153176](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/42341153176)），首次运行即完成。`open-design-v0.4.10` 已发布并标记 Latest，五平台构建全绿（[release run 42273570550](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/42273570550)）；前三次运行因 GitHub 未分配 `macos-15` arm64 runner 被取消（各排队约 15 分钟），第四次重跑取得 runner 后完成。`open-design-v0.4.9` 已发布并标记 Latest，五平台构建全绿（[release run 42206328455](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/42206328455)）；首次运行因 GitHub 未分配 `macos-15` arm64 runner 被取消两次（各排队 15 分钟），第三次重跑取得 runner 后完成。`open-design-v0.4.7` 已发布并标记 Latest，五平台构建全绿（[release run 42167937013](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/42167937013)）；`open-design-v0.4.4` 已从 draft 转为正式 Release。`0.4.5` 与 `0.4.6` 的发布运行未产出可用资产，`0.4.6` 禁止使用；`0.4.0` 的 tag 因 Windows 平台构建失败未产出 Release。分支与生产交付状态见验证记录。

## 上游与许可

- 上游仓库：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 许可证：[MIT](../LICENSE)，保留原版权声明并附带本 fork 的派生版权声明。
- `docs/` 下的其他文档保留为上游参考，本 fork 未逐篇改写。
