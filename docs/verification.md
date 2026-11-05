# OpenDesign 插件验证记录

## 0.4.6 开发检查（未通过安全复核，禁止部署）

`0.4.6` = `0.4.5` 的思考强度映射 + 三处修复（后端模型/规则快照一致性、控制台默认表与保存并发、深拷贝数据竞争）+ 额度耗尽自动暂停。`open-design-v0.4.5` 的 Release 因 macOS runner 被取消未产出资产，故 `0.4.5` 未单独发布。

当前实现尝试参照 ClinePass 的 QuotaBlock 记录额度暂停，但对 CPA 原生人工停用的保护存在缺陷，**不能把下面的单元测试结果理解为真实宿主验收通过**。

| 检查 | 结果与范围 |
| --- | --- |
| 信号 | `quotaPauseSignal` 只接受 `error.code=insufficient_balance` 且状态为 `402`，或同一机器码出现在 `2xx`（含 SSE）且 `error.details.coding_plan_exhausted=true` 的响应中；裸 `402`、文本 `payment required`、普通 `429`、普通 `insufficient_quota` 均不触发 |
| 截止时间 | `quotaPauseDeadline`：缺省 30 分钟，钳制 1 分钟～7 天，受 `pause.max_seconds` 约束；`reset_at` 只取 RFC3339（允许 `resets at ` 前缀），`Retry-After` 支持秒数与 HTTP 日期，溢出按上限处理 |
| 人工停用保护 | 插件管理接口的启停路径有模拟测试；但 CPA 原生界面停用与插件投影的禁用无法可靠区分，到期恢复可能覆盖人工停用，**未通过**。`ManualDisabled=nil` 的保守回退另有本地修复，不能解决上述归因问题 |
| 持久化 | 读—比对—原子替换，只改 `quota_block`／`quota_revision`／`disabled`，保留未知字段；写失败时先发布内存拦截再记 `quotaPauseSyncError`，后台任务重试 |
| 执行路径 | `executor.execute`／`execute_stream`（含 SSE 事件）都调用 `observeQuotaBlock`；已暂停账户在 `selectedCredential` 本地返回 `429 [open-design:quota_block]`，配套 `continue` 规则允许换号，不产生新的宿主冷却归因 |
| 恢复 | 开发分支的到期不再直接启用：后台按账号与模型串行请求官方预检，只有全部已知耗尽模型在最新观察之后取得新鲜 `available`、Workspace 成员一致、同一推理密钥无在途、凭据／修订未变时才解除；失败、未授权、缓存或模型集合不完整均保留暂停。复查总时长受预算限制，提交在 `authFileMu` 下复核磁盘身份与证据新鲜度 |
| 管理接口 | `GET /quota-pause`、`POST /quota-pause/resume`、`POST /quota-pause/recheck`；`GET /status`、`/credentials` 返回 `quota_paused`／`effective_disabled`／`quota_pause`（不含任何 Key 指纹） |
| 配置 | `0.4.6` tag 的 `pause.enabled` 默认开启，不安全；后续开发分支改为缺省关闭，但显式开启仍不安全。`pause.max_seconds` 为 60–604800 秒，默认 86400 |
| Go 用例 | `quota_pause_deadline_test.go`、`quota_pause_signal_test.go`、`quota_pause_identity_test.go`、`quota_pause_manual_test.go`、`quota_pause_persist_test.go`、`quota_pause_executor_test.go`；覆盖到期不构成恢复证明及插件内人工启停后显式恢复等行为 |
| Node 用例 | 额度徽标与人工开关分离、解除按钮不发 `/credentials`、重查未通过提示、暂停配置默认值与越界拒绝 |
| 真实宿主恢复 | `scripts/smoke-abi.py --cpa-live` 新增隔离场景：结构化 402 建立暂停、重启后仍保留、官方预检失败保持暂停、官方新鲜 `available` 后清除插件暂停且插件控制台人工停用保持。针对 `bb3a452` 构建（Linux so SHA-256 `85ed994c7d9e63a9c031abe25fe63fd83fb819499bafe2523b17f49d0582b434`），原版 v8.0.29 隔离构建与本地 `8.0.51` 测试构建均 PASS；原版构建 `--version` 自述 `dev`，不能从二进制自证版本。两种宿主均连接隔离假上游，不代表生产验收。 |
| 磁盘合并缺陷与修复 | 独立复审指出陈旧写回可能丢掉人工停用或导致暂停复活；此前修复又引入了相同暂停 body 被误报同步失败、修复后 stale 标记不清、无修订号时错误压制磁盘暂停。已分别添加复现及修复：相同内容不触发修复，同 revision 但 block 内容变化仍会修复；修复成功或文件已匹配时清除标记；无修订号不能证明已解除，因此保留磁盘有效暂停。另修复复查退避溢出及锁外读取策略。用例：`quota_stale_disk_test.go`；最新修复仍需独立复审和隔离宿主重测。 |
| 真实宿主恢复缺陷与修复 | `--cpa-live` 曾稳定卡在「官方已 `available` 但复查判为未证明」。定位到两处：复查读取复用 60 秒模型级缓存/节流窗口，或加入控制台已发起的查询，拿到的是复查开始前发出的证据；策略比较用 `*bool` 指针身份，宿主重注册用新解码配置重跑 `configure` 使每轮自动复查被判成策略变化。改为复查强制新发请求（在途则先等其结束）并比较 `pauseSettings()` 有效值后，原版与本地 hotfix 构建的 `host_quota_recovery` 均 PASS。回归用例：`TestQuotaRecoveryRejectsCachedEvidence`、`TestQuotaRecoveryProofDoesNotJoinOlderRefresh`、`quota_recheck_policy_generation_test.go` |
| 全仓 Go | `go test ./...`、`go test -race ./internal/opendesign/`、`go vet ./...` 通过 |
| ABI | `scripts/smoke-abi.py`（CGO `c-shared` 构建）PASS |

**真实上游样例与验收边界**：用户报告两个账号周额度耗尽；2026-11-05 从生产相关会话和上游请求获得了结构化 `402` 样例，其中包含 `insufficient_balance`、`coding_plan_exhausted=true` 和约三天后的 `reset_at`。脱敏报文形态已加入 `TestQuotaPauseSignalMatchesObservedCodingPlanExhaustion`。默认 `pause.max_seconds` 为一天，会将首次到期提示钳制在一天；届时还须启用策略并满足复查条件才会查询，不保证等待到上游的三天重置时间。这只验证了真实错误形态，**新版本的生产暂停、自动恢复及换号行为仍未验收**；相关真实宿主测试均使用隔离 fixture。

人工停用保护仍未通过全部入口的真实宿主复核：当前实现返回有效 `Disabled` 给宿主，宿主可能写回文件，不能区分部分原生人工停用；隔离真实宿主已验证插件控制台的人工停用能在自动恢复后保留，但不包括 CPA 原生入口。上游 `402/403` 的冷却由宿主控制，现有宿主为 30 分钟；本插件不主动清除宿主冷却，暂停／恢复对该冷却的副作用仍需宿主实测。`0.4.6` 的 Release 运行 `41906585863` 因 macOS arm64 作业取消而失败，未生成 Release，未部署。

**人工停用缺陷复现**：在隔离临时目录中构建并启动未修改的原版 CPA `v8.0.29`（源码提交 `5fc580b1d72d87a7bcfe5cfb6f479c1aea5625b3`），使用仅包含测试 Key 的临时配置、loopback 代理和假上游返回结构化 402。插件进入额度暂停后，通过 CPA 原生 `PATCH /v0/management/auth-files/status` 设置 `disabled=true`；接着调用插件的 `/quota-pause/resume`，观察到文件 `disabled` 从 `true` 变回 `false`，插件状态也为启用。该复现证明当前实现不能保护这种人工停用操作；它没有访问生产服务或真实密钥，也不涉及宿主补丁。

**思考强度宿主兼容缺陷及修复**：使用隔离假上游，配置 `medium → high` 并经 CPA 的 `/v1/chat/completions` 请求，未修改原版 CPA `v8.0.29` 的上游收到 `high`；旧开发代码在本地 `8.0.51-gemini-usage-hotfix.test` 构建中未能发送该参数。开发分支对由用户配置的模型使用 CPA 公开的 `UserDefined` 元数据，不宣称任何模型的官方推理能力；修改后 `scripts/smoke-abi.py --cpa-live` 在这两个隔离宿主中均 PASS，普通与流式都收到 `high`。后者仍是本地测试构建，不代表已验证原版 `8.0.51` 或生产服务；多标签页并发保存仍待复核。宿主可能在插件之前转换非字符串请求值，本地测试构建将数值 `123` 转成字符串；插件不保证跨宿主原样透传。

## 0.4.4 本轮验收

2026-11-04 发布 `0.4.4`。唯一改动是上游额度耗尽报文的分类：OpenDesign 的 `402` 报文带自有机器码 `insufficient_balance`（`error.code` 或 `[code=...]` 文本）时归类并换号冷却；不带机器码的裸 `402` 仍不分类。

| 检查 | 结果与范围 |
| --- | --- |
| 分类 | `isKnownUpstreamCode` 白名单加入 `insufficient_balance`；默认规则新增 `402 + [open-design:insufficient_balance] → continue-and-cooldown` |
| 报文稳定性 | 插件只输出 `[open-design:insufficient_balance] upstream request failed: payment required`，不带上游的 wallet／plan／`reset_at` 文本 |
| 边界 | 裸 `402 payment required`（无机器码）仍为 `upstream_error` 且不匹配任何默认规则；非白名单码（如 `insufficient_balance_evil`）不进入 message，无法伪造 marker |
| 新增用例 | `TestErrorRulesClassifyInsufficientBalanceWithPlanFlag`、`TestErrorRulesInsufficientBalanceMarkerIsClosed`、`TestErrorRulesUnknownBalanceCodeStaysUnclassified`；Node 侧 `explainErrorRule` 新增 marker 文案断言 |
| 全仓 Go | `go test ./...`、`go test -race ./internal/opendesign/`、`go vet ./...` 通过 |
| Node | `node --test tests/*.test.cjs`，154/154 通过 |

真实报文来自生产会话记录（`402` + `details.coding_plan_exhausted=true` + `reset_at`），当时插件把它当成未分类的 `upstream_error`，只能靠宿主对裸 402 的固定 30 分钟冷却兜底。

### 生产部署

2026-11-04 从提交 `563c6b3` 的 CI 产物（[CI 41842818883](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/41842818883)）部署到 `nuoapi-net`；Release 为 [open-design-v0.4.4](https://github.com/ruruamour/cpa-plugin-open-design/releases/tag/open-design-v0.4.4)（[发布运行 41842864324](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/41842864324) 五平台全绿），并已转为正式发布。

| 核验项 | 结果 |
| --- | --- |
| 库哈希 | `15302e91ebfdefcbe232b52ea6866bce2eddffffd38a6b597a30730971c5da15`，与 Release 的 `linux_amd64` 资产一致 |
| 版本 | `GET /open-design/status` → `version=0.4.4` |
| 规则 | 默认规则 9 条；凭据 auth 文件 `request_scoped_errors` 已写入包含 `402 + [open-design:insufficient_balance]` 的新规则；`sync total=6 succeeded=6 failed=[]` |
| 控制台 | `/v0/resource/plugins/open-design/console` → 200，页面已包含新 marker 文案 |
| 服务 | `cliproxy-cpa.service` `active` |
| 备份 | `/srv/cliproxy/_backups/open-design-20261104T140718Z` |

本轮没有触发真实额度耗尽事件（6 个账号当前都有额度），因此**分类正确性由单元用例锁定**，而“规则命中后宿主换号 + 冷却”的机制沿用 `0.4.0` 已用真实宿主验证过的链路。

### 未做的部分：冷却时长

CPA 对 `402`／`403` 的冷却时长是写死的 30 分钟（`conductor_cooldown.go` 凭据级与模型级两处都是 `now.Add(30 * time.Minute)`），**不读** `Retry-After`；只有 `429`、`404` 与 408/5xx 分支会采纳重试提示。因此插件无法把冷却时长设成上游 `reset_at`：本插件也没有在任何一处发送 `retry_after_seconds`（`pluginabi.Error` 里有这个可选字段，但当前实现不使用）。若要把暂停拉长到真实重置时间，需要在宿主侧改动或改由插件自行暂停账号，两者都超出本仓库只维护插件的边界，本轮未做。

## 0.4.3 本轮验收

2026-11-04 发布 `0.4.3`。唯一改动是控制台额度刷新的触发方式：打开「凭据 · 额度」标签页时按账户顺序串行查询一次。

| 检查 | 结果与范围 |
| --- | --- |
| Node | `node --test tests/*.test.cjs`，154/154 通过（新增 2 项：串行顺序与不并发、离开标签页即停止） |
| 全仓 Go | `go test ./...`、`go vet ./...` 通过（未改 Go 代码） |
| 行为 | 打开标签页按列表顺序逐个查询；同一时刻最多一个额度请求；未配置 controlKey 或已停用的账号跳过；60 秒窗口内的账号跳过；离开标签页停止后续账户 |
| 提示 | 自动过程只对失败账号提示，成功账号不逐个弹消息；「刷新额度」按钮仍可随时手动重查 |
| 范围 | `internal/opendesign/ui/console.js`、`console.html` 与文档；后端路由与节流（每凭据 60 秒、插件内最多 3 个并发槽、30 秒预算）未变 |

## 0.4.2 本轮验收

2026-11-04 发布 `0.4.2`。它把插件内所有持久化重命名统一到一条带有限重试的发布路径，并锁定裸 402 的分类边界；运行时行为与 `0.4.0`／`0.4.1` 相同。

| 检查 | 结果与范围 |
| --- | --- |
| 全仓 Go | Go 1.26.4 的 `go test ./...`、`go test -race ./internal/opendesign/`、`go vet ./...` 全部通过 |
| Node | `node --test tests/*.test.cjs`，152/152 通过 |
| Windows 语义 | 交叉编译 Windows 测试二进制成功；请求日志、快照、settings.json 的重命名占用冲突重试与失败不提交由注入用例锁定 |
| 分类边界 | 裸 402 保持 `upstream_error` 且不匹配任何默认规则；带 `insufficient_quota` 码的 402 仍换号冷却 |

### 0.4.2 改动内容

- `renameFile`／`publishStagedFile`／`publishRetryPauses` 从请求日志专用提升为插件通用；`settings.json`（`saveSettings`）与错误规则文件（`publishErrorRulesFile`）改用同一发布路径，占用冲突按 5ms／20ms／60ms 有限重试，最终失败仍如实返回并清理临时文件。
- 裸 402（上游只给自由文本 `payment required`，无机器可读码）保持 `upstream_error`、不匹配默认规则；生产 2026-11-01～04 共 6 次这样的 402，同批账号 496/500 为 200，无额度耗尽证据。
- 新增用例：`TestSettingsSaveRetriesTransientWindowsShareViolation`、`TestSettingsSaveReportsPersistentWindowsShareViolation`、`TestErrorRulesBarePaymentRequiredStaysUnclassified`、`TestErrorRulesClassifyPaymentRequiredWithQuotaCode`。

### 0.4.2 CI 与发布

- 提交 `d3eb6c0`（统一持久化重试发布）与 `e84a27a`（升版 0.4.2 与文档）；CI `41808678867` 成功。
- 发布 tag `open-design-v0.4.2` 指向 `e84a27a`。`git push` 建 tag 时 GitHub 返回 `Internal Server Error`，改用 REST API 创建 ref 成功，未改写任何已有 tag。
- Release 运行 `41809302685` 成功：`metadata`、`linux-amd64`、`linux-arm64`、`darwin`（amd64／arm64）、`windows-amd64` 与 `release` 全部通过；`windows-amd64` 从 `0.4.0` 的失败转为通过。
- Release 已发布（非草稿、非预发布），资产为五个平台 zip 与 `checksums.txt`；下载 `open-design_0.4.2_linux_amd64.zip` 复算 SHA256 与 `checksums.txt` 一致（`c61c55c2…fa4a7`）。

### 0.4.2 生产观察（只读）

- 最近 500 条请求日志：496 条 200、3 条 402（`upstream_error`）、1 条 `upstream_transport_error`。
- `GET /v0/management/auth-files` 的 OpenDesign 凭据：4 个 `active`、0 冷却；`od6819533028@fldubai.com` 有 1 条模型级冷却（`scope=model`、`reason=payment_required`、`http_status=402`），由 CPA 宿主按 402 生成，非插件写入。
- 未发现插件侧额度暂停（`quota.signals` 为空）与额度耗尽判定。

## 0.4.1 本轮验收

2026-11-04 发布 `0.4.1`。它只修 `0.4.0` 的跨平台缺陷，不改变功能与运行时行为。

| 检查 | 结果与范围 |
| --- | --- |
| 全仓 Go | Go 1.26.4 的 `go test ./...`、`go test -race ./internal/opendesign/`、`go vet ./...` 全部通过 |
| Node | `node --test tests/*.test.cjs`，152/152 通过 |
| Windows 语义 | 交叉编译 Windows 测试二进制成功；重命名占用冲突的重试、失败不提交、先关闭源句柄三类行为由注入用例锁定 |
| 修复范围 | `internal/opendesign/request_log.go` 的日志/快照重命名与 journal 截断；`error_rules_test.go`、`persistence_consistency_test.go` 的跨平台测试 |

### 0.4.1 修复内容

`0.4.0` 的 GitHub Release 运行 `41791392077` 在 `windows-amd64` 上失败，`release` job 因此被跳过，Linux／arm64／macOS 三个平台构建均成功。失败用例与成因：

- `TestErrorRulesFailedWriteKeepsPreviousPolicy`：Windows 不因目录只读而拒绝属主写入，`chmod 0500` 无法制造写失败。
- `TestErrorRulesReconcileParsedCredentialKeepsUnknownFields`：Windows 无 POSIX 权限位，`os.Stat` 返回 `0666`。
- `TestRequestLogCorruptAndOversizedJournalsCompactBeforeClear`、`TestRequestLogJournalAtomicAppendShape`：Windows 不允许替换或截断仍被本进程句柄打开的文件，报 `Access is denied`。

修复：

- 请求日志的 journal 追加在重命名前先关闭源文件句柄；快照发布与 journal 截断在重命名/截断失败时按 5ms／20ms／60ms 做有限重试。失败仍会如实返回并保持「失败不提交」，不会静默丢日志。
- 两处依赖 POSIX 权限语义的用例改为等价注入（重命名失败注入、Windows 跳过权限位断言），不再依赖目录权限。
- 新增 `internal/opendesign/windows_rename_test.go` 锁定上述 Windows 语义。

### 0.4.1 CI 与发布

- 修复提交 `afc3057`（升版与跨平台修复）与 `8f87279`（错误规则写失败用例改为重命名注入）；CI `41797433234` 成功。
- 发布 tag `open-design-v0.4.1` 指向 `8f87279`，Release 运行 `41797477955` 成功：`metadata`、`linux-amd64`、`linux-arm64`、`darwin`（amd64／arm64）、`windows-amd64` 五个构建全部通过，`release` job 产出并校验五个平台压缩包与 `checksums.txt`。`0.4.0` 的 tag 因同一 Windows 失败未产出 Release。
- Release 已从 draft 发布（非草稿、非预发布），资产：`open-design_0.4.1_{linux_amd64,linux_arm64,darwin_amd64,darwin_arm64,windows_amd64}.zip` 与 `checksums.txt`。下载 `open-design_0.4.1_linux_amd64.zip` 复算 SHA256 与 `checksums.txt` 一致（`66a1c963…85fad`）。

## 0.4.0 本轮验收

2026-11-04 完成源码验收。`0.4.0` 在 `0.3.0` 上新增可编辑的重试调度／错误规则：规则随每个凭据写入宿主 auth 文件的 `request_scoped_errors`，由 CPA 的 request-scoped 机制决定停止或换号、是否冷却。插件不做并发限制，也不做请求排队。

| 检查 | 结果与范围 |
| --- | --- |
| 全仓 Go | Go 1.26.4 的 `go test ./...`、`go test -race ./... -count=1`、`go vet ./...` 全部通过 |
| Node | `node --test tests/*.test.cjs`，152/152 通过 |
| 源码 | Go 格式、四个管理 JS 的语法、`git diff --check` 通过 |
| 原生 ABI | ctypes 冒烟 PASS，注册版本严格读取源码 `Version`；`management_v03` 含 `error_rules_roundtrip_and_sync` |
| 原版 CPA | 8.0.48（`dbb70621-hotfix3`）离线加载与 loopback CONNECT／私有 CA TLS 全流程 PASS |
| 真实宿主换号 | 双凭据 + 假上游生产 400 `tier_limit_exceeded`：命中规则后换到另一账号并返回 200，冷却后跳过失败账号，`stop` 规则下不换号 |
| Microsoft Edge | 153.0.4234.48、隔离 profile、两入口 592/592；含错误规则对话框阶段与双主题截图 |
| Debian 12 | 与 CI 完全相同的镜像／步骤／参数重新构建，结果与 CI 产物逐字节相同 |
| ops | 28 项离线部署回归通过；guard 版本 0.4.0 |

### 错误规则专项验证

- **匹配对象**：规则匹配插件生成的错误 Message，不是上游响应体。上游错误、协议错误、传输错误、插件不可用和响应过大分别带 `[open-design:<kind>]` 前缀；marker 只由封闭白名单拼出，上游文本无法伪造规则。测试含「未知 code 不回显」用例。
- **分类**：`tier_limit_exceeded` 纳入受控已知码；上游写在消息里的 `[code=...]` 只在白名单内识别，其余一律归为 `upstream_error`。
- **裸 402 的处置**：只有带白名单码（`insufficient_quota` 的 `code` 字段或 `[code=...]` 文本）的 402 才分类并触发换号冷却；不带机器码的 402 保持 `upstream_error`，不匹配任何默认规则，因此不换号、不冷却。生产 2026-11-01～04 共出现 6 次这样的 402（模型 `deepseek-v4.1-flash`，`err=[open-design:upstream_error] upstream request failed: payment required`），同一批账号的 200 请求占 496/500，未发现额度耗尽证据；把上游的自由文本映射成额度判定会误冷却健康账号，故不改默认策略。判定由 `TestErrorRulesBarePaymentRequiredStaysUnclassified` 与 `TestErrorRulesClassifyPaymentRequiredWithQuotaCode` 锁定。
- **存储**：策略保存在 `<data_dir>/error-rules.json`，严格解析（拒绝未知字段、重复键、尾 JSON、超量条件、无效正则），原子写；文件损坏回退默认并上报 `load_error`，失败写不覆盖旧策略。
- **同步**：`GET`／`PUT /error-rules` 与 `POST /error-rules/{sync,reset}`。同步只重写每个凭据的 `request_scoped_errors` 一个键，保留宿主或运维新增的未知字段（测试覆盖 `operator_note`），逐账号独立报告失败，失败可重试恢复。
- **空数组语义**：空规则表示不写插件自有规则，由 CPA 配置决定，UI 明确显示该边界。
- **真实宿主轮换**（`--cpa-live`）：假上游按 Bearer 返回一次 400 `tier_limit_exceeded`。客户端收到 200，假上游记录两次尝试、两个不同账号；随后的请求只使用健康账号，证明冷却生效；把策略改成 `stop` 后同一故障只尝试一次并把带 marker 的 400 透传给客户端。
- **UI 接线**：Edge 验收发现并修复了「错误规则」按钮与对话框内按钮没有绑定点击事件的问题（Node 用例原先直接调用 handlers，绕过了 DOM 绑定）。修复后在 `bind()` 中接线，并补 2 项 Node 用例锁定该行为。

### 0.4.0 交付产物

- 功能与版本提交 `6109fefdda471ca408053c705602686d36675d47` 已推送到 `origin/open-design`；精确提交 [CI 41776632744](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/41776632744) 成功，下载 artifact `open-design-linux-amd64`。
- 交付库 SHA256 `d1febadb0843c4a29682320fa5f5ed424abfd0d3633bb2dec3fd3ac5476fabda`，最高 GLIBC 2.34。用与 CI 相同的 `golang:1.26-bookworm` 镜像、相同步骤与参数在本机重建，结果与该 artifact 逐字节相同（本机无法直连 `proxy.golang.org`，改为挂载本机模块缓存，其余命令一致）。
- 交付目录 `dist/open-design-0.4.0-linux-amd64/` 含 `.so`、`SHA256SUMS`、`manifest.json` 与 `abi-report.json`；`dist/open-design_0.4.0_linux_amd64.zip` 仅含 `open-design.so`，ZIP SHA256 `2ea49aa2ba6d7844e7c1d578d87fef8123626cbccbde35278cc99f1551056b0e`。GitHub Release 尚未发布。
### 0.4.0 生产交付与只读核验

- 通过 ops `scripts/deploy-cpa-plugin-open-design.sh --local --yes --dry-run` 只读预检后，以同脚本 `--local --yes` 安装到 `nuoapi-net`，两次都取得退出码 0（`0.3.0` 那轮出现的 60 秒等待上限没有复现）。备份 `/srv/cliproxy/_backups/open-design-20261104T052345Z`（`0700`／`0600`），旧库 `fa4b80e4…d0262f6` 与 `0.3.0` 交付库一致。
- 部署后独立只读核验：`GET /status` 的 `version` 为 `0.4.0`，`registered` 与 `effective_enabled` 为 true；两个控制台入口 200，含五选项卡与错误规则对话框，HTML SHA256 `79206ae236dbbffa0f91618b90688f0788731585ba0082de64265448054adc0d` 与本地按 `ui.go` 方式拼装的源码字节相同。
- 错误规则：`GET /error-rules` 返回 8 条默认策略，`sync={"total":4,"succeeded":4,"failed":[]}`；4 个 OpenDesign auth 文件均带 `request_scoped_errors`，各 8 条，状态码覆盖 400／401／402／403／429／500／503。
- 边界核对：宿主 `exe`、`config.yaml`、`settings.json`、模型映射、凭据数（4）与模型数（1）全部与部署前相同；只有 `open-design.so` 从 `fa4b80e4…` 换成 `d1febadb…`，以及 auth 文件新增 `request_scoped_errors` 字段。本轮没有任何真实推理或额度查询。

本轮证据：`/tmp/open-design-04-{go,race,vet,node}.log`、`/tmp/open-design-04-abi.json`、`/tmp/open-design-04-host.json`、`/tmp/open-design-04-edge/report.json`、`/tmp/open-design-04-ci-abi.json`、`/tmp/od04-exact-out/open-design.so`、`/tmp/od04-ci/open-design.so`。浏览器脚本为 `/tmp/open_design_04_edge_acceptance.py`，不随构建产物分发。

## 0.3.0 本轮验收

2026-11-03 完成源码验收与 `nuoapi-net` 生产升级。`0.3.0` 在 `0.2.0` 上新增请求日志／统计、官方模型级覆盖、显式模型测试、插件配置与 runtime，并将控制台整理为五个选项卡。

| 检查 | 结果与范围 |
| --- | --- |
| 全仓 Go | Go 1.26.4 的 `go test ./...`、`go test -race ./... -count=1`、`go vet ./...` 全部通过 |
| Node | `node --test tests/*.test.cjs`，139/139 通过，其中 OpenDesign 76 项 |
| 源码 | Go 格式、三个管理 JS 的语法、`git diff --check` 通过 |
| Debian 12 | Go 1.26.10、CGO、非 root、关闭网络，全仓 Go 测试与 Linux amd64 c-shared 构建通过 |
| CI 交付库 | SHA256 `fa4b80e4e67ced901fa2596984bcee7c79729cb59f2aaf833a7fabe74d0262f6`，最高 GLIBC 2.34；与本地 Debian 12 库逐字节相同 |
| 原生 ABI | 同一 Debian 12 库 ctypes 冒烟 PASS，注册版本严格读取源码 `Version` |
| 原版 CPA | 8.0.48（`dbb70621`）离线加载与 loopback CONNECT／私有 CA TLS 全流程 PASS |
| Microsoft Edge | 153.0.4234.48、隔离 profile、两入口 540/540；五选项卡 × 浅深色 × 1280/390 宽，共 40 个布局，无横向溢出／页面脚本错误，6 张代表截图 |
| ops | 28 项离线部署回归通过，脚本语法通过；guard 版本 0.3.0 |

持久化专项覆盖暂存写入／短写／同步／关闭失败、清除提交后收尾错误、损坏 snapshot／journal、超大 journal、序号间隙、并发新增、取消重试任务与 shutdown flush。管理一致性专项覆盖完整文本先脱敏再截断、旧 API／control Key 与 Workspace、runtime 换 Key 跨宿主读取、credential ABA、上游尝试事实／真实 429、关闭后动态路由 503 和阻塞保存等待。

扩展 ctypes 与原版 CPA 冒烟均断言：日志／统计／runtime 普通读取不访问上游；插件配置仅日志字段；模型覆盖传官方真实 `modelId`、使用 control Bearer、与共享额度缓存隔离且节流；显式模型测试仅一次小型非流式请求、不回 completion；日志只含 allowlist、缺 usage 保持 null、清除后统计为空。ctypes 另外验证 quiesce 的动态路由门禁、关闭后安全调用与空日志快照持久化。

Edge 验证包含 ARIA／键盘焦点、tab 切换零请求、主题跟随系统与切换、统计输入／输出和分模型数据、nullable usage、配置范围、长达 512 字节模型名、显式确认的取消／接受、保存映射后撤销结果、旧会话迟到响应、401 恢复及 XSS 安全文本。首轮发现的系统主题标签未更新和长 runtime 模型撑开配置页已修复，完整复跑通过。浏览器 fixture 不构成真实额度或真实付费推理证据。

### 0.3.0 提交与生产交付

- 功能与版本提交 `2601888247006bbccc1fbc10c4ae899cbb1176e9` 已推送到 `origin/open-design`；精确提交 [CI 41639144166](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/41639144166) 成功，下载 artifact `open-design-linux-amd64`。CI 库与上述离线验收库 SHA256 完全一致。
- 本地交付目录 `dist/open-design-0.3.0-linux-amd64/` 包含 `.so`、SHA256SUMS、manifest 与 ABI 报告；`dist/open-design_0.3.0_linux_amd64.zip` 仅包含 `open-design.so`，ZIP SHA256 为 `84da7276ad744b67296c42af3635d6e5e7dd8150691b9edde7349d9d1c779e99`。GitHub Release 尚未发布。
- 通过 ops `scripts/deploy-cpa-plugin-open-design.sh --local --yes` 安装到 `nuoapi-net`，先执行同脚本 `--dry-run`。生产备份 `/srv/cliproxy/_backups/open-design-20261103T125844Z` 的旧库／配置哈希与 manifest 一致，目录 `0700`、文件 `0600`；旧库 SHA256 保持历史 `dc2c00be...a9accc`。
- 本地命令达到 60 秒等待上限，未取得最终退出码；随后独立只读核验确认部署锁已释放、服务 active、OpenDesign `0.3.0` registered／effective-enabled。两条控制台入口均 200、含五选项卡，HTML SHA256 `109b438dbcc53a813b4295afa2ce931531633b52c53fc194b001f79dcc422462` 与已验收 UI 完全一致。
- `/config`、`/runtime`、`/request-logs`、`/request-stats` 可读取，宿主账户能力可用。日志策略为默认 7 天／500 条。生产基线与部署后均为 **1 凭据、1 映射**；映射、settings、OpenDesign auth 文件哈希均相同。此前历史记录中的 13 映射不代表本次部署前状态。
- CPA 宿主 SHA256 `674052226d4dbb501c873b54c72b71ae2020c2b843f2d986b2156e92c6168c9a` 保持一致；配置去除本插件节后与备份完全相等。`clinepassbridge`、`grok-panel`、`grok-token-ledger` 均保持 registered／effective-enabled 且动态库哈希一致，`quota-pacer` 保持原禁用状态。

本轮模型测试和推理验收均使用离线 fixture；生产验收使用元数据与静态页面读取，不作为新增真实付费推理或额度消费预测证据。旧 `0.2.0` 记录保留原日期、提交、hash 和 CI，均为历史。

本轮本地证据：`/tmp/open-design-03-{go,race,vet,node,bookworm,ops}.log`、`/tmp/open-design-03-final-abi.{json,log}`、`/tmp/open-design-03-edge/report.json`、`/tmp/open-design-03-deploy-{preflight,live}.log`、`/tmp/open-design-03-production-{before,after}.json`、`/tmp/open-design-03-backup.json`。浏览器脚本为 `/tmp/open_design_03_edge_acceptance.py`，不随构建产物分发。

以下为**历史**记录。

## 0.2.0 验证记录（历史）

2026-11-02 完成功能验收、修复与生产升级。`0.2.0` 的提交 `9fd8351` 已推送，在对应 CI 成功后通过 ops 部署脚本上线 `nuoapi-net`；GitHub Release 尚未发布。开发阶段的账户写入、Key 更换、额度错误和竞态均在隔离环境验证；后续获准上线后，仅给与 od1 完整推理 Key 精确匹配的现有账户补充额度授权并实际查询成功。

## 环境与产物（历史）

- 保留 ClinePassBridge v0.4.3（`8ddb9eac5c0cc0c1485671d27373d2210ec2a0c9`）历史与 MIT 版权；实际 Provider 为 `internal/opendesign`。
- 使用公开 C shared ABI v1 和 host HTTP / translation / public RPC，不修改、打包或发布 CPA 宿主。
- 隔离集成使用原版 CPA 8.0.48，commit `dbb70621`；测试宿主 SHA256 为 `74adb62eb8619d2478f546945dc78f42b3e95efce66dd2fadaa41eddc7c1f32a`。
- 最终交付库在 Debian 12（bookworm）中使用 Go 1.26.4、CGO、GCC 构建，Linux amd64；动态符号包含四个插件 ABI 入口，最高所需 GLIBC 为 **2.34**。
- 后端功能以 `fbf9159` 的验收为基线；随后 `2a1295e` 仅收紧可选配置与额度展示，折叠窗口趋势并标明未指定模型的资金预检。交付库已重新构建，使用 `-trimpath -buildvcs=false -buildmode=c-shared -ldflags='-s -w'`。

| 交付项 | 值 |
| --- | --- |
| 动态库 | `dist/open-design-0.2.0-linux-amd64/open-design.so` |
| 插件版本 | `0.2.0` |
| SHA256 | `dc2c00bece347f4f6d0f1ed642efa86d904bd67349d8909559205a5619a9accc` |
| 系统与架构 | ELF shared object / Linux amd64 |
| 最低符号要求 | GLIBC 2.34 |

## 最终检查（历史）

| 检查 | 结果与范围 |
| --- | --- |
| 全仓 Go race | `go test -race -timeout 120s ./...` 通过 |
| Go vet | `go vet ./...` 通过 |
| Node | `node --test tests/*.test.cjs`，115/115 通过 |
| 本机构建 | `make build` 通过 |
| Debian 12 | 非 root 容器内全仓 Go 测试、共享库构建通过，网络关闭 |
| ctypes ABI | 对上表最终库运行，PASS |
| 原版 CPA 加载 | 同一最终库，`--cpa-binary` PASS |
| 原版 CPA 假 TLS 全流程 | 同一最终库，`--cpa-live` PASS |
| Edge 交互 | 两条入口共 122 项断言通过（账号 48、更新后额度 58、复审 16），见下文 |
| 独立复审 | 原 4 项后端 P2、3 项 UI P2 均按最终源码关闭，未发现新的 P1/P2 |
| ops 离线部署检查 | 28 项通过；随后通过规定脚本完成生产 `0.2.0` 升级，来源／备份／注册／版本／控制台检查通过 |

> 上述计数、hash 与 CI 均属于 `0.2.0`（2026-11-02），不是 `0.3.0` 的验收证据。

2026-11-02 的补充运维展示调整通过全仓 `go test -timeout 90s ./...`、Node 115/115、Edge 额度 58 项，以及新交付库的 ctypes／原版 CPA 加载／假 TLS 全流程。未修改 Go 后端算法；后端 race 与 vet 的证据沿用前一轮验收，未把 UI 调整后的普通 Go 测试称为再次 race。新库仍为 Debian 12、Go 1.26.4、非 root、关闭网络构建。

最终 race 在普通用户下实际执行了 unlink 权限失败回归。Debian 12 测试也以非 root 用户运行，没有依赖 root 绕过目录权限。

可重复的检查命令：

```bash
go test -race -timeout 120s ./...
go vet ./...
node --test tests/*.test.cjs
make build
python3 scripts/smoke-abi.py dist/open-design-0.2.0-linux-amd64/open-design.so \
  --cpa-binary=/path/to/cli-proxy-api \
  --cpa-live=/path/to/cli-proxy-api \
  --json-report /tmp/open-design-abi-0.2-final.json
```

Go／Node 测试包含凭据解析重入、写盘失败时不发布新凭据、旧 watcher 回调隔离、SSE 及结构化错误 Key 脱敏、真实阻塞 operation 超时取消、刷新槽释放、缓存 epoch、singleflight、迟到响应、精确十进制解析、预测条件、真实零剩余、窗口到期和 UI 认证兼容。两位独立复审只执行源码核对；上述测试结果由主代理执行，不计为复审者独立测试。

## Edge 交互与布局（历史）

使用独立临时 Microsoft Edge profile 和本地假管理 API，验证两条入口：

- `/v0/resource/plugins/open-design/console`
- `/v0/management/open-design/console.html`

| 验收组 | 最终断言 | 主要内容 |
| --- | --- | --- |
| 账号与模型 | 48 | 创建、编辑、替换／保留 Key，搜索、可见项批选、默认前缀、共享映射、草稿保护、启停、删除及认证分流 |
| 额度与预测 | 58 | 仅钱包配置、独立 control Key、无自动查询、精确大 credits、USD／credits 分离、部分成功、未知／陈旧、多账户并发、迟到响应和清除授权；补充趋势默认折叠／展开、官方零剩余直接可见、未指定模型的预检说明 |
| 复审回归 | 16 | 失败请求清空两种 Key；操作成功但状态读取失败仍显示已确认结果；外部同字段换 Key 撤下旧额度 |

账号／模型与额度两组分别保存 8 张截图，覆盖两条入口、浅／深色、1280 与 390 宽度；无横向溢出、无页面脚本错误。生产只读基线另有相同组合的 8 张截图。浏览器验收覆盖控制台本身；公开路由与宿主的衔接由原版 CPA 集成检查覆盖。

本轮 Edge 发现并修复了真实 DOM 中 `HTMLCollection` 不支持 `.find()` 导致的保存崩溃；单元测试中的假 DOM 已同步模拟该集合类型。

## 原版 CPA 隔离流程（历史）

最终 `--cpa-live` 使用 loopback CONNECT 代理、一次性 CA 和本地 TLS 假上游。信任仅传给测试 CPA 子进程，不改系统信任库或 hosts。常规 allowlist 仅包括官方推理与控制面 origin；非 allowlist 的 CONNECT 实测返回 403、未建立隧道。重定向 probe 仅临时放行，完成后恢复 allowlist。

最终库通过以下流程：

- 创建凭据 → 官方形状的模型发现 → 保存映射 → `/v1/models` 出现别名；JSON 与 SSE 保留别名、工具参数及 usage。
- 停用撤回模型，启用后恢复且可调用；删除后 auth-dir 清空、别名撤回，quota 返回 404。
- 401／429 分别使用独立 CPA 进程，确认请求到达假上游并保留状态码，避免宿主冷却掩盖测试。
- 未配置 control Key 时，普通 GET 和手动 refresh 均为零额度请求。
- 配置后首次刷新严格为钱包与 funding-check 两个 GET，使用独立 control Bearer，通过 CONNECT／TLS 到达 `amr-api.open-design.ai`；不传 `modelId`。
- 钱包 `"12.345"`、`updatedAt:null` 和完整官方套餐窗口可解析，响应仅含允许字段；首次预测为 `insufficient_samples`。
- 普通缓存读取和立即再次刷新均不增加上游请求，节流时间保留。funding 403、wallet 401 和无效套餐响应分别产生正确组件错误及部分可用状态。
- 清除授权后缓存消失且零额度请求，control Key 从 auth-dir 移除；原推理 Key、别名与假上游推理仍可用。
- status、credentials、models、console、`/v1/models`、quota、chat、清除授权响应及测试报告均检查 control Key 不泄漏。
- 跨域 307 首跳确实携带 Bearer；原版 CPA 跟随跳转，跨域后去掉 `Authorization`。不据此声称插件阻止所有重定向。

30 秒总预算和官方零剩余的回归在 Go 路由／operation 测试中验证；Edge 的预测来自假 API，不能将截图当作真实账单或真实余额预测成功。

## 真实服务验证，按时间区分（历史）

下表中 2026-11-02 的记录均属 `0.1.0`／`0.2.0` 历史，不应被解读为 `0.3.0` 已完成验证。

| 阶段 | 已获得的证据 | 结论边界 |
| --- | --- | --- |
| `0.1.0` 初期提供的测试 Key | 两次 `GET /v1/models` 为 `401 invalid_api_key`，`api key not found` | 当时凭据无效，不代表后续有效凭据也失败 |
| 2026-11-02 首次生产交付 | 有效推理 Key 发现 13 个模型；官方直连与生产插件各完成一次最小 JSON 推理，均为 HTTP 200／`OK` | 生产模型 `open-design-minimax-m3.1-flash`，usage 219／3／222；未逐个付费调用 13 个模型 |
| 本轮 `0.2.0` 开发验收早期 | 现有推理 Key 只读请求控制面钱包和账单汇总均为 HTTP 401 | 当时未提供 control 授权；不能否定已成功的推理授权 |
| 2026-11-02 19:07（北京时间）control 授权补验 | 用户下载文件中的 od1／od2 两组授权，各直接 GET 钱包与指定工作区 funding-check，四次均 HTTP 200；字段契约核对和工作区匹配通过 | 两钱包各 `1.5000` USD，均 Go 套餐；5 小时均剩 `50000/50000` credits，7 天分别剩 `149998/150000`、`150000/150000`；仅直接只读查询，未配置或调用生产新版插件 |
| 升级前生产只读复核 | 服务 active，插件 `0.1.0`，1 凭据／13 映射，全部别名带前缀，控制台标题及宿主／插件哈希与原基线一致 | 旧版升级前的基线 |
| 2026-11-02 21:52（北京时间）升级 | 已推送提交 `9fd8351`、CI `41455903714` 成功；经规定脚本安装上表交付库，插件版本 `0.2.0`、四个原有效插件及服务正常 | 宿主哈希、原 13 映射保持一致；未重复付费推理 |
| 2026-11-02 21:56（北京时间）生产额度 | od1 完整推理 Key 与唯一现有凭据在服务器内精确匹配；通过 Management API 补 control Key／工作区后，插件 quota HTTP 200，钱包与套餐均 `available` | 钱包 `1.5000` USD，5 小时剩 `50000/50000`、7 天剩 `149998/150000` credits；首次样本为 `insufficient_samples`，od2 未新增 |

额度协议绑定 OpenDesign 官方客户端 commit `5a0c47777975ddb91d09c1c16b423a3ef74bf572`，来源和预测定义见 [quota.md](quota.md)。现有推理 Key 对控制面的 401、有效 control 授权的直接读取、插件隔离成功、旧版生产推理及新版生产额度成功分别记录。真实消费采样预测仍未验收；未为此额外发起计费推理。账号匹配使用完整 Key 比对，不根据标签推断；预测不影响 CPA 调度、账号启停或冷却。

本次升级 CI：[41455903714](https://github.com/ruruamour/cpa-plugin-open-design/actions/runs/41455903714)，部署源码为 `9fd8351b5f0dbd7fbe8af33483b3cf81bf87fb71`。服务器备份为 `/srv/cliproxy/_backups/open-design-20261102T135234Z`；授权保存前的原 auth 文件备份在其 `quota-authorization/` 子目录（目录 `0700`、文件 `0600`，仅留在服务器）。插件回滚恢复 so/config，额度授权需单独恢复，不能把两者视为同一范围。

上线后的独立 Edge 检查通过两条控制台入口、浅／深色、1280／390 共 8 个布局：版本、两个可选字段、真实余额／窗口、折叠趋势均正确，无溢出或脚本错误。页面使用服务器实时 HTML／status／quota 缓存，GET-only SSH relay 将测试中的刷新点击映射为缓存读取，没有追加额度上游请求；真正的首次刷新证据是上表的生产 Management API 查询。

## 本地证据索引（历史）

以下路径属于 `0.2.0` 工作环境，不随源码分发，不含真实 Key。`0.3.0` 的最终证据路径由主线在验收后补写。

- `/tmp/open-design-race-0.2.log`、`/tmp/open-design-vet-0.2.log`、`/tmp/open-design-node-0.2.log`
- `/tmp/open-design-bookworm-build-0.2.log`（测试通过；首次产物目录中的既有文件权限阻止 header 复制）、`/tmp/open-design-bookworm-artifact-0.2.log`（换用新目录后构建成功）
- `/tmp/open-design-abi-0.2-final.json`、`/tmp/open-design-abi-0.2-final.log`
- `/tmp/open-design-edge-model-final.log`、`/tmp/open-design-edge-quota.log`、`/tmp/open-design-edge-review.log`
- `/tmp/open-design-visual-updated/`、`/tmp/open-design-visual-quota/`、`/tmp/open-design-visual-review/report.json`
- `/tmp/open-design-production-readonly-0.2.json`、`/tmp/open-design-ops-0.2.log`
- `/tmp/open-design-real-quota-20261102.json`、`/tmp/open-design-real-quota-summary-20261102.json`（两账户官方直接只读查询）
- `/tmp/open-design-go-ops-clarity-0.2.log`、`/tmp/open-design-node-ops-clarity-0.2.log`
- `/tmp/open-design-visual-quota-ops-clarity/report.json`、`/tmp/open-design-edge-quota-ops-clarity.log`
- `/tmp/open-design-bookworm-ops-clarity-0.2.log`、`/tmp/open-design-abi-ops-clarity-0.2.json`、`/tmp/open-design-abi-ops-clarity-0.2.log`
- `/tmp/open-design-deploy-0.2-preflight.log`、`/tmp/open-design-deploy-0.2-live.log`
- `/tmp/open-design-predeploy-match-0.2.json`、`/tmp/open-design-production-quota-0.2.json`、`/tmp/open-design-production-final-0.2.json`
- `/tmp/open-design-visual-production-0.2/report.json`、`/tmp/open-design-edge-production-0.2.log`
