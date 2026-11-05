# OpenDesign 插件说明

本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork。插件 ID 与 provider 为 `open-design`；生产版本为 `0.4.4`，`0.4.6` 为未通过安全复核的开发版本，禁止部署。本 fork 不是 OpenDesign 官方发布，也不是 ClinePassBridge 的官方下游。`0.3.0` 已通过本地验收，并由提交 `2601888` 的成功 CI 产物升级 `nuoapi-net`；`0.4.0` 在其上增加重试调度／错误规则，并由提交 `e66497e` 的成功 CI 产物于 2026-11-04 完成生产升级；`0.4.1` 修复 Windows 上的请求日志持久化与跨平台测试问题，`0.4.2` 把其余持久化写入也纳入同一重试发布路径，`0.4.3` 让额度查询在打开「凭据 · 额度」时按账户顺序自动执行，`0.4.4` 识别 OpenDesign 的 `insufficient_balance` 额度耗尽报文并新增对应默认规则，`0.4.5` 新增每个模型独立的思考强度映射，并修复其快照一致性与控制台草稿问题；`0.4.6` 新增额度耗尽自动暂停。详见[验证记录](verification.md)。

## 范围

- 实际 Provider 位于 `internal/opendesign`。
- `internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。
- 使用 CPA 公开的 host HTTP、translation 和 public RPC；不修改、不打包、不发布 CPA 宿主。
- 额度查询按 OpenDesign 官方 control API 实现，可选配置独立 control Key；不使用 Cline 的额度、价格或周期规则。详见[额度说明](quota.md)。
- 模型级覆盖查询同样使用官方 `funding-check` 的 `modelId`，与共享额度缓存分开。
- 请求日志只保存元数据 allowlist；模型测试与日志、统计、覆盖、配置、runtime 都由人工在控制台触发。
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
- 管理接口按 CPA 精确路由契约提供：`GET /credentials` 列出账户；`POST /credentials` 创建或更新，body 为 `{id?, label?, api_key?, disabled?, control_key?, workspace_id?, clear_billing?}`；`DELETE /credentials?id=<id>` 按查询参数删除，不使用 `/credentials/{id}`。编辑时 Key 留空保留原值，`clear_billing:true` 显式清除额度授权和工作区。
- 模型列表由管理页调用 OpenDesign `GET /models` 获取，支持搜索与可见项批量勾选。新增客户端别名默认以 `open-design-` 开头，已保存和手动编辑的别名保留。账户刷新不覆盖未保存的映射草稿。
- 模型映射保存在插件数据目录；获取的上游模型列表不持久化。请求日志保存在同一数据目录（`request-logs.snapshot.json` 与 `request-logs.journal.jsonl`），只含元数据。容器部署时应持久化 CPA 插件目录、CPA 的 `auth-dir` 和该数据目录。
- 管理页沿用 CPA 管理认证。手动输入的管理密钥按 CPA 管理页的现有方式处理，不写入 OpenDesign key 存储。

管理路由（均在 `/v0/management/open-design` 下）：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /status` | 插件状态、模型映射与账户的额度暂停字段 |
| `GET` / `POST` / `DELETE /credentials` | 账户列表、创建或更新、删除 |
| `GET /upstream-models` | 读取 OpenDesign `GET /models` 候选 |
| `GET` / `POST /models` | 读取或保存模型映射 |
| `POST /models/reasoning` | 保存单个模型的思考强度映射（完整替换） |
| `GET /quota-pause` | 当前额度暂停列表（只读；顺带做一次到期清扫） |
| `POST /quota-pause/resume` | 手动解除某个账户的额度暂停（只清插件标记） |
| `POST /quota-pause/recheck` | 用一次官方模型预检证明额度恢复后提前解除暂停 |
| `GET /quota?credential_id=<id>` | 共享钱包／套餐窗口缓存；`refresh=1` 手动查询 |
| `GET /model-coverage?credential_id=<id>&model_id=<id>` | 模型级官方覆盖缓存；`refresh=1` 手动查询 |
| `GET /request-logs` | 分页读取元数据日志 |
| `GET /request-stats` | 汇总统计（含本地拒绝数） |
| `DELETE /request-logs` | 持久化清空日志 |
| `GET` / `PUT /error-rules` | 读取或保存错误规则策略并逐账号同步 |
| `POST /error-rules/sync` | 只重新推送当前策略 |
| `POST /error-rules/reset` | 恢复内置默认策略并推送 |
| `GET` / `POST /config` | 日志开关、保留天数与条数上限；额度暂停开关与单次上限 |
| `GET /runtime` | 插件可见运行时状态 |
| `POST /model-test` | 显式单次非流式模型测试 |

`/logs`、`/logs/stats`、`/logs/clear` 是日志路由的别名。

## 控制台布局

控制台固定为 5 个标签页。切换标签页不会自动发起请求，唯一例外是「凭据 · 额度」：打开该标签页会按账户顺序逐个查询一次额度（串行，不并发），离开标签页即停止；其余路由仍只由按钮手动触发：

| 标签页 | 内容 |
| --- | --- |
| 概览 | 账户与映射数量 |
| 凭据 · 额度 | 账户增删改、可选额度授权、打开标签页时按账户顺序自动查询共享额度（也可手动刷新）、额度暂停徽标与手动解除、编辑重试调度／错误规则 |
| 模型映射 · 覆盖 | 上游模型、映射保存、按模型配置思考强度映射、模型级覆盖、显式模型测试 |
| 请求日志 | 元数据日志、汇总统计、筛选分页、清空 |
| 代理池 · 配置 | 日志开关与保留设置、额度暂停开关与单次上限；宿主代理／路由／冷却只读；runtime 手动读取 |

## 重试调度／错误规则

`GET`／`PUT /error-rules` 读取或保存一组策略，`POST /error-rules/sync` 只重新推送，`POST /error-rules/reset` 恢复内置默认。策略保存在插件数据目录的 `error-rules.json`，写入为原子替换；文件缺失时使用默认策略，损坏时回退默认并在响应里上报 `load_error`。

- 规则形如「HTTP 状态码 + 包含／正则条件 → 动作」。状态码为硬匹配；条件支持子串与正则，正则无效或超出上限会在保存时拒绝。
- 动作有 `stop`、`stop-and-cooldown`、`continue`、`continue-and-cooldown` 四种，含义由 CPA 宿主的 request-scoped 机制执行。
- 匹配对象是**插件生成的错误 Message**，不是上游响应体。插件给上游错误、协议错误、传输错误、插件不可用和响应过大分别加上稳定的 `[open-design:<kind>]` 前缀；marker 只由封闭白名单拼出，上游文本无法伪造规则。
- 同步是覆盖式逐账号推送：`host.auth.save` 只重写每个凭据的 `request_scoped_errors` 一个键，保留宿主或运维写入的未知字段。每个账号独立报告结果，失败的账号列在响应中，可再次同步恢复。
- 空规则数组表示该账号不写插件自有规则，由 CPA 配置决定行为。
- 插件**不做并发限制、不做请求排队**：是否换号与是否冷却全部由宿主按规则决定。

## 额度耗尽自动暂停（开发中，不可部署）

当前实现尝试识别上游额度耗尽并暂时阻断账户，但真实 CPA 宿主的人工停用保护仍有缺陷，不应作为已完成或可用于生产的功能。

- 暂停状态保存在凭据 auth 文件里（`quota_block` 与 `quota_revision` 两个插件自有字段），参照 ClinePass 的 QuotaBlock 做法；写入是「读—比对—原子替换」，只改这两个键和 `disabled`，保留宿主或运维写入的未知字段。
- 当前实现通过 `AuthData.Disabled` 向宿主投影有效禁用状态；CPA 可能将其写回文件。暂停期间通过 CPA 原生界面再次停用时，插件无法可靠区分宿主投影和人工选择，恢复时可能错误地重新启用账户。该问题尚待重新设计和宿主验证。
- 触发信号很窄：`error.code=insufficient_balance` 且状态为 `402`，或者同一机器码出现在 `2xx` 响应体/SSE 事件中且 `error.details.coding_plan_exhausted=true`。裸 `402`、文本里的 `payment required`、普通 `429` 与普通 `insufficient_quota` 都不会触发账户级暂停。
- 截止时间只取结构化的 `error.details.reset_at`（RFC3339，允许 `resets at ` 前缀）或 `Retry-After`；缺省 30 分钟，钳制在 1 分钟～7 天之间，并受配置的 `pause.max_seconds` 上限约束。迟到的重复错误只更新观察时间，不会把截止时间一次次往后推。
- 到期恢复由后台任务（30 秒 ticker）与管理读路径的清扫完成；两者都只做本地到期与失败重试，**不发起任何额度网络请求**。额度标签页的自动查询仍然是串行的，不新增后台轮询。
- 提前恢复有两条显式路径：`POST /quota-pause/recheck` 用一次官方模型级预检，只有在暂停被观察到之后又拿到 `funding_check=available` 才清除；`POST /quota-pause/resume` 由操作员确认后直接清除。多个模型都报告过耗尽时，单次预检不构成证明，会返回 409。
- 持久化失败时本地拦截立即生效（先发布内存状态再写盘），响应与列表里会带同步错误标记，后台任务持续重试。

**宿主边界**：上游 `402/403` 触发的是 CPA 宿主硬编码的 30 分钟冷却，插件无法提前结束；即使插件提前解除暂停，CPA 仍可能在剩余冷却期内不选中该账户。当前实现还不能保证 CPA 原生人工停用不会被恢复路径覆盖，禁止部署。

## 请求日志、统计与配置

日志条目只允许以下元数据：请求 ID、开始／结束时间、耗时、客户端模型、上游模型、凭据 ID、是否流式、状态、HTTP 状态、错误类型、输入／输出／总 token、`upstream_attempted`。**不保存**请求/响应正文、headers、query 字符串、API key、control key 或 Workspace ID。

- 默认开启，保留 7 天、最多 500 条；可配置 1–90 天、1–500 条。超范围值在配置校验时拒绝。
- 开发分支的额度暂停配置为 `pause.enabled`（未明确配置时默认关闭）与 `pause.max_seconds`（60–604800，默认 86400）。已推送的 `0.4.6` tag 仍默认开启且不安全，不要使用。`POST /config` 只接受插件自有的 `logs` 与 `pause` 两段，未知字段直接 400。
- 持久化为异步：25ms 合并写入，写失败后 2 秒重试；`plugin.quiesce`／`plugin.shutdown` 先取消延迟任务再同步 flush。
- 清空是持久化事务：先写入 clear tombstone（同时记录保留策略），再更新内存；崩溃恢复不会让旧日志复活。
- usage 可以是 `null`：响应没有对应 usage 时保持未知，不写成 0。
- 统计包含总数、成功／失败、流式、成功率、token 汇总、带／不带 usage、上游已尝试与本地拒绝，并按模型聚合。`attempted=yes|no` 可筛选已尝试上游与本地拒绝。

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
- 请求值不是字符串（例如数字）时不做映射，原样交给上游校验。

保存是「完整替换」：`POST /models/reasoning` 带 `model`、`upstream_id` 与整个 `reasoning_mapping`。模型不存在返回 404，上游 ID 与当前映射不一致返回 409（要求先刷新状态再配置），规则不合法返回 400。

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
5.（可选）在「请求日志」手动刷新日志与统计，在「代理池 · 配置」调整日志保留策略或读取 runtime。

## 当前验证边界

- `0.4.6` 新增额度耗尽自动暂停（信号判定、钳制、人工停用保护、持久化与恢复路径）；`0.4.5` 新增思考强度映射（每个模型一份「请求传入 → 发往上游」的表，默认关闭）并修复其快照一致性与控制台草稿问题；未配置映射时请求体与 `0.4.4` 完全一致。

- `0.4.4` 把 OpenDesign 的额度耗尽报文（`402` + `insufficient_balance` 机器码）纳入分类白名单并新增默认规则：命中即换号冷却；不带机器码的裸 402 仍不分类。`0.4.3` 让「凭据 · 额度」标签页在打开时按账户顺序串行查询一次额度（不并发，离开即停止，60 秒内不重复请求）；`0.4.2` 把请求日志、快照、settings.json 与错误规则文件的持久化统一到同一条带有限重试的发布路径；`0.4.1` 修复 Windows 上请求日志持久化在重命名目标仍被占用时失败的问题（先关闭源句柄 + 有限重试），并让依赖 POSIX 权限语义的两处测试在 Windows 上使用等价注入。`0.4.4` 之前的行为与 `0.4.0` 相同；`0.4.4` 是 `0.4.0` 之后唯一改变分类结果的改动。
- `0.4.0` 在 `0.3.0` 之上新增重试调度／错误规则；Go／race／vet、Node 测试、离线 ABI 冒烟与原版 CPA 隔离流程通过，并用双账号真实宿主验证了换号与冷却；详细证据见[验证记录](verification.md)。
- `0.3.0` 全仓 Go／race／vet、Node 139 项、Edge 540 项、Debian 12 构建及原版 CPA 隔离流程通过；详细产物哈希与 CI／上线状态见[验证记录](verification.md)。
- 2026-11-02 已用有效推理 Key 发现 13 个真实上游模型。官方直连和生产 CPA 插件各完成一次最小非流式请求，均返回 `OK`；生产路径使用 `open-design-minimax-m3.1-flash`，usage 219／3／222。这是 `0.2.0` 的历史生产事实，不属于 `0.3.0` 验收。
- 新增额度使用官方 control API。现有推理 Key 查询控制面返回 401 后，另用两份独立 control 授权直接查询钱包和对应工作区 funding-check，四次 GET 均为 200。2026-11-02 上线 `0.2.0` 后，od1 授权也已通过生产插件查询成功；账户归属以完整推理 Key 在服务器内精确匹配确认。消费趋势预测尚未实测。
- 本地测试使用原版 CPA 8.0.48 与 loopback CONNECT/TLS 假上游，不修改系统信任库或 hosts；只允许推理、控制面与重定向测试主机。
- 具体 Go／Node、Edge、真实宿主回归结果见[验证记录](verification.md)；[验证脚本说明](../scripts/README.md)区分 ctypes、原版宿主与假上游的边界。

## 发布 tag

- 发布 tag 使用 `open-design-v0.4.6`，不创建或覆盖上游 `v*` tag。
- 工作流触发 `open-design-v*`，去掉前缀后与 `internal/opendesign` 的 `Version` 和 registry 版本比对。
- CPA 插件商店解析 GitHub latest release 时要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本。
- `open-design-v0.4.4` 已发布 GitHub Release；`open-design-v0.4.5` 的 Release 因 macOS runner 被取消而未产出资产，其修复内容并入 `0.4.6` 重新发布；`0.4.0` 的 tag 因 Windows 平台构建失败未产出 Release，分支与生产交付状态见验证记录。

## 上游与许可

- 上游仓库：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 许可证：[MIT](../LICENSE)，保留原版权声明并附带本 fork 的派生版权声明。
- `docs/` 下的其他文档保留为上游参考，本 fork 未逐篇改写。
