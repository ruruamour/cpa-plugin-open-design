# OpenDesign 额度查询与预测

额度查询使用 OpenDesign 官方客户端公开的只读协议，通过原版 CPA 的 host HTTP 接口发起请求。此功能需要单独配置官方账号控制 Key；原有推理 API key 可继续用于模型发现和推理。

## 凭据与查询范围

| 数据 | 官方地址 | 所需凭据 | 显示单位 |
| --- | --- | --- | --- |
| 账号钱包 | `https://amr-api.open-design.ai/api/v1/wallet/balance` | control Key | USD |
| 工作区套餐窗口 | `https://amr-api.open-design.ai/api/v1/workspaces/{workspace_id}/billing/funding-check` | control Key、明确的工作区 ID | credits |
| 模型发现、推理 | `https://amr-link.open-design.ai/v1` | 原有推理 API key | 上游响应的 token usage |

这些路径来自官方客户端的实际请求代码，服务端未提供随本仓库分发的 OpenAPI。账号权限和上游版本仍可能影响接口可用性。2026-11-02 早期用现有生产推理 Key 只读访问钱包和账单汇总接口，均返回 HTTP 401；随后用两份独立 control 授权直接查询官方钱包和指定工作区 funding-check，四个 GET 均返回 HTTP 200，返回字段通过当前契约核对且工作区匹配。直接读取成功与插件内的隔离集成验证分别记录；未为验收费率或预测发起额外计费推理。

新增或编辑账户时展开「可选：额度查询」，按所需范围填写：

| 配置 | 可用能力 |
| --- | --- |
| 仅填写推理 API key，两个额度字段留空 | 模型发现与推理 |
| 再填写同一 OpenDesign 账号的 control Key | 手动查询账号钱包 |
| 同时填写 control Key 和对应 Workspace ID | 手动查询钱包及该工作区套餐窗口 |

保存后打开「凭据 · 额度」标签页会自动按账户顺序逐个查询一次（也可点该账户的「刷新额度」随时重查）。control Key 和工作区 ID 可以在新增时一起填写，也可以随后编辑补充。编辑时留空保留原值；显式清除额度授权同时移除 control Key 和工作区 ID。两种 Key 只经 `host.auth.save` 保存在 CPA `auth-dir` 的原账户文件中，管理列表不回显，浏览器不持久保存。

账户标签只是备注。插件不根据标签合并账号，也不把“拉取模型的账户”当成客户端请求的固定调度账户。模型映射在插件范围共享。

## 刷新与错误状态

控制台在打开「凭据 · 额度」时按账户顺序串行刷新，离开标签页即停止；也可以手动点「刷新额度」。每份凭据的查询尝试至少间隔 60 秒，相同凭据的并发刷新合并，整个插件最多同时刷新三个账户。一次刷新共用 30 秒总预算，包含排队、钱包及套餐请求；到期通过 CPA 公开 operation/cancel 接口取消宿主请求并释放刷新槽。宿主不支持可取消操作时返回错误，不启动无法取消的额度请求。普通缓存读取不请求 OpenDesign；本功能没有后台定时采样，唯一自动触发点是打开「凭据 · 额度」标签页，且逐个账户串行执行。

- **未配置**：没有 control Key，不发送额度请求。
- **等待查询**：已经配置，尚无查询数据。
- **可用**：显示官方返回的余额、credits、重置时间及成功查询时间。
- **部分可用**：例如钱包成功但工作区窗口失败，两部分独立展示。
- **上次数据**：查询失败后保留最后一次成功值及原时间，移除当前预测。
- **查询失败**：首次查询失败时显示错误类型，金额和额度保留未知；未知不显示为零。

当前开发分支的账户窗口摘要显示该部分的查询时间；刷新失败时标记「上次数据」，窗口重置时间已到则提醒刷新。数字是那次查询的快照，不是实时额度，也不直接证明暂停可以解除。读取到暂停已解除时，页面丢弃该账户的旧额度快照，避免将其计入活动账户总额度；需重新查询以取得新数据。

额度组件内的 `unauthorized`／`forbidden` 表示 OpenDesign control Key 或工作区权限未获许可；它们通过 HTTP 200 的额度状态返回，不注销 CPA 管理登录，也不停用推理账号。管理路由本身返回 HTTP 401／403 时，控制台要求重新验证 CPA 管理认证。429、超时、无效响应分别显示；上游响应正文、headers 和 Key 不透传到控制台。

更换 Key、工作区或清除授权会使相关缓存与预测样本失效，旧请求迟到时不得恢复旧数据。状态列表提供随机生成的内存版本 `billing_revision`，供其他已打开页面在刷新状态时识别外部换 Key；当前开发分支还会在额度暂停解除时更新该版本，即使页面未看到中间暂停过程也能丢弃旧额度。该版本不由 Key 派生，也不写入 auth 文件。重启插件后需要重新查询建立样本。

保存请求发送后会清空两种 Key 的输入框，即使响应丢失或服务器返回错误。账户操作已获成功确认、随后状态读取失败时，页面保留已确认的变化并提示刷新状态，避免误报为操作完全失败。

## 模型差异与通用预检

页面优先显示官方钱包余额、每个套餐窗口的已用／剩余 credits、上限和重置时间。多个窗口分别生效，不能相加。共享额度路径请求 funding-check 时不传 `modelId`，要求返回 `modelId:null`；页面里的资金方式与资金检查均是未指定模型的通用预检，不能据此断言某个模型必然使用套餐或钱包。

模型级信息由单独的[官方模型覆盖](#官方模型覆盖)路由提供：它传入 `modelId`，只返回该模型的覆盖与资金检查结果，不改变也不复用上面的共享额度快照。

不同模型的套餐覆盖和消耗可能不同。当前页面不维护模型价格或倍率表，不把 credits 换算成单个模型的剩余 token、请求次数，也不把套餐 credits 换算成钱包 USD。具体请求的准入和扣费遵循官方对该模型的规则。

## 官方模型覆盖

`GET /v0/management/open-design/model-coverage?credential_id=<id>&model_id=<上游模型 ID>` 使用与共享额度相同的 control Key 和 Workspace ID，向官方 `funding-check` **带上 `modelId`**，读取该模型的 `modelCovered`、`funding`、`fundingCheck`。

- 共享额度与模型覆盖是两套独立缓存：覆盖查询不写入、不伪造钱包／套餐窗口快照；共享 credits 也不会被当成某个模型的余额。
- 只有 `refresh=1` 才请求官方接口；普通读取只返回缓存。
- 同一凭据与模型的组合 60 秒内最多成功请求一次，相同组合的并发刷新合并；整个插件最多同时刷新三个模型覆盖。
- 单次覆盖刷新 30 秒总预算，包含排队与请求；超时通过 CPA 公开 operation/cancel 释放。
- 官方返回的 `workspaceId` 和 `modelId` 必须与请求一致，否则按 `malformed` 处理；响应只含允许字段。
- 缓存容量有上限，正在请求的条目不会被驱逐。

覆盖结果状态：

- **未配置**：缺少 control Key 或 Workspace ID，不发请求。
- **等待查询**：已配置但尚未查询。
- **可用**：显示 `model_covered`、`funding`、`funding_check` 与查询时间。
- **上次数据**：刷新失败后保留最后一次成功值并标记 stale。
- **查询失败**：`error.kind` 为 `unauthorized`、`forbidden`、`rate_limited`、`timeout`、`malformed`、`network`、`upstream` 或 `stopped`。

## 预测的含义与计算

预测放在每个窗口默认折叠的「用量趋势（估算）」中，回答“保持最近的整体 credits 消耗速度，这个固定窗口可能何时耗尽”。趋势包含采样期间反映的模型组合；切换模型或流量变化后，未来消耗速度可能改变。真实余额、窗口剩余量及重置时间始终独立显示。官方确认的零剩余直接显示，不折叠为预测。

预测使用官方返回的 credits，保留原单位；不根据 token 数猜余额，也不套用 Cline 的金额估计、套餐时长或月／周倍数。

只有同一份凭据、同一工作区、同一固定窗口的相邻两次成功采样，且间隔至少 60 秒、已用增加、剩余减少时才计算：

```text
每秒消费速度 = (本次 usedCredits − 上次 usedCredits) / 采样间隔秒数
预计剩余秒数 = ceil(本次 remainingCredits / 每秒消费速度)
预计耗尽时间 = 本次采样时间 + 预计剩余秒数
```

示例：12:00 已用 200、剩余 800 credits；12:05 已用 260、剩余 740 credits。速度为 `(260 − 200) / 300 = 0.2 credits/秒`，预计剩余 `740 / 0.2 = 3700 秒`，即预计在 13:06:40 耗尽。若窗口在 13:00 重置，则显示“预计先重置”，不显示跨越重置时刻的耗尽日期。此示例不改变任何余额、价格或额度。

官方成功样本的 `remainingCredits` 已为零时，显示“官方返回的窗口额度已耗尽”（`forecast.state: exhausted`），无需等待第二个样本；此状态不附带估算时间或消费速度依据。到达已知重置点或后续查询失败后撤下该状态，避免把旧的零值当作当前确认。

计算使用大整数与有理数，避免大 credits 被 JavaScript／浮点数四舍五入。金额只接受官方十进制字符串；数字类型或格式异常保留为未知，避免把已经舍入的金额当作精确值。

界面同时标明采样时间与绝对耗尽时间，缓存中的“采样时预计剩余”不会改写为从现在起的倒计时。到达估算时间或窗口重置点后，界面会在本地撤下旧预测；这个计时器不会请求上游。以下情况不输出耗尽时间：

- 样本不足、窗口身份或额度变化、重置、用量下降或数据不一致。
- `activity_triggered` 窗口：消费过期可能释放额度，简单线性外推没有可靠含义。
- 查询失败或数据陈旧。
- 预计时间超过 366 天的显示上限。

该估算涵盖两次官方窗口快照之间反映的消费，可能包含其他客户端的使用。它不能判断消费由哪个客户端产生，结算延迟和未来流量变化也会影响结果。界面明确标注估算依据；预测不修改 CPA 路由、冷却、启停或官方额度。

## 管理 API

```text
POST /v0/management/open-design/credentials
  {id?, label?, api_key?, disabled?, control_key?, workspace_id?, clear_billing?}

GET /v0/management/open-design/quota?credential_id=<id>
  只读缓存，不请求上游

GET /v0/management/open-design/quota?credential_id=<id>&refresh=1
  受最小间隔限制的手动查询

GET /v0/management/open-design/model-coverage?credential_id=<id>&model_id=<id>
  只读模型级覆盖缓存

GET /v0/management/open-design/model-coverage?credential_id=<id>&model_id=<id>&refresh=1
  受 60 秒节流与 30 秒预算限制的手动模型覆盖查询
```

额度返回的钱包与窗口分别包含 `state`、`data`、`fetched_at`、`error`，顶层含 `next_refresh_at`。没有值时为 `null`，窗口内的 `forecast.state` 表示样本不足、滚动窗口、估算、已耗尽、先重置、已到期或超过预测上限。模型覆盖返回 `credential_id`、`model_id`、`billing_configured`、`workspace_id`、`state`、`fetched_at`、`next_refresh_at`、`model_covered`、`funding_check`、`funding` 与 `error`。两类路由都按官方客户端契约校验身份、枚举、时间和数字字段，只返回允许的字段。

## 官方协议依据

均绑定官方仓库 commit `5a0c47777975ddb91d09c1c16b423a3ef74bf572`：

- [钱包请求、字段校验与错误语义](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela-wallet.ts#L148-L203)
- [工作区 funding-check 请求与校验器](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/apps/daemon/src/integrations/vela-billing.ts#L196-L270)
- [窗口、credits 与重置模式类型](https://github.com/nexu-io/open-design/blob/5a0c47777975ddb91d09c1c16b423a3ef74bf572/packages/contracts/src/api/collab.ts#L1066-L1092)

本仓库的隔离测试覆盖这些请求和字段契约。2026-11-02 的两账户真实 control 授权直接读取已成功；随后 `0.2.0` 上线，od1 授权经完整推理 Key 精确匹配后配置到现有账户，生产插件首次手动查询钱包和套餐均为 `available`。真实消费采样和趋势预测尚未验收，详见[验证记录](verification.md)。
