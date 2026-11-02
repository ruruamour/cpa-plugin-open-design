# OpenDesign 0.2.0 验证记录

2026-11-02 完成本轮功能验收与修复。`0.2.0` 的本地交付库已构建，尚未 push、发布或部署；生产继续运行 `0.1.0`。本轮生产保持只读，账户写入、Key 更换、额度错误和竞态均在隔离环境验证。

## 环境与产物

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

## 最终检查

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
| ops 离线部署检查 | 28 项通过；脚本已准备版本 `0.2.0`，没有执行部署 |

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

## Edge 交互与布局

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

## 原版 CPA 隔离流程

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

## 真实服务验证，按时间区分

| 阶段 | 已获得的证据 | 结论边界 |
| --- | --- | --- |
| `0.1.0` 初期提供的测试 Key | 两次 `GET /v1/models` 为 `401 invalid_api_key`，`api key not found` | 当时凭据无效，不代表后续有效凭据也失败 |
| 2026-11-02 首次生产交付 | 有效推理 Key 发现 13 个模型；官方直连与生产插件各完成一次最小 JSON 推理，均为 HTTP 200／`OK` | 生产模型 `open-design-minimax-m3.1-flash`，usage 219／3／222；未逐个付费调用 13 个模型 |
| 本轮 `0.2.0` 开发验收早期 | 现有推理 Key 只读请求控制面钱包和账单汇总均为 HTTP 401 | 当时未提供 control 授权；不能否定已成功的推理授权 |
| 2026-11-02 19:07（北京时间）control 授权补验 | 用户下载文件中的 od1／od2 两组授权，各直接 GET 钱包与指定工作区 funding-check，四次均 HTTP 200；字段契约核对和工作区匹配通过 | 两钱包各 `1.5000` USD，均 Go 套餐；5 小时均剩 `50000/50000` credits，7 天分别剩 `149998/150000`、`150000/150000`；仅直接只读查询，未配置或调用生产新版插件 |
| 本轮生产只读复核 | 服务 active，插件 `0.1.0`，1 凭据／13 映射，全部别名带前缀，控制台标题及宿主／插件哈希与原基线一致 | 只证明旧版生产状态保持，没有部署本次新版 |

额度协议绑定 OpenDesign 官方客户端 commit `5a0c47777975ddb91d09c1c16b423a3ef74bf572`，来源和预测定义见 [quota.md](quota.md)。现有推理 Key 对控制面的 401、有效 control 授权的直接读取、插件隔离成功与旧版生产推理成功分别记录。真实消费采样预测仍未验收；未为此额外发起计费推理。下载文件标签与生产凭据的身份对应关系尚未确认；预测不影响 CPA 调度、账号启停或冷却。

## 本地证据索引

以下路径为本次工作环境的报告，不随源码分发，不含真实 Key：

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
