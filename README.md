# OpenDesign CPA 插件

把 OpenDesign 的 OpenAI 兼容接口接入 [CLIProxyAPI（CPA）](https://github.com/router-for-me/CLIProxyAPI)。

> 本仓库是 [ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge) v0.4.3（commit `8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）的独立 fork，保留 `upstream` 远程用于对照和同步。插件 ID 与 provider 为 `open-design`，当前版本 `0.4.2`。这不是 OpenDesign 官方发布，也不代表 ClinePassBridge 上游。
>
> `0.3.0` 已完成本地 Go／Node、Microsoft Edge、原版 CPA 隔离验收及 `nuoapi-net` 生产升级；精确提交、CI 和上线证据见[验证记录](docs/verification.md)。
>
> `0.4.0` 在 `0.3.0` 之上增加可编辑的重试调度／错误规则，已于 2026-11-04 部署到 `nuoapi-net`；`0.4.1` 修复 Windows 上请求日志持久化与跨平台测试；`0.4.2` 把 settings.json 与错误规则文件也纳入同一条重试发布路径，并锁定裸 402 不并入额度分类。验收记录同样在该文档。

## 功能

`0.4.2` 把插件内所有持久化重命名（请求日志、快照、settings.json、错误规则文件）统一到同一条带有限重试的发布路径，避免 Windows 上目标文件被占用时保存失败；并补两条用例锁定裸 402 不并入额度分类（只有带 `insufficient_quota` 码的 402 才换号冷却）。

`0.4.1` 修复 `0.4.0` 在 Windows 上请求日志持久化会因「目标文件仍被占用」而失败的问题：重命名前先关闭源文件句柄，并对占用冲突做有限重试；同时让两处依赖 POSIX 权限语义的测试在 Windows 上使用等价注入。

`0.4.0` 于 2026-11-04 从提交 `e66497e` 的成功 CI 产物部署到 `nuoapi-net`；库哈希与本地验收库完全一致，服务、控制台、错误规则同步和原有效插件核验通过。

GitHub Release 使用 `open-design-v0.4.2`（`0.4.0` 的 tag 因 Windows 平台构建失败未产出 Release；`0.4.1` 已发布）。

`0.3.0` 于 2026-11-03 从提交 `2601888` 的成功 CI 产物部署到 `nuoapi-net`；库哈希与本地验收库完全一致，服务、控制台和原有效插件核验通过。

`0.2.0` 于 2026-11-02 从提交 `9fd8351` 及其成功 CI 部署，保留为历史记录。

`0.4.0` 在 `0.3.0` 之上新增**重试调度／错误规则**：

- 每个 OpenDesign 账户可配置一组错误规则（HTTP 状态码 + 包含／正则条件 + `stop`／`stop-and-cooldown`／`continue`／`continue-and-cooldown` 四种动作），随凭据写入宿主 auth 文件的 `request_scoped_errors` 字段。
- 规则匹配对象是**插件生成的错误 Message**，不是上游原始响应体；插件为上游错误、协议错误、传输错误、插件不可用和响应过大生成稳定的 `[open-design:<kind>]` 前缀，marker 只由封闭白名单拼出，上游文本无法伪造规则。
- 管理路由 `GET`／`PUT /error-rules` 与 `POST /error-rules/sync`、`POST /error-rules/reset`；策略保存在 `<data_dir>/error-rules.json`，写入为原子替换。
- 同步是**覆盖式逐账号推送**，只重写 `request_scoped_errors` 一个键，保留宿主或运维在 auth 文件中新增的未知字段；每个账号独立报告成功或失败，失败可重试恢复。
- 不做并发限制、不做请求排队：插件只做错误分类与标记，是否换号、是否冷却由 CPA 宿主按规则决定。

`0.3.0` 在 `0.2.0` 的账户、映射与额度能力之上增加以下功能：

- 添加、编辑、启停和删除 OpenDesign 账户，支持替换推理 API key；编辑时留空保留原 Key。
- 一次点击获取官方 `GET /models` 列表，支持搜索、批量勾选、共享模型映射与未保存草稿保护。新增映射默认使用 `open-design-` 前缀。
- 可选配置独立 control Key，手动查询官方账号钱包（USD）与工作区套餐窗口（credits）。新增和编辑账户均可填写两个可选字段：control Key、Workspace ID。实际剩余量与重置时间直接显示；符合采样条件的整体 credits 趋势默认折叠，不换算为单模型剩余 token 或请求数。详见[额度说明](docs/quota.md)。
- 新增**官方模型级覆盖查询**（`GET /model-coverage`）：按官方 `funding-check` 的 `modelId` 读取某个模型的覆盖与资金检查结果，与共享钱包／套餐窗口额度分开展示，手动触发、每模型凭据组合 60 秒节流、30 秒预算。
- 新增**请求日志与统计**，只保存元数据 allowlist，不保存请求/响应正文、headers、query、API key、control key 或 Workspace ID；默认开启，保留 7 天、最多 500 条，上限 90 天／500 条。异步持久化 25ms 合并、失败 2 秒重试，关闭时同步 flush；清空操作持久化为 tombstone。日志条目带 nullable usage 与 `upstream_attempted`，统计单独给出本地拒绝数。
- 新增**显式单次模型测试**（`POST /model-test`）：仅在控制台确认后发送一次非流式请求，不重试、不回传 completion，最多 3 个并发。
- 新增**插件配置**与 **runtime** 管理路由：配置只拥有请求日志开关与保留设置，runtime 只公开宿主可提供的账户状态；代理池、路由与冷却由 CPA 宿主拥有，读取不到时降级为“不可读取（仅插件侧状态）”。
- 提供管理接口 `/v0/management/open-design` 和控制台 `/v0/resource/plugins/open-design/console`。
- 通过 CPA 公开的 host HTTP / translation / public RPC 发起请求，支持流式与非流式 Chat Completions。

额度和预测使用 OpenDesign 官方协议；Cline 的计费接口、价格、额度周期和能力过滤不适用于本插件。

实际 Provider 实现在 `internal/opendesign`。`internal/bridge` 是保留的上游参考实现，不接入本插件的新二进制。

## 控制台

控制台分为 5 个标签页，全部操作都由按钮手动触发，切换标签页不会自动发请求：

| 标签页 | 内容 |
| --- | --- |
| 概览 | 汇总管理接口已返回的账户与映射数量 |
| 凭据 · 额度 | 账户增删改、可选 control Key／Workspace ID、手动刷新钱包与套餐窗口、编辑重试调度／错误规则 |
| 模型映射 · 覆盖 | 拉取上游模型、搜索与批量勾选、保存映射、按模型查询官方覆盖、显式模型测试 |
| 请求日志 | 只读元数据列表、汇总统计、筛选与分页、清空日志 |
| 代理池 · 配置 | 请求日志开关与保留设置；宿主代理／路由／冷却只读展示，runtime 手动读取 |

### 重试调度／错误规则

每个账户可以带一组错误规则，随凭据写入宿主 auth 文件的 `request_scoped_errors`。规则是「HTTP 状态码 + 包含／正则条件 → 动作」：

| 动作 | 含义 |
| --- | --- |
| `stop` | 停止当前请求，不换号、不冷却 |
| `stop-and-cooldown` | 停止当前请求并冷却该账户 |
| `continue` | 换到下一个可用账户，不冷却 |
| `continue-and-cooldown` | 换到下一个可用账户并冷却该账户 |

- 匹配对象是**插件生成的错误 Message**：上游错误、协议错误、传输错误、插件不可用和响应过大分别带稳定的 `[open-design:<kind>]` 前缀。marker 只由封闭白名单拼出，上游响应体无法伪造规则。
- 状态码为硬匹配（400 就是 400，不做状态码族）。条件支持子串与正则；正则无效或字段越界会在保存时被拒绝。
- 插件只负责分类和标记；是否换号、是否冷却、冷却多久由 CPA 宿主按规则与自身配置决定。**插件不做并发限制，也不做请求排队。**
- `PUT /error-rules` 保存策略并逐账号推送，`POST /error-rules/sync` 只重新推送，`POST /error-rules/reset` 恢复内置默认策略。空数组表示该账户不写插件自有规则（由 CPA 配置决定）。
- 同步只重写 `request_scoped_errors` 一个键，保留宿主或运维写入的未知字段；每个账号独立报告结果，失败的账号会列在响应里，可再次同步。

### 请求日志与统计

日志条目只包含以下元数据 allowlist：请求 ID、开始／结束时间、耗时、客户端模型、上游模型、凭据 ID、是否流式、状态、HTTP 状态、错误类型、输入／输出／总 token、`upstream_attempted`。**不保存**请求/响应正文、headers、query 字符串、API key、control key 或 Workspace ID；保存前还会按已知 Key 再做一次脱敏。

- 默认开启，保留 7 天、最多 500 条；可通过配置调整为 1–90 天、1–500 条。
- 写入为异步：25ms 合并持久化，失败后 2 秒重试；`plugin.quiesce`／`plugin.shutdown` 会先取消延迟任务再同步 flush。
- 清空日志是持久化操作：先写入 clear tombstone（含保留策略），再更新内存，因此崩溃恢复后旧日志不会复活。
- usage 字段可为 `null`：响应没有出现对应 usage 时保持未知，不写成 0。
- 统计给出总数、成功／失败、流式、成功率、token 汇总、带／不带 usage 的数量、上游已尝试与**本地拒绝**数，并按模型聚合。`attempted=yes|no` 可筛选两类请求。

### 官方模型覆盖

`GET /model-coverage?credential_id=<id>&model_id=<模型 ID>` 使用同一份 control Key 与 Workspace ID，向官方 `funding-check` 传入 `modelId`，读取 `modelCovered`、`funding` 与 `fundingCheck`。它与 `quota` 的共享钱包／套餐窗口缓存完全分离：覆盖查询不会写入或伪造额度快照，共享额度也不会被当成某个模型的余额。

- 只有点击「检查覆盖」并带 `refresh=1` 才请求官方接口；普通读取只返回缓存。
- 同一凭据与模型的组合在 60 秒内最多尝试一次，并发刷新合并；单次刷新 30 秒总预算，超时通过宿主 operation/cancel 释放。
- 官方返回的身份（`workspaceId`、`modelId`）必须匹配，否则按 `malformed` 处理，不宽松接受。

### 模型测试

`POST /model-test` 只接受已启用的账户与已保存的客户端模型名，由插件自行构造极小的非流式 Chat Completions 请求：

- 控制台必须先确认，一次操作只发一次请求，**不自动重试**。
- 最多 3 个并发测试；超过时返回 429。
- 结果只返回状态、上游 HTTP、耗时、token 计数与错误类型，**不回传 completion**，也不回显任何 Key。
- 测试会产生真实推理用量；账户 Key、映射或启停状态在测试期间变化时，结果不会写入 runtime 历史。

## 安装

需要支持动态库插件和 C shared ABI v1 的原版 CPA。先在 CPA 配置中启用插件，并按插件 ID 打开配置项：

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    open-design:
      enabled: true
      # 可选，默认 plugins/open-design-data
      data_dir: plugins/open-design-data
```

`plugins.enabled` 只打开插件系统，实际加载仍需 `plugins.configs.open-design.enabled: true`。

把构建产物安装到建议位置：

```bash
install -Dm755 dist/open-design.so plugins/linux/amd64/open-design.so
```

然后：

1. 打开 `/v0/resource/plugins/open-design/console`。
2. 在账户管理中添加 OpenDesign 推理 API key。标签仅作备注，不承诺将客户端请求固定调度到该账户。
3. 点击账户的“获取模型”，搜索、勾选模型并保存映射；已有别名会保留，新增别名默认加 `open-design-` 前缀。
4. 在客户端填入 CPA 的 API 地址和 CPA 客户端 API key，使用已保存的客户端模型名调用 `/v1/chat/completions`。
5. 如需额度，在新增或编辑账户时展开可选额度配置：同一账号的 control Key 用于查钱包，再填 Workspace ID 可查套餐窗口。保存后点该账户的「刷新额度」；两项都留空可照常推理。查询和趋势估算不改变账号启停或调度。
6. 如需排查请求，在「请求日志」标签页手动刷新元数据列表或读取统计；如需按模型看官方覆盖，在「模型映射 · 覆盖」里对单个模型点「检查覆盖」；如需发一次小型真实请求，在同一页点「发送测试」并确认。三者都不会自动触发。

管理页需要 CPA 管理认证。插件数据目录默认为 `plugins/open-design-data`，可通过 `data_dir` 修改；容器部署请持久化插件目录、CPA 的 `auth-dir` 和该数据目录（请求日志快照与 journal 也在其中）。

管理接口按 CPA 路由契约提供：

| 方法与路径 | 用途 |
| --- | --- |
| `GET /v0/management/open-design/credentials` | 列出账户（不回显 key） |
| `POST /v0/management/open-design/credentials` | 创建或更新账户，body 为 `{id?, label?, api_key?, disabled?, control_key?, workspace_id?, clear_billing?}` |
| `DELETE /v0/management/open-design/credentials?id=<id>` | 按查询参数删除账户 |
| `GET /v0/management/open-design/status` | 插件状态与模型映射 |
| `GET /v0/management/open-design/upstream-models` | 获取 OpenDesign `GET /models` 候选 |
| `GET` / `POST /v0/management/open-design/models` | 读取或保存模型映射 |
| `GET /v0/management/open-design/quota?credential_id=<id>` | 读取共享额度缓存；加 `&refresh=1` 手动查询官方钱包与套餐窗口，最小间隔 60 秒 |
| `GET /v0/management/open-design/model-coverage?credential_id=<id>&model_id=<id>` | 读取模型级官方覆盖缓存；加 `&refresh=1` 手动查询官方 `funding-check`，每组合最小间隔 60 秒 |
| `GET /v0/management/open-design/request-logs` | 分页读取元数据日志，支持 `limit`、`offset`、`status`、`model`、`credential_id`、`attempted`、`since`、`until` |
| `GET /v0/management/open-design/request-stats` | 读取汇总统计（含本地拒绝数） |
| `DELETE /v0/management/open-design/request-logs` | 持久化清空日志（也接受 `POST /request-logs/clear`） |
| `GET` / `POST /v0/management/open-design/config` | 读取或修改请求日志开关、保留天数与条数上限 |
| `GET /v0/management/open-design/runtime` | 插件可见运行时状态；宿主账户信息不可读时标记降级 |
| `POST /v0/management/open-design/model-test` | 显式单次非流式模型测试，body 为 `{credential_id, model_id, prompt?|messages?}` |
| `GET` / `PUT /v0/management/open-design/error-rules` | 读取或保存错误规则策略并逐账号同步 |
| `POST /v0/management/open-design/error-rules/sync` | 只重新推送当前策略到所有账号 |
| `POST /v0/management/open-design/error-rules/reset` | 恢复内置默认策略并推送 |

`/logs` 与 `/logs/stats`、`/logs/clear` 是同一组日志路由的别名。

## 安全模型

- 默认固定 OpenDesign 官方生产地址 `https://amr-link.open-design.ai/v1`，不接受自定义上游地址。后续 HTTP 重定向由 CPA 宿主处理；本地 CPA 8.0.48 的跨域 307 测试确认其会跟随跳转并去掉 `Authorization`，这不等于插件禁止所有重定向。
- 推理 API key 和可选 control Key 只保存在 CPA 的 `auth-dir`，通过 `host.auth.save` 写入；插件设置中不保存 Key，管理响应不回显。宿主确认保存成功后才发布新凭据；请求发出后两种 Key 的输入框都会清空。
- 映射保存在插件数据目录；上游模型目录、额度缓存、覆盖缓存与预测样本只在内存中。请求日志会落盘，但只写入元数据 allowlist，不含正文、headers、query、Key 或 Workspace ID。日志、截图和提交中不要包含 Key。
- 当前 fork 使用公开 ABI，不修改、不打包也不发布 CPA 宿主。runtime 路由只公开宿主 `host.auth.list` 能提供的能力；读取不到时降级为插件侧状态，不伪造调度、冷却或代理信息。
- 错误规则只写入每个凭据的 `request_scoped_errors`，不改宿主二进制也不写全局路由配置；策略文件是插件数据目录里的 `error-rules.json`，不包含 Key。

## 模型映射

管理页从 OpenDesign `GET /models` 获取可用模型。模型映射在插件内共享，用于发现模型的账户选择不绑定推理调度。保存映射后，插件同步刷新 CPA 的凭据模型注册；客户端请求 `model` 填客户端名称，插件将其转换为已保存的上游模型 ID，再调用 `POST /chat/completions`。未保存的模型名不会自动透传。

页面会保留尚未保存的映射草稿；可以显式还原。若映射已落盘但宿主刷新失败，界面会提示已保存的阶段与需要重试的阶段，避免把部分完成误报为完全未保存。

## 构建

需要 Go 1.26、CGO 和 C 编译器。仓库提供 Makefile：

```bash
make test-js   # 管理页与保留的上游 Node 测试
make test      # Node + Go 测试
make build     # 生成 dist/open-design.so
make smoke     # 构建后运行原生 ABI 与本地假上游验证
make docker    # 使用 Dockerfile.build 构建 Linux amd64 共享库
```

也可以直接构建：

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -buildmode=c-shared \
  -ldflags='-s -w' -o dist/open-design.so ./cmd/passbridge
```

产物是 `dist/open-design.so`（Linux amd64，C shared ABI v1）。

发布 tag 使用 `open-design-v0.4.2` 形式，避免与 fork 保留的上游 `v*` tag 冲突。CPA 插件商店的 latest-release 解析要求 tag 以数字开头，因此从自有源安装时需显式指定该 tag/版本，`github-release` 的自动“取最新”不适用于本 fork。

## 当前验证边界

- `0.4.2`／`0.4.1` 只做跨平台持久化修复与分类边界锁定，不改变功能与运行时行为；`0.4.0` 在 `0.3.0` 之上新增重试调度／错误规则，并已用双账号真实宿主验证：假上游按凭据返回生产 400 `tier_limit_exceeded` 后，宿主按规则换到另一账号并返回 200，冷却后下一次请求不再使用失败账号，`stop` 规则下不换号且响应保留 marker。全仓 Go／race／vet、Node 项、离线与原版 CPA 隔离流程见[验证记录](docs/verification.md)。
- `0.3.0` 在 `0.2.0` 之上新增日志、统计、官方模型覆盖、显式模型测试、配置与 runtime。全仓 Go／race／vet、Node 139 项、Edge 540 项、Debian 12 构建和原版 CPA 隔离流程已通过，具体产物与上线状态见[验证记录](docs/verification.md)。
- 以下为 `0.2.0` 已发生的生产事实，保留为历史，不属于 `0.3.0` 的验收证据：2026-11-02 发现 13 个模型；官方直连与生产 CPA 插件各完成一次最小 JSON 推理并得到 `OK`。生产插件测试使用 `open-design-minimax-m3.1-flash`，usage 为 219 输入／3 输出／222 总 token。13 个模型只验证发现、持久化与注册，没有逐个付费推理。
- 早期现有推理 Key 对官方控制面钱包、账单汇总接口返回 401。随后，两份独立 control 授权直接查询钱包和各自工作区 funding-check，共四个 GET 均返回 200。上线 `0.2.0` 后，现有账户经完整推理 Key 精确比对确认属于 od1，补充授权并通过生产插件成功读取钱包与套餐窗口；真实消费趋势预测仍未验收。
- [验证记录](docs/verification.md) 分别列出 Go／Node、Edge 交互与布局、原版 CPA 配合 loopback TLS 假上游的结果，避免把隔离测试写成真实额度成功。

## 文档与来源

- [OpenDesign 插件说明](docs/open-design.md)
- [额度接口、状态与预测计算](docs/quota.md)
- [验证记录与真实上游限制](docs/verification.md)
- `docs/` 下的其他旧文档保留为上游参考，本 fork 未逐篇改写。
- 上游：[xiao-qiu-qiu/ClinePassBridge](https://github.com/xiao-qiu-qiu/ClinePassBridge)
- 协议参考：[AstroQore/opendesign2api](https://github.com/AstroQore/opendesign2api)（独立网关 prior art，仅参考公开协议行为，未复制源码）
- OpenDesign 官方生产配置参考：[nexu-io/open-design vela.ts](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela.ts#L819-L842)

## 许可

[MIT License](LICENSE)。原项目版权归 xiao-qiu-qiu；本 fork 的修改部分版权归 ruruamour。
